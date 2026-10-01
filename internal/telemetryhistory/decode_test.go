package telemetryhistory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

func loadTestdataMessage(t *testing.T, name string) diff.Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	var msg diff.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshaling testdata/%s: %v", name, err)
	}
	return msg
}

// TestDecodeMessage_RealCapturedFrame decodes the one genuine captured
// frame this project has ever observed on the bus (see
// testdata/README.md). It proves the decoder accepts the real wire
// shape. Because this frame's multiplier is 0, it CANNOT by itself prove
// the multiplier is applied: see the two synthetic-multiplier tests
// below, which exist specifically for that.
func TestDecodeMessage_RealCapturedFrame(t *testing.T) {
	t.Parallel()
	msg := loadTestdataMessage(t, "real_capture_diff_message.json")

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage(real capture) = %d samples, want 1 (got %+v)", len(out), out)
	}
	d := out[0]
	wantKey := SeriesKey{
		Object:    "50B15A48-9611-40DF-983E-93679DA72871",
		Attribute: "DERControl.DERControlBase.opModTargetW",
	}
	if d.Key != wantKey {
		t.Fatalf("Key = %+v, want %+v", d.Key, wantKey)
	}
	if d.Sample.Value != 5000.0 {
		t.Fatalf("Value = %v, want 5000.0 (multiplier 0 => 10^0 == 1, unscaled)", d.Sample.Value)
	}
	if d.Sample.At != 1785390891 {
		t.Fatalf("At = %d, want the envelope's message.timestamp 1785390891", d.Sample.At)
	}
	if d.Lane != LaneCommandedSetpoint {
		t.Fatalf("Lane = %q, want %q", d.Lane, LaneCommandedSetpoint)
	}
}

// TestDecodeMessage_SyntheticPositiveMultiplier is one of the two
// synthetic cases (testdata/README.md explains why they exist): the
// captured frame alone has multiplier 0, at which 10^0 == 1 and an
// implementation that ignores the multiplier entirely produces the same
// answer as a correct one. This frame has multiplier 2, so value 5000
// must decode to 5000 * 10^2 == 500000. An implementation that drops the
// multiplier decodes this to 5000 and fails here, off by 100x.
func TestDecodeMessage_SyntheticPositiveMultiplier(t *testing.T) {
	t.Parallel()
	msg := loadTestdataMessage(t, "synthetic_multiplier_pos2.json")

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage(synthetic +2) = %d samples, want 1 (got %+v)", len(out), out)
	}
	if got := out[0].Sample.Value; got != 500000.0 {
		t.Fatalf("Value = %v, want 500000.0 (5000 * 10^2)", got)
	}
}

// TestDecodeMessage_SyntheticNegativeMultiplier is the other synthetic
// case. A positive-only multiplier test would still pass an
// implementation that has the multiplier's sign backwards (applying
// 10^-multiplier instead of 10^multiplier); this frame has multiplier
// -2, so value 5000 must decode to 5000 * 10^-2 == 50, catching a
// reversed sign.
func TestDecodeMessage_SyntheticNegativeMultiplier(t *testing.T) {
	t.Parallel()
	msg := loadTestdataMessage(t, "synthetic_multiplier_neg2.json")

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage(synthetic -2) = %d samples, want 1 (got %+v)", len(out), out)
	}
	if got := out[0].Sample.Value; got != 50.0 {
		t.Fatalf("Value = %v, want 50.0 (5000 * 10^-2)", got)
	}
}

