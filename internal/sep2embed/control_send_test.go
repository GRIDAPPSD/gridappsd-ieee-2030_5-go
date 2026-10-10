package sep2embed

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// Tests for Embed.ApplyControlFor. They read the stored DERControls, which are
// the records the protocol listener serves, and assert interval, value,
// creationTime and EventStatus rather than only that a call succeeded.

// newSendEmbed builds an Embed over a seeded two-device store with a movable
// clock, without a listener: ApplyControlFor only needs the stores.
func newSendEmbed(t *testing.T, startUnix int64) (*Embed, *registry.Registry, *movableClock, string) {
	t.Helper()
	reg, st := twoDeviceFixture(t)
	clock := newMovableClock(startUnix)
	policy := testControlPolicy
	policy.Control.Now = clock.now
	e := &Embed{stores: st, policy: policy, ended: newEndedControlLedger()}
	return e, reg, clock, derControlScope(urlIndexFor(t, st, "mrid-a"), controlFSAID, controlDERProgramID)
}

func storedControls(t *testing.T, e *Embed, scope string) []sep2.DERControl {
	t.Helper()
	list, err := e.stores.DERControls.List(context.Background(), scope, store.ListOptions{Unbounded: true})
	if err != nil {
		t.Fatalf("DERControls.List(%q): %v", scope, err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return items[i].CreationTime < items[j].CreationTime })
	return items
}

func controlByID(t *testing.T, controls []sep2.DERControl, id string) sep2.DERControl {
	t.Helper()
	for _, c := range controls {
		if derControlID(c.CreationTime, c.MRID) == id {
			return c
		}
	}
	t.Fatalf("no stored control with id %q", id)
	return sep2.DERControl{}
}

// effectiveAt applies the client's rule to opModTargetW controls: a cancelled
// control never runs, a control overlapped anywhere in its window by a newer
// uncancelled one is superseded and never runs, and of the rest the one whose
// interval covers the instant runs. It returns nil when none covers it.
func effectiveAt(controls []sep2.DERControl, at int64) *sep2.DERControl {
	cancelled := func(c *sep2.DERControl) bool {
		s := c.EventStatus.CurrentStatus
		return s == sep2.EventStatusCancelled || s == eventStatusCancelledWithRandomization
	}
	var best *sep2.DERControl
	for i := range controls {
		c := &controls[i]
		if at < c.Interval.Start || at >= c.Interval.Start+int64(c.Interval.Duration) || cancelled(c) {
			continue
		}
		superseded := false
		for j := range controls {
			d := &controls[j]
			if d.CreationTime > c.CreationTime && !cancelled(d) &&
				d.Interval.Start < c.Interval.Start+int64(c.Interval.Duration) &&
				c.Interval.Start < d.Interval.Start+int64(d.Interval.Duration) {
				superseded = true
				break
			}
		}
		if superseded {
			continue
		}
		if best == nil || c.CreationTime > best.CreationTime {
			best = c
		}
	}
	return best
}

// noControl is the targetW a test expects when no control covers an instant,
// so the device runs its DefaultDERControl.
const noControl = math.MinInt

func targetW(t *testing.T, c *sep2.DERControl) int {
	t.Helper()
	if c == nil {
		return noControl
	}
	if c.DERControlBase == nil || c.DERControlBase.OpModTargetW == nil {
		t.Fatalf("control carries no opModTargetW: %+v", c)
	}
	return int(c.DERControlBase.OpModTargetW.Value)
}

func TestApplyControlForIssuesTheControlAndAZeroFollowOn(t *testing.T) {
	t.Parallel()

	const now = controlClockUnix
	e, reg, _, scope := newSendEmbed(t, now)

	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), 300)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}

	controls := storedControls(t, e, scope)
	if len(controls) != 2 {
		t.Fatalf("stored controls = %d, want 2 (requested + follow-on)", len(controls))
	}
	req := controlByID(t, controls, send.ControlID)
	zero := controlByID(t, controls, send.FollowOnID)

	if req.Interval.Start != now || req.Interval.Duration != 300 {
		t.Errorf("requested interval = %+v, want start %d duration 300", *req.Interval, now)
	}
	if got := *req.DERControlBase.OpModTargetW; got.Value != -2000 || got.Multiplier != 0 {
		t.Errorf("requested opModTargetW = %+v, want value -2000 multiplier 0 (charge is negative)", got)
	}
	if req.EventStatus.CurrentStatus != sep2.EventStatusActive {
		t.Errorf("requested status = %d, want Active", req.EventStatus.CurrentStatus)
	}

	if zero.Interval.Start != now+300 || zero.Interval.Duration != testControlSeed.Duration {
		t.Errorf("follow-on interval = %+v, want start %d duration %d (fleet default)", *zero.Interval, now+300, testControlSeed.Duration)
	}
	if got := *zero.DERControlBase.OpModTargetW; got.Value != 0 || got.Multiplier != 0 {
		t.Errorf("follow-on opModTargetW = %+v, want 0 W", got)
	}
	if zero.EventStatus.CurrentStatus != sep2.EventStatusScheduled {
		t.Errorf("follow-on status = %d, want Scheduled", zero.EventStatus.CurrentStatus)
	}
	if req.CreationTime == zero.CreationTime {
		t.Errorf("requested and follow-on share creationTime %d", req.CreationTime)
	}
	if req.MRID == zero.MRID {
		t.Errorf("requested and follow-on share mRID %q", req.MRID)
	}
	if send.Start != now || send.End != now+300 {
		t.Errorf("ControlSend start/end = %d/%d, want %d/%d", send.Start, send.End, now, now+300)
	}

	if got := targetW(t, effectiveAt(controls, now+299)); got != -2000 {
		t.Errorf("effective W one second before the end = %d, want -2000", got)
	}
	if got := targetW(t, effectiveAt(controls, now+300)); got != 0 {
		t.Errorf("effective W at the end = %d, want 0", got)
	}
}

