package sep2embed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// seedStores populates stores.EndDevices and stores.DERs from reg's
// entries. Each registry entry becomes exactly one EndDevice and exactly
// one child DER resource (the bridge does not yet carry more than one
// DER per device's CIM identity; see internal/cim.DictItem, which is all
// the registry entries are built from as of GAGO-025). The mapping,
// field by field (id = Entry.LFDI, the canonical identity):
//
//	store id (both EndDevice and the DER's parent key) = Entry.LFDI
//	EndDevice.LFDI                                      = Entry.LFDI
//	EndDevice.SFDI                                       = Entry.SFDI, or
//	                                                       derivePlaceholderSFDI(Entry.LFDI)
//	                                                       when Entry.SFDI is empty
//	EndDevice.Enabled                                    = true
//	EndDevice.Href                                       = "/edev/" + id
//	EndDevice.DERListLink                                = "/edev/" + id + "/der" (All: 1)
//	DER id (within the EndDevice's DER scope)             = "1"
//	DER.Href                                              = "/edev/" + id + "/der/1"
//
// The store id uses Entry.LFDI (not a sequential counter) so a device's
// EndDeviceStore.GetByLFDI lookup resolves to the same id every time,
// and so re-seeding the same registry entry against a fresh store always
// produces the same store key. A device is stored and advertised under
// its canonical LFDI alone: the store id, the advertised
// EndDevice.LFDI, and the ownership match (see acl.go) are all the same
// value, so there is no separate discovery-vs-ownership identity split
// to reason about. A client that self-hashes its own raw DER
// certificate (see internal/sep2embed's .x509 emission) computes this
// exact value and discovers itself via GET /edev. reg.Snapshot() itself
// iterates a Go map and its element order is unspecified per seeding
// run; that is fine because store id assignment does not depend on
// iteration order (each entry's id is derived solely from its own LFDI,
// not from its position in the snapshot). What IS ordered, and is what
// an /edev GET actually returns, is core's memory.Store[T].List: it
// walks a separately maintained sorted key slice, so list responses are
// sorted by id regardless of the order seedStores wrote them in. LFDI
// is guaranteed non-empty (registry validation requires it) and unique
// per device, and the uppercase-hex canonical LFDI is URL-safe (40 hex
// characters).
//
// An empty registry seeds empty stores without error: the /edev list
// still serves (0 results), it is simply empty rather than absent.
//
// policy carries the caller-supplied values stamped onto the seeded
// resources; see seedPolicy's field comments for what each absence means on
// the wire.
//
// Beyond the EndDevice and its DER, seedOne creates the resources a
// link-traversing client must walk BEFORE it will look at a DERControl at
// all. Each follows the same no-policy-means-no-record discipline:
//
//   - A Registration per device (plus EndDevice.RegistrationLink), when
//     policy.RegistrationPIN is non-nil. A conformant client treats a
//     missing RegistrationLink as a hard failure: the EPRI reference client
//     calls test_fail("registration", "EndDevice does not contain
//     RegistrationLink.") and stops, so with no link the walk ends before
//     any function set is reached. pIN is a REQUIRED wire element with no
//     spec-defined default, so when RegistrationPIN is nil seedOne creates
//     neither the record nor the link: a link pointing at an absent
//     resource, or a record carrying an invented pIN, would both be worse
//     than the honest absence (see [[data-invariants]] Rule 2).
//   - One FunctionSetAssignments per device (plus
//     EndDevice.FunctionSetAssignmentsListLink), unconditionally. This is
//     the only path from an EndDevice to a DERProgram: a client walks
//     EndDevice -> FunctionSetAssignmentsList -> the FSA's own
//     DERProgramListLink. With the FSA store empty the list served all=0
//     and the walk dead-ended, so no control this bridge wrote was ever
//     discoverable. The FSA is seeded at controlFSAID, the SAME id the
//     DOWN path writes DERControls under, so the program a client
//     discovers is the program the bridge actually populates.
//   - One DERProgram per device at controlDERProgramID, plus that program's
//     DefaultDERControl singleton, unconditionally. See seedDERProgram for
//     why this is seed-time work rather than the first-control-delta work
//     it used to be.
func seedStores(ctx context.Context, stores *assembly.Stores, reg *registry.Registry, policy seedPolicy) error {
	for _, e := range reg.Snapshot() {
		if err := seedOne(ctx, stores, e, policy); err != nil {
			return fmt.Errorf("seed entry mRID=%q: %w", e.MRID, err)
		}
	}
	return nil
}

