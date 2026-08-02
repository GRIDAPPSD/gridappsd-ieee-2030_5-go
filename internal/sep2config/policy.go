// Package sep2config carries the IEEE 2030.5-tree shaping facts the CIM
// model cannot express, so that model-only sourcing (see cmd/bridge's
// package doc and internal/registry) stays uncontaminated by policy.
//
// Design boundary (Noor + Craig, 2026-07-17): the CIM model is the source
// of truth for the DER fleet itself (feeder-scoped PowerElectronicsConnection
// query), each device's LFDI (derived from its certificate, spec section
// 6.3.4), its name (CIM IdentifiedObject.name), and its physical ratings
// (ratedS, ratedU, and siblings). None of that belongs in this package.
// SEP2Policy holds only the 2030.5-tree facts that have no CIM analog at
// all: a fallback DefaultDERControl, the modesSupported bitmap, and default
// poll/post rates. Anything CIM can carry stays out of policy; anything CIM
// cannot carry lives here.
package sep2config

import (
	"fmt"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// SEP2Policy is loaded once at bridge boot. Nothing in this package reads
// or seeds any store: consuming SEP2Policy is the job of later cards
// (GAGO-049 for ModesSupported, GAGO-050 for DefaultControl).
type SEP2Policy struct {
	// DefaultControl is the DefaultDERControl fallback GAGO-050 seeds onto
	// every DERProgram's DefaultDERControlLink. See DefaultPolicy's doc
	// comment for the specific field-by-field rationale.
	DefaultControl sep2.DefaultDERControl

	// ModesSupported is the DERControlType bitmap GAGO-049 stamps into
	// each seeded DERCapability. Not derivable from CIM: no CIM class
	// carries which control modes a device advertises over 2030.5.
	// Pointer-typed to match the core convention (sep2.DERCapability's own
	// ModesSupported is *uint32) so a real, deliberate 0 bitmap is
	// distinguishable from unset; nil means policy imposes no default.
	ModesSupported *uint32

	// DefaultPollRate and DefaultPostRate are the default polling and
	// posting intervals, in seconds, that future FunctionSetAssignments
	// seeding may apply. Pointer-typed to match the core convention (e.g.
	// sep2.MirrorUsagePoint's own PostRate is *uint32) so a real,
	// deliberate 0-second rate is distinguishable from unset; nil means no
	// default is imposed and the consumer falls back to its own (or the
	// spec's) default rate.
	DefaultPollRate *uint32
	DefaultPostRate *uint32

	// RegistrationPIN overrides the registration PIN stamped onto every
	// seeded Registration resource (IEEE 2030.5 section 10.6.4; sep.xsd
	// complexType "Registration", element pIN of type PINType).
	//
	// Nil (the default) is the normal production setting: seeding derives
	// a stable per-device PIN from that device's own canonical LFDI, which
	// matches the spec's per-device semantics (a PIN is a per-device fact,
	// conventionally printed on the device label) and needs no
	// configuration to boot. Set this only for an interop or conformance
	// harness whose client is compiled to expect one known fleet-wide
	// value; note that doing so gives every device the SAME PIN, which is
	// weaker than the per-device default and is why it is not the default.
	//
	// A configured value must be in PINType's range [0, 999999];
	// ValidateRegistrationPIN reports one that is not. The value is a
	// shared secret in the registration flow: it is never logged, and no
	// error message this package produces embeds it.
	RegistrationPIN *uint32
}

// MaxRegistrationPIN is the inclusive upper bound of IEEE 2030.5's
// PINType: "6 digit unsigned decimal integer (0 - 999999)" (sep.xsd
// complexType "PINType").
const MaxRegistrationPIN uint32 = 999999

// ValidateRegistrationPIN reports whether p.RegistrationPIN, when set, is
// within PINType's range. A nil RegistrationPIN is valid (it selects the
// per-device derived default).
//
// The returned error names the field and the bound but deliberately does
// NOT echo the offending value: the PIN is a shared secret in the
// registration flow, and configuration errors are commonly logged.
func (p SEP2Policy) ValidateRegistrationPIN() error {
	if p.RegistrationPIN == nil {
		return nil
	}
	if *p.RegistrationPIN > MaxRegistrationPIN {
		return fmt.Errorf("sep2config: RegistrationPIN exceeds the IEEE 2030.5 PINType maximum of %d", MaxRegistrationPIN)
	}
	return nil
}

// DefaultPolicy returns the compiled-in, spec-sane SEP2Policy defaults, so
// `go run ./cmd/bridge` boots with no config file, matching
// cmd/bridge/config.go's own compiled-in-default convention.
//
// DefaultControl follows Vance's physics verdict (GAGO-050 alignment,
// 2026-07-17): opModConnect and opModEnergize are true (the IEEE 1547
// ride-through fail-safe: aggregate DER dropout is the bigger hazard than
// a device staying connected and energized), and every other
// DERControlBase field is left nil. In particular, opModTargetVar and
// opModTargetW stay unset: setting either forces a fixed-power mode that
// silently disables the device's own autonomous volt-var / curtailment
// behavior (1547-2018 clause 5.3 mutual exclusivity). SetGradW and
// SetSoftGradW (siblings of DERControlBase on DefaultDERControl) are also
// left nil: a fixed ramp/enter-service override with no randomization
// would overwrite the device's own commissioned 1547 settings and risks
// synchronized reconnection.
//
// ModesSupported and the poll/post rates default to nil (unset); GAGO-049
// and any future FSA-seeding card supply real values once they exist.
func DefaultPolicy() SEP2Policy {
	connect := true
	energize := true
	return SEP2Policy{
		DefaultControl: sep2.DefaultDERControl{
			DERControlBase: &sep2.DERControlBase{
				OpModConnect:  &connect,
				OpModEnergize: &energize,
			},
		},
	}
}
