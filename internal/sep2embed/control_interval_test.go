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
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
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
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
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

// TestApplyControlDeltaUpdatePreservesIntervalStart pins the supersede
// path. A second delta for the same device merges into the SAME control;
// re-stamping start to "now" on every update would slide the window
// forward indefinitely and, worse, could momentarily place start after a
// polling client's view. The original start is kept and only the end is
// extended, so the control stays continuously active across updates.
func TestApplyControlDeltaUpdatePreservesIntervalStart(t *testing.T) {
	t.Parallel()

	ctx, reg, lfdi := intervalTestSetup(t)
	stores := newStores()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
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
	startAfterFirst := got1.Interval.Start

	second := ControlDelta{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetVar",
		Value:     sep2.ReactivePower{Multiplier: 0, Value: 1200},
	}
	if err := ApplyControlDelta(ctx, stores, nil, reg, sep2.DefaultDERControl{}, second); err != nil {
		t.Fatalf("ApplyControlDelta second: %v", err)
	}

	got2, err := stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get after second: %v", err)
	}
	if got2.Interval == nil {
		t.Fatal("second control has nil Interval")
	}
	if got2.Interval.Start != startAfterFirst {
		t.Errorf("interval start moved from %d to %d across an update; the window must not slide",
			startAfterFirst, got2.Interval.Start)
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

// TestApplyControlDeltaExtendsIntervalOnUpdate asserts the end actually
// moves out when a later delta arrives, so a long-running scenario does
// not let the window lapse while controls are still being issued.
func TestApplyControlDeltaExtendsIntervalOnUpdate(t *testing.T) {
	t.Parallel()

	ctx, reg, lfdi := intervalTestSetup(t)
	stores := newStores()
	pin := uint32(111115)
	if err := seedStores(ctx, stores, reg, nil, &pin, nil); err != nil {
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

	// Rewind the stored start so the update has a measurably older
	// window to extend, without sleeping in a unit test.
	rewound := got1.Copy()
	rewound.Interval.Start -= 30
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
		t.Errorf("interval end did not extend on update: old=%d new=%d", oldEnd, newEnd)
	}
	if got2.Interval.Start != rewound.Interval.Start {
		t.Errorf("interval start = %d, want the preserved %d", got2.Interval.Start, rewound.Interval.Start)
	}
}