// seedPolicy carries the caller-supplied policy values seedStores stamps
// onto the resources it creates. It is a struct rather than a growing
// parameter list because the four values are unrelated to one another and
// three of them are same-typed *uint32: positional arguments of the same
// type are exactly the shape where a caller can silently transpose two and
// still compile (a registration pIN stamped as a poll rate, for example).
//
// Every field is optional in the sense that its zero value is a valid,
// meaningful configuration; see each field's own comment for what absence
// means on the wire. No field is defaulted here: seedOne never invents a
// value policy did not express (see [[data-invariants]] Rule 2).
type seedPolicy struct {
	// ModesSupported is the DERControlType bitmap stamped onto every
	// seeded DERCapability. nil leaves it unset.
	ModesSupported *uint32

	// RegistrationPIN, when non-nil, causes a Registration record and the
	// EndDevice.RegistrationLink that points at it to be created. nil
	// creates neither: pIN is a required wire element with no
	// spec-defined default, so an invented value or a dangling link would
	// both be worse than the honest absence.
	RegistrationPIN *uint32

	// RegistrationPollRate, when non-nil, is stamped as the
	// Registration's pollRate attribute (seconds). nil leaves it unset so
	// the client applies its own default.
	RegistrationPollRate *uint32

	// DefaultControl is the DefaultDERControl seeded under every device's
	// DERProgram, and pointed at by that program's DefaultDERControlLink.
	// The zero value is valid but degenerate (a well-formed
	// DefaultDERControl with every operating-mode field unset); callers
	// source it from sep2config.SEP2Policy.DefaultControl.
	DefaultControl sep2.DefaultDERControl
}

