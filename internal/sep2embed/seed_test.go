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
	if err := seedStores(ctx, stores, reg, nil); err != nil {
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

		// Exactly one DERCapability, keyed "default" under the DER's own
		// parent scope (LFDI + "/1"), matching core's DERSingletonHandlers
		// derParentKey and the fixed singleton key. modesSupported is nil
		// here: seedStores above was called with a nil modesSupported
		// argument, and seedOne must not fabricate a bitmap.
		dercap, err := stores.DERCapabilities.Get(ctx, e.LFDI+"/1", "default")
		if err != nil {
			t.Fatalf("DERCapabilities.Get(%q, %q): %v", e.LFDI+"/1", "default", err)
		}
		wantDERCapHref := "/edev/" + e.LFDI + "/der/1/dercap"
		if dercap.Href != wantDERCapHref {
			t.Errorf("DERCapability.Href = %q, want %q", dercap.Href, wantDERCapHref)
		}
		if dercap.ModesSupported != nil {
			t.Errorf("DERCapability.ModesSupported = %v, want nil (no policy value supplied)", *dercap.ModesSupported)
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

	if err := seedStores(ctx, stores, reg, nil); err != nil {
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
	if err := seedOne(ctx, stores, entry, nil); err != nil {
		t.Fatalf("pre-seed via seedOne: %v", err)
	}

	reg := registry.New()
	if err := reg.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := seedStores(ctx, stores, reg, nil)
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

// TestSeedStoresStampsModesSupportedFromPolicyWhenNonNil confirms
// seedStores threads a non-nil modesSupported bitmap into every seeded
// DERCapability, exactly (no truncation, no reinterpretation), per
// GAGO-049. A battery-flavored registry.Entry (Name mentions "Battery")
// is included to exercise the tolerance that seeding a DERCapability
// never requires or inspects any battery-specific rating (ratedE,
// storedE): registry.Entry carries none of that, and this test is the
// standing assertion that seeding still succeeds and produces the same
// modesSupported stamp for a battery entry as for a non-battery one.
func TestSeedStoresStampsModesSupportedFromPolicyWhenNonNil(t *testing.T) {
	t.Parallel()

	const wantModes uint32 = 0x00000005 // arbitrary non-zero test bitmap

	reg := registry.New()
	entries := []registry.Entry{
		{MRID: "mrid-inv-2", Name: "Inverter 2", LFDI: "EEEE000000000000000000000000000000EEEE", Placeholder: true},
		{MRID: "mrid-bat-2", Name: "Battery 2", LFDI: "FFFF000000000000000000000000000000FFFF", Placeholder: true},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	modes := wantModes
	if err := seedStores(ctx, stores, reg, &modes); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	for _, e := range entries {
		dercap, err := stores.DERCapabilities.Get(ctx, e.LFDI+"/1", "default")
		if err != nil {
			t.Fatalf("DERCapabilities.Get(%q, %q): %v", e.LFDI+"/1", "default", err)
		}
		if dercap.ModesSupported == nil {
			t.Fatalf("DERCapability.ModesSupported is nil for LFDI %q, want %#x", e.LFDI, wantModes)
		}
		if *dercap.ModesSupported != wantModes {
			t.Errorf("DERCapability.ModesSupported = %#x, want %#x", *dercap.ModesSupported, wantModes)
		}
	}

	// Mutating the caller's pointee after seeding must not retroactively
	// change what was stored: seedOne must copy the value, not alias the
	// pointer, matching sep2.DERCapability.Copy's own by-value semantics.
	modes = 0xFFFFFFFF
	dercap, err := stores.DERCapabilities.Get(ctx, entries[0].LFDI+"/1", "default")
	if err != nil {
		t.Fatalf("DERCapabilities.Get after mutating caller's pointee: %v", err)
	}
	if dercap.ModesSupported == nil || *dercap.ModesSupported != wantModes {
		t.Errorf("DERCapability.ModesSupported aliased the caller's pointer: got %v, want %#x", dercap.ModesSupported, wantModes)
	}
}

// TestSeedStoresStampsRTGMaxVarFromEntryMaxQ confirms seedOne builds
// DERCapability.RTGMaxVar from registry.Entry.MaxQ (the CIM
// PowerElectronicsConnection maxQ attribute) with the exact
// value/multiplier magnitude, and leaves RTGMaxVar nil (not a
// fabricated zero) for an entry whose MaxQ is nil, per
// [[data-invariants]].
func TestSeedStoresStampsRTGMaxVarFromEntryMaxQ(t *testing.T) {
	t.Parallel()

	var wantMaxQ int64 = 250000 // 250 kVAr in unscaled base VAr

	reg := registry.New()
	entries := []registry.Entry{
		{MRID: "mrid-maxq-1", Name: "Inverter MaxQ", LFDI: "1111000000000000000000000000000000AAAA", MaxQ: &wantMaxQ},
		{MRID: "mrid-nomaxq-1", Name: "Inverter NoMaxQ", LFDI: "2222000000000000000000000000000000BBBB"},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	withMaxQ, err := stores.DERCapabilities.Get(ctx, entries[0].LFDI+"/1", "default")
	if err != nil {
		t.Fatalf("DERCapabilities.Get(%q, %q): %v", entries[0].LFDI+"/1", "default", err)
	}
	if withMaxQ.RTGMaxVar == nil {
		t.Fatalf("DERCapability.RTGMaxVar is nil for LFDI %q, want value+multiplier for maxQ=%d", entries[0].LFDI, wantMaxQ)
	}
	if withMaxQ.RTGMaxVar.Value != wantMaxQ {
		t.Errorf("DERCapability.RTGMaxVar.Value = %d, want %d", withMaxQ.RTGMaxVar.Value, wantMaxQ)
	}
	if withMaxQ.RTGMaxVar.Multiplier != 0 {
		t.Errorf("DERCapability.RTGMaxVar.Multiplier = %d, want 0 (unscaled base VAr)", withMaxQ.RTGMaxVar.Multiplier)
	}

	withoutMaxQ, err := stores.DERCapabilities.Get(ctx, entries[1].LFDI+"/1", "default")
	if err != nil {
		t.Fatalf("DERCapabilities.Get(%q, %q): %v", entries[1].LFDI+"/1", "default", err)
	}
	if withoutMaxQ.RTGMaxVar != nil {
		t.Errorf("DERCapability.RTGMaxVar = %+v for an entry with nil MaxQ, want nil (no fabricated rating)", withoutMaxQ.RTGMaxVar)
	}
}

// TestSeedStoresStampsDERCapabilityLinkOnDER confirms seedOne stamps
// der.DERCapabilityLink onto the seeded DER so a client GETting the DER
// can discover its capability resource, and that the link resolves to
// the exact href the seeded DERCapability was created under (Dutch LOW
// #1, GAGO-049 follow-up).
func TestSeedStoresStampsDERCapabilityLinkOnDER(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entry := registry.Entry{MRID: "mrid-link-1", Name: "Inverter Link", LFDI: "3333000000000000000000000000000000CCCC"}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, nil); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	der, err := stores.DERs.Get(ctx, entry.LFDI, "1")
	if err != nil {
		t.Fatalf("DERs.Get(%q, %q): %v", entry.LFDI, "1", err)
	}
	if der.DERCapabilityLink == nil {
		t.Fatalf("DER.DERCapabilityLink is nil for LFDI %q, want a populated link", entry.LFDI)
	}

	wantHref := "/edev/" + entry.LFDI + "/der/1/dercap"
	if der.DERCapabilityLink.Href != wantHref {
		t.Errorf("DER.DERCapabilityLink.Href = %q, want %q", der.DERCapabilityLink.Href, wantHref)
	}

	dercap, err := stores.DERCapabilities.Get(ctx, entry.LFDI+"/1", "default")
	if err != nil {
		t.Fatalf("DERCapabilities.Get(%q, %q): %v", entry.LFDI+"/1", "default", err)
	}
	if der.DERCapabilityLink.Href != dercap.Href {
		t.Errorf("DER.DERCapabilityLink.Href = %q does not resolve to the seeded DERCapability's own Href %q", der.DERCapabilityLink.Href, dercap.Href)
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
