package sender

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

const wAttr = AttrActivePower

func envelope(fwd string) string {
	return `{"command":"update","input":{"message":{"timestamp":1791201600,"difference_mrid":"m-1","reverse_differences":[],"forward_differences":` + fwd + `}}}`
}

func powerDiff(obj string, mult, val any) string {
	return fmt.Sprintf(`{"object":%q,"attribute":%q,"value":{"multiplier":%v,"value":%v}}`, obj, wAttr, mult, val)
}

func TestValidateRawRefusals(t *testing.T) {
	ok := powerDiff("_dev-a", 0, 100)
	tests := []struct {
		name     string
		body     string
		wantPath string
		wantMsg  string
	}{
		{"empty body", ``, "body", "empty"},
		{"not json", `{`, "body", "invalid JSON"},
		{"trailing data", envelope("["+ok+"]") + ` {}`, "body", "data after"},
		{"array root", `[]`, "body", "must be a JSON object"},
		{"unknown top field", `{"command":"update","extra":1,"input":{}}`, "extra", "unknown field"},
		{"missing command", `{"input":{}}`, "command", "is required"},
		{"command not update", `{"command":"create","input":{}}`, "command", `"create"`},
		{"command not string", `{"command":1,"input":{}}`, "command", "must be a string"},
		{"missing input", `{"command":"update"}`, "input", "is required"},
		{"unknown input field", `{"command":"update","input":{"x":1}}`, "input.x", "unknown field"},
		{"simulation_id not string", `{"command":"update","input":{"simulation_id":5,"message":{}}}`, "input.simulation_id", "must be a string"},
		{"missing message", `{"command":"update","input":{}}`, "input.message", "is required"},
		{"unknown message field", `{"command":"update","input":{"message":{"nope":1}}}`, "input.message.nope", "unknown field"},
		{"timestamp fraction", `{"command":"update","input":{"message":{"timestamp":1.5}}}`, "input.message.timestamp", "not an integer"},
		{"timestamp string", `{"command":"update","input":{"message":{"timestamp":"1"}}}`, "input.message.timestamp", "must be an integer"},
		{"missing mrid", `{"command":"update","input":{"message":{}}}`, "input.message.difference_mrid", "is required"},
		{"empty mrid", `{"command":"update","input":{"message":{"difference_mrid":""}}}`, "input.message.difference_mrid", "must not be empty"},
		{"long mrid", `{"command":"update","input":{"message":{"difference_mrid":"` + strings.Repeat("x", 257) + `"}}}`, "input.message.difference_mrid", "exceeds"},
		{"missing forward", `{"command":"update","input":{"message":{"difference_mrid":"m"}}}`, "input.message.forward_differences", "is required"},
		{"forward not array", envelope(`{}`), "input.message.forward_differences", "must be an array"},
		{"forward empty", envelope(`[]`), "input.message.forward_differences", "holds 0 entries"},
		{"reverse not array", `{"command":"update","input":{"message":{"difference_mrid":"m","reverse_differences":{}}}}`, "input.message.reverse_differences", "must be an array"},
		{"reverse unknown field", `{"command":"update","input":{"message":{"difference_mrid":"m","reverse_differences":[{"object":"o","attribute":"a","v":1}]}}}`, "input.message.reverse_differences[0].v", "unknown field"},
		{"forward entry not object", envelope(`[1]`), "input.message.forward_differences[0]", "must be an object"},
		{"forward unknown field", envelope(`[{"object":"o","attribute":"a","value":1,"x":2}]`), "input.message.forward_differences[0].x", "unknown field"},
		{"forward missing object", envelope(`[{"attribute":"a","value":1}]`), "input.message.forward_differences[0].object", "is required"},
		{"forward empty attribute", envelope(`[{"object":"o","attribute":"","value":1}]`), "input.message.forward_differences[0].attribute", "must not be empty"},
		{"forward missing value", envelope(`[{"object":"o","attribute":"a"}]`), "input.message.forward_differences[0].value", "is required"},
		{"unknown device", envelope("[" + powerDiff("_nobody", 0, 1) + "]"), "input.message.forward_differences[0].object", "unregistered device"},
		{"unsupported attribute", envelope(`[{"object":"_dev-a","attribute":"DERControl.DERControlBase.opModMaxLimW","value":1}]`), "input.message.forward_differences[0].attribute", "unsupported"},
		{"fractional multiplier", envelope("[" + powerDiff("_dev-a", 2.5, 1) + "]"), "input.message.forward_differences[0].value.multiplier", "2.5"},
		{"value above int16", envelope("[" + powerDiff("_dev-a", 0, 32768) + "]"), "input.message.forward_differences[0].value.value", "int16"},
		{"connect as string", envelope(`[{"object":"_dev-a","attribute":"DERControl.DERControlBase.opModConnect","value":"true"}]`), "input.message.forward_differences[0].value", "bool"},
		{"repeated object and attribute", envelope("[" + powerDiff("_dev-a", 0, 1) + "," + powerDiff("_dev-b", 0, 1) + "," + powerDiff("_dev-a", 1, 2) + "]"), "input.message.forward_differences[2]", "repeats the object and attribute of forward_differences[0]"},
		{"first failure wins", envelope("[" + powerDiff("_dev-a", 0, 1) + "," + powerDiff("_dev-b", 2.5, 1) + "," + powerDiff("_nobody", 0, 1) + "]"), "input.message.forward_differences[1].value.multiplier", "2.5"},
		{"alphabetically first unknown field", `{"command":"update","zeta":1,"alpha":2,"input":{}}`, "alpha", "unknown field"},
	}
	reg := newRegistry(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateRaw(reg, []byte(tc.body))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("path = %q, want %q (msg %q)", ve.Path, tc.wantPath, ve.Msg)
			}
			if !strings.Contains(ve.Msg, tc.wantMsg) {
				t.Errorf("msg = %q, want it to contain %q", ve.Msg, tc.wantMsg)
			}
		})
	}
}

