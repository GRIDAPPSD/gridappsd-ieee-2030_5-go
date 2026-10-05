package sender

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// stripMRID replaces the fresh difference_mrid so the rest of the envelope can
// be compared as exact JSON.
func stripMRID(t *testing.T, msg *diff.Message) string {
	t.Helper()
	if !uuidV4.MatchString(msg.Input.Message.DifferenceMRID) {
		t.Fatalf("difference_mrid %q is not a v4 UUID", msg.Input.Message.DifferenceMRID)
	}
	cp := *msg
	cp.Input.Message.DifferenceMRID = "ID"
	b, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFormMessagesAreExactEnvelopes(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	epoch := "1791201600"
	tests := []struct {
		name  string
		build func() (*diff.Message, error)
		want  string
	}{
		{"active power", func() (*diff.Message, error) { return ActivePowerMessage(now, "_dev-a", 3, -1200) },
			`{"command":"update","input":{"message":{"timestamp":` + epoch + `,"difference_mrid":"ID","reverse_differences":[],"forward_differences":[{"object":"_dev-a","attribute":"DERControl.DERControlBase.opModTargetW","value":{"multiplier":3,"value":-1200}}]}}}`},
		{"reactive power", func() (*diff.Message, error) { return ReactivePowerMessage(now, "_dev-b", -2, 40) },
			`{"command":"update","input":{"message":{"timestamp":` + epoch + `,"difference_mrid":"ID","reverse_differences":[],"forward_differences":[{"object":"_dev-b","attribute":"DERControl.DERControlBase.opModTargetVar","value":{"multiplier":-2,"value":40}}]}}}`},
		{"connect", func() (*diff.Message, error) { return ConnectMessage(now, "_dev-a", false) },
			`{"command":"update","input":{"message":{"timestamp":` + epoch + `,"difference_mrid":"ID","reverse_differences":[],"forward_differences":[{"object":"_dev-a","attribute":"DERControl.DERControlBase.opModConnect","value":false}]}}}`},
		{"energize", func() (*diff.Message, error) { return EnergizeMessage(now, "_dev-c", true) },
			`{"command":"update","input":{"message":{"timestamp":` + epoch + `,"difference_mrid":"ID","reverse_differences":[],"forward_differences":[{"object":"_dev-c","attribute":"DERControl.DERControlBase.opModEnergize","value":true}]}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := tc.build()
			if err != nil {
				t.Fatal(err)
			}
			if got := stripMRID(t, msg); got != tc.want {
				t.Errorf("envelope\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestFormDifferenceMRIDIsFreshPerMessage(t *testing.T) {
	a, err := ConnectMessage(t0, "_dev-a", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ConnectMessage(t0, "_dev-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Input.Message.DifferenceMRID == b.Input.Message.DifferenceMRID {
		t.Errorf("two builds share difference_mrid %q", a.Input.Message.DifferenceMRID)
	}
}

func TestFormBoundsAreInclusive(t *testing.T) {
	tests := []struct {
		name       string
		multiplier int
		value      int
		wantErr    bool
	}{
		{"max multiplier", 9, 0, false},
		{"min multiplier", -9, 0, false},
		{"multiplier above max", 10, 0, true},
		{"multiplier below min", -10, 0, true},
		{"max value", 0, 32767, false},
		{"min value", 0, -32768, false},
		{"value above max", 0, 32768, true},
		{"value below min", 0, -32769, true},
	}
	for _, tc := range tests {
		for name, build := range map[string]func() (*diff.Message, error){
			"W":   func() (*diff.Message, error) { return ActivePowerMessage(t0, "_dev-a", tc.multiplier, tc.value) },
			"var": func() (*diff.Message, error) { return ReactivePowerMessage(t0, "_dev-a", tc.multiplier, tc.value) },
		} {
			t.Run(tc.name+" "+name, func(t *testing.T) {
				msg, err := build()
				if tc.wantErr {
					if !errors.Is(err, ErrFieldRange) || msg != nil {
						t.Errorf("got msg=%v err=%v, want ErrFieldRange and no message", msg, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				v := msg.Input.Message.ForwardDifferences[0].Value.(map[string]any)
				if v["multiplier"] != tc.multiplier || v["value"] != tc.value {
					t.Errorf("value = %v, want multiplier %d value %d", v, tc.multiplier, tc.value)
				}
			})
		}
	}
}

func TestFormRefusesEmptyDevice(t *testing.T) {
	for name, build := range map[string]func() (*diff.Message, error){
		"W":        func() (*diff.Message, error) { return ActivePowerMessage(t0, "", 0, 1) },
		"connect":  func() (*diff.Message, error) { return ConnectMessage(t0, "", true) },
		"energize": func() (*diff.Message, error) { return EnergizeMessage(t0, "", true) },
		"reactive": func() (*diff.Message, error) { return ReactivePowerMessage(t0, "", 0, 1) },
	} {
		if _, err := build(); !errors.Is(err, ErrFieldRange) {
			t.Errorf("%s: err = %v, want ErrFieldRange", name, err)
		}
	}
}

// Each form must pass the subscriber's own validator once decoded the way the
// subscriber decodes it.
func TestFormsPassTheSubscribersValidator(t *testing.T) {
	reg := newRegistry(t)
	builders := map[string]func() (*diff.Message, error){
		"W":        func() (*diff.Message, error) { return ActivePowerMessage(t0, "_dev-a", -3, 500) },
		"var":      func() (*diff.Message, error) { return ReactivePowerMessage(t0, "_dev-a", 0, -500) },
		"connect":  func() (*diff.Message, error) { return ConnectMessage(t0, "_dev-a", true) },
		"energize": func() (*diff.Message, error) { return EnergizeMessage(t0, "_dev-a", false) },
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			msg, err := build()
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			var decoded diff.Message
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := len(decoded.Input.Message.ForwardDifferences); got != 1 {
				t.Fatalf("decoded %d forward differences, want 1", got)
			}
			if err := sep2embed.ValidateControlDelta(reg, decoded.Input.Message.ForwardDifferences[0]); err != nil {
				t.Errorf("subscriber validator refused the form: %v", err)
			}
		})
	}
}
