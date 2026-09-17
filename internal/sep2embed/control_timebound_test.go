package sep2embed

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// Wire-level tests for the bound on the same-second creationTime tie-break.
//
// WHAT THEY EXIST TO CATCH. The tie-break advances creationTime past a
// same-mode control issued in the same second, because TimeType has one-second
// resolution (sep.xsd:6382) and the client comparison is strictly greater, so
// two controls sharing a second supersede in neither direction. That mechanism
// is sound. Two things about it were not:
//
//   - The advance had no ceiling against the wall clock and the lead never
//     decayed, so sustained same-mode deltas above one per second drifted the
//     stamp forward indefinitely.
//   - It moved interval.start with it while newEventStatus hardcoded Active,
//     so a bumped control was served currentStatus 1 with a start in the
//     future. That is the half a client reads, and sep.xsd:5603 and
//     sep.xsd:5623 both speak to it.
//
// Everything here asserts the BYTES the embedded server writes to a client,
// for the reason given at the top of control_interval_test.go.
//
// The sibling bound on the same code path, the change bound that keeps a
// restated setpoint from minting an event at all, is in
// control_changebound_test.go.

// applyControlDeltaErr drives one delta through the real DOWN path and returns
// the error rather than failing the test, for the cases that assert on a
// refusal.
func applyControlDeltaErr(e *Embed, reg *registry.Registry, mrid, field string, value float64) error {
	return e.ApplyControlDelta(context.Background(), reg, diff.Difference{
		Object:    mrid,
		Attribute: "DERControl.DERControlBase." + field,
		Value:     map[string]any{"multiplier": 0.0, "value": value},
	})
}

// newestCreationTime returns the largest creationTime among the controls a
// device currently holds, read straight off the store rather than off a served
// page: the assertions it feeds are about the stamp the write path chose, and
// a paged read could hide the newest control behind a page boundary.
func newestCreationTime(t *testing.T, e *Embed, edevID string) int64 {
	t.Helper()
	scope := derControlScope(edevID, controlFSAID, controlDERProgramID)
	list, err := e.stores.DERControls.List(context.Background(), scope, store.ListOptions{Unbounded: true})
	if err != nil {
		t.Fatalf("list controls for %s: %v", edevID, err)
	}
	var newest int64
	for _, c := range list.Items {
		if c.CreationTime > newest {
			newest = c.CreationTime
		}
	}
	return newest
}

// assertNoActiveEventStartsInTheFuture fails when any served DERControl
// carries currentStatus 1 (Active) while its interval.start is later than
// wallUnix.
//
// This is the direct statement of sep.xsd:5603, read in the direction the
// server can violate it. The clause says that for an event whose start time is
// less than or equal to the current time, status 0 Scheduled "SHALL never be
// indicated, the event SHALL start with a status of Active". Its converse is
// what constrains a server that advances a start: an event that has not
// started is not Active, so serving Active with a future start misstates the
// event to every client that reads it, and a client acting on it actuates
// early.
func assertNoActiveEventStartsInTheFuture(t *testing.T, body []byte, wallUnix int64) {
	t.Helper()
	for _, el := range derControlElements(t, body) {
		start := elementInt64(t, el, "start")
		status := elementInt64(t, el, "currentStatus")
		if status == int64(sep2.EventStatusActive) && start > wallUnix {
			t.Errorf("served DERControl carries currentStatus 1 (Active) with interval start %d, which is %d second(s) after the current time %d; an event that has not started is not Active (sep.xsd:5603)\nelement=%s",
				start, start-wallUnix, wallUnix, el)
		}
	}
}

