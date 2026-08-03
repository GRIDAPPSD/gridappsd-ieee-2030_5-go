package telemetrypub

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// This file's tests moved here verbatim with MapDERStatusToDifferences
// itself (GAGO-121). The mapping did not change: only the package it
// lives in, so that the 2030.5 embed no longer carries any GridAPPS-D
// wire-shape code at all.

func TestMapDERStatusToDifferences(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{Value: 1, DateTime: 100}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 200}
	alarm := sep2.HexBinary32(7)

	status := sep2.DERStatus{
		GenConnectStatus:      &conn,
		OperationalModeStatus: &mode,
		AlarmStatus:           &alarm,
	}

	diffs, err := MapDERStatusToDifferences("mrid-a", status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 3 {
		t.Fatalf("len(diffs) = %d, want 3", len(diffs))
	}

	byAttr := make(map[string]diff.Difference, len(diffs))
	for _, d := range diffs {
		if d.Object != "mrid-a" {
			t.Errorf("difference %q: Object = %q, want %q", d.Attribute, d.Object, "mrid-a")
		}
		byAttr[d.Attribute] = d
	}

	// ConnectStatusType.Value is sep2.HexBinary8 (sep.xsd:4471,
	// IEEECORE-047), unlike its OperationalModeStatusType sibling below,
	// which stays plain UInt8 (sep.xsd:4559) and so stays uint8 here too.
	if v, ok := byAttr["DERStatus.genConnectStatus"]; !ok || v.Value != sep2.HexBinary8(1) {
		t.Errorf("DERStatus.genConnectStatus = %+v, want Value=1", v)
	}
	if v, ok := byAttr["DERStatus.operationalModeStatus"]; !ok || v.Value != uint8(2) {
		t.Errorf("DERStatus.operationalModeStatus = %+v, want Value=2", v)
	}
	if v, ok := byAttr["DERStatus.alarmStatus"]; !ok || v.Value != sep2.HexBinary32(7) {
		t.Errorf("DERStatus.alarmStatus = %+v, want Value=7", v)
	}
}

func TestMapDERStatusToDifferencesEmptyStatusYieldsNoDifferences(t *testing.T) {
	t.Parallel()

	diffs, err := MapDERStatusToDifferences("mrid-a", sep2.DERStatus{})
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("len(diffs) = %d, want 0 for an empty DERStatus", len(diffs))
	}
}

func TestMapDERStatusToDifferencesRejectsEmptyMRID(t *testing.T) {
	t.Parallel()

	if _, err := MapDERStatusToDifferences("", sep2.DERStatus{}); err == nil {
		t.Fatal("MapDERStatusToDifferences(\"\", ...): want error, got nil")
	}
}

// wantExistingThreeJSON pins the exact JSON the three original mapped
// attributes produce, in order. Field order and value type are both
// part of the contract existing bus consumers already depend on, so any
// of the three drifting fails here rather than silently on the wire.
const wantExistingThreeJSON = `[` +
	`{"object":"mrid-a","attribute":"DERStatus.genConnectStatus","value":1},` +
	`{"object":"mrid-a","attribute":"DERStatus.operationalModeStatus","value":2},` +
	`{"object":"mrid-a","attribute":"DERStatus.alarmStatus","value":7}` +
	`]`

func TestMapDERStatusToDifferencesExistingThreeAreByteIdentical(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{Value: 1, DateTime: 100}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 200}
	alarm := sep2.HexBinary32(7)
	status := sep2.DERStatus{
		GenConnectStatus:      &conn,
		OperationalModeStatus: &mode,
		AlarmStatus:           &alarm,
	}

	diffs, err := MapDERStatusToDifferences("mrid-a", status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	got, err := json.Marshal(diffs)
	if err != nil {
		t.Fatalf("marshal diffs: %v", err)
	}
	if string(got) != wantExistingThreeJSON {
		t.Errorf("existing three-field output changed.\n got = %s\nwant = %s", got, wantExistingThreeJSON)
	}
}

// socEqualEpsilon bounds the tolerance socEqual allows. A sep2.PerCent
// value divided by 100 is not always exactly representable in binary
// floating point (6501/100 = 65.01, whose nearest float64 carries a
// residual on the order of 1e-14). 6500/100 = 65.0 happens to be exact
// (the division of two exactly representable integers whose true
// quotient is itself an integer is exactly representable), so the tests
// below that use 6500 could use `==` and still pass; socEqual is used
// anyway so the assertions do not become spuriously fragile if a test
// value ever changes to a non-round hundredths-of-a-percent input.
const socEqualEpsilon = 1e-9

// socEqual reports whether got and want are equal to within
// socEqualEpsilon. See socEqualEpsilon's doc comment for why an exact
// `==` is not the right tool for a value derived from float64 division.
func socEqual(got, want float64) bool {
	return math.Abs(got-want) < socEqualEpsilon
}

