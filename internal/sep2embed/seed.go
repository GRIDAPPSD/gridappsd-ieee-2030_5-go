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
// policy carries the sep2config.SEP2Policy-sourced values seeding stamps
// onto the resources it creates. Each field's nil/zero value means "no
// policy value was supplied", and seedOne then either derives a
// per-device value or leaves the wire field unset, never fabricating one
// (see [[data-invariants]] on not silently inventing values).
func seedStores(ctx context.Context, stores *assembly.Stores, reg *registry.Registry, policy seedPolicy) error {
	for _, e := range reg.Snapshot() {
		if err := seedOne(ctx, stores, e, policy); err != nil {
			return fmt.Errorf("seed entry mRID=%q: %w", e.MRID, err)
		}
	}
	return nil
}

// seedPolicy is the seeding-relevant projection of
// sep2config.SEP2Policy. It is a struct rather than a widening parameter
// list because seeding now stamps three independent optional policy
// values, and three same-typed *uint32 positional arguments would be
// trivially transposable at a call site.
//
// The zero value is NOT fully valid: nil modesSupported leaves
// DERCapability.ModesSupported nil and nil pollRate omits the
// Registration's optional pollRate attribute (both benign), but a nil
// resolvePIN means no device has a configured registration PIN, and
// seeding then fails closed rather than inventing one.
type seedPolicy struct {
	// modesSupported is the DERControlType bitmap (sep2config.SEP2Policy's
	// own field of the same name) stamped onto every seeded
	// DERCapability. Typed as *sep2.DERControlType (IEEECORE-047),
	// matching sep2.DERCapability.ModesSupported's own field type
	// exactly, so this struct assigns straight through with no
	// conversion at line ~251 below. nil means seedOne leaves the seeded
	// DERCapability.ModesSupported nil rather than fabricating a bitmap
	// (GAGO-049).
	modesSupported *sep2.DERControlType

	// resolvePIN returns the operator-supplied registration PIN for the
	// device with the given canonical LFDI, and whether one is configured
	// at all. It is the ONLY source of a PIN: there is deliberately no
	// derived fallback, because the PIN must not be computable from device
	// identity (see sep2config.SEP2Policy.RegistrationPINs).
	//
	// A nil resolvePIN means no PIN policy was supplied at all and is
	// treated exactly like a resolver that answers false for every device:
	// seeding refuses rather than inventing a value. Never logged.
	resolvePIN func(lfdi string) (uint32, bool)

	// pollRate, when non-nil, is stamped onto each seeded Registration's
	// optional pollRate attribute. nil leaves it zero, which marshals as
	// absent (omitempty) so the client applies sep.xsd's own 900-second
	// default rather than a value the bridge invented.
	pollRate *uint32
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

	registrationHref := "/edev/" + id + "/rg"

	enabled := true
	dev := sep2.EndDevice{
		Enabled: &enabled,
		LFDI:    id,
		SFDI:    sfdi,
	}
	dev.Href = "/edev/" + id
	dev.DERListLink = &sep2.ListLink{Href: "/edev/" + id + "/der", All: 1}
	dev.RegistrationLink = &sep2.Link{Href: registrationHref}

	if err := stores.EndDevices.Create(ctx, id, dev); err != nil {
		return fmt.Errorf("create EndDevice: %w", err)
	}

	// The Registration is keyed by the same store id as its EndDevice
	// because core's registration.HandleGetRegistration looks it up with
	// the {id} segment of /edev/{id}/rg, which is the EndDeviceStore key.
	// Seeding it here (rather than lazily on first GET) is what makes the
	// advertised RegistrationLink resolvable: a link to a resource the
	// store does not hold would 404, which reads to a client exactly like
	// the missing-link failure this seeding exists to fix.
	// sep.xsd's Registration sequence (lines 172-197) makes pIN
	// minOccurs=1 at line 184, so a served Registration cannot legally omit
	// it, and 0 is a schema-valid value that carries no meaning. There is
	// therefore no safe default: refuse to provision the device instead.
	// Failing here rather than at first GET is deliberate, because the
	// EndDevice above already advertises a RegistrationLink, and a link to
	// a resource the server will not serve is the exact failure mode the
	// seeding path exists to prevent.
	pin, ok := uint32(0), false
	if policy.resolvePIN != nil {
		pin, ok = policy.resolvePIN(id)
	}
	if !ok {
		return fmt.Errorf(
			"no registration PIN configured for device %s: set a per-device entry in "+
				"sep2config.SEP2Policy.RegistrationPINs or a fleet-wide DefaultRegistrationPIN; "+
				"a PIN must be operator-supplied and is never derived", id)
	}

	reg := sep2.Registration{
		DateTimeRegistered: time.Now().Unix(),
		PIN:                pin,
	}
	reg.Href = registrationHref
	if policy.pollRate != nil {
		reg.PollRate = *policy.pollRate
	}

	if err := stores.Registrations.Create(ctx, id, reg); err != nil {
		return fmt.Errorf("create Registration: %w", err)
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
		ModesSupported: policy.modesSupported,
		RTGMaxVar:      rtgMaxVar,
	}
	dercap.Href = dercapHref

	if err := stores.DERCapabilities.Create(ctx, id+"/1", singletonKey, dercap); err != nil {
		return fmt.Errorf("create DERCapability: %w", err)
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
