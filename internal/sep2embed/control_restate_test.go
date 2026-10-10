package sep2embed

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// restateStep is one write in a restatement sequence: a direct send when dur
// is non-zero, otherwise a bus delta. at is seconds after t0.
type restateStep struct {
	label string
	at    int64
	delta diff.Difference
	dur   uint32
}

// restateCase is a sequence of writes on device A and what the last write
// must report, what must run, and what must be served.
type restateCase struct {
	name  string
	steps []restateStep
	// want is the outcome of the last step, which is always a bus delta.
	want ControlOutcome
	// watts maps seconds after t0 to the effective opModTargetW.
	watts map[int64]int
	// statuses maps a control label to its served status. A send labelled
	// "x" yields "x" and "x follow-on"; a bus delta yields its own label.
	statuses map[string]wantStatus
}

func otherModeDelta(field string, value any) diff.Difference {
	return diff.Difference{Object: "mrid-a", Attribute: derControlAttributePrefix + field, Value: value}
}

// runRestateCase plays tc against a fresh device and asserts every
// expectation on the stored controls, which are what the listener serves.
func runRestateCase(t *testing.T, tc restateCase) {
	t.Helper()
	const t0 = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, t0)
	ids := map[string]string{}
	var last ControlOutcome
	for i, st := range tc.steps {
		clock.set(t0 + st.at)
		before := storedIDs(t, e, scope)
		if st.dur != 0 {
			send, err := e.ApplyControlFor(context.Background(), reg, st.delta, st.dur)
			if err != nil {
				t.Fatalf("step %d (%s) send: %v", i, st.label, err)
			}
			ids[st.label], ids[st.label+" follow-on"] = send.ControlID, send.FollowOnID
			continue
		}
		out, err := e.ApplyControlDeltaOutcome(context.Background(), reg, st.delta)
		if err != nil {
			t.Fatalf("step %d (%s) bus delta: %v", i, st.label, err)
		}
		var added []string
		for _, id := range storedIDs(t, e, scope) {
			if !slices.Contains(before, id) {
				added = append(added, id)
			}
		}
		switch {
		case out == ControlIssued && len(added) == 1:
			ids[st.label] = added[0]
		case out == ControlRestated && len(added) == 0:
		default:
			t.Fatalf("step %d (%s): outcome %v wrote %d controls", i, st.label, out, len(added))
		}
		last = out
	}
	if last != tc.want {
		t.Fatalf("last bus delta outcome = %v, want %v", last, tc.want)
	}

	controls := storedControls(t, e, scope)
	var wControls []sep2.DERControl
	for _, c := range controls {
		if c.DERControlBase != nil && c.DERControlBase.OpModTargetW != nil {
			wControls = append(wControls, c)
		}
	}
	for at, want := range tc.watts {
		if got := targetW(t, effectiveAt(wControls, t0+at)); got != want {
			t.Errorf("effective W at t0+%d = %d, want %d", at, got, want)
		}
	}
	for label, want := range tc.statuses {
		id, ok := ids[label]
		if !ok {
			t.Fatalf("no control labelled %q was written", label)
		}
		assertEventStatus(t, label, controlByID(t, controls, id), want)
	}
}

func storedIDs(t *testing.T, e *Embed, scope string) []string {
	t.Helper()
	var ids []string
	for _, c := range storedControls(t, e, scope) {
		ids = append(ids, derControlID(c.CreationTime, c.MRID))
	}
	return ids
}

// During a page send any bus watts delta takes over, even one whose value
// equals the send's: a pending 0 W follow-on is a command the delta overturns,
// so it is not a restatement. Once nothing is pending, an equal value is.
func TestBusRestatementAgainstPendingFollowOn(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	w := func(v float64) diff.Difference { return targetWDelta("mrid-a", 0, v) }
	cases := []restateCase{
		{
			name:  "A equal value during a short send",
			steps: []restateStep{{"send", 0, w(5000), 300}, {"bus", 10, w(5000), 0}},
			want:  ControlIssued,
			watts: map[int64]int{10: 5000, 300: 5000, 1809: 5000, 1810: noControl},
			statuses: map[string]wantStatus{
				"send":           {sep2.EventStatusSuperseded, t0 + 10},
				"send follow-on": {sep2.EventStatusSuperseded, t0 + 10},
				"bus":            {sep2.EventStatusActive, t0 + 10},
			},
		},
		{
			name:  "B equal value during a long send",
			steps: []restateStep{{"send", 0, w(5000), 3600}, {"bus", 10, w(5000), 0}},
			want:  ControlIssued,
			watts: map[int64]int{10: 5000, 1809: 5000, 1810: noControl, 3600: noControl},
			statuses: map[string]wantStatus{
				"send":           {sep2.EventStatusSuperseded, t0 + 10},
				"send follow-on": {sep2.EventStatusCancelled, t0 + 10},
				"bus":            {sep2.EventStatusActive, t0 + 10},
			},
		},
		{
			name:  "C equal to a started follow-on",
			steps: []restateStep{{"send", 0, w(5000), 300}, {"bus", 300, w(0), 0}},
			want:  ControlRestated,
			watts: map[int64]int{300: 0, 2099: 0, 2100: noControl},
			statuses: map[string]wantStatus{
				"send follow-on": {sep2.EventStatusActive, t0 + 300},
			},
		},
		{
			name:  "D equal to a bus control in force",
			steps: []restateStep{{"first bus", 0, w(5000), 0}, {"bus", 10, w(5000), 0}},
			want:  ControlRestated,
			watts: map[int64]int{0: 5000, 1799: 5000, 1800: noControl},
			statuses: map[string]wantStatus{
				"first bus": {sep2.EventStatusActive, t0},
			},
		},
		{
			name: "E equal value after a repeated send",
			steps: []restateStep{
				{"first", 0, w(5000), 300},
				{"second", 10, w(5000), 300},
				{"bus", 20, w(5000), 0},
			},
			want:  ControlIssued,
			watts: map[int64]int{20: 5000, 1819: 5000, 1820: noControl},
			statuses: map[string]wantStatus{
				"first follow-on":  {sep2.EventStatusSuperseded, t0 + 10},
				"second":           {sep2.EventStatusSuperseded, t0 + 20},
				"second follow-on": {sep2.EventStatusSuperseded, t0 + 20},
				"bus":              {sep2.EventStatusActive, t0 + 20},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runRestateCase(t, tc)
		})
	}
}