// fullDERStatus returns a DERStatus with every field core models set to a
// distinct, non-zero value, so a mapping that reads the wrong source
// field cannot pass by coincidence. The dateTime sentinels are large and
// mutually non-overlapping so
// TestMapDERStatusToDifferencesDropsPerFieldDateTime can assert on them
// as substrings without colliding with any published value.
func fullDERStatus() sep2.DERStatus {
	conn := sep2.ConnectStatusType{Value: 1, DateTime: 987654001}
	inv := sep2.InverterStatusType{Value: 3, DateTime: 987654003}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 987654002}
	soc := sep2.StateOfChargeStatusType{Value: 6500, DateTime: -5838048000}
	storage := sep2.StorageModeStatusType{Value: 1, DateTime: 987654005}
	alarm := sep2.HexBinary32(7)
	return sep2.DERStatus{
		AlarmStatus:           &alarm,
		GenConnectStatus:      &conn,
		InverterStatus:        &inv,
		OperationalModeStatus: &mode,
		ReadingTime:           1785714218,
		StateOfChargeStatus:   &soc,
		StorageModeStatus:     &storage,
	}
}

func TestMapDERStatusToDifferencesMapsEveryModelledField(t *testing.T) {
	t.Parallel()

	diffs, err := MapDERStatusToDifferences("mrid-a", fullDERStatus())
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 7 {
		t.Fatalf("len(diffs) = %d, want 7 (one per field core's sep2.DERStatus models)", len(diffs))
	}

	byAttr := make(map[string]diff.Difference, len(diffs))
	for _, d := range diffs {
		if d.Object != "mrid-a" {
			t.Errorf("difference %q: Object = %q, want %q", d.Attribute, d.Object, "mrid-a")
		}
		if _, dup := byAttr[d.Attribute]; dup {
			t.Errorf("duplicate difference for attribute %q", d.Attribute)
		}
		byAttr[d.Attribute] = d
	}

	// Values are asserted at their exact wire type as well as their
	// magnitude: a HexBinary8 silently widened to uint16, or a
	// stateOfChargeStatus published raw instead of scaled to percent, is
	// exactly the invisible data change [[data-invariants]] exists to
	// catch. stateOfChargeStatus is compared with tolerance, not `!=`:
	// see socEqual's doc comment for why.
	want := map[string]any{
		"DERStatus.genConnectStatus":      sep2.HexBinary8(1),
		"DERStatus.operationalModeStatus": uint8(2),
		"DERStatus.alarmStatus":           sep2.HexBinary32(7),
		"DERStatus.readingTime":           int64(1785714218),
		"DERStatus.inverterStatus":        uint8(3),
		"DERStatus.stateOfChargeStatus":   float64(65),
		"DERStatus.storageModeStatus":     uint8(1),
	}
	for attr, wantVal := range want {
		got, ok := byAttr[attr]
		if !ok {
			t.Errorf("missing difference for attribute %q", attr)
			continue
		}
		if attr == "DERStatus.stateOfChargeStatus" {
			gotFloat, ok := got.Value.(float64)
			if !ok || !socEqual(gotFloat, wantVal.(float64)) {
				t.Errorf("difference %q: Value = %v (%T), want %v (float64)", attr, got.Value, got.Value, wantVal)
			}
			continue
		}
		if got.Value != wantVal {
			t.Errorf("difference %q: Value = %v (%T), want %v (%T)", attr, got.Value, got.Value, wantVal, wantVal)
		}
	}
	for attr := range byAttr {
		if _, ok := want[attr]; !ok {
			t.Errorf("unexpected difference for attribute %q", attr)
		}
	}
}