// seedOne writes the EndDevice and its single child DER for one registry
// entry. Both writes go through the public store Create methods (not
// direct map manipulation), so the EndDeviceStore's SFDI/LFDI secondary
// indexes are built as a side effect, exactly as they would be for a
// device that self-registered over HTTP.
//
// Advertised identity: the store id, the EndDevice.LFDI field, and every
// derived href all use Entry.LFDI, the canonical DER-hash identity. A
// client that is handed the device's raw DER certificate (see
// internal/sep2embed's .x509 emission) self-hashes to this exact value
// and discovers itself by walking GET /edev, with no separate alias to
// reconcile. Ownership is matched against the same canonical LFDI (see
// acl.go's storeOwnerResolver), so the advertised identity and the
// ownership identity are one and the same value. SFDI is unchanged: the
// canonical certificate-derived SFDI (or the LFDI-derived placeholder).
//
// GAGO-049 adds a third resource per entry: a DERCapability, scoped
// under the DER's own parent key (id + "/1", matching core's
// DERSingletonHandlers.derParentKey) at the fixed singleton key
// "default" (core's coresingleton.SingletonKey; duplicated locally as
// snapshot.go's singletonKey constant rather than imported, see that
// constant's own doc comment for why). Fields set:
//
//   - Href: "/edev/" + id + "/der/1/dercap", matching the DER's own href
//     pattern.
//   - ModesSupported: modesSupported, passed straight through unchanged
//     (nil stays nil; a real bitmap is copied by value via
//     stores.DERCapabilities.Create -> sep2.DERCapability.Copy, so a
//     caller mutating its own pointee afterward cannot retroactively
//     change what was stored).
//   - RTGMaxVar: built from e.MaxQ (the CIM PowerElectronicsConnection
//     maxQ attribute, a genuine rated maximum, distinct from the live q
//     operating point) when e.MaxQ is non-nil; left nil, not a
//     fabricated zero, when e.MaxQ is nil (the CIM binding was absent
//     for this device). See buildRTGMaxVar's own doc comment for the
//     value/multiplier construction.
//
// The DER created just above also gets der.DERCapabilityLink stamped to
// this same href before its own Create call, so a client GETting the DER
// can discover its capability resource without a separate list walk;
// see der's construction below.
//
// No other rtg* field (RTGMaxW, RTGMaxA, RTGMaxChargeRateW,
// RTGMaxDischargeRateW) is populated here from the CIM
// PowerElectronicsConnection query results, and this is deliberate, not
// a placeholder for later completion of this card:
//
//   - The core sep2.DERCapability type (as vendored) has no RTGMaxVA or
//     RTGMaxV field at all, so the spec-correct ratedS -> rtgMaxVA /
//     ratedU -> rtgMaxV mapping this card was scoped to has no target to
//     write into. This is a real gap in the vendored core library
//     against the full IEEE 2030.5 DERCapability schema, not a staleness
//     artifact; see this card's report for the cross-checked evidence.
//   - None of the other already-queried CIM fields cleanly retarget onto
//     the rtg* fields core's type DOES have: maxIFault is a per-unit
//     fault-current multiplier (a protection-study parameter, not an
//     absolute current rating), and p/q are live operating-point values,
//     not rated/maximum capability values. Mapping either class onto
//     RTGMaxA/RTGMaxW would be a forced, semantically wrong mapping,
//     which this card's spec explicitly forbids ("do not force a
//     mapping"; "do NOT silently invent capability bits"). maxQ is the
//     one exception: it is itself a rated maximum, not a live value, so
//     RTGMaxVar is populated from it.
func seedOne(ctx context.Context, stores *assembly.Stores, e registry.Entry, policy seedPolicy) error {
	id := e.LFDI

	sfdi := e.SFDI
	if sfdi == "" {
		sfdi = derivePlaceholderSFDI(e.LFDI)
	}

	enabled := true
	dev := sep2.EndDevice{
		Enabled: &enabled,
		LFDI:    id,
		SFDI:    sfdi,
	}
	dev.Href = "/edev/" + id
	dev.DERListLink = &sep2.ListLink{Href: "/edev/" + id + "/der", All: 1}

	// changedTime is a REQUIRED element on EndDevice with no default, and
	// it is the resource's own last-modified timestamp in TimeType
	// (seconds since the Unix epoch, spec section 10.1.4), so seeding
	// time is the honest value: that is when this bridge created the
	// record. Leaving the zero value would advertise 1970 to every
	// client. It is stamped once here at seed time and not refreshed on
	// telemetry updates, which is consistent with the rest of the seeded
	// EndDevice (nothing else about the device record changes after
	// seeding; the live values live on DERStatus, not here).
	dev.ChangedTime = time.Now().UTC().Unix()

	// The FSA list link is stamped before Create, so the stored EndDevice
	// carries it: core serves the stored record verbatim, so a link added
	// after the fact would never reach a client. all=1 matches the single
	// FSA seeded below, and it is the count a client's list walk reads.
	dev.FunctionSetAssignmentsListLink = &sep2.ListLink{Href: "/edev/" + id + "/fsa", All: 1}

	// The registration link is stamped only when a real pIN exists to
	// back it. See seedStores' doc comment: a link to an absent resource
	// makes a client 404 partway through registration, which is a worse
	// failure than the absent link it would be replacing.
	if policy.RegistrationPIN != nil {
		dev.RegistrationLink = &sep2.Link{Href: registrationHref(id)}
	}

	if err := stores.EndDevices.Create(ctx, id, dev); err != nil {
		return fmt.Errorf("create EndDevice: %w", err)
	}

	if policy.RegistrationPIN != nil {
		if err := seedRegistration(ctx, stores, id, *policy.RegistrationPIN, policy.RegistrationPollRate); err != nil {
			return err
		}
	}

	if err := seedFSA(ctx, stores, id); err != nil {
		return err
	}

	if err := seedDERProgram(ctx, stores, id, policy.DefaultControl); err != nil {
		return err
	}

	dercapHref := "/edev/" + id + "/der/1/dercap"

	der := sep2.DER{}
	der.Href = "/edev/" + id + "/der/1"
	der.DERCapabilityLink = &sep2.Link{Href: dercapHref}

	if err := stores.DERs.Create(ctx, id, "1", der); err != nil {
		return fmt.Errorf("create DER: %w", err)
	}

	rtgMaxVar, err := buildRTGMaxVar(e.MaxQ, id)
	if err != nil {
		return fmt.Errorf("build RTGMaxVar: %w", err)
	}

	dercap := sep2.DERCapability{
		ModesSupported: policy.ModesSupported,
		RTGMaxVar:      rtgMaxVar,
	}
	dercap.Href = dercapHref

	if err := stores.DERCapabilities.Create(ctx, id+"/1", singletonKey, dercap); err != nil {
		return fmt.Errorf("create DERCapability: %w", err)
	}

	return nil
}