// TestDecodeMessage_BareScalarValueShape decodes the UP-lane value
// shape: a bare float64, exactly what
// telemetrypub.MapDERStatusToDifferences emits for
// DERStatus.stateOfChargeStatus (mapping.go:192). This also carries the
// "SOC scale preserved end to end" acceptance case: a published 65.0
// must decode to exactly 65.0, never 6500 (the wire hundredths-of-a-
// percent the mapping layer already scaled away) and never 0.65 (an
// accidental re-division). A zero-valued SOC is deliberately not used
// here, since a zero-valued case cannot distinguish "scaled correctly"
// from "silently coerced to zero."
func TestDecodeMessage_BareScalarValueShape(t *testing.T) {
	t.Parallel()
	msg := diff.Message{
		Command: "update",
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1785390900,
				ForwardDifferences: []diff.Difference{
					{
						Object:    "dev-1",
						Attribute: "DERStatus.stateOfChargeStatus",
						Value:     65.0,
					},
				},
				ReverseDifferences: []diff.Difference{
					{
						Object:    "dev-1",
						Attribute: "DERStatus.stateOfChargeStatus",
						Value:     65.0,
					},
				},
			},
		},
	}

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage(bare scalar) = %d samples, want 1 (got %+v)", len(out), out)
	}
	d := out[0]
	if d.Sample.Value != 65.0 {
		t.Fatalf("Value = %v, want exactly 65.0 (not 6500, not 0.65)", d.Sample.Value)
	}
	if d.Lane != LaneReportedState {
		t.Fatalf("Lane = %q, want %q", d.Lane, LaneReportedState)
	}
}

// TestDecodeMessage_ForwardOnly asserts only forward_differences are
// plotted. The fixture's forward (5000) and reverse (0) genuinely
// differ, so a decoder that accidentally decoded reverse instead of, or
// in addition to, forward would fail this assertion. A DERStatus frame
// cannot carry this assertion (message.go's DiffMessageBuilder sets
// reverse equal to forward on that lane by design), which is why this
// uses the DERControl lane, same as the multiplier fixtures.
func TestDecodeMessage_ForwardOnly(t *testing.T) {
	t.Parallel()
	msg := loadTestdataMessage(t, "real_capture_diff_message.json")

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage = %d samples, want exactly 1", len(out))
	}
	if got := out[0].Sample.Value; got != 5000.0 {
		t.Fatalf("Value = %v, want the FORWARD value 5000.0, not the reverse value 0.0", got)
	}
}

// TestDecodeMessage_LaneClassificationByPrefix asserts each known
// attribute prefix classifies to its own lane, and an unrecognized
// prefix is skipped rather than defaulted into either lane.
func TestDecodeMessage_LaneClassificationByPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		attribute string
		wantLane  Lane
		wantOK    bool
	}{
		{"DERStatus prefix", "DERStatus.stateOfChargeStatus", LaneReportedState, true},
		{"DERControl prefix", "DERControl.DERControlBase.opModTargetW", LaneCommandedSetpoint, true},
		{"unrecognized prefix", "SomeOtherThing.value", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lane, ok := classifyLane(tt.attribute)
			if ok != tt.wantOK || lane != tt.wantLane {
				t.Fatalf("classifyLane(%q) = (%q, %v), want (%q, %v)", tt.attribute, lane, ok, tt.wantLane, tt.wantOK)
			}
		})
	}
}

// TestDecodeMessage_UnrecognizedPrefixSkipped asserts a full
// DecodeMessage run drops a difference whose attribute prefix is
// unrecognized, and does not fabricate a zero-valued sample for it.
func TestDecodeMessage_UnrecognizedPrefixSkipped(t *testing.T) {
	t.Parallel()
	msg := diff.Message{
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1,
				ForwardDifferences: []diff.Difference{
					{Object: "dev-1", Attribute: "SomeOtherThing.value", Value: 42.0},
				},
			},
		},
	}
	out := DecodeMessage(msg, nil)
	if len(out) != 0 {
		t.Fatalf("DecodeMessage(unrecognized prefix) = %+v, want no samples", out)
	}
}

// TestDecodeMessage_AllowlistEnforced asserts that a frame carrying a
// bitmap (alarmStatus) and a non-numeric-line field (readingTime)
// alongside an allowlisted attribute (stateOfChargeStatus) yields ONLY
// the allowlisted series. alarmStatus is a HexBinary32 bitmap, not a
// plottable scalar.
func TestDecodeMessage_AllowlistEnforced(t *testing.T) {
	t.Parallel()
	msg := diff.Message{
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1785390900,
				ForwardDifferences: []diff.Difference{
					{Object: "dev-1", Attribute: "DERStatus.alarmStatus", Value: "00000000"},
					{Object: "dev-1", Attribute: "DERStatus.readingTime", Value: 1785390900.0},
					{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus", Value: 65.0},
				},
			},
		},
	}

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage(allowlist) = %d samples, want exactly 1 (got %+v)", len(out), out)
	}
	wantKey := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}
	if out[0].Key != wantKey {
		t.Fatalf("Key = %+v, want %+v (only the allowlisted attribute should survive)", out[0].Key, wantKey)
	}
}