// TestMapDERStatusToDifferencesPerField sets exactly one field at a time.
// It proves both halves of the nil-guard contract: a present field always
// publishes, and every ABSENT field publishes nothing (a synthesized zero
// would be indistinguishable from a device genuinely reporting zero).
func TestMapDERStatusToDifferencesPerField(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{Value: 1, DateTime: 100}
	inv := sep2.InverterStatusType{Value: 3, DateTime: 300}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 200}
	soc := sep2.StateOfChargeStatusType{Value: 6500, DateTime: -5838048000}
	// socFractional is deliberately NOT a round percent: 6501/100 = 65.01
	// is not exactly representable in binary floating point, so this case
	// exercises the residual-precision path 6500 (an exact case) cannot.
	socFractional := sep2.StateOfChargeStatusType{Value: 6501, DateTime: -5838048000}
	storage := sep2.StorageModeStatusType{Value: 1, DateTime: 500}
	alarm := sep2.HexBinary32(7)

	tests := []struct {
		name     string
		status   sep2.DERStatus
		wantAttr string
		wantVal  any
	}{
		{"genConnectStatus", sep2.DERStatus{GenConnectStatus: &conn}, "DERStatus.genConnectStatus", sep2.HexBinary8(1)},
		{"operationalModeStatus", sep2.DERStatus{OperationalModeStatus: &mode}, "DERStatus.operationalModeStatus", uint8(2)},
		{"alarmStatus", sep2.DERStatus{AlarmStatus: &alarm}, "DERStatus.alarmStatus", sep2.HexBinary32(7)},
		{"readingTime", sep2.DERStatus{ReadingTime: 1785714218}, "DERStatus.readingTime", int64(1785714218)},
		{"inverterStatus", sep2.DERStatus{InverterStatus: &inv}, "DERStatus.inverterStatus", uint8(3)},
		{"stateOfChargeStatus", sep2.DERStatus{StateOfChargeStatus: &soc}, "DERStatus.stateOfChargeStatus", float64(65)},
		{"stateOfChargeStatus fractional", sep2.DERStatus{StateOfChargeStatus: &socFractional}, "DERStatus.stateOfChargeStatus", float64(65.01)},
		{"storageModeStatus", sep2.DERStatus{StorageModeStatus: &storage}, "DERStatus.storageModeStatus", uint8(1)},
	}

	for _, tc := range tests {
		t.Run(tc.name+" alone publishes exactly one difference", func(t *testing.T) {
			t.Parallel()

			diffs, err := MapDERStatusToDifferences("mrid-a", tc.status)
			if err != nil {
				t.Fatalf("MapDERStatusToDifferences: %v", err)
			}
			if len(diffs) != 1 {
				t.Fatalf("len(diffs) = %d, want exactly 1 (only %s is set)", len(diffs), tc.name)
			}
			if diffs[0].Attribute != tc.wantAttr {
				t.Errorf("Attribute = %q, want %q", diffs[0].Attribute, tc.wantAttr)
			}
			// stateOfChargeStatus is a float64 division result and is
			// compared with tolerance; see socEqual's doc comment.
			if wantFloat, isFloat := tc.wantVal.(float64); isFloat {
				gotFloat, ok := diffs[0].Value.(float64)
				if !ok || !socEqual(gotFloat, wantFloat) {
					t.Errorf("Value = %v (%T), want %v (float64)", diffs[0].Value, diffs[0].Value, wantFloat)
				}
			} else if diffs[0].Value != tc.wantVal {
				t.Errorf("Value = %v (%T), want %v (%T)", diffs[0].Value, diffs[0].Value, tc.wantVal, tc.wantVal)
			}
			if diffs[0].Object != "mrid-a" {
				t.Errorf("Object = %q, want %q", diffs[0].Object, "mrid-a")
			}
		})
	}
}

// TestMapDERStatusToDifferencesPresentZeroValuesPublish is the other side
// of the nil guard: a device that genuinely reports zero must reach the
// bus as zero. Only ABSENCE suppresses a difference.
func TestMapDERStatusToDifferencesPresentZeroValuesPublish(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{}
	soc := sep2.StateOfChargeStatusType{}
	alarm := sep2.HexBinary32(0)
	status := sep2.DERStatus{GenConnectStatus: &conn, StateOfChargeStatus: &soc, AlarmStatus: &alarm}

	diffs, err := MapDERStatusToDifferences("mrid-a", status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 3 {
		t.Fatalf("len(diffs) = %d, want 3: a present zero is a real reading, not an absent field", len(diffs))
	}
	for _, d := range diffs {
		switch d.Attribute {
		case "DERStatus.genConnectStatus":
			if d.Value != sep2.HexBinary8(0) {
				t.Errorf("%s: Value = %v, want HexBinary8(0)", d.Attribute, d.Value)
			}
		case "DERStatus.stateOfChargeStatus":
			// 0/100 = 0.0 exactly, so a direct comparison is safe here;
			// see socEqual's doc comment for the general case.
			if d.Value != float64(0) {
				t.Errorf("%s: Value = %v, want float64(0)", d.Attribute, d.Value)
			}
		case "DERStatus.alarmStatus":
			if d.Value != sep2.HexBinary32(0) {
				t.Errorf("%s: Value = %v, want HexBinary32(0)", d.Attribute, d.Value)
			}
		default:
			t.Errorf("unexpected attribute %q", d.Attribute)
		}
	}
}

// TestMapDERStatusToDifferencesDropsPerFieldDateTime pins the deliberate
// decision to publish only the value half of each complex-typed field.
// The EPRI client observed in e2e run 10 reports
// stateOfChargeStatus/dateTime = -5838048000, a negative epoch landing
// around the year 1785; republishing it would put a bogus timestamp on
// the GridAPPS-D bus where a consumer could reasonably read it as real.
func TestMapDERStatusToDifferencesDropsPerFieldDateTime(t *testing.T) {
	t.Parallel()

	diffs, err := MapDERStatusToDifferences("mrid-a", fullDERStatus())
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	encoded, err := json.Marshal(diffs)
	if err != nil {
		t.Fatalf("marshal diffs: %v", err)
	}
	bannedDateTimeMaterial := []string{
		"dateTime",
		"-5838048000",
		"987654001",
		"987654002",
		"987654003",
		"987654005",
	}
	for _, banned := range bannedDateTimeMaterial {
		if strings.Contains(string(encoded), banned) {
			t.Errorf("mapped output contains per-field dateTime material %q; dateTime must be dropped.\ngot = %s", banned, encoded)
		}
	}
}