func TestApplyControlForCarriesMultiplier(t *testing.T) {
	t.Parallel()

	e, reg, _, scope := newSendEmbed(t, controlClockUnix)
	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 1, 200), 60)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}
	req := controlByID(t, storedControls(t, e, scope), send.ControlID)
	if got := *req.DERControlBase.OpModTargetW; got.Value != 200 || got.Multiplier != 1 {
		t.Errorf("opModTargetW = %+v, want value 200 multiplier 1", got)
	}
}

func TestApplyControlForSupersedesLikeTheBusPath(t *testing.T) {
	t.Parallel()

	e, reg, clock, scope := newSendEmbed(t, controlClockUnix)
	if _, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 5000)); err != nil {
		t.Fatalf("bus control: %v", err)
	}
	clock.set(controlClockUnix + 10)
	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -1000), 120)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}

	controls := storedControls(t, e, scope)
	if len(controls) != 3 {
		t.Fatalf("stored controls = %d, want 3", len(controls))
	}
	bus := controls[0]
	if bus.EventStatus.CurrentStatus != sep2.EventStatusSuperseded {
		t.Errorf("bus control status = %d, want Superseded", bus.EventStatus.CurrentStatus)
	}
	if got := controlByID(t, controls, send.ControlID).EventStatus.CurrentStatus; got != sep2.EventStatusActive {
		t.Errorf("sent control status = %d, want Active", got)
	}
}

// A repeated 0 W send (a Stop sent twice) restates both the requested control
// and its follow-on; both must still be written so the second duration holds.
func TestApplyControlForIssuesARestatedSetpoint(t *testing.T) {
	t.Parallel()

	e, reg, clock, scope := newSendEmbed(t, controlClockUnix)
	if _, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 0), 300); err != nil {
		t.Fatalf("first send: %v", err)
	}
	clock.set(controlClockUnix + 5)
	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 0), 900)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	controls := storedControls(t, e, scope)
	if len(controls) != 4 {
		t.Fatalf("stored controls = %d, want 4 (restatement must not be swallowed)", len(controls))
	}
	if got := controlByID(t, controls, send.ControlID).Interval.Duration; got != 900 {
		t.Errorf("restated send duration = %d, want 900", got)
	}
	if got := controlByID(t, controls, send.FollowOnID).Interval.Start; got != controlClockUnix+5+900 {
		t.Errorf("restated follow-on start = %d, want %d", got, controlClockUnix+5+900)
	}
}

// wantStatus is the served EventStatus a test expects on one control.
type wantStatus struct {
	status uint8
	at     int64
}

func assertEventStatus(t *testing.T, name string, c sep2.DERControl, want wantStatus) {
	t.Helper()
	if c.EventStatus == nil {
		t.Fatalf("%s carries no EventStatus", name)
	}
	if got := *c.EventStatus; got.CurrentStatus != want.status || got.DateTime != want.at {
		t.Errorf("%s EventStatus = status %d dateTime %d, want status %d dateTime %d",
			name, got.CurrentStatus, got.DateTime, want.status, want.at)
	}
}

