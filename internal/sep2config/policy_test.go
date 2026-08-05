package sep2config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestDefaultPolicy_CommandsNothing asserts the single most consequential
// property of the shipped DefaultDERControl: it commands nothing.
//
// The two fields named here are called out separately from the reflective
// sweep below because they are the ones that would do physical damage.
// opModConnect is documented as connecting or disconnecting the DER "from
// the grid", with the annotation invoking galvanic isolation
// (sep.xsd:3758), so a non-nil pointer is a commanded device closure or
// opening, not a neutral statement of intent. Under the 2023 edition's
// per-mode fallback evaluation it would be asserted continuously whenever
// no control is active, rather than once at expiry.
//
// Both halves of the assertion matter and neither substitutes for the
// other: a present-but-false opModConnect is not "off", it is a commanded
// DISCONNECT of every DER in the fleet, which is why the test demands nil
// rather than merely a falsy value.
func TestDefaultPolicy_CommandsNothing(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()

	// Present but empty, not absent: DERControlBase is minOccurs=1 on
	// DefaultDERControl (sep.xsd:3270), so a nil base would omit a required
	// element and make the served document non-conformant.
	base := got.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want a present but empty base (minOccurs=1, sep.xsd:3270)")
	}

	if base.OpModConnect != nil {
		t.Errorf("DefaultControl.DERControlBase.OpModConnect = %v, want nil; "+
			"any non-nil value commands a grid connect or disconnect (galvanic isolation, sep.xsd:3758), "+
			"and a fallback must command nothing", *base.OpModConnect)
	}
	if base.OpModEnergize != nil {
		t.Errorf("DefaultControl.DERControlBase.OpModEnergize = %v, want nil; "+
			"a fallback must not energize or de-energize a device, it must leave it on its own IEEE 1547 behavior", *base.OpModEnergize)
	}
}

// exemptDERControlBaseFields lists the DERControlBase fields DefaultPolicy is
// allowed to set. It is EMPTY, and that is the point: the shipped default
// commands nothing, so no field is exempt from the nil sweep below.
//
// It is kept as a declared empty map rather than deleted so that the sweep
// keeps its shape and a future decision to ship a non-empty default has one
// obvious place to record itself, next to the reasoning for why that is
// normally wrong.
var exemptDERControlBaseFields = map[string]bool{}

