package sep2embed

import (
	"context"
	"crypto/sha256"
	"fmt"

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
// modesSupported is the DERControlType bitmap (sep2config.SEP2Policy's
// own field of the same name) stamped onto every seeded DERCapability.
// nil means no policy value was supplied: seedOne leaves the seeded
// DERCapability.ModesSupported nil rather than fabricating a bitmap
// (GAGO-049; see [[data-invariants]] on not silently inventing values).
func seedStores(ctx context.Context, stores *assembly.Stores, reg *registry.Registry, modesSupported *uint32) error {
	for _, e := range reg.Snapshot() {
		if err := seedOne(ctx, stores, e, modesSupported); err != nil {
			return fmt.Errorf("seed entry mRID=%q: %w", e.MRID, err)
		}
	}
	return nil
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
// constant's own doc comment for why). Only two fields are set:
//
//   - Href: "/edev/" + id + "/der/1/dercap", matching the DER's own href
//     pattern.
//   - ModesSupported: modesSupported, passed straight through unchanged
//     (nil stays nil; a real bitmap is copied by value via
//     stores.DERCapabilities.Create -> sep2.DERCapability.Copy, so a
//     caller mutating its own pointee afterward cannot retroactively
//     change what was stored).
//
// No rtg* field (RTGMaxW, RTGMaxA, RTGMaxVar, RTGMaxChargeRateW,
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
//     RTGMaxA/RTGMaxW/RTGMaxVar would be a forced, semantically wrong
//     mapping, which this card's spec explicitly forbids ("do not force
//     a mapping"; "do NOT silently invent capability bits").
func seedOne(ctx context.Context, stores *assembly.Stores, e registry.Entry, modesSupported *uint32) error {
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

	if err := stores.EndDevices.Create(ctx, id, dev); err != nil {
		return fmt.Errorf("create EndDevice: %w", err)
	}

	der := sep2.DER{}
	der.Href = "/edev/" + id + "/der/1"

	if err := stores.DERs.Create(ctx, id, "1", der); err != nil {
		return fmt.Errorf("create DER: %w", err)
	}

	dercap := sep2.DERCapability{ModesSupported: modesSupported}
	dercap.Href = "/edev/" + id + "/der/1/dercap"

	if err := stores.DERCapabilities.Create(ctx, id+"/1", singletonKey, dercap); err != nil {
		return fmt.Errorf("create DERCapability: %w", err)
	}

	return nil
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