// A later send overlapping the first control takes over from its own start,
// and the first send's follow-on never overrides it. Every control's served
// status and dateTime is asserted: the first control is superseded at the
// later send's start (not at its follow-on's), a stale follow-on the later
// pair overlaps is superseded where the overlap begins, and one it does not
// overlap is cancelled at the later send.
func TestApplyControlForLaterSendIsNotOverriddenByStaleFollowOn(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	const fleet = int64(1800) // testControlSeed.Duration, the follow-on's window
	cases := []struct {
		name     string
		firstDur uint32
		laterDur uint32
		// instant -> watts that must run, per the client's creationTime rule.
		want map[int64]int
		// served EventStatus of the first control, its follow-on, the later
		// control and the later follow-on.
		first, firstFollow, later, laterFollow wantStatus
	}{
		{
			name:     "later send outlasts the first control",
			firstDur: 300,
			laterDur: 600,
			want:     map[int64]int{t0 + 150: 1500, t0 + 299: 1500, t0 + 300: 1500, t0 + 699: 1500, t0 + 700: 0},
			first:    wantStatus{sep2.EventStatusSuperseded, t0 + 100},
			// The later control itself overlaps the stale follow-on.
			firstFollow: wantStatus{sep2.EventStatusSuperseded, t0 + 100},
			later:       wantStatus{sep2.EventStatusActive, t0 + 100},
			laterFollow: wantStatus{sep2.EventStatusScheduled, t0 + 100},
		},
		{
			name:     "later send ends before the first control would have",
			firstDur: 300,
			laterDur: 60,
			want:     map[int64]int{t0 + 150: 1500, t0 + 160: 0, t0 + 300: 0},
			first:    wantStatus{sep2.EventStatusSuperseded, t0 + 100},
			// Only the later follow-on overlaps it, from t0+160.
			firstFollow: wantStatus{sep2.EventStatusSuperseded, t0 + 160},
			later:       wantStatus{sep2.EventStatusActive, t0 + 100},
			laterFollow: wantStatus{sep2.EventStatusScheduled, t0 + 100},
		},
		{
			name:     "later pair ends before the stale follow-on starts",
			firstDur: 3600,
			laterDur: 60,
			want: map[int64]int{
				t0 + 150: 1500, t0 + 160: 0, t0 + 160 + fleet - 1: 0,
				t0 + 160 + fleet: noControl, t0 + 3600: noControl,
			},
			first:       wantStatus{sep2.EventStatusSuperseded, t0 + 100},
			firstFollow: wantStatus{sep2.EventStatusCancelled, t0 + 100},
			later:       wantStatus{sep2.EventStatusActive, t0 + 100},
			laterFollow: wantStatus{sep2.EventStatusScheduled, t0 + 100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, reg, clock, scope := newSendEmbed(t, t0)
			first, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), tc.firstDur)
			if err != nil {
				t.Fatalf("first send: %v", err)
			}
			clock.set(t0 + 100)
			later, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 1500), tc.laterDur)
			if err != nil {
				t.Fatalf("later send: %v", err)
			}

			controls := storedControls(t, e, scope)
			if len(controls) != 4 {
				t.Fatalf("stored controls = %d, want 4", len(controls))
			}
			for at, want := range tc.want {
				if got := targetW(t, effectiveAt(controls, at)); got != want {
					t.Errorf("effective W at t0+%d = %d, want %d", at-t0, got, want)
				}
			}

			assertEventStatus(t, "first control", controlByID(t, controls, first.ControlID), tc.first)
			assertEventStatus(t, "first follow-on", controlByID(t, controls, first.FollowOnID), tc.firstFollow)
			assertEventStatus(t, "later control", controlByID(t, controls, later.ControlID), tc.later)
			assertEventStatus(t, "later follow-on", controlByID(t, controls, later.FollowOnID), tc.laterFollow)

			laterCtl := controlByID(t, controls, later.ControlID)
			if laterCtl.CreationTime <= controlByID(t, controls, first.FollowOnID).CreationTime {
				t.Errorf("later creationTime %d not newer than the stale follow-on's %d", laterCtl.CreationTime, controlByID(t, controls, first.FollowOnID).CreationTime)
			}
			if laterFollow := controlByID(t, controls, later.FollowOnID); laterFollow.CreationTime <= laterCtl.CreationTime {
				t.Errorf("later follow-on creationTime %d not newer than its control's %d", laterFollow.CreationTime, laterCtl.CreationTime)
			}
		})
	}
}

func TestApplyControlForRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		delta    diff.Difference
		duration uint32
		wantErr  error
	}{
		{"unknown device", targetWDelta("mrid-nope", 0, -2000), 300, ErrUnknownControlDevice},
		{"value above int16", targetWDelta("mrid-a", 0, 40000), 300, ErrControlValueInvalid},
		{"value below int16", targetWDelta("mrid-a", 0, -40000), 300, ErrControlValueInvalid},
		{"fractional value", targetWDelta("mrid-a", 0, 1.5), 300, ErrControlValueInvalid},
		{"zero duration", targetWDelta("mrid-a", 0, -2000), 0, ErrControlDurationInvalid},
		{"duration above the cap", targetWDelta("mrid-a", 0, -2000), maxControlSendSeconds + 1, ErrControlDurationInvalid},
		{"other mode", diff.Difference{Object: "mrid-a", Attribute: "DERControl.DERControlBase.opModTargetVar", Value: map[string]any{"multiplier": 0.0, "value": 100.0}}, 300, ErrUnsupportedControlAttribute},
		{"percent mode", diff.Difference{Object: "mrid-a", Attribute: "DERControl.DERControlBase.opModFixedW", Value: map[string]any{"multiplier": 0.0, "value": 50.0}}, 300, ErrUnsupportedControlAttribute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, reg, _, scopeA := newSendEmbed(t, controlClockUnix)
			scopeB := derControlScope(urlIndexFor(t, e.stores, "mrid-b"), controlFSAID, controlDERProgramID)

			_, err := e.ApplyControlFor(context.Background(), reg, tc.delta, tc.duration)
			if err == nil {
				t.Fatal("ApplyControlFor succeeded, want a refusal")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			for name, scope := range map[string]string{"device a": scopeA, "device b": scopeB} {
				if n := len(storedControls(t, e, scope)); n != 0 {
					t.Errorf("%s holds %d control(s) after a refused send, want 0", name, n)
				}
			}
		})
	}
}

