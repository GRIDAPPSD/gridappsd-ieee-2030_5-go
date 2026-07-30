package sep2embed

import (
	"context"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// intervalTestSetup seeds one device and returns the pieces
// ApplyControlDelta needs, so each interval test below drives the real
// DOWN path rather than constructing a DERControl by hand.
func intervalTestSetup(t *testing.T) (context.Context, *registry.Registry, string) {
	t.Helper()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"
	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: lfdi, Placeholder: true},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	return context.Background(), reg, lfdi
}

// TestApplyControlDeltaSetsActivatableInterval is the MEDIUM-4
// assertion. EPRI's scheduler computes eb->start = ev->interval.start and
// eb->end = eb->start + ev->interval.duration (schedule.c:180-182), then
// DISCARDS the block when eb->end <= now (schedule.c:419). With no
// <interval> both fields are zero, so end is 0, which is always <= now:
// the event is dropped before it can ever activate.
//
// The assertions are therefore not "an interval exists" but the two
// semantic properties that make an external scheduler activate:
// start <= now (already begun, so eb->start <= now at schedule.c:430
// makes it current) and end > now (not already expired).
func TestApplyControlDeltaSetsActivatableInterval(t *testing.T) {
	t.Parallel()

	ctx, reg, lfdi := intervalTestSetup(t)
	stores := newStores()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, seedPolicy{RegistrationPIN: &pin}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	before := time.Now().UTC().Unix()
	delta := ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     sep2.ActivePower{Multiplier: 0, Value: 5000},
	}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}
	after := time.Now().UTC().Unix()

	scope := derControlScope(lfdi, controlFSAID, controlDERProgramID)
	ctrl, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get: %v", err)
	}

	if ctrl.Interval == nil {
		t.Fatal("ctrl.Interval is nil; EPRI's scheduler computes eb->end = 0 and drops the block at schedule.c:419")
	}

	// TimeType is seconds since the Unix epoch (2030.5 section 10.1.4),
	// the same base time.Unix() returns. A millisecond value here would
	// place start ~55000 years in the future and never activate, so the
	// epoch base is asserted by bracketing against locally observed
	// wall-clock seconds.
	if ctrl.Interval.Start < before || ctrl.Interval.Start > after {
		t.Errorf("ctrl.Interval.Start = %d, want within [%d, %d] (Unix epoch SECONDS, not millis)",
			ctrl.Interval.Start, before, after)
	}

	if ctrl.Interval.Duration == 0 {
		t.Error("ctrl.Interval.Duration = 0; eb->end == eb->start so the event expires the instant it starts")
	}
	if ctrl.Interval.Duration != defaultControlDurationSeconds {
		t.Errorf("ctrl.Interval.Duration = %d, want %d", ctrl.Interval.Duration, defaultControlDurationSeconds)
	}

	// The two properties an external scheduler actually branches on.
	end := ctrl.Interval.Start + int64(ctrl.Interval.Duration)
	now := time.Now().UTC().Unix()
	if ctrl.Interval.Start > now {
		t.Errorf("interval start %d is in the future relative to now %d; the block would not be current", ctrl.Interval.Start, now)
	}
	if end <= now {
		t.Errorf("interval end %d <= now %d; EPRI drops this block at schedule.c:419", end, now)
	}
}

// TestApplyControlDeltaIntervalXMLShape asserts the emitted wire bytes.
// The EPRI client parses XML against sep.xsd, so <interval> must be
// present with both child elements, and duration must precede start per
// the DateTimeInterval sequence.
func TestApplyControlDeltaIntervalXMLShape(t *testing.T) {
	t.Parallel()

	ctx, reg, lfdi := intervalTestSetup(t)
	stores := newStores()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, seedPolicy{RegistrationPIN: &pin}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	delta := ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     sep2.ActivePower{Multiplier: 0, Value: 5000},
	}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	scope := derControlScope(lfdi, controlFSAID, controlDERProgramID)
	ctrl, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get: %v", err)
	}

	out, err := xml.Marshal(&ctrl)
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, "<interval>") {
		t.Errorf("marshalled DERControl missing <interval>:\n%s", got)
	}
	if !strings.Contains(got, "<duration>") {
		t.Errorf("marshalled DERControl missing <duration>:\n%s", got)
	}
	if !strings.Contains(got, "<start>") {
		t.Errorf("marshalled DERControl missing <start>:\n%s", got)
	}
	// EventStatus must still say Active, or the client treats the control
	// as scheduled/cancelled regardless of a valid interval.
	if !strings.Contains(got, "<currentStatus>1</currentStatus>") {
		t.Errorf("marshalled DERControl EventStatus.currentStatus != 1 (Active):\n%s", got)
	}
}

