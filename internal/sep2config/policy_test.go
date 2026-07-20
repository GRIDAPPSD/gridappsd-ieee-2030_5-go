package sep2config

import "testing"

// TestDefaultPolicy_Connect_Energize asserts the two DefaultControl fields
// DefaultPolicy sets: both must be non-nil and true. Per data-invariants,
// a nil-check alone would not catch a present-but-false pointer, so both
// the nil-check and the dereferenced value are asserted.
func TestDefaultPolicy_Connect_Energize(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()
	base := got.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want a populated base")
	}

	if base.OpModConnect == nil || *base.OpModConnect != true {
		t.Errorf("OpModConnect = %v, want true", base.OpModConnect)
	}
	if base.OpModEnergize == nil || *base.OpModEnergize != true {
		t.Errorf("OpModEnergize = %v, want true", base.OpModEnergize)
	}
}

// TestDefaultPolicy_EverythingElseUnset asserts the deliberate absence of
// every other DERControlBase field, plus DefaultDERControl's own two
// sibling ramp fields (SetGradW, SetSoftGradW). Per data-invariants and
// Vance's GAGO-050 physics verdict, a stray non-nil value here (e.g. a
// forced opModTargetVar) is the failure mode this guards against:
// present-but-wrong is worse than absent.
func TestDefaultPolicy_EverythingElseUnset(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()
	base := got.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want a populated base")
	}

	if base.OpModTargetW != nil {
		t.Errorf("OpModTargetW = %v, want nil (forcing it disables autonomous curtailment)", base.OpModTargetW)
	}
	if base.OpModTargetVar != nil {
		t.Errorf("OpModTargetVar = %v, want nil (forcing it disables autonomous volt-var per 1547-2018 clause 5.3)", base.OpModTargetVar)
	}
	if base.OpModFixedW != nil {
		t.Errorf("OpModFixedW = %v, want nil", base.OpModFixedW)
	}
	if base.OpModFixedVar != nil {
		t.Errorf("OpModFixedVar = %v, want nil", base.OpModFixedVar)
	}
	if base.OpModMaxLimW != nil {
		t.Errorf("OpModMaxLimW = %v, want nil", base.OpModMaxLimW)
	}
	if base.RampTms != nil {
		t.Errorf("RampTms = %v, want nil", base.RampTms)
	}

	if got.DefaultControl.SetGradW != nil {
		t.Errorf("DefaultControl.SetGradW = %v, want nil (would overwrite the device's commissioned 1547 ramp)", got.DefaultControl.SetGradW)
	}
	if got.DefaultControl.SetSoftGradW != nil {
		t.Errorf("DefaultControl.SetSoftGradW = %v, want nil", got.DefaultControl.SetSoftGradW)
	}
}

// TestDefaultPolicy_ModesAndRatesZero asserts the remaining SEP2Policy
// fields default to their zero (unset) values: ModesSupported and the
// poll/post rates have no spec-sane compiled-in default yet (GAGO-049 and
// any future FSA-seeding card own supplying real values).
func TestDefaultPolicy_ModesAndRatesZero(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()
	if got.ModesSupported != 0 {
		t.Errorf("ModesSupported = %d, want 0 (unset)", got.ModesSupported)
	}
	if got.DefaultPollRate != 0 {
		t.Errorf("DefaultPollRate = %d, want 0 (unset)", got.DefaultPollRate)
	}
	if got.DefaultPostRate != 0 {
		t.Errorf("DefaultPostRate = %d, want 0 (unset)", got.DefaultPostRate)
	}
}
