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
// field by field (id = Entry.StoreID: the file-hash alias when set,
// otherwise the canonical LFDI):
//
//	store id (both EndDevice and the DER's parent key) = Entry.StoreID
//	EndDevice.LFDI                                      = Entry.StoreID (advertised)
//	EndDevice.SFDI                                       = Entry.SFDI, or
//	                                                       derivePlaceholderSFDI(Entry.LFDI)
//	                                                       when Entry.SFDI is empty
//	EndDevice.Enabled                                    = true
//	EndDevice.Href                                       = "/edev/" + id
//	EndDevice.DERListLink                                = "/edev/" + id + "/der" (All: 1)
//	DER id (within the EndDevice's DER scope)             = "1"
//	DER.Href                                              = "/edev/" + id + "/der/1"
//
// The store id uses Entry.StoreID (not a sequential counter) so a
// device's EndDeviceStore.GetByLFDI lookup resolves to the same id every
// time, and so re-seeding the same registry entry against a fresh store
// always produces the same store key. A device with a file-hash alias is
// advertised under that alias so a file-mode EPRI client discovers it;
// the advertised EndDevice.LFDI is a discovery identity, NOT the
// ownership identity (ownership matches the canonical LFDI via the
// registry: see acl.go). reg.Snapshot() itself iterates a Go map and its
// element order is unspecified per seeding run; that is fine because
// store id assignment does not depend on iteration order (each entry's
// id is derived solely from its own StoreID, not from its position in
// the snapshot). What IS ordered, and is what an /edev GET actually
// returns, is core's memory.Store[T].List: it walks a separately
// maintained sorted key slice, so list responses are sorted by id
// regardless of the order seedStores wrote them in. StoreID is
// guaranteed non-empty (StoreID falls back to LFDI, which registry
// validation requires non-empty) and unique per device, and both the
// uppercase-hex canonical LFDI and the lowercase-hex file-hash alias are
// URL-safe (40 hex characters).
//
// An empty registry seeds empty stores without error: the /edev list
// still serves (0 results), it is simply empty rather than absent.
func seedStores(ctx context.Context, stores *assembly.Stores, reg *registry.Registry) error {
	for _, e := range reg.Snapshot() {
		if err := seedOne(ctx, stores, e); err != nil {
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
// Advertised identity (dual-index): the store id, the EndDevice.LFDI
// field, and every derived href use Entry.StoreID (the file-hash alias
// when the device has one, otherwise the canonical DER-hash LFDI). A
// device with an alias is therefore discoverable by an EPRI file-mode
// client, which walks GET /edev matching EndDevice.LFDI against its OWN
// self-computed file-hash. A device with no alias advertises under its
// canonical LFDI exactly as before this change. The advertised
// EndDevice.LFDI is NOT the ownership identity: ownership is matched
// against the canonical LFDI via the registry, never against this
// advertised field (see acl.go's registryOwnerResolver). SFDI is unchanged:
// still the canonical certificate-derived SFDI (or the LFDI-derived
// placeholder), which is a shape-valid device SFDI regardless of which
// LFDI the device advertises under.
func seedOne(ctx context.Context, stores *assembly.Stores, e registry.Entry) error {
	id := e.StoreID()

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
