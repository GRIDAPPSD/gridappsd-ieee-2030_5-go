package sep2config

import (
	"reflect"
	"testing"
)

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

// exemptDERControlBaseFields are the only DERControlBase fields
// DefaultPolicy is allowed to set. Every other field on the struct is
// asserted nil by TestDefaultPolicy_EverythingElseUnset below, driven by
// reflection rather than a hand-picked subset: DefaultControl is
// GAGO-050's direct seed source, so a future stray assignment on ANY of
// its ~19 fields (not just the handful reviewed today) must fail this
// test, per data-invariants (present-but-wrong is worse than absent).
var exemptDERControlBaseFields = map[string]bool{
	"OpModConnect":  true,
	"OpModEnergize": true,
}

// TestDefaultPolicy_EverythingElseUnset asserts the deliberate absence of
// every DERControlBase field except OpModConnect/OpModEnergize, plus
// DefaultDERControl's own two sibling ramp fields (SetGradW,
// SetSoftGradW). Per Vance's GAGO-050 physics verdict, opModTargetVar in
// particular must never be set here: it would silently disable the
// device's autonomous volt-var (1547-2018 clause 5.3 mutual exclusivity).
func TestDefaultPolicy_EverythingElseUnset(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()
	base := got.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want a populated base")
	}

	v := reflect.ValueOf(*base)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if exemptDERControlBaseFields[name] {
			continue
		}
		field := v.Field(i)
		if field.Kind() != reflect.Ptr {
			t.Fatalf("DERControlBase.%s is not a pointer field; this test assumes every non-exempt field is a *T (update exemptDERControlBaseFields or the assertion if the type changed)", name)
		}
		if !field.IsNil() {
			t.Errorf("DERControlBase.%s = %v, want nil (unset)", name, field.Elem())
		}
	}

	if got.DefaultControl.SetGradW != nil {
		t.Errorf("DefaultControl.SetGradW = %v, want nil (would overwrite the device's commissioned 1547 ramp)", got.DefaultControl.SetGradW)
	}
	if got.DefaultControl.SetSoftGradW != nil {
		t.Errorf("DefaultControl.SetSoftGradW = %v, want nil", got.DefaultControl.SetSoftGradW)
	}
}

// TestDefaultPolicy_ModesAndRatesUnset asserts the remaining SEP2Policy
// fields default to nil (unset): ModesSupported and the poll/post rates
// have no spec-sane compiled-in default yet (GAGO-049 and any future
// FSA-seeding card own supplying real values). Pointer-typed to match the
// core convention, so nil (not a real 0) is the unset sentinel.
func TestDefaultPolicy_ModesAndRatesUnset(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()
	if got.ModesSupported != nil {
		t.Errorf("ModesSupported = %v, want nil (unset)", *got.ModesSupported)
	}
	if got.DefaultPollRate != nil {
		t.Errorf("DefaultPollRate = %v, want nil (unset)", *got.DefaultPollRate)
	}
	if got.DefaultPostRate != nil {
		t.Errorf("DefaultPostRate = %v, want nil (unset)", *got.DefaultPostRate)
	}
}
