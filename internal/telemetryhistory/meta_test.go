package telemetryhistory

import "testing"

// TestMetaForAllowlistedAttributes asserts the exact Lane and Unit for
// both entries on the decoder's allowlist, by value: a client must never
// have to infer either from the attribute name itself.
func TestMetaForAllowlistedAttributes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		attribute string
		wantLane  Lane
		wantUnit  string
	}{
		{
			name:      "reported state SOC is percent",
			attribute: "DERStatus.stateOfChargeStatus",
			wantLane:  LaneReportedState,
			wantUnit:  "percent",
		},
		{
			name:      "commanded setpoint target is watts",
			attribute: "DERControl.DERControlBase.opModTargetW",
			wantLane:  LaneCommandedSetpoint,
			wantUnit:  "watts",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta, ok := MetaFor(tc.attribute)
			if !ok {
				t.Fatalf("MetaFor(%q) ok = false, want true", tc.attribute)
			}
			if meta.Lane != tc.wantLane {
				t.Errorf("MetaFor(%q).Lane = %q, want %q", tc.attribute, meta.Lane, tc.wantLane)
			}
			if meta.Unit != tc.wantUnit {
				t.Errorf("MetaFor(%q).Unit = %q, want %q", tc.attribute, meta.Unit, tc.wantUnit)
			}
		})
	}

	// The two units must actually differ: this is the whole reason a
	// percent series and a watts series can never share a y-axis. A test
	// that let both resolve to the same unit string would pass while the
	// axis-separation invariant silently broke.
	socMeta, _ := MetaFor("DERStatus.stateOfChargeStatus")
	targetMeta, _ := MetaFor("DERControl.DERControlBase.opModTargetW")
	if socMeta.Unit == targetMeta.Unit {
		t.Fatalf("SOC unit %q equals target-W unit %q; percent and watts must never share a unit string", socMeta.Unit, targetMeta.Unit)
	}
}

// TestMetaForRejectsNonRetainableAttributes: an attribute DecodeMessage
// would never retain a sample for (unrecognized prefix, or a recognized
// prefix that is not on the allowlist) must return ok=false, never a
// fabricated Lane/Unit.
func TestMetaForRejectsNonRetainableAttributes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		attribute string
	}{
		{"unrecognized prefix", "SomeOtherThing.value"},
		{"empty attribute", ""},
		{"recognized DERStatus prefix but not allowlisted: bitmap", "DERStatus.alarmStatus"},
		{"recognized DERStatus prefix but not allowlisted: enum", "DERStatus.genConnectStatus"},
		{"recognized DERStatus prefix but not allowlisted: device timestamp", "DERStatus.readingTime"},
		{"recognized DERControl prefix but not allowlisted", "DERControl.DERControlBase.opModEnergize"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta, ok := MetaFor(tc.attribute)
			if ok {
				t.Fatalf("MetaFor(%q) ok = true, want false; got %+v", tc.attribute, meta)
			}
			if meta != (SeriesMeta{}) {
				t.Errorf("MetaFor(%q) = %+v on ok=false, want the zero value", tc.attribute, meta)
			}
		})
	}
}