// TestSameSecondControlsAreNeverServedActiveWithAFutureStart is the wire
// half of the creationTime tie-break bound, and it is the half that reaches a client.
//
// Two deltas inside one wall-clock second are the case the creationTime
// tie-break exists to handle: TimeType has one-second resolution
// (sep.xsd:6382) and the client comparison is strictly greater, so without the
// bump neither event supersedes the other. The bump is right; moving
// interval.start and EventStatus.dateTime with it was not.
//
// Two clauses decide it, and both are read off the served bytes here:
//
//   - sep.xsd:5603, on status 0: "For events with a start time less than or
//     equal to the current time, this status SHALL never be indicated, the
//     event SHALL start with a status of Active." Keeping start at the wall
//     clock is what makes the Active this server stamps correct BY
//     CONSTRUCTION rather than by luck.
//   - sep.xsd:5623, on EventStatus.dateTime: it "MUST be set to the time at
//     which the status change occurred, not a time in the future or past".
//     A dateTime carrying the bumped stamp is a future timestamp, which that
//     clause forbids outright.
//
// creationTime is the one field that carries the bump, and nothing in the
// standard fixes it to the wall clock: sep.xsd:5578 defines it only as "the
// time at which the Event was created", and it exists here solely as the
// discriminator rule f) p.90 makes it.
func TestSameSecondControlsAreNeverServedActiveWithAFutureStart(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	baseURL, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCRATE4")
	d := devices[0]

	// Both inside ONE wall second, and both changing the value, so the change
	// bound does not apply and the tie-break must.
	applyTestControlDeltaValue(t, e, reg, "mrid-DERCRATE4", "opModTargetW", 5000)
	applyTestControlDeltaValue(t, e, reg, "mrid-DERCRATE4", "opModTargetW", 7500)

	body := servedDERControlList(t, d, baseURL)
	if n := len(derControlElements(t, body)); n != 2 {
		t.Fatalf("two same-second deltas produced %d served DERControl elements, want 2\nbody=%s", n, body)
	}

	assertNoActiveEventStartsInTheFuture(t, body, controlClockUnix)

	superseded := derControlElementWith(t, body, targetWElement(5000))
	superseding := derControlElementWith(t, body, targetWElement(7500))

	// Both events start NOW. The platform's delta means "this setpoint, from
	// here", and a burst inside one second does not change when either
	// setpoint was asked for.
	for name, el := range map[string][]byte{"superseded": superseded, "superseding": superseding} {
		if !bytes.Contains(el, []byte(wantIntervalElement)) {
			t.Errorf("%s DERControl does not carry %s; a same-second tie-break must not push the window forward\nelement=%s", name, wantIntervalElement, el)
		}
	}

	// The bump lands on creationTime alone, which is the only field that has
	// to differ for rule f) p.90 to order the two.
	if got := elementInt64(t, superseded, "creationTime"); got != controlClockUnix {
		t.Errorf("first control creationTime = %d, want %d", got, controlClockUnix)
	}
	if got := elementInt64(t, superseding, "creationTime"); got != controlClockUnix+1 {
		t.Errorf("second control creationTime = %d, want %d; two events in one second must still be strictly ordered", got, controlClockUnix+1)
	}

	// EventStatus, byte for byte, on both. dateTime is the wall clock on each,
	// never the bumped stamp (sep.xsd:5623: not a time in the future).
	if !bytes.Contains(superseding, []byte(wantActiveStatusAtFirst)) {
		t.Errorf("superseding DERControl does not carry %s\nelement=%s", wantActiveStatusAtFirst, superseding)
	}
	wantSuperseded := `<EventStatus><currentStatus>4</currentStatus><dateTime>1767225600</dateTime>` +
		`<potentiallySuperseded>false</potentiallySuperseded></EventStatus>`
	if !bytes.Contains(superseded, []byte(wantSuperseded)) {
		t.Errorf("superseded DERControl does not carry %s\nelement=%s", wantSuperseded, superseded)
	}
}