// TestConcurrentBusAndDirectWritesKeepOneActiveControl runs both writers on
// the same device at the same wall second, the case where an unserialized pair
// reads one prior list and stamps one creationTime. Without the lock two
// controls share a creationTime and neither supersedes the other.
func TestConcurrentBusAndDirectWritesKeepOneActiveControl(t *testing.T) {
	t.Parallel()

	for round := 0; round < 25; round++ {
		e, reg, _, scope := newSendEmbed(t, controlClockUnix)

		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(w float64) {
				defer wg.Done()
				if _, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, w)); err != nil {
					errs <- err
				}
			}(float64(1000 + 100*i))
		}
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(w float64) {
				defer wg.Done()
				if _, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, w), 300); err != nil {
					errs <- err
				}
			}(float64(-500 - 100*i))
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: concurrent write failed: %v", round, err)
		}

		controls := storedControls(t, e, scope)
		if len(controls) != 7 {
			t.Fatalf("round %d: stored controls = %d, want 7 (3 bus + 2x2 direct)", round, len(controls))
		}
		seen := map[int64]bool{}
		active := 0
		for _, c := range controls {
			if seen[c.CreationTime] {
				t.Fatalf("round %d: two controls share creationTime %d", round, c.CreationTime)
			}
			seen[c.CreationTime] = true
			if c.EventStatus.CurrentStatus == sep2.EventStatusActive {
				active++
			}
		}
		if active != 1 {
			t.Fatalf("round %d: %d Active controls, want exactly 1", round, active)
		}
	}
}

// A send refused by the creationTime lead bound writes neither control: the
// control in force keeps its status and nothing is scheduled behind it. The
// loop sends in one wall second until the bound refuses one.
func TestApplyControlForRefusalWritesNeitherControl(t *testing.T) {
	t.Parallel()

	e, reg, _, scope := newSendEmbed(t, controlClockUnix)
	for i := 0; i < 12; i++ {
		before := storedControls(t, e, scope)
		send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, float64(-1000-10*i)), 60)
		if err == nil {
			continue
		}
		if i == 0 {
			t.Fatalf("first send refused: %v", err)
		}
		if !errors.Is(err, ErrControlDeltaRateUnrepresentable) {
			t.Fatalf("send %d error = %v, want ErrControlDeltaRateUnrepresentable", i+1, err)
		}
		if send != (ControlSend{}) {
			t.Errorf("refused send returned %+v, want the zero ControlSend", send)
		}
		if after := storedControls(t, e, scope); !reflect.DeepEqual(before, after) {
			t.Fatalf("refused send %d changed the store: %d controls before, %d after", i+1, len(before), len(after))
		}
		return
	}
	t.Fatal("twelve sends in one wall second were all issued; the lead bound never refused one")
}

// Direct sends bypass the change bound, so they are held to their own share of
// the creationTime lead and the bus path keeps the rest. Direct sends repeated
// in one second until refused must leave a stated number of bus deltas their
// issue, then the bound refuses the next. The counts are literals so a change
// to either budget fails here: 4 is the guarantee, reached when the last
// direct pair ends exactly at its budget, and 5 when it ends one short.
func TestDirectSendsLeaveTheBusPathItsReservedLead(t *testing.T) {
	t.Parallel()

	const now = controlClockUnix
	cases := []struct {
		name       string
		busFirst   bool
		busIssued  int
		directSent int
	}{
		{"direct pairs end at the direct budget", true, 4, 3},
		{"direct pairs end one short of it", false, 5, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, reg, _, scope := newSendEmbed(t, now)
			if tc.busFirst {
				if _, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 100)); err != nil {
					t.Fatalf("first bus delta: %v", err)
				}
			}
			sent := 0
			for i := 0; i < 8; i++ {
				_, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 0), 60)
				if err == nil {
					sent++
					continue
				}
				if !errors.Is(err, ErrControlDeltaRateUnrepresentable) {
					t.Fatalf("direct send %d error = %v, want ErrControlDeltaRateUnrepresentable", i+1, err)
				}
			}
			if sent != tc.directSent {
				t.Fatalf("direct sends issued = %d, want %d", sent, tc.directSent)
			}

			for i := 0; i < tc.busIssued; i++ {
				w := float64(3000 + 100*i)
				out, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, w))
				if err != nil || out != ControlIssued {
					t.Fatalf("bus delta %d of %d after the direct sends = (%v, %v), want issued", i+1, tc.busIssued, out, err)
				}
			}
			last := 3000 + 100*(tc.busIssued-1)
			if _, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 9000)); !errors.Is(err, ErrControlDeltaRateUnrepresentable) {
				t.Fatalf("bus delta %d error = %v, want ErrControlDeltaRateUnrepresentable", tc.busIssued+1, err)
			}

			controls := storedControls(t, e, scope)
			newest := controls[len(controls)-1]
			if got := targetW(t, &newest); got != last {
				t.Errorf("newest control = %d W, want the last bus delta's %d W", got, last)
			}
			if newest.CreationTime != now+maxCreationTimeLeadSeconds {
				t.Errorf("newest creationTime = t0+%d, want t0+%d", newest.CreationTime-now, maxCreationTimeLeadSeconds)
			}
			assertEventStatus(t, "last bus control", newest, wantStatus{sep2.EventStatusActive, now})
			if got := targetW(t, effectiveAt(controls, now)); got != last {
				t.Errorf("effective W = %d, want the last bus delta's %d W", got, last)
			}
		})
	}
}