// registrationHref returns the canonical Registration path for the device
// stored under id. Both the EndDevice.RegistrationLink and the
// Registration record's own Href use this one function, so the link a
// client follows and the resource it lands on cannot drift apart. The
// shape matches the route core mounts for the Registration function set
// (/edev/{id}/rg).
func registrationHref(id string) string { return "/edev/" + id + "/rg" }

// fsaHref returns the canonical path for the single FunctionSetAssignments
// resource seeded under the device stored under id, at controlFSAID: the
// same FSA id the DOWN path writes its DERControls under (see control.go).
// That coupling is the point: an FSA seeded at any other id would be
// perfectly discoverable and lead to an empty program, silently stranding
// every control this bridge applies.
func fsaHref(id string) string { return "/edev/" + id + "/fsa/" + controlFSAID }

// fsaDescription is the FunctionSetAssignments description this bridge
// advertises. Kept at or under maxDescriptionChars: the String32 bound is
// enforced by a client on the simple value, and exceeding it makes the
// client reject the entire FSA rather than truncate one field, which
// would take the whole DERProgram walk down with it. There is a test
// asserting the length for exactly that reason.
const fsaDescription = "GridAPPS-D DER assignment"

// seedRegistration writes the Registration resource that
// EndDevice.RegistrationLink points at, for the device stored under id.
//
// The store key is id, the device's own canonical LFDI, because that is
// what core's Registration handler looks up: it resolves the {id} path
// segment against the EndDevice store, rejects a mismatch against the
// TLS-presented LFDI, and then reads the Registration under that same
// id. Keying by anything else (a counter, a snapshot position) would
// produce a record that resolves for no device, which is the
// index-confusion failure [[data-invariants]] Rule 2 forbids.
//
// Both wire fields are REQUIRED elements, so both are set to real values:
//
//   - pIN comes from policy and is passed through unchanged. It is not
//     defaulted here; the caller decides whether a pIN exists at all
//     (see seedStores).
//   - dateTimeRegistered is the seeding instant in TimeType (seconds since
//     the Unix epoch). This is the semantically correct value rather than
//     a filler: the bridge really did establish this registration at
//     seed time, since the device is pre-provisioned from the CIM model
//     rather than self-registering over HTTP. A zero here would advertise
//     a 1970 registration.
//
// pollRate is an optional attribute; nil leaves it unset so the client
// applies its own default, rather than this bridge asserting a rate that
// policy never expressed.
func seedRegistration(ctx context.Context, stores *assembly.Stores, id string, pin uint32, pollRate *uint32) error {
	rec := sep2.Registration{
		DateTimeRegistered: time.Now().UTC().Unix(),
		PIN:                pin,
	}
	rec.Href = registrationHref(id)
	if pollRate != nil {
		rec.PollRate = *pollRate
	}

	if err := stores.Registrations.Create(ctx, id, rec); err != nil {
		return fmt.Errorf("create Registration: %w", err)
	}
	return nil
}