// TestDefaultPolicy_EverythingElseUnset asserts the deliberate absence of
// EVERY DERControlBase field, plus DefaultDERControl's own two sibling ramp
// fields (SetGradW, SetSoftGradW).
//
// Driven by reflection rather than a hand-picked list because DefaultControl
// is the direct seed source for every device's fallback: a stray assignment
// on ANY of the roughly nineteen fields must fail here, not just on the
// handful anyone reviewed. Two of those fields have named reasons.
// opModTargetVar would silently disable the device's autonomous volt-var
// (1547-2018 clause 5.3 mutual exclusivity), and SetGradW SHALL update the
// corresponding DERSettings value (sep.xsd:3306), which is an
// installer-owned persistent write rather than a control-channel fallback.
func TestDefaultPolicy_EverythingElseUnset(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()
	base := got.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want a present but empty base")
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
// have no spec-sane compiled-in default yet. Pointer-typed to match the
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

// TestDefaultPolicy_DERControlIsServiceable asserts the shipped issued-control
// temporal policy: a real non-zero window, no expiry randomization, and a
// value the boot validator accepts.
//
// The zero-duration case is the one that matters. A DERControl whose interval
// duration is 0 has an end equal to its start, so a conformant client marks it
// expired the moment it arrives; it still fetches, parses and acknowledges the
// event, so every observable signal short of the device itself reports
// success. Shipping that as the compiled-in default would reintroduce the
// exact defect this policy exists to prevent, which is why the default is
// asserted usable rather than merely present.
func TestDefaultPolicy_DERControlIsServiceable(t *testing.T) {
	t.Parallel()

	got := DefaultPolicy()

	if got.DERControl.Duration != DefaultDERControlDuration {
		t.Errorf("DERControl.Duration = %d, want %d (DefaultDERControlDuration)",
			got.DERControl.Duration, DefaultDERControlDuration)
	}
	if got.DERControl.Duration == 0 {
		t.Error("DERControl.Duration = 0: a zero-length interval is expired on arrival and never actuates")
	}

	// Zero, and asserted rather than assumed: a co-simulation must be
	// reproducible, and a client-chosen random expiry offset makes two runs of
	// the same scenario diverge. A field deployment sets this non-zero.
	if got.DERControl.RandomizeDuration != 0 {
		t.Errorf("DERControl.RandomizeDuration = %d, want 0 for reproducible co-simulation runs",
			got.DERControl.RandomizeDuration)
	}

	if err := got.ValidateDERControl(); err != nil {
		t.Errorf("DefaultPolicy().ValidateDERControl() = %v, want nil; the shipped default must pass its own boot gate", err)
	}
}

// TestValidateDERControl covers the three domain rules on the issued-control
// temporal policy. sep.xsd enforces none of them: OneHourRangeType
// (sep.xsd:5929) is a bare xs:extension of Int16 carrying no facets, exactly
// like PINType, so its documented -3600 to 3600 range is inert to a validator,
// and nothing in the schema relates randomizeDuration to the duration it
// perturbs.
func TestValidateDERControl(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		control     DERControlPolicy
		wantErr     bool
		wantSubstrs []string
	}{
		{
			name:    "compiled-in default is accepted",
			control: DefaultPolicy().DERControl,
		},
		{
			name:    "a duration of one second is accepted",
			control: DERControlPolicy{Duration: 1},
		},
		{
			name:    "randomization just inside the duration is accepted",
			control: DERControlPolicy{Duration: 100, RandomizeDuration: 99},
		},
		{
			name:    "negative randomization just inside the duration is accepted",
			control: DERControlPolicy{Duration: 100, RandomizeDuration: -99},
		},
		{
			name:    "the OneHourRangeType bound itself is accepted when the duration allows it",
			control: DERControlPolicy{Duration: 7200, RandomizeDuration: 3600},
		},
		{
			name:    "zero duration is refused",
			control: DERControlPolicy{Duration: 0},
			wantErr: true,
			// The flag, not the struct field: the flag is what an operator
			// can act on. Same convention as ValidateRates.
			wantSubstrs: []string{"-sep2-control-duration"},
		},
		{
			name:        "randomization above the OneHourRangeType bound is refused",
			control:     DERControlPolicy{Duration: 100000, RandomizeDuration: 3601},
			wantErr:     true,
			wantSubstrs: []string{"-sep2-control-randomize-duration", "3601"},
		},
		{
			name:        "randomization below the OneHourRangeType bound is refused",
			control:     DERControlPolicy{Duration: 100000, RandomizeDuration: -3601},
			wantErr:     true,
			wantSubstrs: []string{"-sep2-control-randomize-duration", "-3601"},
		},
		{
			// The most negative int32 has no positive counterpart, so
			// negating it in int32 width wraps back to itself and stays
			// negative. A magnitude check that did not widen first would
			// compare a negative number against the bound and let this pass.
			name:        "the most negative int32 randomization is refused rather than wrapping",
			control:     DERControlPolicy{Duration: 100000, RandomizeDuration: -2147483648},
			wantErr:     true,
			wantSubstrs: []string{"-sep2-control-randomize-duration"},
		},
		{
			name:        "randomization equal to the duration is refused",
			control:     DERControlPolicy{Duration: 100, RandomizeDuration: 100},
			wantErr:     true,
			wantSubstrs: []string{"-sep2-control-randomize-duration", "-sep2-control-duration"},
		},
		{
			name:        "randomization wider than the duration is refused",
			control:     DERControlPolicy{Duration: 60, RandomizeDuration: -600},
			wantErr:     true,
			wantSubstrs: []string{"-sep2-control-randomize-duration", "-sep2-control-duration"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := SEP2Policy{DERControl: tt.control}.ValidateDERControl()
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateDERControl(%+v) = nil, want an error", tt.control)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateDERControl(%+v) = %v, want nil", tt.control, err)
				}
				return
			}
			for _, want := range tt.wantSubstrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ValidateDERControl(%+v) error = %q, want it to name %q", tt.control, err, want)
				}
			}
		})
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