// A duration the fleet's randomizeDuration can shorten to zero or below would
// serve an event some device ends before it starts; a duration over the cap
// is refused too. Both write nothing.
func TestApplyControlForBoundsTheDuration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		randomize int32
		duration  uint32
		refused   bool
	}{
		{"negative randomization reaches zero", -120, 120, true},
		{"negative randomization goes below zero", -120, 60, true},
		{"negative randomization leaves one second", -120, 121, false},
		{"positive randomization cannot shorten", 120, 60, false},
		{"at the cap", 0, maxControlSendSeconds, false},
		{"over the cap", 0, maxControlSendSeconds + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, reg, _, scope := newSendEmbed(t, controlClockUnix)
			e.policy.Control.RandomizeDuration = tc.randomize
			send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), tc.duration)
			controls := storedControls(t, e, scope)
			if tc.refused {
				if !errors.Is(err, ErrControlDurationInvalid) {
					t.Fatalf("error = %v, want ErrControlDurationInvalid", err)
				}
				if len(controls) != 0 {
					t.Errorf("refused send stored %d controls, want 0", len(controls))
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyControlFor: %v", err)
			}
			req := controlByID(t, controls, send.ControlID)
			if req.Interval.Duration != tc.duration || *req.RandomizeDuration != sep2.OneHourRange(tc.randomize) {
				t.Errorf("served duration %d randomizeDuration %d, want %d and %d", req.Interval.Duration, *req.RandomizeDuration, tc.duration, tc.randomize)
			}
		})
	}
}

// ApplyControlFor takes ended controls out of service before it writes, as
// the bus path does, so a send never supersedes or counts an event that has
// already left service.
func TestApplyControlForSweepsEndedControlsFirst(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, t0)
	if _, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), 60); err != nil {
		t.Fatalf("first send: %v", err)
	}
	old := storedControls(t, e, scope)

	// Past the follow-on's end too: t0+60 plus the 1800 s fleet window.
	clock.set(t0 + 1900)
	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 1000), 60)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	controls := storedControls(t, e, scope)
	if len(controls) != 2 {
		t.Fatalf("stored controls = %d, want 2 (the ended pair swept)", len(controls))
	}
	controlByID(t, controls, send.ControlID)
	controlByID(t, controls, send.FollowOnID)
	// The first control's retention closed at t0+1860; its follow-on's is open.
	if _, ok := e.EndedControl(old[1].MRID); !ok {
		t.Errorf("swept follow-on %s has no retained record", old[1].MRID)
	}
}

// The sweep and both writers serialize on controlMu: while it is held, none
// of them returns, and each completes once it is released.
func TestControlWritersAndSweepWaitForTheControlLock(t *testing.T) {
	t.Parallel()

	e, reg, _, _ := newSendEmbed(t, controlClockUnix)
	calls := []struct {
		name string
		call func() error
	}{
		{"sweep", func() error { _, err := e.expireEndedControls(context.Background()); return err }},
		{"direct send", func() error {
			_, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -500), 60)
			return err
		}},
		{"bus delta", func() error {
			_, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 700))
			return err
		}},
	}
	for _, c := range calls {
		e.controlMu.Lock()
		done := make(chan error, 1)
		go func() { done <- c.call() }()
		select {
		case <-done:
			e.controlMu.Unlock()
			t.Fatalf("%s returned while controlMu was held", c.name)
		case <-time.After(100 * time.Millisecond):
		}
		e.controlMu.Unlock()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not return after controlMu was released", c.name)
		}
	}
}

// A caller that gives up while waiting for the lock writes nothing.
func TestApplyControlForChecksTheContextAfterTheLock(t *testing.T) {
	t.Parallel()

	e, reg, _, scope := newSendEmbed(t, controlClockUnix)
	ctx, cancel := context.WithCancel(context.Background())
	e.controlMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-a", 0, -2000), 60)
		done <- err
	}()
	cancel()
	e.controlMu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if n := len(storedControls(t, e, scope)); n != 0 {
		t.Errorf("a cancelled send stored %d controls, want 0", n)
	}
}