func TestValidateRawForwardCountBounds(t *testing.T) {
	reg := newRegistry(t)
	attrs := []string{AttrActivePower, AttrReactivePower, AttrConnect, AttrEnergize}
	build := func(n int) string {
		var items []string
		for i := 0; i < n; i++ {
			obj := "_dev-extra"
			if i/4 < len(devices) {
				obj = devices[i/4].MRID
			}
			attr := attrs[i%4]
			val := `{"multiplier":0,"value":1}`
			if i%4 >= 2 {
				val = `true`
			}
			items = append(items, fmt.Sprintf(`{"object":%q,"attribute":%q,"value":%s}`, obj, attr, val))
		}
		return envelope("[" + strings.Join(items, ",") + "]")
	}
	got, err := validateRaw(reg, []byte(build(MaxForwardDifferences)))
	if err != nil {
		t.Fatalf("16 entries refused: %v", err)
	}
	if len(got.Forward) != 16 {
		t.Errorf("parsed %d differences, want 16", len(got.Forward))
	}
	_, err = validateRaw(reg, []byte(build(MaxForwardDifferences+1)))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Path != "input.message.forward_differences" || !strings.Contains(ve.Msg, "holds 17 entries") {
		t.Errorf("17 entries: err = %v, want a forward_differences count refusal", err)
	}
}

func TestValidateRawSizeBoundary(t *testing.T) {
	reg := newRegistry(t)
	base := `{"command":"update","input":{"simulation_id":"","message":{"difference_mrid":"m","forward_differences":[` + powerDiff("_dev-a", 0, 1) + `]}}}`
	pad := MaxRawBytes - len(base)
	body := strings.Replace(base, `"simulation_id":""`, `"simulation_id":"`+strings.Repeat("x", pad)+`"`, 1)
	if len(body) != MaxRawBytes {
		t.Fatalf("test body is %d bytes, want %d", len(body), MaxRawBytes)
	}
	if _, err := validateRaw(reg, []byte(body)); err != nil {
		t.Errorf("a body of exactly %d bytes was refused: %v", MaxRawBytes, err)
	}
	_, err := validateRaw(reg, []byte(body+" "))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Path != "body" || !strings.Contains(ve.Msg, "exceeds") {
		t.Errorf("a body of %d bytes: err = %v, want a size refusal", MaxRawBytes+1, err)
	}
}

func TestValidateRawReturnsWhatTheSubscriberWillDecode(t *testing.T) {
	reg := newRegistry(t)
	body := envelope("[" + powerDiff("_dev-a", -3, 250) + `,{"object":"_dev-b","attribute":"` + AttrEnergize + `","value":true}]`)
	got, err := validateRaw(reg, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got.DifferenceMRID != "m-1" {
		t.Errorf("difference_mrid = %q, want m-1", got.DifferenceMRID)
	}
	want := []diff.Difference{
		{Object: "_dev-a", Attribute: wAttr, Value: map[string]any{"multiplier": float64(-3), "value": float64(250)}},
		{Object: "_dev-b", Attribute: AttrEnergize, Value: true},
	}
	if fmt.Sprint(got.Forward) != fmt.Sprint(want) {
		t.Errorf("forward = %v, want %v", got.Forward, want)
	}
}

func TestValidateRawAcceptsOptionalFieldsAndReverse(t *testing.T) {
	reg := newRegistry(t)
	body := `{"command":"update","input":{"simulation_id":"7","message":{"difference_mrid":"m","reverse_differences":[{"object":"o","attribute":"a","value":null}],"forward_differences":[` + powerDiff("_dev-a", 0, 1) + `]}}}`
	if _, err := validateRaw(reg, []byte(body)); err != nil {
		t.Errorf("valid message with simulation_id and reverse differences refused: %v", err)
	}
}
