package sep2embed

import (
	"context"
	"errors"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

func targetWDelta(mrid string, mult, value any) diff.Difference {
	return diff.Difference{
		Object:    mrid,
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": mult, "value": value},
	}
}

// A restated setpoint is reported as restated and writes nothing; a changed
// one is issued and a restatement of it is restated again.
func TestApplyControlDeltaOutcomeSeparatesIssuedFromRestated(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	_, devices, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DEROUT1")
	mrid := "mrid-DEROUT1"

	steps := []struct {
		value float64
		want  ControlOutcome
		held  int
	}{
		{5000, ControlIssued, 1},
		{5000, ControlRestated, 1},
		{7500, ControlIssued, 2},
		{7500, ControlRestated, 2},
	}
	for i, st := range steps {
		clock.set(controlClockUnix + int64(i))
		got, err := e.ApplyControlDeltaOutcome(context.Background(), reg, targetWDelta(mrid, 0.0, st.value))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got != st.want {
			t.Errorf("step %d value %v: outcome = %d, want %d", i, st.value, got, st.want)
		}
		list, err := e.Stores().DERControls.List(context.Background(),
			derControlScope(devices[0].edevID, controlFSAID, controlDERProgramID), store.ListOptions{Unbounded: true})
		if err != nil {
			t.Fatalf("step %d list: %v", i, err)
		}
		if len(list.Items) != st.held {
			t.Errorf("step %d: %d stored controls, want %d", i, len(list.Items), st.held)
		}
	}
}

// ValidateControlDelta is the one check: it must refuse exactly what the
// apply path refuses and accept what it accepts, and it writes nothing.
func TestValidateControlDelta(t *testing.T) {
	t.Parallel()

	_, _, _, reg := newEmbedTestServer(t, nil, "DEROUT2")
	mrid := "mrid-DEROUT2"
	cases := []struct {
		name    string
		delta   diff.Difference
		wantErr error
	}{
		{"valid power target", targetWDelta(mrid, 0.0, 5000.0), nil},
		{"valid connect", diff.Difference{Object: mrid, Attribute: "DERControl.DERControlBase.opModConnect", Value: true}, nil},
		{"unknown device", targetWDelta("nope", 0.0, 5000.0), ErrUnknownControlDevice},
		{"wrong attribute prefix", diff.Difference{Object: mrid, Attribute: "DERStatus.x", Value: 1.0}, ErrUnsupportedControlAttribute},
		{"unsupported field", diff.Difference{Object: mrid, Attribute: "DERControl.DERControlBase.opModFoo", Value: 1.0}, ErrUnsupportedControlAttribute},
		{"fractional multiplier", targetWDelta(mrid, 2.5, 5000.0), errAny},
		{"string for boolean", diff.Difference{Object: mrid, Attribute: "DERControl.DERControlBase.opModConnect", Value: "true"}, errAny},
		{"bare number for power", diff.Difference{Object: mrid, Attribute: "DERControl.DERControlBase.opModTargetW", Value: 5000.0}, errAny},
	}
	for _, tc := range cases {
		err := ValidateControlDelta(reg, tc.delta)
		switch {
		case tc.wantErr == nil && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr == errAny && err == nil:
			t.Errorf("%s: accepted, want a refusal", tc.name)
		case tc.wantErr != nil && tc.wantErr != errAny && !errors.Is(err, tc.wantErr):
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

var errAny = errors.New("any refusal")
