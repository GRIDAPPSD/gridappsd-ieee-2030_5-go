package sep2config

import (
	"fmt"
	"reflect"
	"strings"
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

// TestValidateRegistrationPIN covers the PINType range guard on the
// operator-supplied registration PIN. sep.xsd's PINType (lines 5972-5980)
// is an xs:extension of UInt32 with NO facets, so the documented
// "6 digit unsigned decimal integer (0 - 999999)" range is enforced here
// and nowhere else: a schema validator will not catch an out-of-range
// value, which is why this guard exists at config load.
func TestValidateRegistrationPIN(t *testing.T) {
	t.Parallel()

	const lfdi = "AAAA00000000000000000000000000000000AAAA"

	tests := []struct {
		name    string
		policy  SEP2Policy
		wantErr bool
	}{
		{name: "no PIN configured at all is not a range error", policy: SEP2Policy{}},
		{name: "zero is a valid PINType value", policy: SEP2Policy{RegistrationPINs: map[string]uint32{lfdi: 0}}},
		{name: "a checksum-valid value at the top of the range is accepted", policy: SEP2Policy{RegistrationPINs: map[string]uint32{lfdi: 999995}}},
		{
			name:    "the range maximum still fails the checksum rule",
			policy:  SEP2Policy{RegistrationPINs: map[string]uint32{lfdi: MaxRegistrationPIN}},
			wantErr: true,
		},
		{
			name:    "one past the maximum is refused",
			policy:  SEP2Policy{RegistrationPINs: map[string]uint32{lfdi: MaxRegistrationPIN + 1}},
			wantErr: true,
		},
		{
			name:    "a seven digit value is refused",
			policy:  SEP2Policy{RegistrationPINs: map[string]uint32{lfdi: 1234567}},
			wantErr: true,
		},
		{
			name:    "an out of range fleet default is refused",
			policy:  SEP2Policy{DefaultRegistrationPIN: u32(1000000)},
			wantErr: true,
		},
		{name: "an in range fleet default is accepted", policy: SEP2Policy{DefaultRegistrationPIN: u32(123455)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.policy.ValidateRegistrationPIN()
			if tt.wantErr && err == nil {
				t.Fatal("ValidateRegistrationPIN returned nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateRegistrationPIN returned %v, want nil", err)
			}
			// The PIN is a shared secret in the registration flow and boot
			// errors are logged, so the message must name the bound and the
			// device without echoing the rejected value.
			if err != nil {
				for _, v := range tt.policy.RegistrationPINs {
					if strings.Contains(err.Error(), fmt.Sprintf("%d", v)) {
						t.Error("ValidateRegistrationPIN error message echoes the rejected PIN value")
					}
				}
				if tt.policy.DefaultRegistrationPIN != nil &&
					strings.Contains(err.Error(), fmt.Sprintf("%d", *tt.policy.DefaultRegistrationPIN)) {
					t.Error("ValidateRegistrationPIN error message echoes the rejected fleet PIN value")
				}
			}
		})
	}
}

// TestResolveRegistrationPINPrefersPerDeviceOverFleetDefault asserts the
// provisioning precedence and, critically, that an unconfigured device
// reports false rather than yielding a zero. sep.xsd:184 makes pIN
// minOccurs=1 in the Registration sequence (lines 172-197), so 0 is
// schema-valid but semantically empty; the caller must be able to tell
// "not configured" from "configured as 0" and refuse rather than serve it.
func TestResolveRegistrationPINPrefersPerDeviceOverFleetDefault(t *testing.T) {
	t.Parallel()

	const (
		known   = "AAAA00000000000000000000000000000000AAAA"
		unknown = "BBBB00000000000000000000000000000000BBBB"
	)
	// Obvious dummies, never plausible operator values.
	const perDevice, fleet = uint32(123455), uint32(222220)

	t.Run("per-device entry wins over the fleet default", func(t *testing.T) {
		t.Parallel()
		p := SEP2Policy{
			RegistrationPINs:       map[string]uint32{known: perDevice},
			DefaultRegistrationPIN: u32(fleet),
		}
		got, ok := p.ResolveRegistrationPIN(known)
		if !ok {
			t.Fatal("ResolveRegistrationPIN reported no PIN for a configured device")
		}
		if got != perDevice {
			t.Error("ResolveRegistrationPIN returned the fleet default for a device with its own entry")
		}
	})

	t.Run("an unlisted device falls back to the fleet default", func(t *testing.T) {
		t.Parallel()
		p := SEP2Policy{
			RegistrationPINs:       map[string]uint32{known: perDevice},
			DefaultRegistrationPIN: u32(fleet),
		}
		got, ok := p.ResolveRegistrationPIN(unknown)
		if !ok {
			t.Fatal("ResolveRegistrationPIN reported no PIN despite a fleet default")
		}
		if got != fleet {
			t.Error("ResolveRegistrationPIN did not return the fleet default for an unlisted device")
		}
	})

	t.Run("no entry and no default reports unconfigured, not zero", func(t *testing.T) {
		t.Parallel()
		p := SEP2Policy{RegistrationPINs: map[string]uint32{known: perDevice}}
		got, ok := p.ResolveRegistrationPIN(unknown)
		if ok {
			t.Fatal("ResolveRegistrationPIN claimed a PIN for an unconfigured device")
		}
		if got != 0 {
			t.Error("the unconfigured return value should be the zero value, paired with ok=false")
		}
	})

	t.Run("lookup is case-insensitive on the canonical LFDI", func(t *testing.T) {
		t.Parallel()
		p := SEP2Policy{RegistrationPINs: map[string]uint32{strings.ToLower(known): perDevice}}
		got, ok := p.ResolveRegistrationPIN(known)
		if !ok || got != perDevice {
			t.Error("ResolveRegistrationPIN did not match a lowercase-configured LFDI key")
		}
	})
}

// TestResolveRegistrationPINIsNotAFunctionOfLFDI is the regression guard
// for the removed derivation. The PIN must be operator-supplied: it must
// not vary with device identity, because the LFDI is a SHA-256 digest of
// the device certificate that every TLS peer already sees, so a PIN
// computed from it would be computable by anyone who can reach the device.
func TestResolveRegistrationPINIsNotAFunctionOfLFDI(t *testing.T) {
	t.Parallel()

	const shared = uint32(123455) // obvious dummy
	p := SEP2Policy{DefaultRegistrationPIN: u32(shared)}

	lfdis := []string{
		"0000000000000000000000000000000000000000",
		"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
		"0011223344556677889900112233445566778899",
	}
	for _, lfdi := range lfdis {
		got, ok := p.ResolveRegistrationPIN(lfdi)
		if !ok {
			t.Fatalf("no PIN resolved for %q despite a fleet default", lfdi)
		}
		if got != shared {
			t.Errorf("served PIN varies with the LFDI %q; it must be the configured value alone", lfdi)
		}
	}

	// Two distinct devices configured with the same PIN serve the same
	// value: identity contributes nothing.
	same := SEP2Policy{RegistrationPINs: map[string]uint32{lfdis[0]: shared, lfdis[1]: shared}}
	a, aOK := same.ResolveRegistrationPIN(lfdis[0])
	b, bOK := same.ResolveRegistrationPIN(lfdis[1])
	if !aOK || !bOK || a != b {
		t.Error("two devices configured with the same PIN did not resolve to the same value")
	}
}

// TestHasValidPINCheckDigit pins the IEEE 2030.5 section 6.3.5 checksum
// rule, which is normative on input validation: "the sum of the digits of
// the PIN including the checksum digit, modulo 10, SHALL be zero". The
// cases below use the standard's own worked example (PIN 12345, digits
// summing to 15, check digit 5, displayed value 123455, total 20).
//
// Note the sep.xsd annotation's "See Section 8.3.2" cross-reference is an
// erratum carrying 2013 numbering; in 2018 and 2023, 8.3.2 is "List
// ordering". The rule lives in the body text at 6.3.5, identical word for
// word across the 2013, 2018, and 2023 editions.
func TestHasValidPINCheckDigit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pin  uint32
		want bool
	}{
		{name: "zero has a consistent check digit", pin: 0, want: true},
		{name: "digits summing to a multiple of ten pass", pin: 123455, want: true},
		{name: "digits not summing to a multiple of ten fail", pin: 123456, want: false},
		{name: "out of PINType range fails", pin: MaxRegistrationPIN + 1, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HasValidPINCheckDigit(tt.pin); got != tt.want {
				t.Errorf("HasValidPINCheckDigit() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDefaultPolicyConfiguresNoRegistrationPIN locks the production
// default: no compiled-in PIN, fleet-wide or per-device. A PIN is
// operator-supplied, so an unconfigured bridge must fail closed at boot
// rather than boot with a value nobody chose.
func TestDefaultPolicyConfiguresNoRegistrationPIN(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()
	if p.DefaultRegistrationPIN != nil {
		t.Error("DefaultPolicy sets a compiled-in fleet-wide registration PIN; it must stay unset")
	}
	if len(p.RegistrationPINs) != 0 {
		t.Error("DefaultPolicy ships per-device registration PINs; they must be operator-supplied")
	}
	if _, ok := p.ResolveRegistrationPIN("AAAA00000000000000000000000000000000AAAA"); ok {
		t.Error("DefaultPolicy resolves a registration PIN; an unconfigured bridge must fail closed")
	}
}

func u32(v uint32) *uint32 { return &v }
