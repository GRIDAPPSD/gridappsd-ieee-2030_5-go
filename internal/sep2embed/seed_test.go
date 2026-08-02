package sep2embed

import (
	"bytes"
	"context"
	"log"
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
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: "AAAA00000000000000000000000000000000AAAA", Placeholder: true},
		{MRID: "mrid-bat-1", Name: "Battery 1", LFDI: "BBBB00000000000000000000000000000000BBBB", Placeholder: true},
		{MRID: "mrid-sol-1", Name: "Solar 1", LFDI: "CCCC00000000000000000000000000000000CCCC", Placeholder: true},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
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

	if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
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
	const lfdi = "999999999999999999999999999999999999DDDD"
	entry := registry.Entry{MRID: "mrid-a", LFDI: lfdi}

	ctx := context.Background()
	stores := newStores()
	if err := seedOne(ctx, stores, entry, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
		t.Fatalf("pre-seed via seedOne: %v", err)
	}

	reg := registry.New()
	if err := reg.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN})
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
		{MRID: "mrid-inv-2", Name: "Inverter 2", LFDI: "EEEE00000000000000000000000000000000EEEE", Placeholder: true},
		{MRID: "mrid-bat-2", Name: "Battery 2", LFDI: "FFFF00000000000000000000000000000000FFFF", Placeholder: true},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	modes := wantModes
	if err := seedStores(ctx, stores, reg, seedPolicy{modesSupported: &modes, resolvePIN: testResolvePIN}); err != nil {
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
// PowerElectronicsConnection maxQ attribute) via computePowerOfTen (GAGO-083:
// Value is now int16, so a real fleet's unscaled maxQ needs the multiplier
// computed, not hardcoded at 0), asserting the exact scaled value and
// multiplier plus the reconstructed effective VAr, and leaves RTGMaxVar nil
// (not a fabricated zero) for an entry whose MaxQ is nil, per
// [[data-invariants]].
func TestSeedStoresStampsRTGMaxVarFromEntryMaxQ(t *testing.T) {
	t.Parallel()

	var wantMaxQ int64 = 250000 // 250 kVAr in unscaled base VAr; does not fit int16 at multiplier 0

	reg := registry.New()
	entries := []registry.Entry{
		{MRID: "mrid-maxq-1", Name: "Inverter MaxQ", LFDI: "111100000000000000000000000000000000AAAA", MaxQ: &wantMaxQ},
		{MRID: "mrid-nomaxq-1", Name: "Inverter NoMaxQ", LFDI: "222200000000000000000000000000000000BBBB"},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	withMaxQ, err := stores.DERCapabilities.Get(ctx, entries[0].LFDI+"/1", "default")
	if err != nil {
		t.Fatalf("DERCapabilities.Get(%q, %q): %v", entries[0].LFDI+"/1", "default", err)
	}
	if withMaxQ.RTGMaxVar == nil {
		t.Fatalf("DERCapability.RTGMaxVar is nil for LFDI %q, want value+multiplier for maxQ=%d", entries[0].LFDI, wantMaxQ)
	}
	const wantValue int16 = 25000
	const wantMult int8 = 1
	if withMaxQ.RTGMaxVar.Value != wantValue {
		t.Errorf("DERCapability.RTGMaxVar.Value = %d, want %d", withMaxQ.RTGMaxVar.Value, wantValue)
	}
	if withMaxQ.RTGMaxVar.Multiplier != wantMult {
		t.Errorf("DERCapability.RTGMaxVar.Multiplier = %d, want %d (smallest multiplier that fits maxQ=%d into int16)", withMaxQ.RTGMaxVar.Multiplier, wantMult, wantMaxQ)
	}
	reconstructed := int64(withMaxQ.RTGMaxVar.Value) * 10
	if reconstructed != wantMaxQ {
		t.Errorf("reconstructed effective VAr = %d, want %d (Value %d * 10^Multiplier %d)", reconstructed, wantMaxQ, withMaxQ.RTGMaxVar.Value, withMaxQ.RTGMaxVar.Multiplier)
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
	entry := registry.Entry{MRID: "mrid-link-1", Name: "Inverter Link", LFDI: "333300000000000000000000000000000000CCCC"}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
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

// TestBuildRTGMaxVar is a table-driven test on buildRTGMaxVar directly,
// asserting the returned struct's field values plus the reconstructed
// effective VAr (not just non-nil), per [[data-invariants]]. Covers: nil
// input (absent CIM binding), zero, a magnitude that fits int16 unscaled,
// a realistic fleet magnitude that needs computePowerOfTen's scaling
// (GAGO-083), the GAGO-068 negative-maxQ guard (Cyrus's GAGO-049 review
// LOW finding), and the over-range refusal.
func TestBuildRTGMaxVar(t *testing.T) {
	t.Parallel()

	fleetMaxQ := int64(250000) // 250 kVAr; does not fit int16 at multiplier 0
	smallMaxQ := int64(5000)
	zeroMaxQ := int64(0)
	negMaxQ := int64(-12345)
	overRangeMaxQ := int64(40000000000000) // even multiplier 9 does not fit int16

	tests := []struct {
		name      string
		maxQ      *int64
		wantNil   bool
		wantErr   bool
		wantVal   int16
		wantMult  int8
		wantRecon int64
	}{
		{name: "nil maxQ stays nil (absent CIM binding)", maxQ: nil, wantNil: true},
		{name: "zero maxQ passes through unchanged", maxQ: &zeroMaxQ, wantVal: 0, wantMult: 0, wantRecon: 0},
		{name: "small maxQ fits int16 without scaling", maxQ: &smallMaxQ, wantVal: 5000, wantMult: 0, wantRecon: 5000},
		{name: "fleet maxQ needs computed multiplier (GAGO-083)", maxQ: &fleetMaxQ, wantVal: 25000, wantMult: 1, wantRecon: 250000},
		{name: "negative maxQ is dropped to nil (GAGO-068 guard)", maxQ: &negMaxQ, wantNil: true},
		{name: "over-range maxQ is refused, not truncated", maxQ: &overRangeMaxQ, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildRTGMaxVar(tt.maxQ, "test-device-id")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildRTGMaxVar(%v) = (%+v, nil), want error", tt.maxQ, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildRTGMaxVar(%v): unexpected error: %v", tt.maxQ, err)
			}
			if tt.wantNil {
				if got != nil {
					t.Fatalf("buildRTGMaxVar(%v) = %+v, want nil", tt.maxQ, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("buildRTGMaxVar(%v) = nil, want &ReactivePower{Multiplier:%d, Value:%d}", tt.maxQ, tt.wantMult, tt.wantVal)
			}
			if got.Value != tt.wantVal {
				t.Errorf("buildRTGMaxVar(%v).Value = %d, want %d", tt.maxQ, got.Value, tt.wantVal)
			}
			if got.Multiplier != tt.wantMult {
				t.Errorf("buildRTGMaxVar(%v).Multiplier = %d, want %d", tt.maxQ, got.Multiplier, tt.wantMult)
			}
			recon := int64(got.Value) * pow10Int64(got.Multiplier)
			if recon != tt.wantRecon {
				t.Errorf("buildRTGMaxVar(%v) reconstructed %d * 10^%d = %d, want %d", tt.maxQ, got.Value, got.Multiplier, recon, tt.wantRecon)
			}
		})
	}
}

// TestBuildRTGMaxVarLogsNegativeMaxQWithDeviceID confirms the negative
// path is observable (logged), and that the log line names the device
// id passed in, so an operator can trace the warning back to the
// offending CIM record, per [[secure-coding]] ("errors are signals, not
// noise").
func TestBuildRTGMaxVarLogsNegativeMaxQWithDeviceID(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	negMaxQ := int64(-500)
	const deviceID = "DEADBEEF0000000000000000000000000000CAFE"

	got, err := buildRTGMaxVar(&negMaxQ, deviceID)
	if err != nil {
		t.Fatalf("buildRTGMaxVar with negative maxQ: unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("buildRTGMaxVar with negative maxQ = %+v, want nil", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, deviceID) {
		t.Errorf("log output %q does not name the device id %q", logged, deviceID)
	}
	if !strings.Contains(logged, "-500") {
		t.Errorf("log output %q does not include the offending maxQ value", logged)
	}
}

// TestSeedStoresDropsNegativeMaxQToNilRTGMaxVar confirms the guard is
// wired end-to-end through seedStores: a registry.Entry carrying a
// negative MaxQ produces a seeded DERCapability with RTGMaxVar nil, the
// same outcome as an absent MaxQ, rather than a wrong-sign rating
// reaching the wire (GAGO-068).
func TestSeedStoresDropsNegativeMaxQToNilRTGMaxVar(t *testing.T) {
	t.Parallel()

	var negMaxQ int64 = -75000

	reg := registry.New()
	entry := registry.Entry{MRID: "mrid-negmaxq-1", Name: "Inverter NegMaxQ", LFDI: "444400000000000000000000000000000000DDDD", MaxQ: &negMaxQ}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	dercap, err := stores.DERCapabilities.Get(ctx, entry.LFDI+"/1", "default")
	if err != nil {
		t.Fatalf("DERCapabilities.Get(%q, %q): %v", entry.LFDI+"/1", "default", err)
	}
	if dercap.RTGMaxVar != nil {
		t.Errorf("DERCapability.RTGMaxVar = %+v for a negative MaxQ, want nil (no wrong-sign capability advertised)", dercap.RTGMaxVar)
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

// TestSeedStoresCreatesRegistrationAndLinkPerDevice is the regression test
// for the end-to-end blocker where every EPRI client failed registration
// with "EndDevice does not contain RegistrationLink": seeding wrote the
// EndDevice but never the RegistrationLink nor a Registration record, so
// the client had nothing to follow. Asserts both halves, by value.
func TestSeedStoresCreatesRegistrationAndLinkPerDevice(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	entries := []registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: "AAAA00000000000000000000000000000000AAAA", Placeholder: true},
		{MRID: "mrid-bat-1", Name: "Battery 1", LFDI: "BBBB00000000000000000000000000000000BBBB", Placeholder: true},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	seenPINs := make(map[uint32]string, len(entries))
	for _, e := range entries {
		wantHref := "/edev/" + e.LFDI + "/rg"

		dev, err := stores.EndDevices.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("EndDevices.Get(%q): %v", e.LFDI, err)
		}
		if dev.RegistrationLink == nil {
			t.Fatalf("dev.RegistrationLink is nil for LFDI %q; the EPRI client fails registration outright on this", e.LFDI)
		}
		// Exact href string: the client GETs this path verbatim, so a
		// near-miss (missing leading slash, "/reg" instead of "/rg") is
		// a 404 at the client, not a cosmetic difference.
		if dev.RegistrationLink.Href != wantHref {
			t.Errorf("dev.RegistrationLink.Href = %q, want %q", dev.RegistrationLink.Href, wantHref)
		}

		// The advertised link must resolve to a record the store
		// actually holds, keyed by the same id the route's {id} segment
		// carries.
		got, err := stores.Registrations.Get(ctx, e.LFDI)
		if err != nil {
			t.Fatalf("Registrations.Get(%q): %v (advertised link would 404)", e.LFDI, err)
		}
		if got.Href != wantHref {
			t.Errorf("Registration.Href = %q, want %q", got.Href, wantHref)
		}
		if got.DateTimeRegistered <= 0 {
			t.Errorf("Registration.DateTimeRegistered = %d, want a positive epoch second (required xsd element)", got.DateTimeRegistered)
		}
		// PINType is a 6-digit unsigned decimal, 0 to 999999 inclusive.
		if got.PIN > 999999 {
			t.Errorf("Registration.PIN is outside the PINType range [0, 999999] for LFDI %q", e.LFDI)
		}
		// No policy poll rate supplied: the attribute must stay unset so
		// the client applies sep.xsd's own 900-second default rather
		// than a rate the bridge invented.
		if got.PollRate != 0 {
			t.Errorf("Registration.PollRate = %d, want 0 (unset) when policy supplies none", got.PollRate)
		}

		// The served PIN is the operator-configured value, identical for
		// every device under a fleet-wide config. It must NOT vary with
		// the LFDI: IEEE 2030.5 section 6.3.5 exists precisely because
		// the SFDI and LFDI "are derived from public information (i.e., a
		// Certificate), therefore can potentially be recreated by an
		// eavesdropper", so a PIN computed from the LFDI would defeat the
		// purpose of the field.
		if got.PIN != testRegistrationPIN {
			t.Errorf("Registration.PIN for LFDI %q is not the configured value", e.LFDI)
		}
		seenPINs[got.PIN] = e.LFDI
	}
}

// TestSeedStoresRegistrationPINIsStableAcrossReseeding locks the property
// the wire depends on: a client may re-fetch its Registration, so the PIN
// must not vary between reads. Re-seeding a fresh store from the same
// registry is the strongest form of that check, since it also covers a
// process restart (no persisted PIN state to reload).
func TestSeedStoresRegistrationPINIsStableAcrossReseeding(t *testing.T) {
	t.Parallel()

	const lfdi = "CCCC00000000000000000000000000000000CCCC"
	entry := registry.Entry{MRID: "mrid-stable", Name: "Stable", LFDI: lfdi}

	ctx := context.Background()

	seedAndRead := func() uint32 {
		t.Helper()
		reg := registry.New()
		if err := reg.Add(entry); err != nil {
			t.Fatalf("Add: %v", err)
		}
		stores := newStores()
		if err := seedStores(ctx, stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
			t.Fatalf("seedStores: %v", err)
		}
		got, err := stores.Registrations.Get(ctx, lfdi)
		if err != nil {
			t.Fatalf("Registrations.Get(%q): %v", lfdi, err)
		}
		return got.PIN
	}

	if first, second := seedAndRead(), seedAndRead(); first != second {
		t.Fatal("registration PIN differed across two seedings of the same device; the PIN must be stable for a device that re-fetches its Registration")
	}
}

// TestSeedStoresStampsRegistrationPolicyWhenNonNil asserts the policy
// override path: a configured PIN replaces the derived one on every
// device, and a configured poll rate reaches the wire attribute.
func TestSeedStoresStampsRegistrationPolicyWhenNonNil(t *testing.T) {
	t.Parallel()

	const lfdi = "DDDD00000000000000000000000000000000DDDD"
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-policy", Name: "Policy", LFDI: lfdi}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// An obvious dummy, not a credential: this is a test fixture value
	// only and is never a default anywhere in the shipped code.
	wantPIN := uint32(123455)
	wantPollRate := uint32(300)

	stores := newStores()
	ctx := context.Background()
	if err := seedStores(ctx, stores, reg, seedPolicy{
		resolvePIN: func(string) (uint32, bool) { return wantPIN, true },
		pollRate:   &wantPollRate,
	}); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	got, err := stores.Registrations.Get(ctx, lfdi)
	if err != nil {
		t.Fatalf("Registrations.Get(%q): %v", lfdi, err)
	}
	if got.PIN != wantPIN {
		t.Error("Registration.PIN did not take the configured policy override")
	}
	if got.PollRate != wantPollRate {
		t.Errorf("Registration.PollRate = %d, want %d", got.PollRate, wantPollRate)
	}
}

// TestSeedRefusesDeviceWithNoConfiguredPIN pins the fail-closed contract.
// sep.xsd's Registration sequence (lines 172-197) makes pIN minOccurs=1 at
// line 184, so a Registration cannot legally omit it, and 0 is a
// schema-valid value that means nothing. Seeding must therefore refuse the
// device rather than invent a value, and the error must name the device so
// an operator can fix the config.
func TestSeedRefusesDeviceWithNoConfiguredPIN(t *testing.T) {
	t.Parallel()

	const lfdi = "EEEE00000000000000000000000000000000EEEE"
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-nopin", Name: "No PIN", LFDI: lfdi}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		policy seedPolicy
	}{
		{"nil resolver", seedPolicy{}},
		{"resolver reports unconfigured", seedPolicy{
			resolvePIN: func(string) (uint32, bool) { return 0, false },
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores := newStores()
			err := seedStores(ctx, stores, reg, tc.policy)
			if err == nil {
				t.Fatal("seedStores succeeded with no configured PIN; it must fail closed")
			}
			if !strings.Contains(err.Error(), lfdi) {
				t.Errorf("error does not name the offending device %q: %v", lfdi, err)
			}

			// Fail closed means nothing was provisioned for the device: no
			// Registration to serve, and so no EndDevice advertising a
			// RegistrationLink to a resource the server would refuse.
			if _, err := stores.Registrations.Get(ctx, lfdi); err == nil {
				t.Error("a Registration was stored for a device with no configured PIN")
			}
		})
	}
}