// TestApplyControlDeltaReplacementStampsFreshIntervalStart pins the
// supersede path as GAGO-094 redefined it.
//
// This test previously asserted the OPPOSITE: that a second delta merged
// into the same control and PRESERVED the original interval start. That
// behavior was the defect Devi found. Mutating a control in place under a
// stable mRID gives a client no wire signal that a new command arrived
// (EPRI's schedule_event short-circuits on the known mRID, update_existing
// discards everything but EventStatus off an equal-mRID event, and
// activate_block will not re-fire EVENT_START on an already-Active block),
// so the second setpoint was silently dropped. Each delta now produces a
// distinct event generation instead.
//
// A distinct event owns a distinct window: there is no earlier start to
// preserve, because the generation that had one no longer exists. So the
// assertion is that start is re-stamped to the replacement's own issue
// instant, bracketed against locally observed wall-clock seconds, and that
// the window is live. The old worry that re-stamping could place start
// after a polling client's view does not arise: start is "now" at write
// time, never a future instant.
//
// Carrying the DERControlBase fields forward across the replacement is
// still required and still asserted: DERControlBase is a bag of
// independent fields, so a delta naming opModTargetVar must not erase an
// earlier opModTargetW.
func TestApplyControlDeltaReplacementStampsFreshIntervalStart(t *testing.T) {
	t.Parallel()

	ctx, reg, lfdi := intervalTestSetup(t)
	stores := newStores()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, seedPolicy{RegistrationPIN: &pin}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	first := ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     sep2.ActivePower{Multiplier: 0, Value: 5000},
	}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, first); err != nil {
		t.Fatalf("ApplyControlDelta first: %v", err)
	}

	scope := derControlScope(lfdi, controlFSAID, controlDERProgramID)
	got1, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get after first: %v", err)
	}
	if got1.Interval == nil {
		t.Fatal("first control has nil Interval")
	}

	beforeSecond := time.Now().UTC().Unix()
	second := ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetVar",
		Value:     sep2.ReactivePower{Multiplier: 0, Value: 1200},
	}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, second); err != nil {
		t.Fatalf("ApplyControlDelta second: %v", err)
	}
	afterSecond := time.Now().UTC().Unix()

	got2, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get after second: %v", err)
	}
	if got2.Interval == nil {
		t.Fatal("second control has nil Interval")
	}
	if got2.Interval.Start < beforeSecond || got2.Interval.Start > afterSecond {
		t.Errorf("replacement interval start = %d, want within [%d, %d] (the replacement's own issue instant, in Unix epoch SECONDS)",
			got2.Interval.Start, beforeSecond, afterSecond)
	}
	if got2.Interval.Duration != defaultControlDurationSeconds {
		t.Errorf("replacement interval duration = %d, want the plain constant %d (start is re-stamped, so there is no elapsed time to compensate for)",
			got2.Interval.Duration, defaultControlDurationSeconds)
	}

	// Both merged fields must survive, and the window must still be live.
	if got2.DERControlBase == nil {
		t.Fatal("merged control lost its DERControlBase")
	}
	if got2.DERControlBase.OpModTargetW == nil || got2.DERControlBase.OpModTargetW.Value != 5000 {
		t.Errorf("OpModTargetW = %+v, want value 5000 preserved across the merge", got2.DERControlBase.OpModTargetW)
	}
	if got2.DERControlBase.OpModTargetVar == nil || got2.DERControlBase.OpModTargetVar.Value != 1200 {
		t.Errorf("OpModTargetVar = %+v, want value 1200", got2.DERControlBase.OpModTargetVar)
	}
	end := got2.Interval.Start + int64(got2.Interval.Duration)
	if now := time.Now().UTC().Unix(); end <= now {
		t.Errorf("interval end %d <= now %d after the update; the control expired", end, now)
	}
}

// TestApplyControlDeltaReplacementReopensAnExpiringWindow asserts the
// served window moves OUT when a later delta arrives, so a long-running
// scenario does not let the window lapse while controls are still being
// issued.
//
// The stored generation's window is rewound close to expiry first, which
// is the only interesting case: if a replacement inherited the old start it
// would inherit the old end too, and a scenario issuing controls for longer
// than defaultControlDurationSeconds would serve an already-expired event
// (EPRI's update_schedule drops any block whose eb->end <= now). Because
// each generation is stamped with its own start, the replacement's end is
// measured from the moment it was issued, so the window reopens.
func TestApplyControlDeltaReplacementReopensAnExpiringWindow(t *testing.T) {
	t.Parallel()

	ctx, reg, lfdi := intervalTestSetup(t)
	stores := newStores()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, seedPolicy{RegistrationPIN: &pin}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	scope := derControlScope(lfdi, controlFSAID, controlDERProgramID)
	delta := ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     sep2.ActivePower{Multiplier: 0, Value: 5000},
	}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, delta); err != nil {
		t.Fatalf("ApplyControlDelta first: %v", err)
	}
	got1, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}

	// Rewind the stored start so the stored window is nearly expired,
	// without sleeping in a unit test.
	rewound := got1.Copy()
	rewound.Interval.Start -= int64(defaultControlDurationSeconds) - 5
	if err := stores.DERControls.ForParent(scope).Update(ctx, activeControlID, rewound); err != nil {
		t.Fatalf("rewind update: %v", err)
	}
	oldEnd := rewound.Interval.Start + int64(rewound.Interval.Duration)

	delta.Value = sep2.ActivePower{Multiplier: 0, Value: 6000}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, delta); err != nil {
		t.Fatalf("ApplyControlDelta second: %v", err)
	}
	got2, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}

	newEnd := got2.Interval.Start + int64(got2.Interval.Duration)
	if newEnd <= oldEnd {
		t.Errorf("replacement window did not move out: old end=%d new end=%d", oldEnd, newEnd)
	}
	if now := time.Now().UTC().Unix(); newEnd <= now {
		t.Errorf("replacement window end %d <= now %d; the client drops an already-ended event", newEnd, now)
	}
	// The replaced setpoint is what the client must end up executing.
	if got2.DERControlBase == nil || got2.DERControlBase.OpModTargetW == nil {
		t.Fatalf("replacement lost OpModTargetW: base = %+v", got2.DERControlBase)
	}
	if got2.DERControlBase.OpModTargetW.Value != 6000 {
		t.Errorf("OpModTargetW.Value = %d, want the replacing delta's 6000", got2.DERControlBase.OpModTargetW.Value)
	}
}
