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
	"sort"
	"strings"

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
	// Typed as *sep2.DERControlType (an alias for *sep2.HexBinary32,
	// IEEECORE-047), matching sep2.DERCapability.ModesSupported's own
	// field type exactly, so the type that owns the hexBinary wire
	// encoding flows end to end rather than being carried as a plain
	// integer and converted at the seeding boundary. Pointer-typed so a
	// real, deliberate 0 bitmap is distinguishable from unset; nil means
	// policy imposes no default.
	ModesSupported *sep2.DERControlType

	// DefaultPollRate and DefaultPostRate are the FLEET-WIDE polling and
	// posting intervals, in seconds. DefaultPollRate is stamped onto every
	// seeded Registration's pollRate attribute (sep.xsd:190);
	// DefaultPostRate is stamped onto every MirrorUsagePoint a client
	// creates via POST /mup (sep.xsd:6485).
	//
	// Pointer-typed to match the core convention (sep2.MirrorUsagePoint's
	// own PostRate is *uint32); nil means no default is imposed and the
	// consumer falls back to its own (or the schema's) default rate. Unlike
	// the registration PIN, 0 is NOT a meaningful value here and
	// ValidateRates rejects it: a 0-second rate reads as "poll or post
	// continuously", so the pointer distinguishes unset from configured
	// rather than unset from a deliberate zero.
	//
	// Read these through ResolvePollRate / ResolvePostRate, never directly.
	// See PollRates below for why.
	DefaultPollRate *uint32
	DefaultPostRate *uint32

	// PollRates and PostRates are the PER-DEVICE overrides, keyed on
	// canonical LFDI exactly as RegistrationPINs is, and matched
	// case-insensitively by the resolvers. A per-device entry wins over the
	// corresponding fleet-wide default.
	//
	// Both are nil today: no flag or file populates them yet, and the
	// operator-facing surface is fleet-wide only. They exist now because
	// the eventual shape is settled (per-device rates, editable from the
	// admin UI, over a fleet-wide default), and the cost of retrofitting
	// that later is paid entirely at the CONSUMERS if they read a bare
	// field. Every consumer instead calls ResolvePollRate/ResolvePostRate
	// with a device LFDI and cannot tell whether the answer came from an
	// override or the default. Populating these maps is therefore a change
	// to this package alone.
	//
	// Where they will eventually be populated FROM is a separate decision
	// already pointed at by IEEECORE-050, which settled that registration
	// PINs are auto-generated per device and persisted to an
	// operator-editable file that is the source of truth, with the UI as an
	// editor over it. Per-device rates belong in that same per-device
	// provisioning record rather than a parallel store, so that two files
	// can never disagree about the same device. Building that record is not
	// this card's work.
	PollRates map[string]uint32
	PostRates map[string]uint32

	// RegistrationPINs maps a device's canonical LFDI to that device's
	// registration PIN (IEEE 2030.5 section 10.6.4; sep.xsd complexType
	// "Registration" lines 172-197, element pIN of type PINType at line
	// 184). Keys are matched case-insensitively against the canonical
	// uppercase-hex LFDI; ResolveRegistrationPIN does the normalizing.
	//
	// The PIN is OPERATOR-SUPPLIED and must never be derived from device
	// identity. The standard describes it as a value "for basic server
	// validation", conventionally printed on the unit for an installer to
	// read. The LFDI is a SHA-256 digest of the device certificate, which
	// every TLS peer already sees, so a PIN computed from it would be
	// computable by anyone who can reach the device and would validate
	// nothing. An earlier revision of this package derived the PIN from
	// the LFDI; that path has been removed outright rather than left as a
	// fallback, because a silent fallback to a publicly computable value
	// looks configured when it is not.
	//
	// Per-device is the standard's model. A fleet-wide value is available
	// via DefaultRegistrationPIN for a dev or interop harness, but it is a
	// fallback, not the shape to reach for in production: one PIN shared
	// across a fleet is exactly as weak as no PIN once any single unit is
	// read.
	//
	// Every configured value must be in PINType's documented range
	// [0, 999999]; ValidateRegistrationPIN reports one that is not. Values
	// are a shared secret in the registration flow: they are never logged,
	// and no error message this package produces embeds one.
	RegistrationPINs map[string]uint32

	// DefaultRegistrationPIN is the fleet-wide fallback used for a device
	// with no RegistrationPINs entry. Nil means there is no fallback, and
	// a device with no entry has no PIN at all, which is a boot-time
	// failure rather than a value to invent: see ResolveRegistrationPIN
	// and the seeding path's fail-closed behavior.
	DefaultRegistrationPIN *uint32
}