// seedFSA writes the single FunctionSetAssignments resource that carries
// the device's DERProgramListLink, for the device stored under id.
//
// This is the hop a client cannot skip: there is no other route from an
// EndDevice to a DERProgram. The record is scoped (parent = the device's
// store id, id = controlFSAID) so it is served by both the list route
// (/edev/{id}/fsa) and the single-resource route
// (/edev/{id}/fsa/{fsaId}).
//
// DERProgramListLink points at derProgramListHref(id, controlFSAID), the
// exact list the DOWN path's ensureDERProgram creates its program in, so
// discovery and control converge on one program rather than two.
//
// The mRID is derived rather than composed. mRIDType is hexBinary capped
// at 16 octets, and a client rejects the entire FSA on a malformed value,
// so an LFDI-composed identifier here would make the FSA unparseable and
// re-break the very walk this function exists to open. See
// deriveResourceMRID.
func seedFSA(ctx context.Context, stores *assembly.Stores, id string) error {
	fsa := sep2.FunctionSetAssignments{
		DERProgramListLink: &sep2.ListLink{
			Href: derProgramListHref(id, controlFSAID),
			All:  1,
		},
		MRID:        deriveResourceMRID(id, fsaMRIDKind),
		Description: fsaDescription,
	}
	fsa.Href = fsaHref(id)

	if err := stores.FSAs.Create(ctx, id, controlFSAID, fsa); err != nil {
		return fmt.Errorf("create FunctionSetAssignments: %w", err)
	}
	return nil
}

// seedDERProgram writes the DERProgram (and its DefaultDERControl
// singleton) that the FSA seeded just above advertises, for the device
// stored under id.
//
// Why this is SEED-time work and not first-control-delta work (GAGO-094,
// Devi's HIGH finding). It used to be created lazily by control.go's
// ensureDERProgram on the first ApplyControlDelta for a device, which made
// the discovery walk depend on message arrival ORDER. A client that walked
// /edev/{lfdi}/fsa/1/derp before the first delta arrived got a
// DERProgramList serving all="0", and the consequences compound rather than
// merely delay:
//
//   - The list carries pollRate="900" (core's own value for that route), and
//     a conformant client honors it. The EPRI reference client's poll_derpl
//     pins the DERProgramList stub at that rate, so the client does not
//     re-walk for up to 15 minutes: every control issued in that window is
//     missed. Measured: two runs where the client started 25 seconds before
//     the delta both read all="0" and never actuated.
//   - Worse than a delay: the fast poll on the DERControlList is armed only
//     from an EXISTING DERProgram resource (oeg_client.c calls der_program
//     only for a parsed SE_DERProgram, and der_program is what sets the
//     DERControlList's own active_poll_rate). With an empty program list
//     that arming never happens at all, so the control list is not merely
//     polled late, its fast poll is never established.
//
// Seeding unconditionally removes the ordering dependence: the walk finds a
// real program whether or not any control has been issued yet.
//
// An empty program is legitimate, not a hollow shell. sep.xsd's DERProgram
// requires exactly two children, mRID (via SubscribableIdentifiedObject)
// and primacy; all four of its ListLinks are minOccurs="0". Both required
// children are set below, so the served program satisfies a strict,
// schema-driven parser (the EPRI client's parser does enforce minOccurs:
// xml_parse.c fails the document when a required element is absent). Its
// DERControlList in turn declares DERControl minOccurs="0"
// maxOccurs="unbounded", so a program with no controls yet serves a valid
// empty list rather than an invalid one.
//
// defaultControl is the caller's policy value, written verbatim except for
// Href and MRID, which are stamped to match this program's own scope. A
// client that follows DefaultDERControlLink must land on a well-formed
// resource, so the singleton is created in the same call as the program
// that links to it, never separately.
func seedDERProgram(ctx context.Context, stores *assembly.Stores, id string, defaultControl sep2.DefaultDERControl) error {
	if err := ensureDERProgram(ctx, stores, id, controlFSAID, controlDERProgramID, defaultControl); err != nil {
		return fmt.Errorf("create DERProgram: %w", err)
	}
	return nil
}