// The sweep promotes every Scheduled control whose earliest Effective Start
// Time has passed, whatever its mode, and leaves a superseded or cancelled one
// as it is.
func TestSweepPromotesEveryStartedScheduledControl(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, t0)
	// Creates the DERProgram the stored controls below hang under.
	if _, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, 100)); err != nil {
		t.Fatalf("bus delta: %v", err)
	}

	early := sep2.OneHourRange(-10)
	scheduled := func(n int, status uint8, randomizeStart *sep2.OneHourRange) sep2.DERControl {
		c := sep2.DERControl{}
		c.MRID = strings.Repeat(strconv.Itoa(n), 32)
		c.CreationTime = t0 - int64(n)
		c.Interval = &sep2.DateTimeInterval{Start: t0 + 100, Duration: 60}
		c.RandomizeStart = randomizeStart
		c.EventStatus = &sep2.EventStatus{CurrentStatus: status, DateTime: t0 - 5}
		c.DERControlBase = &sep2.DERControlBase{OpModTargetVar: &sep2.ReactivePower{Value: int16(10 * n)}}
		return c
	}
	stored := map[string]sep2.DERControl{
		"randomized var":   scheduled(1, sep2.EventStatusScheduled, &early),
		"plain var":        scheduled(2, sep2.EventStatusScheduled, nil),
		"superseded early": scheduled(3, sep2.EventStatusSuperseded, nil),
		"cancelled early":  scheduled(4, sep2.EventStatusCancelled, nil),
	}
	ids := map[string]string{}
	for name, c := range stored {
		ids[name] = derControlID(c.CreationTime, c.MRID)
		if err := e.stores.DERControls.Create(context.Background(), scope, ids[name], c); err != nil {
			t.Fatalf("store %s: %v", name, err)
		}
	}

	check := func(at int64, want map[string]wantStatus) {
		t.Helper()
		clock.set(at)
		if _, err := e.expireEndedControls(context.Background()); err != nil {
			t.Fatalf("sweep at t0+%d: %v", at-t0, err)
		}
		controls := storedControls(t, e, scope)
		for name, w := range want {
			assertEventStatus(t, name+" at t0+"+strconv.FormatInt(at-t0, 10), controlByID(t, controls, ids[name]), w)
		}
	}
	untouched := func(status uint8) wantStatus { return wantStatus{status, t0 - 5} }

	check(t0+89, map[string]wantStatus{
		"randomized var":   untouched(sep2.EventStatusScheduled),
		"plain var":        untouched(sep2.EventStatusScheduled),
		"superseded early": untouched(sep2.EventStatusSuperseded),
		"cancelled early":  untouched(sep2.EventStatusCancelled),
	})
	// randomizeStart -10 lets a device start at t0+90.
	check(t0+90, map[string]wantStatus{
		"randomized var":   {sep2.EventStatusActive, t0 + 90},
		"plain var":        untouched(sep2.EventStatusScheduled),
		"superseded early": untouched(sep2.EventStatusSuperseded),
		"cancelled early":  untouched(sep2.EventStatusCancelled),
	})
	check(t0+130, map[string]wantStatus{
		"randomized var":   {sep2.EventStatusActive, t0 + 90},
		"plain var":        {sep2.EventStatusActive, t0 + 100},
		"superseded early": untouched(sep2.EventStatusSuperseded),
		"cancelled early":  untouched(sep2.EventStatusCancelled),
	})
}

// busDuringSend is a direct send followed by a bus delta on the same device,
// and what must then run and be served.
type busDuringSend struct {
	name              string
	sendW             float64
	sendDur           uint32
	busAt             int64 // seconds after the send
	busW              float64
	want              map[int64]int // seconds after the send -> watts
	send, follow, bus wantStatus
}

// A bus delta during a direct send is a new command: it takes effect at once
// and no control of the send runs after it.
func TestBusDeltaDuringADirectSendTakesEffectAtOnce(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	cases := []busDuringSend{
		{
			// The 0 W bus delta matches the send's Scheduled follow-on, which
			// has not started, so it restates nothing in force.
			name: "stop during a short send", sendW: 5000, sendDur: 300, busAt: 10, busW: 0,
			want:   map[int64]int{10: 0, 200: 0, 299: 0, 300: 0, 1809: 0, 1810: noControl},
			send:   wantStatus{sep2.EventStatusSuperseded, t0 + 10},
			follow: wantStatus{sep2.EventStatusSuperseded, t0 + 10},
			bus:    wantStatus{sep2.EventStatusActive, t0 + 10},
		},
		{
			// The bus control ends before the follow-on would start, so the
			// follow-on is cancelled rather than left to drive 0 W later.
			name: "bus delta ends before the follow-on starts", sendW: -2000, sendDur: 3600, busAt: 10, busW: 3000,
			want:   map[int64]int{10: 3000, 1809: 3000, 1810: noControl, 3599: noControl, 3600: noControl, 5399: noControl},
			send:   wantStatus{sep2.EventStatusSuperseded, t0 + 10},
			follow: wantStatus{sep2.EventStatusCancelled, t0 + 10},
			bus:    wantStatus{sep2.EventStatusActive, t0 + 10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, reg, clock, scope := newSendEmbed(t, t0)
			send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, tc.sendW), tc.sendDur)
			if err != nil {
				t.Fatalf("ApplyControlFor: %v", err)
			}
			clock.set(t0 + tc.busAt)
			out, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta("mrid-a", 0, tc.busW))
			if err != nil {
				t.Fatalf("bus delta: %v", err)
			}
			if out != ControlIssued {
				t.Fatalf("bus delta outcome = %v, want ControlIssued", out)
			}

			controls := storedControls(t, e, scope)
			if len(controls) != 3 {
				t.Fatalf("stored controls = %d, want 3", len(controls))
			}
			bus := controls[2]
			if targetW(t, &bus) != int(tc.busW) {
				t.Fatalf("newest control = %d W, want the bus delta's %v W", targetW(t, &bus), tc.busW)
			}
			for at, want := range tc.want {
				if got := targetW(t, effectiveAt(controls, t0+at)); got != want {
					t.Errorf("effective W at t0+%d = %d, want %d", at, got, want)
				}
			}
			assertEventStatus(t, "sent control", controlByID(t, controls, send.ControlID), tc.send)
			assertEventStatus(t, "follow-on", controlByID(t, controls, send.FollowOnID), tc.follow)
			assertEventStatus(t, "bus control", bus, tc.bus)
		})
	}
}

