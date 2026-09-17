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
// or seeds any store: consuming SEP2Policy is the caller's job.
type SEP2Policy struct {
	// DefaultControl is the DefaultDERControl fallback seeded onto
	// every DERProgram's DefaultDERControlLink. See DefaultPolicy's doc
	// comment for the specific field-by-field rationale.
	DefaultControl sep2.DefaultDERControl

	// DefaultProgram is the DERProgram seeded for every device at boot, the
	// resource DefaultControl above hangs off. The two are companions and
	// are deliberately adjacent: a DefaultDERControl is reachable only
	// through its containing DERProgram's DefaultDERControlLink, so
	// configuring one without the other is not a meaningful state.
	//
	// Validate with ValidateDefaultProgram before use.
	DefaultProgram DERProgramPolicy

	// DERControl is the temporal shape stamped onto every DERControl this
	// bridge issues from a GridAPPS-D control delta: how long the control
	// applies, and how widely its expiry is staggered across the fleet.
	//
	// It is a third companion to the two fields above. DefaultControl is
	// what a device falls back to when no control is active, DefaultProgram
	// is the resource both hang off, and this is what decides WHEN that
	// fallback takes over.
	//
	// Validate with ValidateDERControl before use.
	DERControl DERControlPolicy

	// ModesSupported is the DERControlType bitmap stamped into
	// each seeded DERCapability. Not derivable from CIM: no CIM class
	// carries which control modes a device advertises over 2030.5.
	// Typed as *sep2.DERControlType (an alias for *sep2.HexBinary32),
	// matching sep2.DERCapability.ModesSupported's own
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
	// Where they will eventually be populated FROM is a separate decision:
	// registration PINs are auto-generated per device and persisted to an
	// operator-editable file that is the source of truth, with the UI as an
	// editor over it. Per-device rates belong in that same per-device
	// provisioning record rather than a parallel store, so that two files
	// can never disagree about the same device. Building that record is
	// a separate piece of work, out of scope here.
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

// DERProgramPolicy carries the operator-settable fields of the DERProgram
// this bridge seeds for every device at boot.
//
// WHY A SEEDED PROGRAM AT ALL. The bridge used to create a device's
// DERProgram lazily, on the first control delta to arrive for that device
// (sep2embed's ensureDERProgram). Until then the device's DERProgramList was
// legitimately empty, and that is a real interoperability defect for two
// separate reasons:
//
//   - A client that walks the tree once at startup and does not re-poll the
//     list sees no program and never comes back to look.
//   - DefaultDERControl hangs off DERProgram. CSIP is explicit that "in the
//     absence of any active events, the inverter executes the
//     DefaultDERControl of the DERProgram with the highest priority" (CSIP
//     Implementation Guide v2.0, section 8). An empty DERProgramList
//     therefore makes the configured DefaultControl unreachable, so the
//     no-active-control fallback the operator configured never applies.
//
// The program is the operator's control channel. It should exist whether or
// not a control is currently active, so it is seeded rather than lazily
// created.
//
// Only the fields an operator has a real decision to make about are carried
// here. mRID is derived (see sep2embed's deriveMRID), and the links are
// structural, so neither is configurable: an operator cannot usefully choose
// them and letting them be set would only create ways to break traversal.
type DERProgramPolicy struct {
	// Primacy is the DERProgram's primacy value: sep.xsd's PrimacyType,
	// which is a plain UInt8 with no facets, so the schema itself enforces
	// nothing beyond the byte range and ValidateDefaultProgram carries the
	// documented meaning.
	//
	// It is not a placeholder. Primacy governs precedence when more than one
	// DERProgram applies to a device: "the priority of a DERControl is
	// determined by the primacy setting of its containing DERProgram with a
	// lower primacy value indicating higher priority" (CSIP Implementation
	// Guide v2.0, section 8; the same rule appears at 5.2.4.2). It also
	// selects which DefaultDERControl applies when several programs are in
	// scope and no event is active.
	//
	// FIELD DEPLOYMENTS MUST SET THIS. The compiled-in default of 1 is
	// correct for a co-simulation, where this bridge is the only program
	// any device sees and its rank relative to others is moot. It is wrong
	// for a real interconnection. Lower values are higher priority
	// (sep.xsd:6008), and the user-defined band a DSO or aggregator program
	// occupies is 65 to 191, so a bridge left at 1 outranks every one of
	// them and its controls win against a utility program that should have
	// taken precedence. The right value comes from the interconnection
	// agreement, not from this file.
	//
	// Not pointer-typed, unlike the poll and post rates: sep.xsd makes
	// primacy minOccurs=1 on DERProgram, so there is no "absent" state to
	// represent, and 0 is a meaningful value (the highest priority) rather
	// than a stand-in for unset.
	Primacy uint8

	// Description is the DERProgram's human-readable description. sep.xsd
	// types IdentifiedObject.description as String32, so values longer than
	// 32 characters are rejected by ValidateDefaultProgram rather than
	// truncated: an over-length value is not trimmed by the serializer, it
	// goes out on the wire and a conformant client fails the whole document
	// parse on it, losing every sibling field including the links.
	//
	// Empty is valid and marshals as absent: description is minOccurs=0.
	Description string
}

// PrimacyContractedServiceProvider is sep.xsd's documented PrimacyType value
// 1, "Contracted premises service provider".
//
// This is the default this bridge seeds, and the value it has always used
// for the lazily-created program, so seeding does not change what an existing
// deployment serves.
//
// It is the right reading of what this bridge is. The GridAPPS-D platform
// operating a distribution feeder is the service provider the DER is
// interconnected with under an agreement, which is what value 1 names. The
// two neighbouring values are both worse fits: 0 is "In home energy
// management system", a premises-side controller this bridge is not, and 2
// is "Non-contractual service provider", which would rank the utility's own
// program below any contracted aggregator sharing the device and is the
// opposite of the intended precedence.
//
// CO-SIMULATION DEFAULT, NOT A FIELD ONE. Everything above argues that 1 is
// the right CATEGORY. It is still the wrong NUMBER for a deployment where
// other programs are in scope, because primacy is a ranking and not a label:
// lower is higher priority (sep.xsd:6008), and a program left at 1 outranks
// the 65 to 191 user-defined band a DSO program sits in. See
// DERProgramPolicy.Primacy; the field value is set per interconnection
// agreement.
const PrimacyContractedServiceProvider uint8 = 1

// MaxDERProgramDescription is the inclusive maximum length of
// DERProgram.description, from sep.xsd's String32 (xs:maxLength 32 on
// IdentifiedObject.description).
const MaxDERProgramDescription = 32

// ValidateDefaultProgram reports whether the configured default DERProgram
// can be served. Called at bridge boot, before anything is seeded, for the
// same reason ValidateRates is: a bad value should stop the process rather
// than reach a client as a document it silently refuses to parse.
//
// Two rules are enforced, both from sep.xsd:
//
//   - description fits String32. This is the exact defect that made a
//     conformant client reject the seeded FunctionSetAssignments document
//     outright, so it is checked rather than assumed.
//   - primacy is not in a range the standard reserves. PrimacyType documents
//     0 to 2 as assigned, 3 to 64 and 192 to 255 as Reserved, and 65 to 191
//     as User-defined. Reserved means not available for use, so a value
//     there is a configuration error, not a deployment choice. The
//     user-defined band is permitted: CSIP's own worked examples sit in it
//     (the Implementation Guide uses 80 through 89 for a program hierarchy),
//     so rejecting it would refuse configurations the standard and the
//     profile both endorse.
//
// This is deliberately narrower than "reject anything unusual". uint8
// already bounds the range from above, and the assigned and user-defined
// bands together are what an operator may legitimately choose from.
//
// Errors name the flag an operator would set rather than the struct field,
// matching ValidateRates: the flag is what they can act on.
func (p SEP2Policy) ValidateDefaultProgram() error {
	if n := len(p.DefaultProgram.Description); n > MaxDERProgramDescription {
		return fmt.Errorf(
			"sep2config: -sep2-program-description is %d characters; sep.xsd bounds DERProgram.description at %d (String32)",
			n, MaxDERProgramDescription)
	}
	if reservedPrimacy(p.DefaultProgram.Primacy) {
		return fmt.Errorf(
			"sep2config: -sep2-program-primacy %d is in a range IEEE 2030.5 reserves; "+
				"use 0 (in-home energy management system), 1 (contracted premises service provider), "+
				"2 (non-contractual service provider), or 65 to 191 (user-defined)",
			p.DefaultProgram.Primacy)
	}
	return nil
}

// reservedPrimacy reports whether v falls in one of the two bands sep.xsd's
// PrimacyType annotation marks "Reserved": 3 to 64 and 192 to 255.
func reservedPrimacy(v uint8) bool {
	return (v >= 3 && v <= 64) || v >= 192
}

// DERControlPolicy carries the operator-settable temporal shape of every
// DERControl this bridge issues in response to a GridAPPS-D control delta.
//
// WHY THE BRIDGE AUTHORS THESE VALUES AT ALL. sep.xsd:5584 makes interval
// minOccurs=1 on Event, so a DERControl without one is not a gap in this
// bridge's feature set, it is a non-conformant document. The GridAPPS-D
// delta contract carries no temporal information: a delta says "opModTargetW
// is now this", not "for the next N seconds". Something has to supply the
// window, and the bridge is the correct author because it is the event
// CREATOR. That distinction is load-bearing rather than pedantic: IEEE
// 2030.5-2018 rule c) on p.90 forbids a server from editing an event it has
// already issued ("Editing Events SHALL NOT be allowed except for updating
// status"), so retrofitting an interval onto a received event would be
// wrong, while minting one on an event this bridge originates is not.
//
// WHAT HAPPENS WHEN THE WINDOW ELAPSES. The control expires and the device
// falls back to its DERProgram's DefaultDERControl, which is the standard's
// own designed fallback (clause 10.10.4.2, p.120). There is deliberately no
// heartbeat re-issue: a live platform holds a setpoint by superseding the
// control with each new delta, and a bridge that has died should leave the
// DER on a defined state rather than on the last thing it was told.
type DERControlPolicy struct {
	// Duration is the DateTimeInterval.duration, in seconds, stamped onto
	// every issued DERControl. The matching start is the instant the bridge
	// writes the control, so the window opens immediately.
	//
	// It must outlive the client's own discovery latency. A client learns of
	// a new control by polling its DERProgram (or by subscription), so a
	// duration shorter than that interval leaves the device on
	// DefaultDERControl between deltas even while the platform is actively
	// commanding it. Choosing it too long has the opposite cost: a bridge
	// that dies holds its last setpoint for the remainder of the window.
	//
	// Not pointer-typed, unlike the poll and post rates: duration is
	// minOccurs=1 inside DateTimeInterval (sep.xsd:5785), so there is no
	// absent state to represent, and 0 is not a usable stand-in for unset.
	// ValidateDERControl rejects 0 outright, because a zero-length window
	// reproduces exactly the defect this field exists to fix: the reference
	// client computes end = start + duration and expires any event whose end
	// is already in the past.
	Duration uint32

	// RandomizeDuration is the RandomizableEvent.randomizeDuration, in
	// seconds, served on every issued DERControl. sep.xsd:5650 defines it as
	// the boundary inside which a client picks a random offset to apply to
	// the interval duration, and states its purpose plainly: "to avoid
	// sudden synchronized demand changes". Valid range is -3600 to 3600.
	//
	// It staggers EXPIRY, not start, which is precisely why it matters here.
	// Every device in this fleet is served by ONE bridge and receives its
	// controls with the same duration at nearly the same instant, so without
	// randomization every DER in the model would revert to its
	// DefaultDERControl in the same second.
	//
	// The compiled-in default is 0, and that is a co-simulation choice, not
	// a field one. A co-simulation run must be reproducible: an offset the
	// client picks at random makes two runs of the same scenario diverge in
	// when each device reverts. A field deployment wants the opposite and
	// should set a non-zero value.
	//
	// 0 is served explicitly rather than omitted. sep.xsd says an absent
	// element defaults to 0, so the two are semantically identical, and
	// stating it removes any dependence on the client applying that default.
	RandomizeDuration int32
}

// DefaultDERControlDuration is the compiled-in DERControlPolicy.Duration:
// 1800 seconds.
//
// It is twice sep.xsd's own documented default Registration pollRate of 900
// seconds (sep.xsd:190, and RecommendedPollRate above). A client polling at
// the schema default therefore gets two chances to see a superseding control
// before the current one expires, so a single missed or failed poll does not
// drop the device back to its DefaultDERControl mid-dispatch. The same
// number bounds the other direction: a bridge that dies holds its last
// commanded setpoint for at most half an hour before the DER lands on the
// operator-configured fallback.
//
// An operator running a faster poll rate should shorten it to match. This is
// a starting value with a stated derivation, not a constant with a hidden
// one.
const DefaultDERControlDuration uint32 = 1800

// MaxRandomizeSeconds is the inclusive magnitude bound of
// randomizeDuration, from sep.xsd's OneHourRangeType (sep.xsd:5929): "a
// signed time offset ... with range -3600 to 3600".
//
// It is enforced here because the schema does not enforce it. Like PINType,
// OneHourRangeType is a bare xs:extension of Int16 carrying NO facets, so
// the range text in its xs:documentation is inert to any validator.
const MaxRandomizeSeconds int32 = 3600

// RandomizeDurationInRange reports whether value's magnitude fits the
// sep.xsd OneHourRangeType bound: -MaxRandomizeSeconds to
// MaxRandomizeSeconds inclusive. ValidateDERControl below calls this at
// boot; internal/sep2embed.New calls it again at the package boundary
// boot validation does not reach (a caller of New that skips cmd/bridge's
// boot sequence), so the bound lives in one place rather than two.
func RandomizeDurationInRange(value int32) bool {
	// Widened to int64 before taking the magnitude: int32's most negative
	// value has no positive counterpart, so negating it in int32 would
	// silently yield a negative number and pass a bound check it should
	// fail.
	magnitude := int64(value)
	if magnitude < 0 {
		magnitude = -magnitude
	}
	return magnitude <= int64(MaxRandomizeSeconds)
}

// ValidateDERControl reports whether the configured DERControl temporal
// policy can be served. Called at bridge boot alongside ValidateRates and
// ValidateDefaultProgram, in the same before-we-dial-anything window, so an
// unusable value stops the process rather than surfacing as a client that
// silently discards every control it is sent.
//
// Errors name the flag an operator would set, not the struct field,
// matching ValidateRates and ValidateDefaultProgram: the flag is what they
// can act on.
func (p SEP2Policy) ValidateDERControl() error {
	if p.DERControl.Duration == 0 {
		return fmt.Errorf(
			"sep2config: -sep2-control-duration must be at least 1 second; " +
				"a zero-length interval makes a client compute end = start + 0 and expire the control on arrival, " +
				"which is the exact defect a served interval exists to fix")
	}

	if !RandomizeDurationInRange(p.DERControl.RandomizeDuration) {
		return fmt.Errorf(
			"sep2config: -sep2-control-randomize-duration %d is outside the sep.xsd OneHourRangeType range of -%d to %d seconds",
			p.DERControl.RandomizeDuration, MaxRandomizeSeconds, MaxRandomizeSeconds)
	}

	// A randomization window at least as wide as the duration itself lets a
	// client pick an offset that cancels or reverses the interval, which
	// lands back on the zero-or-negative effective duration rejected above.
	// Checked against the configured duration rather than a fixed ceiling,
	// because the two knobs are only wrong in combination. Recomputed here
	// (rather than reusing a magnitude RandomizeDurationInRange already
	// took) because that helper reports in-range/out-of-range only and
	// does not hand its magnitude back to the caller.
	magnitude := int64(p.DERControl.RandomizeDuration)
	if magnitude < 0 {
		magnitude = -magnitude
	}
	if magnitude >= int64(p.DERControl.Duration) {
		return fmt.Errorf(
			"sep2config: -sep2-control-randomize-duration %d is not smaller in magnitude than -sep2-control-duration %d; "+
				"a client applying an offset that wide can reduce the interval to zero or less",
			p.DERControl.RandomizeDuration, p.DERControl.Duration)
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
// THE SHIPPED DefaultControl COMMANDS NOTHING. Its DERControlBase is
// present but empty: no active-power element, and in particular no
// opModConnect and no opModEnergize. Every child of DERControlBase is
// minOccurs=0 (sep.xsd:3757), so a near-empty base is schema-valid, and the
// containing element itself stays non-nil because DERControlBase is
// minOccurs=1 on DefaultDERControl (sep.xsd:3270).
//
// Omission is the only construction that commands nothing, which is why an
// asserted "safe" value is not an acceptable substitute. opModConnect is
// documented as connecting or disconnecting "from the grid" with the
// annotation noting galvanic isolation (sep.xsd:3758), so serving true is a
// commanded device closure, not a neutral statement. This is a fallback:
// under the 2023 edition's per-mode fallback evaluation it is not a value
// applied once at expiry but one asserted CONTINUOUSLY whenever no control
// is active. A protocol flag never belongs in the isolation path.
//
// What the DER runs instead is its own IEEE 1547 autonomous baseline:
// volt-var, volt-watt, frequency droop and ride-through, all of which
// operate regardless of the control channel and none of which this bridge
// has to (or should) restate. That baseline also carries no signed
// quantity, so this default is unaffected by the open sign-convention
// question.
//
// This REPLACES the earlier reading of Vance's verdict, which set
// opModConnect and opModEnergize true as an IEEE 1547 ride-through
// fail-safe. The ride-through concern is real and unchanged; what changed is
// where it is answered. Ride-through is the DER's own commissioned 1547
// behavior, and asserting a connect flag from the control channel does not
// improve it while it does put a remotely-set protocol value in the
// isolation path.
//
// Everything else on the control stays nil for reasons that did not change.
// opModTargetW and opModTargetVar unset, because setting either forces a
// fixed-power mode that silently disables the device's autonomous volt-var
// and curtailment behavior (1547-2018 clause 5.3 mutual exclusivity).
// SetGradW and the SetES* family (siblings of DERControlBase on
// DefaultDERControl) unset, because sep.xsd:3306 and sep.xsd:3271 say each
// SHALL update the corresponding DERSettings value: that is an
// installer-owned persistent write, not a fallback.
//
// DefaultProgram carries the program DefaultControl hangs off. Primacy is
// PrimacyContractedServiceProvider (1), which is both the spec-correct
// reading of what this bridge is and the value the lazily-created program
// already used, so seeding changes no served value. See that constant and
// DERProgramPolicy for the full rationale, including why a field deployment
// must choose its own number.
//
// DERControl carries the temporal shape of every issued control:
// DefaultDERControlDuration with no expiry randomization, which is the
// reproducible co-simulation setting. See DERControlPolicy.
//
// ModesSupported and the poll/post rates default to nil (unset); a
// future FSA-seeding change supplies real values once they exist.
func DefaultPolicy() SEP2Policy {
	return SEP2Policy{
		DefaultControl: sep2.DefaultDERControl{
			// Non-nil and empty, deliberately. Nil would omit a
			// minOccurs=1 element and make the document non-conformant;
			// empty is what "no mode is commanded" looks like on the wire.
			DERControlBase: &sep2.DERControlBase{},
		},
		DefaultProgram: DERProgramPolicy{
			Primacy: PrimacyContractedServiceProvider,
			// 22 characters, inside String32. Names the bridge that
			// serves it rather than the feeder or device, because one
			// description is served for every device in the fleet.
			Description: "GridAPPS-D DER program",
		},
		DERControl: DERControlPolicy{
			Duration:          DefaultDERControlDuration,
			RandomizeDuration: 0,
		},
	}
}
