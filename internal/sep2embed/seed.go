package sep2embed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"sort"
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
// field by field (id = the opaque URL index allocated for Entry.MRID):
//
//	store id (both EndDevice and the DER's parent key) = index for Entry.MRID
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
// ADDRESSING AND IDENTITY ARE SEPARATE (IEEECORE-URLINDEX). The store id,
// and therefore the {id} segment of every URL, is an opaque server-chosen
// index ("/edev/3/rg"), allocated by core's memory.EndDeviceIndex. It was
// previously Entry.LFDI. The device's IDENTITY is unchanged and is still
// Entry.LFDI: it is what EndDevice.LFDI advertises, what
// EndDeviceStore.GetByLFDI indexes, what the ownership gate compares the
// caller's certificate against (acl.go's storeOwnerResolver), and what the
// registration-PIN resolver is keyed by. A client that self-hashes its own
// raw DER certificate (see internal/sep2embed's .x509 emission) still
// computes that exact LFDI and still discovers itself by walking GET /edev
// and matching on the LFDI field; what it must NOT do is construct its own
// URL from that LFDI, because paths are server-chosen and discovered through
// links.
//
// The allocator is keyed by Entry.MRID rather than Entry.LFDI on purpose.
// The LFDI is SHA-256 over the device certificate, so a certificate rotation
// changes it; the CIM mRID does not move when a cert rotates, so keying on
// it is what lets a device keep its URLs across rotation. Both are non-empty
// and unique per device by registry validation.
//
// reg.Snapshot() iterates a Go map, so its order is randomized per process.
// That used to be harmless because each id was derived solely from its own
// entry, but index allocation is sequential, so seedStores now sorts by mRID
// before allocating. An unchanged fleet therefore produces the same URLs on
// every run. What an /edev GET returns is ordered separately by core's
// memory.Store[T].List, which walks a sorted key slice.
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
	entries := reg.Snapshot()

	// reg.Snapshot() iterates a Go map, whose order is randomized per
	// process. Index assignment is sequential, so seeding straight off that
	// order would give the same fleet different URLs on every run. Sort by
	// mRID (the allocator's device key, unique and non-empty by registry
	// validation) so an unchanged fleet produces the same URLs every time and
	// an end-to-end run is reproducible.
	sort.Slice(entries, func(i, j int) bool { return entries[i].MRID < entries[j].MRID })

	for _, e := range entries {
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

	// resolvePollRate returns the polling interval, in seconds, configured
	// for the device with the given canonical LFDI, and whether one is
	// configured at all. When it reports a rate, that rate is stamped onto
	// the device's seeded Registration as the optional pollRate attribute
	// (sep.xsd:190).
	//
	// A nil resolver, or one reporting false, leaves reg.PollRate zero,
	// which marshals as absent (omitempty) so the client applies sep.xsd's
	// own 900-second default rather than a value the bridge invented.
	// Unlike resolvePIN this is never fatal: pollRate is an optional
	// attribute, while pIN is minOccurs=1 in the Registration sequence.
	//
	// Keyed on LFDI so a per-device rate policy can be added without
	// touching this file; see sep2config.SEP2Policy.PollRates.
	resolvePollRate func(lfdi string) (uint32, bool)
}