// The change bound compares only against a control every device is running:
// started even with the largest randomizeStart, and not cancelled.
func TestRestatementNeedsAStartedUncancelledControl(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	late := sep2.OneHourRange(20)
	stored := func(status uint8) sep2.DERControl {
		c := sep2.DERControl{}
		c.MRID = strings.Repeat("a", 32)
		c.CreationTime = t0
		c.Interval = &sep2.DateTimeInterval{Start: t0 + 100, Duration: 600}
		c.RandomizeStart = &late
		c.EventStatus = &sep2.EventStatus{CurrentStatus: status, DateTime: t0}
		c.DERControlBase = &sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: 0}}
		return c
	}
	cases := []struct {
		name   string
		status uint8
		at     int64
		want   bool
	}{
		{"before the nominal start", sep2.EventStatusScheduled, t0 + 99, false},
		{"started on some devices only", sep2.EventStatusActive, t0 + 119, false},
		{"started on every device", sep2.EventStatusActive, t0 + 120, true},
		{"cancelled", sep2.EventStatusCancelled, t0 + 200, false},
		{"cancelled with randomization", eventStatusCancelledWithRandomization, t0 + 200, false},
		{"superseded is left to creationTime", sep2.EventStatusSuperseded, t0 + 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: 0}}
			got, err := restatesControlInForce([]sep2.DERControl{stored(tc.status)}, &base, tc.at)
			if err != nil {
				t.Fatalf("restatesControlInForce: %v", err)
			}
			if got != tc.want {
				t.Errorf("restatement = %v, want %v", got, tc.want)
			}
		})
	}
}

var errInjected = errors.New("injected store failure")

// faultyDERControls fails the Update or Delete calls its predicates select and
// passes everything else to the wrapped store.
type faultyDERControls struct {
	store.ScopedStore[sep2.DERControl]
	failUpdate func(scope string, c sep2.DERControl) bool
	failDelete func(scope string) bool
}

func (f faultyDERControls) Update(ctx context.Context, scope, id string, c sep2.DERControl) error {
	if f.failUpdate != nil && f.failUpdate(scope, c) {
		return errInjected
	}
	return f.ScopedStore.Update(ctx, scope, id, c)
}

func (f faultyDERControls) Delete(ctx context.Context, scope, id string) error {
	if f.failDelete != nil && f.failDelete(scope) {
		return errInjected
	}
	return f.ScopedStore.Delete(ctx, scope, id)
}

// When only the cancellation of an older follow-on fails, both new controls
// are in service, so the caller gets the ControlSend naming them with the
// error, and the store shows exactly that.
func TestApplyControlForReportsTheSendWhenOnlyTheCancelFails(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, t0)
	first, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), 3600)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	e.stores.DERControls = faultyDERControls{
		ScopedStore: e.stores.DERControls,
		failUpdate: func(_ string, c sep2.DERControl) bool {
			return c.EventStatus != nil && c.EventStatus.CurrentStatus == sep2.EventStatusCancelled
		},
	}
	clock.set(t0 + 100)
	later, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 1500), 60)
	if !errors.Is(err, errInjected) {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	if !strings.Contains(err.Error(), "in service") {
		t.Errorf("error %q does not say the controls are in service", err)
	}
	if later.ControlID == "" || later.FollowOnID == "" || later.Start != t0+100 || later.End != t0+160 {
		t.Fatalf("ControlSend = %+v, want both ids, start t0+100 and end t0+160", later)
	}

	controls := storedControls(t, e, scope)
	if len(controls) != 4 {
		t.Fatalf("stored controls = %d, want 4", len(controls))
	}
	assertEventStatus(t, "later control", controlByID(t, controls, later.ControlID), wantStatus{sep2.EventStatusActive, t0 + 100})
	assertEventStatus(t, "later follow-on", controlByID(t, controls, later.FollowOnID), wantStatus{sep2.EventStatusScheduled, t0 + 100})
	assertEventStatus(t, "uncancelled follow-on", controlByID(t, controls, first.FollowOnID), wantStatus{sep2.EventStatusScheduled, t0})
}