// TestCreationTimeLeadIsBoundedAndDecays is the magnitude half of the
// creationTime tie-break bound.
//
// The tie-break advances creationTime past a same-mode control issued in the
// same second, which is right; it did so with no ceiling against the wall
// clock and no decay, which was not. Sustained same-mode deltas above one per
// second drifted the stamp forward indefinitely: sixty deltas inside one wall
// second left the newest control stamped fifty-nine seconds in the future,
// and the lead persisted for as long as the rate did.
//
// What makes an unbounded lead more than cosmetic is that it does not survive
// a restart. A restarted bridge stamps from the true wall clock again, so its
// first controls carry creationTimes LOWER than the drifted ones a client is
// still holding, and under rule f) p.90 that client keeps running the stale
// event and discards the new one.
//
// The excess is refused rather than clamped. A clamped stamp equals one
// already issued, and equal creationTimes compare false in both directions
// under the client's strict-greater comparison, which is the defect the
// tie-break exists to prevent.
func TestCreationTimeLeadIsBoundedAndDecays(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	_, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERCRATE5")
	d := devices[0]
	scope := derControlScope(d.edevID, controlFSAID, controlDERProgramID)

	// Every value distinct, so the change bound never applies and
	// each delta genuinely asks for a new event. All of them land inside ONE
	// wall second, which is the condition the tie-break absorbs.
	absorbed := int(maxCreationTimeLeadSeconds) + 1
	for i := range absorbed {
		if err := applyControlDeltaErr(e, reg, "mrid-DERCRATE5", "opModTargetW", float64(5000+i)); err != nil {
			t.Fatalf("delta %d of %d inside one second: %v; a burst up to the bound is absorbed, not refused", i+1, absorbed, err)
		}
	}
	if got, want := newestCreationTime(t, e, d.edevID), controlClockUnix+maxCreationTimeLeadSeconds; got != want {
		t.Fatalf("newest creationTime = %d, want %d (a lead of exactly the bound)", got, want)
	}

	// One more in the same second would exceed the bound.
	err := applyControlDeltaErr(e, reg, "mrid-DERCRATE5", "opModTargetW", 9999)
	if !errors.Is(err, ErrControlDeltaRateUnrepresentable) {
		t.Fatalf("delta %d inside one second: error = %v, want ErrControlDeltaRateUnrepresentable; the lead has to stop somewhere", absorbed+1, err)
	}

	// The refusal wrote nothing. A guard that refused the delta after
	// creating its control would leave the collection one event larger and
	// the stamp one second further ahead, which is the condition it exists to
	// prevent.
	if got := controlCount(t, context.Background(), e.stores, scope); got != absorbed {
		t.Errorf("device holds %d controls after the refusal, want %d; the bound is checked before any store write", got, absorbed)
	}
	if got, want := newestCreationTime(t, e, d.edevID), controlClockUnix+maxCreationTimeLeadSeconds; got != want {
		t.Errorf("newest creationTime = %d after the refusal, want %d unchanged", got, want)
	}

	// DECAY. Nothing is reset and no state is carried: once the wall clock
	// passes the drifted stamp, the same code path returns the wall clock
	// unmodified and the lead is zero again.
	const caughtUp = controlClockUnix + maxCreationTimeLeadSeconds + 1
	clock.set(caughtUp)
	if err := applyControlDeltaErr(e, reg, "mrid-DERCRATE5", "opModTargetW", 9999); err != nil {
		t.Fatalf("delta once the clock caught up: %v", err)
	}
	if got := newestCreationTime(t, e, d.edevID); got != caughtUp {
		t.Errorf("newest creationTime = %d once the wall clock reached %d, want %d: the lead must decay rather than persist", got, caughtUp, caughtUp)
	}
}

// TestNewEventStatusEvaluatesWhetherTheEventHasStarted pins the status
// half of the creationTime tie-break bound at the one site that decides it.
//
// newEventStatus used to hardcode Active on the strength of a property of a
// DIFFERENT file: that control.go stamped interval.start at the wall clock.
// The tie-break broke that property and the hardcoded value did not fail,
// which is how a bumped control came to be served currentStatus 1 with a start
// in the future.
//
// sep.xsd:5603 fixes both directions, so both are asserted here even though
// today's write path can only produce the second.
func TestNewEventStatusEvaluatesWhetherTheEventHasStarted(t *testing.T) {
	t.Parallel()

	const wall = int64(1000)

	tests := []struct {
		name       string
		start      int64
		wantStatus uint8
		why        string
	}{
		{
			name: "start in the future", start: wall + 1, wantStatus: sep2.EventStatusScheduled,
			why: "sep.xsd:5603 defines 0 as scheduled and not yet started; serving 1 Active tells every client the event is running when it is not",
		},
		{
			name: "start is now", start: wall, wantStatus: sep2.EventStatusActive,
			why: "sep.xsd:5603: for a start time less than or equal to the current time, Scheduled SHALL never be indicated",
		},
		{
			name: "start in the past", start: wall - 60, wantStatus: sep2.EventStatusActive,
			why: "same clause, on the other side of the boundary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newEventStatus(edition2018, tt.start, wall)
			if got.CurrentStatus != tt.wantStatus {
				t.Errorf("newEventStatus(start=%d, wall=%d).CurrentStatus = %d, want %d; %s", tt.start, wall, got.CurrentStatus, tt.wantStatus, tt.why)
			}
			// dateTime is the instant the status was set, on every branch.
			// sep.xsd:5623: it "MUST be set to the time at which the status
			// change occurred, not a time in the future or past".
			if got.DateTime != wall {
				t.Errorf("newEventStatus(start=%d, wall=%d).DateTime = %d, want %d (the wall clock, never the event's own start)", tt.start, wall, got.DateTime, wall)
			}
		})
	}
}