// MaxRegistrationPIN is the inclusive upper bound of the IEEE 2030.5
// registration PIN: a "6-digit PIN (5 plus check digit)" with range
// 0 to 999999 per the 2018 resource tables and section 6.3.5.
//
// The bound is enforced here because the schema does not enforce it:
// sep.xsd's PINType (lines 5972-5980) is an xs:extension of UInt32
// carrying NO facets, no maxInclusive and no pattern, so the range text
// in its xs:documentation annotation is inert to any validator.
const MaxRegistrationPIN uint32 = 999999

// ValidateRegistrationPIN reports whether every configured PIN, per-device
// and fleet-wide, is a conformant IEEE 2030.5 registration PIN: within the
// 0 to 999999 range AND carrying a valid checksum digit per section 6.3.5.
//
// Both checks are hard rejects at config load. 6.3.5 is normative on the
// checksum: "For input validation purposes, the sum of the digits of the
// PIN including the checksum digit, modulo 10, SHALL be zero." A value
// that fails it is non-conformant input, not a warning.
//
// A returned error names the offending device's LFDI and the failing rule
// but deliberately does NOT echo the offending value: the PIN is a shared
// secret in the registration flow, and configuration errors are commonly
// logged. The LFDI is not secret (it is derived from the certificate the
// device presents to every peer), so naming it is safe and is what makes
// the error actionable.
func (p SEP2Policy) ValidateRegistrationPIN() error {
	if p.DefaultRegistrationPIN != nil {
		if *p.DefaultRegistrationPIN > MaxRegistrationPIN {
			return fmt.Errorf("sep2config: DefaultRegistrationPIN exceeds the IEEE 2030.5 maximum of %d", MaxRegistrationPIN)
		}
		if !HasValidPINCheckDigit(*p.DefaultRegistrationPIN) {
			return fmt.Errorf("sep2config: DefaultRegistrationPIN fails the IEEE 2030.5 section 6.3.5 checksum rule " +
				"(the six digits including the check digit must sum to a multiple of 10)")
		}
	}
	// Iterate a sorted key list rather than the map directly: Go's map
	// iteration order is randomized, so an unsorted loop over a config
	// with two bad entries would report a different device on each boot.
	lfdis := make([]string, 0, len(p.RegistrationPINs))
	for lfdi := range p.RegistrationPINs {
		lfdis = append(lfdis, lfdi)
	}
	sort.Strings(lfdis)
	for _, lfdi := range lfdis {
		if p.RegistrationPINs[lfdi] > MaxRegistrationPIN {
			return fmt.Errorf(
				"sep2config: RegistrationPINs[%s] exceeds the IEEE 2030.5 maximum of %d",
				strings.ToUpper(lfdi), MaxRegistrationPIN)
		}
		if !HasValidPINCheckDigit(p.RegistrationPINs[lfdi]) {
			return fmt.Errorf(
				"sep2config: RegistrationPINs[%s] fails the IEEE 2030.5 section 6.3.5 checksum rule "+
					"(the six digits including the check digit must sum to a multiple of 10)",
				strings.ToUpper(lfdi))
		}
	}
	return nil
}

