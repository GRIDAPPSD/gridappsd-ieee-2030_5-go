package sep2embed

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// signLoopback: co-simulation loopback verification of the DERControl
// active and reactive sign convention.
//
// This file is the Go half of the verification. It runs the REAL
// ApplyControlDelta control path (the unit under test) for two commanded
// 2030.5 intents, reads the resulting DERControlBase target values back
// out of the store, and asserts the sign the bridge produces. The Python
// HELICS plus OpenDSS harness (harness/ in this worktree) then closes the
// physical half: it drives the SAME captured bridge output into an
// OpenDSS DER and reads the measured active/reactive power sign back out.
//
// The two halves are coupled by the capture file written by
// TestSignLoopbackCaptureBridgeSetpoints when SIGN_LOOPBACK_CAPTURE_OUT is set, so
// the Python side consumes the bridge's genuine output, not a hand typed
// value.
//
// 2030.5 semantics asserted against (IEEE 2030.5 section 10.10):
//   - opModTargetW positive  = discharging / exporting real power.
//   - opModTargetW negative  = charging / importing real power.
//   - opModTargetVar positive = over excited / injecting VARs.
//   - opModTargetVar negative = under excited / absorbing VARs.

// signLoopbackPowerToFloat collapses a sep2 multiplier plus value pair to plain
// watts (or vars), so the sign and magnitude are directly comparable to
// the commanded intent. value times 10^multiplier.
func signLoopbackPowerToFloat(multiplier int8, value int16) float64 {
	return float64(value) * math.Pow10(int(multiplier))
}

// TestSignLoopbackConventionPinned locks BOTH the two sign constants AND
// the resulting signed direction mapping: an accidental flip of either constant changes the sign of a
// commanded target and fails a concrete signed assertion here, not just a
// non crash check.
//
// The mapping under test is identity (no flip): a discharge command
// (positive watts) must produce a positive OpModTargetW, and a charge
// command (negative watts) a negative OpModTargetW. Same for reactive.
func TestSignLoopbackConventionPinned(t *testing.T) {
	t.Parallel()

	// Pin the constants themselves. If a future edit flips one, this
	// fails first with a clear pointer to update the direction asserts
	// below in the same commit and state why.
	if activeSignFlip {
		t.Fatal("activeSignFlip changed from the sign-loopback verified default false; a flip inverts every commanded opModTargetW, re verify the OpenDSS direction in harness/ before changing it")
	}
	if reactiveSignFlip {
		t.Fatal("reactiveSignFlip changed from the sign-loopback verified default false; a flip inverts every commanded opModTargetVar, re verify the OpenDSS direction in harness/ before changing it")
	}

	cases := []struct {
		name      string
		attribute string
		commanded float64 // commanded watts (opModTargetW) or vars (opModTargetVar)
		wantSign  int     // +1 or -1: the sign the bridge output MUST carry
	}{
		{"discharge exports positive watts", "opModTargetW", 5000, +1},
		{"charge imports negative watts", "opModTargetW", -5000, -1},
		{"inject vars positive", "opModTargetVar", 3000, +1},
		{"absorb vars negative", "opModTargetVar", -3000, -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, st := twoDeviceFixture(t)
			notifier := coresub.NewManager(st.Subscriptions, 1, 10)
			ctx := context.Background()

			delta := diff.Difference{
				Object:    "mrid-a",
				Attribute: "DERControl.DERControlBase." + tc.attribute,
				Value:     map[string]any{"multiplier": 0.0, "value": tc.commanded},
			}
			if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta); err != nil {
				t.Fatalf("ApplyControlDelta: %v", err)
			}

			scope := derControlScope(urlIndexFor(t, st, "mrid-a"), controlFSAID, controlDERProgramID)
			control, _ := soleControl(t, ctx, st, scope)
			base := control.DERControlBase
			if base == nil {
				t.Fatalf("control has no DERControlBase: %+v", control)
			}

			var got float64
			switch tc.attribute {
			case "opModTargetW":
				if base.OpModTargetW == nil {
					t.Fatalf("control has no OpModTargetW: %+v", base)
				}
				got = signLoopbackPowerToFloat(base.OpModTargetW.Multiplier, base.OpModTargetW.Value)
			case "opModTargetVar":
				if base.OpModTargetVar == nil {
					t.Fatalf("control has no OpModTargetVar: %+v", base)
				}
				got = signLoopbackPowerToFloat(base.OpModTargetVar.Multiplier, base.OpModTargetVar.Value)
			}

			// Assert the exact signed value, not just the sign: the
			// mapping is identity, so the commanded magnitude must also
			// survive unchanged.
			if got != tc.commanded {
				t.Fatalf("%s: bridge output = %v W/var, want %v (identity, no flip)", tc.attribute, got, tc.commanded)
			}
			if sign := signOf(got); sign != tc.wantSign {
				t.Fatalf("%s: bridge output sign = %d, want %d (commanded %v)", tc.attribute, sign, tc.wantSign, tc.commanded)
			}
		})
	}
}