// TestDecodeMessage_TimestampIgnoresDeviceReportedDateTime asserts the
// sample timestamp always comes from the envelope's message.timestamp,
// never from a device-reported per-field dateTime that might be smuggled
// into the value payload. An observed EPRI client reported
// stateOfChargeStatus/dateTime landing in the year 1785
// (-5838048000); that value must never appear in decoder output.
func TestDecodeMessage_TimestampIgnoresDeviceReportedDateTime(t *testing.T) {
	t.Parallel()
	const badDateTime = -5838048000.0
	msg := diff.Message{
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1785390900,
				ForwardDifferences: []diff.Difference{
					{
						Object:    "dev-1",
						Attribute: "DERControl.DERControlBase.opModTargetW",
						Value: map[string]any{
							"multiplier": 0.0,
							"value":      5000.0,
							"dateTime":   badDateTime,
						},
					},
				},
			},
		},
	}

	out := DecodeMessage(msg, nil)
	if len(out) != 1 {
		t.Fatalf("DecodeMessage = %d samples, want 1", len(out))
	}
	d := out[0]
	if d.Sample.At != 1785390900 {
		t.Fatalf("At = %d, want the envelope timestamp 1785390900", d.Sample.At)
	}
	if d.Sample.At == int64(badDateTime) || d.Sample.Value == badDateTime {
		t.Fatalf("the device-reported dateTime %v leaked into the sample: %+v", badDateTime, d)
	}
}

// TestDecodeMessage_UnrecognizedValueShapeSkipped asserts a Value shape
// the decoder does not recognize (a bare string here) is skipped and
// never coerced to a zero-valued sample, and that the caller-supplied
// logf is invoked.
func TestDecodeMessage_UnrecognizedValueShapeSkipped(t *testing.T) {
	t.Parallel()
	msg := diff.Message{
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1,
				ForwardDifferences: []diff.Difference{
					{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus", Value: "not-a-number"},
				},
			},
		},
	}

	var logged bool
	out := DecodeMessage(msg, func(string, ...any) { logged = true })
	if len(out) != 0 {
		t.Fatalf("DecodeMessage(unrecognized value shape) = %+v, want no samples", out)
	}
	if !logged {
		t.Fatalf("logf was not called for a skipped difference")
	}
}

func TestDecodeMessage_NonFiniteValueSkippedAndLogged(t *testing.T) {
	t.Parallel()
	msg := diff.Message{
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1700000000,
				ForwardDifferences: []diff.Difference{
					{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus",
						Value: map[string]any{"multiplier": 400.0, "value": 1.0}},
				},
			},
		},
	}
	var logged string
	out := DecodeMessage(msg, func(f string, a ...any) { logged = fmt.Sprintf(f, a...) })
	if len(out) != 0 {
		t.Fatalf("DecodeMessage(+Inf) = %+v, want no samples", out)
	}
	if !strings.Contains(logged, "non-finite") {
		t.Errorf("logged = %q, want a non-finite fault", logged)
	}
}

func TestDecodeMessage_RoutineSkipsAreSilent(t *testing.T) {
	t.Parallel()
	msg := diff.Message{
		Input: diff.Input{
			Message: diff.MessagePayload{
				Timestamp: 1700000000,
				ForwardDifferences: []diff.Difference{
					{Object: "dev-1", Attribute: "DERStatus.readingTime", Value: 1.0},
					{Object: "dev-1", Attribute: "Other.thing", Value: 1.0},
				},
			},
		},
	}
	DecodeMessage(msg, func(f string, a ...any) { t.Errorf("routine skip logged: "+f, a...) })
}