// A sweep failure on device A, in a promotion or a removal, must not refuse a
// write for device B; a write for device A itself is still refused.
func TestSweepFailureOnOneDeviceRefusesOnlyItsOwnWrites(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	writers := map[string]func(e *Embed, reg *registry.Registry, mrid string) error{
		"bus delta": func(e *Embed, reg *registry.Registry, mrid string) error {
			_, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta(mrid, 0, 700))
			return err
		},
		"direct send": func(e *Embed, reg *registry.Registry, mrid string) error {
			_, err := e.ApplyControlFor(context.Background(), reg, targetWDelta(mrid, 0, 700), 60)
			return err
		},
	}
	for _, failing := range []string{"promotion", "removal"} {
		for name, write := range writers {
			t.Run(failing+"/"+name, func(t *testing.T) {
				t.Parallel()

				e, reg, clock, scopeA := newSendEmbed(t, t0)
				scopeB := derControlScope(urlIndexFor(t, e.stores, "mrid-b"), controlFSAID, controlDERProgramID)
				// At t0+70 the sweep removes A's ended control and promotes its
				// started follow-on.
				if _, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), 60); err != nil {
					t.Fatalf("send to device a: %v", err)
				}
				faulty := faultyDERControls{ScopedStore: e.stores.DERControls}
				if failing == "promotion" {
					faulty.failUpdate = func(scope string, _ sep2.DERControl) bool { return scope == scopeA }
				} else {
					faulty.failDelete = func(scope string) bool { return scope == scopeA }
				}
				e.stores.DERControls = faulty
				clock.set(t0 + 70)

				if err := write(e, reg, "mrid-b"); err != nil {
					t.Fatalf("write to device b refused: %v", err)
				}
				controlsB := storedControls(t, e, scopeB)
				if len(controlsB) == 0 {
					t.Fatal("device b holds no control after its write")
				}
				newest := controlsB[0]
				if got := targetW(t, &newest); got != 700 {
					t.Errorf("device b control = %d W, want 700", got)
				}
				assertEventStatus(t, "device b control", newest, wantStatus{sep2.EventStatusActive, t0 + 70})

				before := storedControls(t, e, scopeA)
				if err := write(e, reg, "mrid-a"); !errors.Is(err, errInjected) {
					t.Fatalf("write to device a error = %v, want the injected sweep failure", err)
				}
				for _, c := range storedControls(t, e, scopeA) {
					if targetW(t, &c) == 700 {
						t.Errorf("device a holds the refused 700 W control (%d before)", len(before))
					}
				}
			})
		}
	}
}

// recordNotifications returns a notifier with a subscription on each device's
// DERProgramList, and the hrefs Notify was called for, in order. The manager
// is never started, so nothing is delivered; its subscriber check runs inside
// Notify and records the call.
func recordNotifications(t *testing.T, e *Embed, mrids ...string) func() []string {
	t.Helper()
	notifier := coresub.NewManager(e.stores.Subscriptions, 1, 64)
	var mu sync.Mutex
	var got []string
	notifier.SetSubscriberCheck(func(_ context.Context, sub sep2.Subscription) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, sub.SubscribedResource)
		return nil
	})
	for i, mrid := range mrids {
		edev := urlIndexFor(t, e.stores, mrid)
		if err := e.stores.Subscriptions.Create(context.Background(), "sub-"+strconv.Itoa(i), sep2.Subscription{
			SubscribableResource: sep2.SubscribableResource{Resource: sep2.Resource{Href: "/edev/" + edev + "/sub/1"}},
			SubscribedResource:   derProgramListHref(edev, controlFSAID),
			NotificationURI:      "http://127.0.0.1:9/notify",
		}); err != nil {
			t.Fatalf("seed subscription for %s: %v", mrid, err)
		}
	}
	e.notifier = notifier
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
}

// A direct send notifies its device's DERProgramList once, and a sweep that
// only promotes a started follow-on, removing nothing, notifies it too.
func TestSendAndPromoteOnlySweepNotifyTheDevice(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	e, reg, clock, scope := newSendEmbed(t, t0)
	// Positive randomization keeps the ended control in service past the
	// follow-on's start, so the sweep below promotes without removing.
	e.policy.Control.RandomizeDuration = 120
	drain := recordNotifications(t, e, "mrid-a", "mrid-b")
	wantA := []string{derProgramListHref(urlIndexFor(t, e.stores, "mrid-a"), controlFSAID)}

	send, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), 60)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}
	if got := drain(); !reflect.DeepEqual(got, wantA) {
		t.Errorf("notifications after the send = %v, want %v", got, wantA)
	}

	clock.set(t0 + 70)
	removed, err := e.expireEndedControls(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("sweep removed %d controls, want 0 (promotion only)", removed)
	}
	assertEventStatus(t, "follow-on", controlByID(t, storedControls(t, e, scope), send.FollowOnID), wantStatus{sep2.EventStatusActive, t0 + 60})
	if got := drain(); !reflect.DeepEqual(got, wantA) {
		t.Errorf("notifications after the promote-only sweep = %v, want %v", got, wantA)
	}

	if _, err := e.expireEndedControls(context.Background()); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := drain(); len(got) != 0 {
		t.Errorf("notifications after a sweep that changed nothing = %v, want none", got)
	}
}