// seedOne writes the EndDevice and its single child DER for one registry
// entry. Both writes go through the public store Create methods (not
// direct map manipulation), so the EndDeviceStore's SFDI/LFDI secondary
// indexes are built as a side effect, exactly as they would be for a
// device that self-registered over HTTP.
//
// Advertised identity vs addressing: EndDevice.LFDI carries Entry.LFDI,
// the canonical DER-hash identity, and ownership is matched against that
// same value (see acl.go's storeOwnerResolver). The store id and every
// derived href carry the opaque URL index instead. A client handed the
// device's raw DER certificate (see internal/sep2embed's .x509 emission)
// self-hashes to the LFDI and discovers itself by walking GET /edev and
// matching the LFDI field, then follows the hrefs it finds there; it never
// constructs a path from the LFDI. SFDI is unchanged: the canonical
// certificate-derived SFDI (or the LFDI-derived placeholder).
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
	// The store key, and therefore the {id} segment of every URL this device
	// is served under, is an opaque server-chosen index rather than the
	// device's LFDI (IEEECORE-URLINDEX).
	//
	// The allocator is keyed by the CIM mRID, not by the LFDI, and that
	// choice is load-bearing. The LFDI is SHA-256 over the device
	// certificate, so rotating a cert changes it; keying on the mRID (which a
	// cert rotation does not touch) is what lets a device keep its URLs
	// across rotation. Keying on the LFDI here would reintroduce exactly the
	// breakage this change exists to remove.
	//
	// IDENTITY IS UNAFFECTED. e.LFDI is still what goes into
	// EndDevice.LFDI, still what the ownership gate compares the caller's
	// certificate against (acl.go), and still what the PIN resolver is keyed
	// by. Only addressing moved.
	id, err := stores.EndDeviceIndexes.Allocate(e.MRID)
	if err != nil {
		return fmt.Errorf("allocate URL index: %w", err)
	}

	sfdi := e.SFDI
	if sfdi == "" {
		sfdi = derivePlaceholderSFDI(e.LFDI)
	}

	registrationHref := "/edev/" + id + "/rg"

	enabled := true
	dev := sep2.EndDevice{
		Enabled: &enabled,
		// Identity, NOT addressing: this is the canonical certificate-derived
		// LFDI the ownership gate matches on. It is deliberately not id.
		LFDI: e.LFDI,
		SFDI: sfdi,
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
	// resolvePIN is keyed by the canonical LFDI, which is the identity the
	// operator's config names devices by. It must NOT be handed the URL index:
	// the index is an addressing artifact with no meaning in that config, and
	// passing it would look up a device the operator never configured.
	pin, ok := uint32(0), false
	if policy.resolvePIN != nil {
		pin, ok = policy.resolvePIN(e.LFDI)
	}
	if !ok {
		return fmt.Errorf(
			"no registration PIN configured for device %s: set a per-device entry in "+
				"sep2config.SEP2Policy.RegistrationPINs or a fleet-wide DefaultRegistrationPIN; "+
				"a PIN must be operator-supplied and is never derived", e.LFDI)
	}

	reg := sep2.Registration{
		DateTimeRegistered: time.Now().Unix(),
		PIN:                pin,
	}
	reg.Href = registrationHref
	// Keyed by the canonical LFDI, exactly as resolvePIN above is, and for
	// the same reason: the URL index is an addressing artifact that means
	// nothing in an operator's config.
	if policy.resolvePollRate != nil {
		if rate, ok := policy.resolvePollRate(e.LFDI); ok {
			reg.PollRate = rate
		}
	}

	if err := stores.Registrations.Create(ctx, id, reg); err != nil {
		return fmt.Errorf("create Registration: %w", err)
	}

	dercapHref := "/edev/" + id + "/der/1/dercap"
	dergHref := "/edev/" + id + "/der/1/derg"
	dersHref := "/edev/" + id + "/der/1/ders"
	deraHref := "/edev/" + id + "/der/1/dera"

	der := sep2.DER{}
	der.Href = "/edev/" + id + "/der/1"
	der.DERCapabilityLink = &sep2.Link{Href: dercapHref}
	// DERSettingsLink, DERStatusLink, and DERAvailabilityLink (GAGO-DERLINKS):
	// the EPRI client's put_der_settings (oeg_client.c:352-356) gates each of
	// its four PUTs (dera, dercap, derg, ders) independently on
	// se_exists(der, <Type>Link), so a DER that advertises only
	// DERCapabilityLink gets only the dercap PUT; the other three are
	// silently skipped client-side, which is exactly what eight prior
	// end-to-end runs showed (PUT dercap and nothing else, zero bus frames
	// downstream because no DERStatus PUT ever reaches the telemetry relay).
	// No content is seeded into these three resources: core's
	// HandleSingletonGetPut (pkg/sep2srv/handlers/singleton) already GETs a
	// spec-valid empty default when the backing store holds nothing yet, and
	// upserts on the client's first PUT, so advertising the link is
	// sufficient to unblock the client; seeding placeholder content here
	// would just be fabricated data with no source.
	der.DERSettingsLink = &sep2.Link{Href: dergHref}
	der.DERStatusLink = &sep2.Link{Href: dersHref}
	der.DERAvailabilityLink = &sep2.Link{Href: deraHref}

	if err := stores.DERs.Create(ctx, id, "1", der); err != nil {
		return fmt.Errorf("create DER: %w", err)
	}

	rtgMaxVar, err := buildRTGMaxVar(e.MaxQ, e.LFDI)
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
