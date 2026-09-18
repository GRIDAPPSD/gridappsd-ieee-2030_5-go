package sep2embed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// TestApplyControlDeltaRefusesOutOfRangeRandomizeDuration proves the
// package-level ApplyControlDelta enforces the sep.xsd OneHourRangeType
// bound on policy.Control.RandomizeDuration itself, for a caller that
// reaches this function directly and bypasses both sep2embed.New and the
// boot validator. 40000 is the value issue 89's own reproduction used.
func TestApplyControlDeltaRefusesOutOfRangeRandomizeDuration(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	ctx := context.Background()

	policy := testControlPolicy
	policy.Control.RandomizeDuration = 40000

	err := ApplyControlDelta(ctx, st, nil, reg, policy, diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	})
	if !errors.Is(err, ErrDERControlRandomizeDurationOutOfRange) {
		t.Fatalf("ApplyControlDelta with RandomizeDuration=40000 = %v, want ErrDERControlRandomizeDurationOutOfRange", err)
	}
	if got := err.Error(); !strings.Contains(got, "RandomizeDuration") || !strings.Contains(got, "3600") {
		t.Errorf("error = %q, want it to name RandomizeDuration and the 3600 bound", got)
	}

	// Nothing was written, mirroring TestApplyControlDeltaRefusesUnconfiguredDuration:
	// the refusal happens before any resource is created.
	edevA := urlIndexFor(t, st, "mrid-a")
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)
	if n := controlCount(t, ctx, st, scope); n != 0 {
		t.Errorf("%d DERControls were written despite the refusal; the guard must run before any store write", n)
	}
}

// TestApplyControlDeltaAcceptsInRangeRandomizeDuration is issue 89's
// regression case: testControlPolicy's own RandomizeDuration (0, mirroring
// sep2config.DefaultDERControlDuration's default policy) is accepted and
// served unchanged by the guard above.
func TestApplyControlDeltaAcceptsInRangeRandomizeDuration(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	ctx := context.Background()

	err := ApplyControlDelta(ctx, st, nil, reg, testControlPolicy, diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	})
	if err != nil {
		t.Fatalf("ApplyControlDelta with the default RandomizeDuration = %v, want nil", err)
	}

	edevA := urlIndexFor(t, st, "mrid-a")
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)
	control, _ := soleControl(t, ctx, st, scope)
	if control.RandomizeDuration == nil {
		t.Fatal("stored DERControl.RandomizeDuration = nil, want the served 0 value")
	}
	if got := int32(*control.RandomizeDuration); got != testControlSeed.RandomizeDuration {
		t.Errorf("stored DERControl.RandomizeDuration = %d, want %d (testControlSeed's configured value)", got, testControlSeed.RandomizeDuration)
	}
}