// ResolveRegistrationPIN returns the registration PIN configured for the
// device with the given canonical LFDI, and whether one was configured at
// all. A per-device entry wins over DefaultRegistrationPIN.
//
// The false return is meaningful and must not be papered over with a
// zero: sep.xsd:184 makes pIN minOccurs=1 in the Registration sequence, so
// a Registration cannot legally omit it, and 0 is a schema-valid value
// that means nothing. A caller that cannot resolve a PIN must refuse to
// provision the device rather than serve a meaningless one.
func (p SEP2Policy) ResolveRegistrationPIN(lfdi string) (uint32, bool) {
	return resolvePerDevice(p.RegistrationPINs, p.DefaultRegistrationPIN, lfdi)
}

// ResolvePollRate returns the polling interval, in seconds, configured for
// the device with the given canonical LFDI, and whether one was configured
// at all. A PollRates entry wins over DefaultPollRate.
//
// Consumers call this instead of reading DefaultPollRate so that the
// fleet-default and per-device cases are indistinguishable at the point of
// use; see the PollRates field comment.
//
// The false return means "no rate configured" and must be honored, not
// papered over with a zero. sep.xsd:190 makes pollRate an OPTIONAL attribute
// with its own documented default of 900, so omitting the attribute entirely
// is the correct representation of "unconfigured" and lets the client apply
// the schema default. Stamping 0 instead would advertise a continuous poll.
func (p SEP2Policy) ResolvePollRate(lfdi string) (uint32, bool) {
	return resolvePerDevice(p.PollRates, p.DefaultPollRate, lfdi)
}

// ResolvePostRate returns the posting interval, in seconds, configured for
// the device with the given canonical LFDI, and whether one was configured
// at all. A PostRates entry wins over DefaultPostRate.
//
// This is the function passed to core as its metering.PostRateProvider, so
// the LFDI it receives is the LFDI of the client that created the
// MirrorUsagePoint. Same contract as ResolvePollRate on the false return:
// postRate is minOccurs=0 (sep.xsd:6485), so unconfigured means the element
// is absent, never present-and-zero.
func (p SEP2Policy) ResolvePostRate(lfdi string) (uint32, bool) {
	return resolvePerDevice(p.PostRates, p.DefaultPostRate, lfdi)
}

// resolvePerDevice implements the one precedence rule shared by every
// per-device policy value in this package: a per-device entry keyed on the
// canonical LFDI wins, a fleet-wide default is the fallback, and neither
// configured reports false rather than inventing a zero.
//
// Keys are compared case- and space-insensitively against the canonical
// uppercase-hex LFDI, so an operator-written config file that differs only
// in casing still matches the identity derived from the certificate.
func resolvePerDevice(perDevice map[string]uint32, fleetDefault *uint32, lfdi string) (uint32, bool) {
	key := strings.ToUpper(strings.TrimSpace(lfdi))
	if key != "" {
		for k, v := range perDevice {
			if strings.ToUpper(strings.TrimSpace(k)) == key {
				return v, true
			}
		}
	}
	if fleetDefault != nil {
		return *fleetDefault, true
	}
	return 0, false
}

// RecommendedPollRate is the IEEE 2030.5 schema's own documented default for
// Registration.pollRate: "If not specified, a default of 900 seconds (15
// minutes) is used" (sep.xsd:190). Advertising it explicitly rather than
// relying on the client to know the schema default costs one attribute and
// removes an assumption about the client's schema handling.
const RecommendedPollRate uint32 = 900

// RecommendedPostRate is the suggested MirrorUsagePoint.postRate for a
// co-simulation stepping at 30 seconds.
//
// postRate is how often a client posts mirrored data. Setting it equal to
// the simulation step means each POST carries the readings from a single
// step rather than an accumulation of several: raising it multiplies the
// readings per request roughly linearly, which is the parameter to tune if
// request size turns out to matter. Below the step it buys nothing, because
// no new reading exists to send.
//
// Neither constant is applied automatically. Both are compiled-in
// RECOMMENDATIONS an operator can reach for; an unset flag leaves the
// corresponding policy field nil and the bridge advertises nothing, exactly
// as it did before these existed.
const RecommendedPostRate uint32 = 30