// A bus delta on another mode is independent of opModTargetW (2018 rule t)
// p.91): it neither cancels nor supersedes the send's 0 W follow-on, and the
// pending follow-on does not stop it from being a restatement.
func TestOtherModeBusDeltaLeavesTheFollowOn(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	send := restateStep{"send", 0, targetWDelta("mrid-a", 0, 5000), 300}
	vars := otherModeDelta("opModTargetVar", map[string]any{"multiplier": 0.0, "value": 100.0})
	watts := map[int64]int{299: 5000, 300: 0, 2099: 0, 2100: noControl}
	statuses := map[string]wantStatus{
		"send":           {sep2.EventStatusActive, t0},
		"send follow-on": {sep2.EventStatusScheduled, t0},
		"bus":            {sep2.EventStatusActive, t0 + 10},
	}
	cases := []restateCase{
		{name: "F1 TargetVar", steps: []restateStep{send, {"bus", 10, vars, 0}}},
		{name: "F2 Connect false", steps: []restateStep{send, {"bus", 10, otherModeDelta("opModConnect", false), 0}}},
		{name: "F3 Energize false", steps: []restateStep{send, {"bus", 10, otherModeDelta("opModEnergize", false), 0}}},
	}
	for i := range cases {
		cases[i].want, cases[i].watts, cases[i].statuses = ControlIssued, watts, statuses
	}
	cases = append(cases, restateCase{
		name:     "F4 the same TargetVar again",
		steps:    []restateStep{send, {"bus", 10, vars, 0}, {"again", 20, vars, 0}},
		want:     ControlRestated,
		watts:    watts,
		statuses: statuses,
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runRestateCase(t, tc)
		})
	}
}

// syncBuf is a log writer safe to read while another goroutine logs.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A bus control is in service once committed, so a failed cancel of a
// non-overlapping follow-on is logged with that control's id and start, not
// returned: refusing would report a written control as refused.
// Not parallel: it swaps the process-wide log writer.
func TestBusDeltaLogsAFailedCancelAndKeepsItsControl(t *testing.T) {
	var logs syncBuf
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })

	const t0 = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, t0)
	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 5000), 3600)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	e.stores.DERControls = faultyDERControls{
		ScopedStore: e.stores.DERControls,
		failUpdate: func(_ string, c sep2.DERControl) bool {
			return c.EventStatus != nil && c.EventStatus.CurrentStatus == sep2.EventStatusCancelled
		},
	}
	clock.set(t0 + 10)
	out, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 3000))
	if err != nil || out != ControlIssued {
		t.Fatalf("bus delta = %v, %v; want ControlIssued and no error", out, err)
	}

	controls := storedControls(t, e, scope)
	if len(controls) != 3 {
		t.Fatalf("stored controls = %d, want 3", len(controls))
	}
	bus := controls[2]
	if got := targetW(t, &bus); got != 3000 {
		t.Fatalf("newest control = %d W, want the bus delta's 3000", got)
	}
	assertEventStatus(t, "bus control", bus, wantStatus{sep2.EventStatusActive, t0 + 10})
	assertEventStatus(t, "follow-on", controlByID(t, controls, send.FollowOnID), wantStatus{sep2.EventStatusScheduled, t0})

	line := logs.String()
	for _, want := range []string{"not cancelled", send.FollowOnID, "start " + strconv.FormatInt(t0+3600, 10), errInjected.Error()} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q does not name %q", line, want)
		}
	}
}

// malformedEndDevices lists one extra EndDevice whose href the sweep cannot
// map to a device, so the sweep fails with an error tied to no device.
type malformedEndDevices struct {
	store.EndDeviceStore
}

func (m malformedEndDevices) List(ctx context.Context, opts store.ListOptions) (store.ListResult[sep2.EndDevice], error) {
	res, err := m.EndDeviceStore.List(ctx, opts)
	if err != nil {
		return res, err
	}
	bad := sep2.EndDevice{}
	bad.Href = "/edev/bad/extra"
	res.Items = append(slices.Clip(res.Items), bad)
	return res, nil
}

// A sweep failure that names no device may involve any device, so it refuses
// both writers and writes nothing.
func TestSweepFailureOnNoDeviceRefusesTheWrite(t *testing.T) {
	t.Parallel()

	writers := map[string]func(e *Embed, reg *registry.Registry) error{
		"bus delta": func(e *Embed, reg *registry.Registry) error {
			_, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 700))
			return err
		},
		"direct send": func(e *Embed, reg *registry.Registry) error {
			_, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 700), 60)
			return err
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, reg, _, scope := newSendEmbed(t, controlClockUnix)
			e.stores.EndDevices = malformedEndDevices{e.stores.EndDevices}
			err := write(e, reg)
			var dev *deviceSweepError
			if err == nil || !strings.Contains(err.Error(), "/edev/bad/extra") || errors.As(err, &dev) {
				t.Fatalf("error = %v, want the sweep failure that names no device", err)
			}
			if n := len(storedControls(t, e, scope)); n != 0 {
				t.Errorf("stored controls = %d after a refused write, want 0", n)
			}
		})
	}
}
