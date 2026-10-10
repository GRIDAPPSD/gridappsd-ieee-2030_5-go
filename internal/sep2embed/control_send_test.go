package sep2embed

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
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

// effectiveAt applies the client's rule: of the controls whose interval covers
// the instant, the one with the largest creationTime runs. It returns nil when
// none covers it.
func effectiveAt(controls []sep2.DERControl, at int64) *sep2.DERControl {
	var best *sep2.DERControl
	for i := range controls {
		c := &controls[i]
		if at < c.Interval.Start || at >= c.Interval.Start+int64(c.Interval.Duration) {
			continue
		}
		if best == nil || c.CreationTime > best.CreationTime {
			best = c
		}
	}
	return best
}

func targetW(t *testing.T, c *sep2.DERControl) int {
	t.Helper()
	if c == nil || c.DERControlBase == nil || c.DERControlBase.OpModTargetW == nil {
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

func TestApplyControlForLaterSendIsNotOverriddenByStaleFollowOn(t *testing.T) {
	t.Parallel()

	const t0 = controlClockUnix
	cases := []struct {
		name     string
		laterDur uint32
		// instant -> watts that must run, per the client's creationTime rule.
		want map[int64]int
	}{
		{
			name:     "later send outlasts the first control",
			laterDur: 600,
			want:     map[int64]int{t0 + 150: 1500, t0 + 299: 1500, t0 + 300: 1500, t0 + 699: 1500, t0 + 700: 0},
		},
		{
			name:     "later send ends before the first control would have",
			laterDur: 60,
			want:     map[int64]int{t0 + 150: 1500, t0 + 160: 0, t0 + 300: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, reg, clock, scope := newSendEmbed(t, t0)
			first, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, -2000), 300)
			if err != nil {
				t.Fatalf("first send: %v", err)
			}
			clock.set(t0 + 100)
			later, err := e.ApplyControlFor(context.Background(), reg, targetWDelta("mrid-a", 0, 1500), tc.laterDur)
			if err != nil {
				t.Fatalf("later send: %v", err)
			}

			controls := storedControls(t, e, scope)
			for at, want := range tc.want {
				if got := targetW(t, effectiveAt(controls, at)); got != want {
					t.Errorf("effective W at t0+%d = %d, want %d", at-t0, got, want)
				}
			}

			firstCtl := controlByID(t, controls, first.ControlID)
			if firstCtl.EventStatus.CurrentStatus != sep2.EventStatusSuperseded {
				t.Errorf("first control status = %d, want Superseded", firstCtl.EventStatus.CurrentStatus)
			}
			laterCtl := controlByID(t, controls, later.ControlID)
			if laterCtl.CreationTime <= controlByID(t, controls, first.FollowOnID).CreationTime {
				t.Errorf("later creationTime %d not newer than the stale follow-on's %d", laterCtl.CreationTime, controlByID(t, controls, first.FollowOnID).CreationTime)
			}
			if laterCtl.EventStatus.CurrentStatus != sep2.EventStatusActive {
				t.Errorf("later control status = %d, want Active", laterCtl.EventStatus.CurrentStatus)
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
		{"value above int16", targetWDelta("mrid-a", 0, 40000), 300, nil},
		{"value below int16", targetWDelta("mrid-a", 0, -40000), 300, nil},
		{"fractional value", targetWDelta("mrid-a", 0, 1.5), 300, nil},
		{"zero duration", targetWDelta("mrid-a", 0, -2000), 0, ErrControlDurationInvalid},
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
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
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