func signOf(v float64) int {
	switch {
	case v > 0:
		return +1
	case v < 0:
		return -1
	default:
		return 0
	}
}

// signLoopbackCapture is the JSON schema the Python HELICS plus OpenDSS
// harness reads. Each case carries the commanded 2030.5 intent and the
// bridge's REAL output value (read from the store after ApplyControlDelta),
// so the co-simulation drives the genuine control path output.
type signLoopbackCapture struct {
	ActiveSignFlip   bool                      `json:"activeSignFlip"`
	ReactiveSignFlip bool                      `json:"reactiveSignFlip"`
	Cases            []signLoopbackCaptureCase `json:"cases"`
}

type signLoopbackCaptureCase struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	Attribute      string  `json:"attribute"`
	CommandedValue float64 `json:"commanded_value"`
	// The bridge's real target values after ApplyControlDelta, in absolute
	// watts / vars (sep2 multiplier already applied). Unset targets are 0.
	BridgeTargetW   float64 `json:"bridge_target_w"`
	BridgeTargetVar float64 `json:"bridge_target_var"`
}

// TestSignLoopbackCaptureBridgeSetpoints runs the real control path for the
// discharge and VAR inject cases and, when SIGN_LOOPBACK_CAPTURE_OUT names an
// output path, writes the bridge's genuine target values there for the
// HELICS plus OpenDSS harness to consume. With the env var unset this is
// a plain assertion (the capture write is skipped), so a normal
// go test ./... run does not litter files.
func TestSignLoopbackCaptureBridgeSetpoints(t *testing.T) {
	out := os.Getenv("SIGN_LOOPBACK_CAPTURE_OUT")

	commands := []struct {
		id        int
		name      string
		attribute string
		commanded float64
	}{
		{0, "active_discharge", "opModTargetW", 5000},
		{1, "var_inject", "opModTargetVar", 3000},
	}

	capture := signLoopbackCapture{ActiveSignFlip: activeSignFlip, ReactiveSignFlip: reactiveSignFlip}

	for _, cmd := range commands {
		reg, st := twoDeviceFixture(t)
		notifier := coresub.NewManager(st.Subscriptions, 1, 10)
		ctx := context.Background()

		delta := diff.Difference{
			Object:    "mrid-a",
			Attribute: "DERControl.DERControlBase." + cmd.attribute,
			Value:     map[string]any{"multiplier": 0.0, "value": cmd.commanded},
		}
		if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta); err != nil {
			t.Fatalf("ApplyControlDelta(%s): %v", cmd.name, err)
		}

		scope := derControlScope(urlIndexFor(t, st, "mrid-a"), controlFSAID, controlDERProgramID)
		control, _ := soleControl(t, ctx, st, scope)
		base := control.DERControlBase

		cc := signLoopbackCaptureCase{ID: cmd.id, Name: cmd.name, Attribute: cmd.attribute, CommandedValue: cmd.commanded}
		if base != nil && base.OpModTargetW != nil {
			cc.BridgeTargetW = signLoopbackPowerToFloat(base.OpModTargetW.Multiplier, base.OpModTargetW.Value)
		}
		if base != nil && base.OpModTargetVar != nil {
			cc.BridgeTargetVar = signLoopbackPowerToFloat(base.OpModTargetVar.Multiplier, base.OpModTargetVar.Value)
		}
		capture.Cases = append(capture.Cases, cc)
	}

	// Sanity: the discharge case carries the commanded watts on
	// OpModTargetW with the commanded sign, the VAR case carries the
	// commanded vars on OpModTargetVar. This holds regardless of whether
	// the capture is written to disk.
	if capture.Cases[0].BridgeTargetW != 5000 {
		t.Fatalf("discharge case bridge_target_w = %v, want 5000", capture.Cases[0].BridgeTargetW)
	}
	if capture.Cases[1].BridgeTargetVar != 3000 {
		t.Fatalf("var inject case bridge_target_var = %v, want 3000", capture.Cases[1].BridgeTargetVar)
	}

	if out == "" {
		return
	}
	b, err := json.MarshalIndent(capture, "", "  ")
	if err != nil {
		t.Fatalf("marshal capture: %v", err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatalf("write capture to %s: %v", out, err)
	}
	t.Logf("sign-loopback bridge capture written to %s", out)
}

// Compile time guard: the capture struct is only meaningful while the
// assembly.Stores and diff.Difference types this test drives exist with
// the fields used above. Referencing them keeps the imports honest if the
// bodies above are ever trimmed.
var _ = assembly.Stores{}