// TestDefaultPolicyDERProgramIsSpecSaneAndServable pins the compiled-in
// default DERProgram, the one an operator who configures nothing gets.
//
// Primacy 1 is asserted as the named constant rather than a bare literal so
// the test states the standard's meaning, not just a number: sep.xsd's
// PrimacyType documents 1 as "Contracted premises service provider", which
// is what a utility platform operating the feeder is.
func TestDefaultPolicyDERProgramIsSpecSaneAndServable(t *testing.T) {
	t.Parallel()

	p := DefaultPolicy()

	if p.DefaultProgram.Primacy != PrimacyContractedServiceProvider {
		t.Errorf("DefaultPolicy DefaultProgram.Primacy = %d, want %d (contracted premises service provider)",
			p.DefaultProgram.Primacy, PrimacyContractedServiceProvider)
	}
	if p.DefaultProgram.Description == "" {
		t.Error("DefaultPolicy DefaultProgram.Description is empty; the seeded program should name what serves it")
	}
	// The compiled-in default must itself pass the validator. A shipped
	// default that its own validation rejects would fail every boot.
	if err := p.ValidateDefaultProgram(); err != nil {
		t.Errorf("DefaultPolicy does not satisfy ValidateDefaultProgram: %v", err)
	}
}

// TestValidateDefaultProgramBoundsDescriptionAndPrimacy covers the two
// sep.xsd rules this validator enforces. The description case is the exact
// defect that made a conformant client reject the seeded
// FunctionSetAssignments document: 42 characters against a String32 bound.
func TestValidateDefaultProgramBoundsDescriptionAndPrimacy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		program DERProgramPolicy
		wantErr bool
		// wantIn, when set, must appear in the error so an operator is told
		// the flag to change rather than a struct field they cannot reach.
		wantIn string
	}{
		{"compiled-in default", DefaultPolicy().DefaultProgram, false, ""},
		{"primacy 0 in-home EMS is assigned and legal", DERProgramPolicy{Primacy: 0}, false, ""},
		{"primacy 2 non-contractual is assigned and legal", DERProgramPolicy{Primacy: 2}, false, ""},
		{"primacy 65 lower edge of user-defined", DERProgramPolicy{Primacy: 65}, false, ""},
		{"primacy 89 as used by the CSIP guide examples", DERProgramPolicy{Primacy: 89}, false, ""},
		{"primacy 191 upper edge of user-defined", DERProgramPolicy{Primacy: 191}, false, ""},
		{"primacy 3 lower edge of the first reserved band", DERProgramPolicy{Primacy: 3}, true, "-sep2-program-primacy"},
		{"primacy 64 upper edge of the first reserved band", DERProgramPolicy{Primacy: 64}, true, "-sep2-program-primacy"},
		{"primacy 192 lower edge of the second reserved band", DERProgramPolicy{Primacy: 192}, true, "-sep2-program-primacy"},
		{"primacy 255 upper edge of the second reserved band", DERProgramPolicy{Primacy: 255}, true, "-sep2-program-primacy"},
		{"description empty is legal and marshals as absent", DERProgramPolicy{Primacy: 1}, false, ""},
		{
			"description at exactly the String32 bound",
			DERProgramPolicy{Primacy: 1, Description: strings.Repeat("x", 32)},
			false, "",
		},
		{
			"description one character over the String32 bound",
			DERProgramPolicy{Primacy: 1, Description: strings.Repeat("x", 33)},
			true, "-sep2-program-description",
		},
		{
			"the 42-character description the reference client rejected",
			DERProgramPolicy{Primacy: 1, Description: strings.Repeat("x", 42)},
			true, "-sep2-program-description",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := SEP2Policy{DefaultProgram: tt.program}.ValidateDefaultProgram()
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateDefaultProgram(%+v) = nil, want an error", tt.program)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateDefaultProgram(%+v) = %v, want nil", tt.program, err)
			}
			if tt.wantIn != "" && !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q does not name %q; an operator needs the flag to change", err, tt.wantIn)
			}
		})
	}
}
