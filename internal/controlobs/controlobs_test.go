package controlobs

import (
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

func TestSnapshotOnZeroValueHookIsAllZero(t *testing.T) {
	t.Parallel()

	var h Hook
	snap := h.Snapshot()

	if snap.Applied != 0 {
		t.Errorf("Snapshot().Applied = %d, want 0", snap.Applied)
	}
	if snap.Skipped != 0 {
		t.Errorf("Snapshot().Skipped = %d, want 0", snap.Skipped)
	}
	if snap.Last != nil {
		t.Errorf("Snapshot().Last = %+v, want nil", snap.Last)
	}
	if snap.OutputTopic != "" {
		t.Errorf("Snapshot().OutputTopic = %q, want empty", snap.OutputTopic)
	}
	if snap.InputTopic != "" {
		t.Errorf("Snapshot().InputTopic = %q, want empty", snap.InputTopic)
	}
}

func TestSetTopicsRecordsExactStrings(t *testing.T) {
	t.Parallel()

	var h Hook
	h.SetTopics("/topic/goss.gridappsd.simulation.output.12345", "/topic/goss.gridappsd.simulation.input.12345")

	snap := h.Snapshot()
	if snap.OutputTopic != "/topic/goss.gridappsd.simulation.output.12345" {
		t.Errorf("Snapshot().OutputTopic = %q, want %q", snap.OutputTopic, "/topic/goss.gridappsd.simulation.output.12345")
	}
	if snap.InputTopic != "/topic/goss.gridappsd.simulation.input.12345" {
		t.Errorf("Snapshot().InputTopic = %q, want %q", snap.InputTopic, "/topic/goss.gridappsd.simulation.input.12345")
	}
}

// TestAppliedRecordsCounterAndLastDeltaFieldValues drives one synthetic
// delta through Applied and asserts the recorded snapshot values, per the
// GAGO-057 acceptance criteria: "a synthetic delta drives the hook, a
// unit test asserts the recorded snapshot values."
func TestAppliedRecordsCounterAndLastDeltaFieldValues(t *testing.T) {
	t.Parallel()

	var h Hook
	delta := diff.Difference{
		Object:    "mrid-inv-1",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0, "value": int64(4200)},
	}

	h.Applied(delta)

	snap := h.Snapshot()
	if snap.Applied != 1 {
		t.Errorf("Snapshot().Applied = %d, want 1", snap.Applied)
	}
	if snap.Skipped != 0 {
		t.Errorf("Snapshot().Skipped = %d, want 0", snap.Skipped)
	}
	if snap.Last == nil {
		t.Fatalf("Snapshot().Last is nil, want a populated LastDelta")
	}
	if snap.Last.Object != "mrid-inv-1" {
		t.Errorf("Snapshot().Last.Object = %q, want %q", snap.Last.Object, "mrid-inv-1")
	}
	if snap.Last.Attribute != "DERControl.DERControlBase.opModTargetW" {
		t.Errorf("Snapshot().Last.Attribute = %q, want %q", snap.Last.Attribute, "DERControl.DERControlBase.opModTargetW")
	}
	gotValue, ok := snap.Last.Value.(map[string]any)
	if !ok {
		t.Fatalf("Snapshot().Last.Value = %#v (%T), want map[string]any", snap.Last.Value, snap.Last.Value)
	}
	if gotValue["value"] != int64(4200) {
		t.Errorf("Snapshot().Last.Value[\"value\"] = %v, want 4200", gotValue["value"])
	}
	if snap.Last.AppliedAt.IsZero() {
		t.Errorf("Snapshot().Last.AppliedAt is zero, want a real timestamp")
	}
}

// TestAppliedTwiceKeepsMostRecentAsLast confirms Last is replaced, not
// accumulated: after two Applied calls, Last must reflect the SECOND
// delta's field values, not the first, while Applied has counted both.
func TestAppliedTwiceKeepsMostRecentAsLast(t *testing.T) {
	t.Parallel()

	var h Hook
	first := diff.Difference{Object: "mrid-inv-1", Attribute: "DERControl.DERControlBase.opModTargetW", Value: int64(1000)}
	second := diff.Difference{Object: "mrid-bat-1", Attribute: "DERControl.DERControlBase.opModConnect", Value: true}

	h.Applied(first)
	h.Applied(second)

	snap := h.Snapshot()
	if snap.Applied != 2 {
		t.Errorf("Snapshot().Applied = %d, want 2", snap.Applied)
	}
	if snap.Last == nil {
		t.Fatalf("Snapshot().Last is nil, want a populated LastDelta")
	}
	if snap.Last.Object != "mrid-bat-1" {
		t.Errorf("Snapshot().Last.Object = %q, want %q (the second, most recent delta)", snap.Last.Object, "mrid-bat-1")
	}
	if snap.Last.Attribute != "DERControl.DERControlBase.opModConnect" {
		t.Errorf("Snapshot().Last.Attribute = %q, want %q", snap.Last.Attribute, "DERControl.DERControlBase.opModConnect")
	}
}

// TestSkippedIncrementsCounterWithoutTouchingLast confirms a skipped
// delta (one that failed to apply, e.g. an unknown device or an
// unsupported attribute) is counted but does NOT become Last: Last only
// ever reflects a delta that actually took effect on bridge state.
func TestSkippedIncrementsCounterWithoutTouchingLast(t *testing.T) {
	t.Parallel()

	var h Hook
	applied := diff.Difference{Object: "mrid-inv-1", Attribute: "DERControl.DERControlBase.opModTargetW", Value: int64(1000)}
	h.Applied(applied)

	h.Skipped()
	h.Skipped()

	snap := h.Snapshot()
	if snap.Applied != 1 {
		t.Errorf("Snapshot().Applied = %d, want 1", snap.Applied)
	}
	if snap.Skipped != 2 {
		t.Errorf("Snapshot().Skipped = %d, want 2", snap.Skipped)
	}
	if snap.Last == nil {
		t.Fatalf("Snapshot().Last is nil, want the last APPLIED delta to remain recorded")
	}
	if snap.Last.Object != "mrid-inv-1" {
		t.Errorf("Snapshot().Last.Object = %q, want %q (Skipped must not overwrite Last)", snap.Last.Object, "mrid-inv-1")
	}
}

// TestSnapshotReturnsIndependentCopyOfLast confirms the returned Last
// pointer is a copy: mutating the caller's copy must not affect the
// hook's own internal state, since Snapshot's whole contract is a safe,
// read only value with no shared mutable handle back into the hook.
func TestSnapshotReturnsIndependentCopyOfLast(t *testing.T) {
	t.Parallel()

	var h Hook
	h.Applied(diff.Difference{Object: "mrid-inv-1", Attribute: "DERControl.DERControlBase.opModTargetW", Value: int64(1000)})

	snap := h.Snapshot()
	snap.Last.Object = "mutated-by-caller"

	again := h.Snapshot()
	if again.Last.Object != "mrid-inv-1" {
		t.Errorf("Hook.Snapshot().Last.Object after caller mutation = %q, want %q (Snapshot must return an independent copy)", again.Last.Object, "mrid-inv-1")
	}
}
