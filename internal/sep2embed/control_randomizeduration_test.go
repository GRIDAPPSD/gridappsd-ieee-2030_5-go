package sep2embed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"

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

// TestApplyControlDeltaRandomizeDurationBoundary pins the range guard's
// THRESHOLD rather than only its direction. The two tests above use 40000
// (far outside) and 0 (far inside), so neither one distinguishes the real
// bound (+/-3600) from a guard that is wrong by an order of magnitude: a
// guard refusing only past +/-36000 still refuses 40000 and still accepts
// 0. This table probes both edges directly, on both sides of them, so a
// guard with the wrong threshold fails here even though it passes the two
// tests above.
func TestApplyControlDeltaRandomizeDurationBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		randomize  int32
		wantRefuse bool
	}{
		{"upper bound accepted", 3600, false},
		{"lower bound accepted", -3600, false},
		{"one past the upper bound refused", 3601, true},
		{"one past the lower bound refused", -3601, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg, st := twoDeviceFixture(t)
			ctx := context.Background()

			policy := testControlPolicy
			policy.Control.RandomizeDuration = tc.randomize

			err := ApplyControlDelta(ctx, st, nil, reg, policy, diff.Difference{
				Object:    "mrid-a",
				Attribute: "DERControl.DERControlBase.opModTargetW",
				Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
			})

			edevA := urlIndexFor(t, st, "mrid-a")
			scope := derControlScope(edevA, controlFSAID, controlDERProgramID)

			if tc.wantRefuse {
				if !errors.Is(err, ErrDERControlRandomizeDurationOutOfRange) {
					t.Fatalf("ApplyControlDelta(RandomizeDuration=%d) = %v, want ErrDERControlRandomizeDurationOutOfRange", tc.randomize, err)
				}
				if n := controlCount(t, ctx, st, scope); n != 0 {
					t.Errorf("%d DERControls were written despite the refusal", n)
				}
				return
			}

			if err != nil {
				t.Fatalf("ApplyControlDelta(RandomizeDuration=%d) = %v, want nil", tc.randomize, err)
			}
			control, _ := soleControl(t, ctx, st, scope)
			if control.RandomizeDuration == nil {
				t.Fatalf("stored DERControl.RandomizeDuration = nil, want %d", tc.randomize)
			}
			if got := int32(*control.RandomizeDuration); got != tc.randomize {
				t.Errorf("stored DERControl.RandomizeDuration = %d, want %d", got, tc.randomize)
			}
		})
	}
}

// TestApplyControlDeltaRefusesBeforeEnsuringDERProgram pins the "refuse
// before anything is written" ordering that issue 89's acceptance criteria
// and the guard's own comment both state. twoDeviceFixture seeds every
// device with its DERProgram already in place, so ensureDERProgram's
// create path (the function's first store write) never runs against that
// fixture and an early relocation of the guard would pass every test above
// unnoticed. This test removes device A's seeded DERProgram and
// DefaultDERControl first, so ensureDERProgram takes its lazy-create
// fallback, and asserts that a refused delta leaves both absent: a guard
// relocated below ensureDERProgram would let it create them before the
// refusal fires.
func TestApplyControlDeltaRefusesBeforeEnsuringDERProgram(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	ctx := context.Background()
	edevA := urlIndexFor(t, st, "mrid-a")

	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)
	if err := st.DefaultDERControls.Delete(ctx, scope, singletonKey); err != nil {
		t.Fatalf("DefaultDERControls.Delete(%q, %q): %v", scope, singletonKey, err)
	}
	if err := st.DERPrograms.Delete(ctx, edevA, controlDERProgramID); err != nil {
		t.Fatalf("DERPrograms.Delete(%q, %q): %v", edevA, controlDERProgramID, err)
	}

	policy := testControlPolicy
	policy.Control.RandomizeDuration = 40000

	err := ApplyControlDelta(ctx, st, nil, reg, policy, diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	})
	if !errors.Is(err, ErrDERControlRandomizeDurationOutOfRange) {
		t.Fatalf("ApplyControlDelta with RandomizeDuration=40000 on a device whose program is absent = %v, want ErrDERControlRandomizeDurationOutOfRange", err)
	}

	if _, err := st.DERPrograms.Get(ctx, edevA, controlDERProgramID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DERPrograms.Get after the refusal = %v, want ErrNotFound: the refusal must precede ensureDERProgram's write", err)
	}
	if _, err := st.DefaultDERControls.Get(ctx, scope, singletonKey); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DefaultDERControls.Get after the refusal = %v, want ErrNotFound: the refusal must precede ensureDERProgram's write", err)
	}
}