// ValidateRates reports whether every configured poll and post rate, fleet-wide
// and per-device, is usable. It is called at bridge boot, before anything is
// seeded or served, so a bad rate stops the process rather than being
// discovered as a wire-level oddity by a client hours later.
//
// The only rejected value is 0. uint32 already bounds the range from above,
// and sep.xsd applies no facets to either rate (both are plain UInt32), so
// there is no schema ceiling to enforce and inventing one here would reject
// configurations the standard permits. 0 is different in kind: as an
// interval it means "with no delay", which for postRate is an unbounded
// request rate against this bridge and for pollRate is an unbounded request
// rate against every client. Neither is a rate an operator can have meant,
// and both are schema-valid, so nothing downstream would catch it.
//
// Errors name the flag an operator would set, not the struct field, because
// the flag is what they can act on.
func (p SEP2Policy) ValidateRates() error {
	if p.DefaultPollRate != nil && *p.DefaultPollRate == 0 {
		return fmt.Errorf("sep2config: -sep2-poll-rate must be at least 1 second; 0 would advertise a continuous poll")
	}
	if p.DefaultPostRate != nil && *p.DefaultPostRate == 0 {
		return fmt.Errorf("sep2config: -sep2-post-rate must be at least 1 second; 0 would advertise a continuous post")
	}
	// Sort before iterating: Go randomizes map order, so an unsorted loop
	// over a config with two bad entries would blame a different device on
	// each boot. Same reasoning as ValidateRegistrationPIN.
	for _, m := range []struct {
		rates map[string]uint32
		flag  string
	}{
		{p.PollRates, "-sep2-poll-rate"},
		{p.PostRates, "-sep2-post-rate"},
	} {
		lfdis := make([]string, 0, len(m.rates))
		for lfdi := range m.rates {
			lfdis = append(lfdis, lfdi)
		}
		sort.Strings(lfdis)
		for _, lfdi := range lfdis {
			if m.rates[lfdi] == 0 {
				return fmt.Errorf(
					"sep2config: per-device %s for %s must be at least 1 second; 0 would advertise a continuous interval",
					m.flag, strings.ToUpper(lfdi))
			}
		}
	}
	return nil
}

// HasValidPINCheckDigit reports whether pin satisfies the IEEE 2030.5
// section 6.3.5 checksum rule.
//
// 6.3.5 states the rule as VALIDATION, not as a generation formula: "For
// input validation purposes, the sum of the digits of the PIN including
// the checksum digit, modulo 10, SHALL be zero." That is what this
// implements directly. It deliberately does NOT regenerate a check digit
// and compare: the standard states the rule in this form, and a round
// trip through a generator would be a weaker check than the normative
// condition itself.
//
// Worked example from the standard: PIN 12345 has digits summing to 15,
// so the check digit is 5 and the displayed value is 123455, whose digits
// sum to 20 and 20 mod 10 is 0.
//
// Leading zeros need no special handling: they contribute nothing to a
// digit sum, so summing the numeric value's digits is equivalent to
// summing all six displayed digits.
//
// Edition note: the normative text is word for word identical in 2013
// (section 8.3.4), 2018 (6.3.5), and 2023 (6.3.5). Only the section
// number moved, so one implementation is correct for every edition and no
// edition seam is needed here. Do not trust the sep.xsd annotation
// cross-reference: it cites "Section 8.3.2 for check digit calculation",
// which is inherited 2013 numbering and lands on "List ordering" in 2018
// and 2023. Cite the body text, not the schema annotation.
func HasValidPINCheckDigit(pin uint32) bool {
	if pin > MaxRegistrationPIN {
		return false
	}
	sum := 0
	for v := pin; v > 0; v /= 10 {
		sum += int(v % 10)
	}
	return sum%10 == 0
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
