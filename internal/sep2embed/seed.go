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
// field by field:
//
//	store id (both EndDevice and the DER's parent key) = Entry.LFDI
//	EndDevice.LFDI                                      = Entry.LFDI
//	EndDevice.SFDI                                       = derivePlaceholderSFDI(Entry.LFDI)
//	EndDevice.Enabled                                    = true
//	EndDevice.Href                                       = "/edev/" + id
//	EndDevice.DERListLink                                = "/edev/" + id + "/der" (All: 1)
//	DER id (within the EndDevice's DER scope)             = "1"
//	DER.Href                                              = "/edev/" + id + "/der/1"
//
// The store id uses the LFDI (not a sequential counter) so seeding is
// deterministic across restarts against the same registry snapshot and
// so a device's EndDeviceStore.GetByLFDI lookup resolves to the same id
// every time. LFDI is guaranteed non-empty and unique by
// registry.Registry's own Add/AddBatch validation, and is already
// URL-safe (40 uppercase hex characters).
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
func seedOne(ctx context.Context, stores *assembly.Stores, e registry.Entry) error {
	id := e.LFDI

	enabled := true
	dev := sep2.EndDevice{
		Enabled: &enabled,
		LFDI:    e.LFDI,
		SFDI:    derivePlaceholderSFDI(e.LFDI),
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
// Registry entries at this stage carry only an LFDI, itself a Stage 1
// placeholder per cmd/bridge/lfdi.go (a deterministic SHA-256 hash of
// the device's mRID, not yet derived from a real device certificate).
// GAGO-033 replaces both the LFDI and this SFDI with certificate-derived
// values per spec section 6.3.4. Until then, EndDevice.SFDI is a
// required (non-omitempty) wire field and the EndDeviceStore's
// GetBySFDI index needs a stable, unique key, so this function mirrors
// sepTLS.SFDI's derivation shape (36-bit truncation of a SHA-256 digest,
// formatted as 11 decimal digits plus a digit-sum check digit) but
// starting from the placeholder LFDI string instead of a certificate's
// DER bytes. Downstream code that validates SFDI shape (sepTLS.ValidateSFDI)
// is not special-cased for the placeholder.
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