// buildRTGMaxVar constructs the sep2.ReactivePower value for
// DERCapability.RTGMaxVar from a registry.Entry's MaxQ (the CIM
// PowerElectronicsConnection.maxQ attribute), or returns nil when maxQ
// is nil (the CIM binding was absent for this device; a nil result here
// is not a fabricated zero, per data-invariants).
//
// CIM stores PowerElectronicsConnection.maxQ in whole, unscaled base
// volt-amperes reactive: cross-checked against CIMHub_2_0's linkml
// PEC/storage schema comment ("CIM stores in VAr (SI base)") and real
// CIM100 instance data (ieee9500_2025 fixtures carry raw integers such
// as 250000 for a 250 kVAr rating). registry.Entry.MaxQ is populated
// straight from the SPARQL binding in that same unscaled form (see
// cmd/bridge/main.go's queryDevices).
//
// IEEECORE-014 narrowed sep2.ReactivePower.Value to int16 (per the
// PowerOfTenMultiplierType-scaled wire encoding IEEE 2030.5 actually
// uses), so a real fleet's unscaled maxQ (hundreds of thousands of VAr
// is a realistic rated magnitude) no longer fits Value directly at
// Multiplier 0: GAGO-083 replaces the old hardcoded Multiplier: 0 with
// computePowerOfTen, which picks the smallest multiplier that lets maxQ's
// magnitude fit int16, preserving as many significant digits as int16
// allows. See computePowerOfTen's own doc comment for the scaling and
// rounding rules.
//
// deviceID names the entry this call is seeding (the store id, which is
// the device's canonical LFDI); it is used only for the warnings below,
// never mixed into the returned value.
//
// A negative maxQ is malformed CIM data: maxQ is the positive
// generation-side reactive rating (CIMHub's minQ carries the negative
// absorption side), and the IEEE 2030.5 rtgMaxVar field expects the
// delivered/positive rating. Rather than propagate a wrong-sign rating
// or silently clamp it to a guessed magnitude, buildRTGMaxVar logs a
// warning naming the device and returns nil, exactly as it does for an
// absent maxQ: no capability advertised is safer than a wrong-sign one
// (Cyrus, GAGO-049 review LOW finding; GAGO-068).
//
// If maxQ's magnitude is so large that computePowerOfTen cannot fit it
// into int16 even at the maximum multiplier, buildRTGMaxVar returns that
// error to its caller rather than fabricating or truncating a value: per
// [[data-invariants]] Rule 2, a value that cannot be represented validly
// must be refused, not silently corrupted.
func buildRTGMaxVar(maxQ *int64, deviceID string) (*sep2.ReactivePower, error) {
	if maxQ == nil {
		return nil, nil
	}
	if *maxQ < 0 {
		log.Printf("sep2embed: WARNING: device %q has negative CIM maxQ (%d); rtgMaxVar expects the positive delivered rating, dropping to nil instead of advertising a wrong-sign capability", deviceID, *maxQ)
		return nil, nil
	}
	value, mult, err := computePowerOfTen(*maxQ)
	if err != nil {
		return nil, fmt.Errorf("device %q: %w", deviceID, err)
	}
	return &sep2.ReactivePower{Multiplier: mult, Value: value}, nil
}

// derivePlaceholderSFDI returns a syntactically valid (spec 6.3.3 shaped,
// sepTLS.ValidateSFDI-passing) placeholder SFDI, deterministically
// derived from lfdi.
//
// GAGO-033 gives every device a certificate-derived registry.Entry.SFDI
// (spec section 6.3.3, computed directly from the device's own
// certificate by internal/sep2embed.EnsureDeviceIdentities), so seedOne
// reaches this fallback only for an Entry that predates that change or
// was constructed directly by a caller that never set SFDI (e.g. an
// older fixture or a registry-only test). EndDevice.SFDI is a required
// (non-omitempty) wire field and the EndDeviceStore's GetBySFDI index
// needs a stable, unique key regardless, so this function mirrors
// sepTLS.SFDI's derivation shape (36-bit truncation of a SHA-256 digest,
// formatted as 11 decimal digits plus a digit-sum check digit) but
// starting from the LFDI string instead of a certificate's DER bytes.
// Downstream code that validates SFDI shape (sepTLS.ValidateSFDI) is not
// special-cased for the placeholder.
func derivePlaceholderSFDI(lfdi string) string {
	sum := sha256.Sum256([]byte(lfdi))

	val := uint64(sum[0])<<28 |
		uint64(sum[1])<<20 |
		uint64(sum[2])<<12 |
		uint64(sum[3])<<4 |
		uint64(sum[4])>>4

	digits := fmt.Sprintf("%011d", val)

	check := 0
	for _, c := range digits {
		check += int(c - '0')
	}
	checkDigit := (10 - check%10) % 10

	return digits + fmt.Sprintf("%d", checkDigit)
}
