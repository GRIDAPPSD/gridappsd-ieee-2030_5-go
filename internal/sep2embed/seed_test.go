package sep2embed

import (
	"context"
	"strings"
	"testing"

	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

func TestSeedStoresPopulatesEndDevicesAndDERsFromRegistry(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := []registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: "AAAA000000000000000000000000000000AAAA", Placeholder: true},
		{MRID: "mrid-bat-1", Name: "Battery 1", LFDI: "BBBB000000000000000000000000000000BBBB", Placeholder: true},
		{MRID: "mrid-sol-1", Name: "Solar 1", LFDI: "CCCC000000000000000000000000000000CCCC", Placeholder: true},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	// Assert the EndDeviceStore holds exactly len(entries) devices, one
	// per registry entry, addressable by the entry's own LFDI (both via
	// the plain Get(id) path and the GetByLFDI secondary index).
	count, err := stores.EndDevices.Count(ctx)
	if err != nil {
		t.Fatalf("EndDevices.Count: %v", err)
	}
	if int(count) != len(entries) {
		t.Fatalf("EndDevices.Count = %d, want %d", count, len(entries))
	}

	for _, e := range entries {
		dev, err := stores.EndDevices.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.Get(%q): %v", e.LFDI, err)
		}
		if dev.LFDI != e.LFDI {
			t.Errorf("dev.LFDI = %q, want %q", dev.LFDI, e.LFDI)
		}
		if dev.Href != "/edev/"+e.LFDI {
			t.Errorf("dev.Href = %q, want %q", dev.Href, "/edev/"+e.LFDI)
		}
		if dev.Enabled == nil || !*dev.Enabled {
			t.Errorf("dev.Enabled = %v, want true", dev.Enabled)
		}
		if dev.SFDI == "" {
			t.Errorf("dev.SFDI is empty for LFDI %q", e.LFDI)
		}
		if !sepTLS.ValidateSFDI(dev.SFDI) {
			t.Errorf("dev.SFDI = %q fails sepTLS.ValidateSFDI (check digit invalid)", dev.SFDI)
		}
		if dev.DERListLink == nil {
			t.Fatalf("dev.DERListLink is nil for LFDI %q", e.LFDI)
		}
		if dev.DERListLink.Href != "/edev/"+e.LFDI+"/der" {
			t.Errorf("dev.DERListLink.Href = %q, want %q", dev.DERListLink.Href, "/edev/"+e.LFDI+"/der")
		}
		if dev.DERListLink.All != 1 {
			t.Errorf("dev.DERListLink.All = %d, want 1", dev.DERListLink.All)
		}

		// Same lookup via the GetByLFDI secondary index: exercised by the
		// EndDeviceStore.Create side effect (indexDevice), not a
		// separate write path.
		byLFDI, err := stores.EndDevices.GetByLFDI(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.GetByLFDI(%q): %v", e.LFDI, err)
		}
		if byLFDI.Href != dev.Href {
			t.Errorf("GetByLFDI returned Href %q, want %q", byLFDI.Href, dev.Href)
		}

		// Exactly one child DER, keyed "1" under the device's own id.
		derList, err := stores.DERs.List(ctx, e.LFDI, store.ListOptions{Limit: 10})
		if err != nil {
			t.Fatalf("DERs.List(%q): %v", e.LFDI, err)
		}
		if derList.All != 1 {
			t.Fatalf("DERs.List(%q).All = %d, want 1", e.LFDI, derList.All)
		}
		if len(derList.Items) != 1 {
			t.Fatalf("DERs.List(%q) returned %d items, want 1", e.LFDI, len(derList.Items))
		}
		wantDERHref := "/edev/" + e.LFDI + "/der/1"
		if derList.Items[0].Href != wantDERHref {
			t.Errorf("DER.Href = %q, want %q", derList.Items[0].Href, wantDERHref)
		}
	}

	// Distinct registry entries must not collide on SFDI: derivePlaceholderSFDI
	// is deterministic per-LFDI, and the three fixture LFDIs above are
	// distinct, so the derived SFDIs must be too.
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		dev, err := stores.EndDevices.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.Get(%q): %v", e.LFDI, err)
		}
		if prior, ok := seen[dev.SFDI]; ok {
			t.Errorf("SFDI collision: LFDI %q and %q both derived SFDI %q", prior, e.LFDI, dev.SFDI)
		}
		seen[dev.SFDI] = e.LFDI
	}
}

func TestSeedStoresEmptyRegistrySeedsEmptyStoresWithoutError(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	stores := newStores()
	ctx := context.Background()

	if err := seedStores(ctx, stores, reg); err != nil {
		t.Fatalf("seedStores on empty registry: %v", err)
	}

	count, err := stores.EndDevices.Count(ctx)
	if err != nil {
		t.Fatalf("EndDevices.Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("EndDevices.Count = %d, want 0", count)
	}

	// The list endpoint's data path (List, not just Count) must also
	// serve zero results without error: empty is not absent.
	result, err := stores.EndDevices.List(ctx, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("EndDevices.List on empty store: %v", err)
	}
	if result.All != 0 || len(result.Items) != 0 {
		t.Fatalf("EndDevices.List = %+v, want All=0 and no items", result)
	}
}

func TestSeedStoresWrapsCreateErrorWithMRID(t *testing.T) {
	t.Parallel()

	// seedOne keys the EndDeviceStore by LFDI. If the store already
	// holds an id collision (here, forced directly rather than via two
	// registry entries, since Registry itself dedupes by mRID and would
	// never hand seedStores two entries sharing a store key), the
	// underlying Create call fails and seedStores must surface that,
	// wrapped with the failing entry's mRID, not swallow it.
	const lfdi = "9999999999999999999999999999999999DDDD"
	entry := registry.Entry{MRID: "mrid-a", LFDI: lfdi}

	ctx := context.Background()
	stores := newStores()
	if err := seedOne(ctx, stores, entry); err != nil {
		t.Fatalf("pre-seed via seedOne: %v", err)
	}

	reg := registry.New()
	if err := reg.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := seedStores(ctx, stores, reg)
	if err == nil {
		t.Fatal("seedStores against a store pre-populated with the same id: want error, got nil")
	}
	got := err.Error()
	if !strings.Contains(got, "mrid-a") {
		t.Errorf("seedStores error = %q, want it to mention the failing mRID %q", got, "mrid-a")
	}
	if !strings.Contains(got, "create EndDevice") {
		t.Errorf("seedStores error = %q, want it to mention the failing create", got)
	}
}

func TestDerivePlaceholderSFDIIsDeterministicAndValid(t *testing.T) {
	t.Parallel()

	const lfdi = "0011223344556677889900112233445566778899"

	got1 := derivePlaceholderSFDI(lfdi)
	got2 := derivePlaceholderSFDI(lfdi)

	if got1 != got2 {
		t.Fatalf("derivePlaceholderSFDI not deterministic: %q vs %q", got1, got2)
	}
	if len(got1) != 12 {
		t.Fatalf("derivePlaceholderSFDI(%q) = %q, want 12 digits", lfdi, got1)
	}
	if !sepTLS.ValidateSFDI(got1) {
		t.Fatalf("derivePlaceholderSFDI(%q) = %q fails sepTLS.ValidateSFDI", lfdi, got1)
	}
}
