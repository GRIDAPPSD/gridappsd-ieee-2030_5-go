package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// mockCIMRequester implements cim.Requester and always returns the same
// canned SPARQL envelope regardless of the query body, mirroring the
// documented current-schema behavior where QueryInverter, QuerySolar,
// and QueryBattery all return the same PowerElectronicsConnection row
// set (see internal/cim/queries.go's doc comment). This exercises
// bootstrapRegistry's dedupe path: three identical query responses must
// still collapse to one registry entry per distinct mRID.
type mockCIMRequester struct {
	resp []byte
	err  error
}

func (m *mockCIMRequester) Request(_ context.Context, _ string, _ []byte) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	// Return a copy: callEnveloped must not observe a mutated slice
	// across repeated calls.
	out := make([]byte, len(m.resp))
	copy(out, m.resp)
	return out, nil
}

// threeDeviceBinding is one SPARQL binding row shaped the way
// queryDevices expects: "id", "pecid", and "name" fields, matching what
// internal/cim.QueryDataResult.Results.Bindings decodes into. maxQ is
// omitted (OPTIONAL binding absent), matching the current threeDevice*
// fixtures' scope; see deviceBindingWithMaxQ for a row that carries it.
// pecid is set equal to mrid: device identity is anchored on
// ?pecid, so these fixtures (which are not exercising the
// unit-vs-PEC-identity split; see
// TestBootstrapRegistryCollapsesUnitAndPECIdentityToOnePEC for that)
// model the case where the row's ?id and ?pecid bindings already agree.
func threeDeviceBinding(mrid, name string) map[string]any {
	return map[string]any{
		"id":    map[string]string{"type": "literal", "value": mrid},
		"pecid": map[string]string{"type": "literal", "value": mrid},
		"name":  map[string]string{"type": "literal", "value": name},
	}
}

// deviceBindingWithMaxQ is threeDeviceBinding plus a maxQ literal
// binding, exercising the OPTIONAL ?maxQ path queryDevices parses.
func deviceBindingWithMaxQ(mrid, name, maxQ string) map[string]any {
	b := threeDeviceBinding(mrid, name)
	b["maxQ"] = map[string]string{"type": "literal", "value": maxQ}
	return b
}

// threeDeviceEnvelope builds the {"data": {...}, "responseComplete":
// true} envelope callEnveloped expects, containing three device rows.
func threeDeviceEnvelope(t *testing.T) []byte {
	t.Helper()

	data := map[string]any{
		"head": map[string]any{"vars": []string{"id", "name"}},
		"results": map[string]any{
			"bindings": []map[string]any{
				threeDeviceBinding("mrid-inv-1", "Inverter 1"),
				threeDeviceBinding("mrid-bat-1", "Battery 1"),
				threeDeviceBinding("mrid-sol-1", "Solar 1"),
			},
		},
	}
	env := map[string]any{
		"data":             data,
		"responseComplete": true,
		"id":               "x",
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// pecUnitBinding builds a single SPARQL binding row with independently
// controllable ?id (the COALESCE(unitID, pecid) result) and ?pecid
// bindings, exercising the device-identity-anchoring fix
// directly: two rows sharing the same pecid but carrying different id
// values must still dedupe to one device once identity is anchored on
// ?pecid.
func pecUnitBinding(id, pecid, name string) map[string]any {
	return map[string]any{
		"id":    map[string]string{"type": "literal", "value": id},
		"pecid": map[string]string{"type": "literal", "value": pecid},
		"name":  map[string]string{"type": "literal", "value": name},
	}
}

// singleRowEnvelope wraps one binding row in the {"data": {...},
// "responseComplete": true} envelope callEnveloped expects.
func singleRowEnvelope(t *testing.T, row map[string]any) []byte {
	t.Helper()

	data := map[string]any{
		"head": map[string]any{"vars": []string{"id", "pecid", "name"}},
		"results": map[string]any{
			"bindings": []map[string]any{row},
		},
	}
	env := map[string]any{"data": data, "responseComplete": true, "id": "x"}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// kindRoutedMockCIMRequester discriminates between the four SPARQL
// templates bootstrapRegistry issues (inverter, solar, battery, and the
// discovery count) by inspecting the request body for each
// template's distinctive substring (matching the discrimination
// approach routedMockCIMRequester in empty_fleet_guard_test.go already
// uses for the count-vs-device split), and returns an independently
// controllable canned envelope for each. Unlike mockCIMRequester's
// single canned response, this lets a test drive a scenario where the
// SAME PowerElectronicsConnection surfaces under a different ?id
// binding depending on which query produced the row (the
// diagnosed failure mode): the inverter and solar templates' OPTIONAL
// Unit blocks bind a PhotovoltaicUnit and yield its mRID, while the
// battery template's BatteryUnit type filter never matches that same
// PV unit and falls through to the pecid fallback.
type kindRoutedMockCIMRequester struct {
	inverterResp []byte
	solarResp    []byte
	batteryResp  []byte
	countResp    []byte
}

func (m *kindRoutedMockCIMRequester) Request(_ context.Context, _ string, body []byte) ([]byte, error) {
	var src []byte
	switch {
	case bytes.Contains(body, []byte("COUNT(DISTINCT")):
		src = m.countResp
	case bytes.Contains(body, []byte("DistSolar")):
		src = m.solarResp
	case bytes.Contains(body, []byte("DistStorage")):
		src = m.batteryResp
	default:
		src = m.inverterResp
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

// TestBootstrapRegistryAnchorsIdentityOnPECNotUnit is the core PEC
// identity-anchoring regression test. A single PowerElectronicsConnection ("PEC-MRID-1") with a
// bound child PhotovoltaicUnit ("UNIT-MRID-1") surfaces:
//
//   - a unit-mRID identity from the inverter and solar queries, whose
//     OPTIONAL Unit block binds the PhotovoltaicUnit and COALESCEs ?id
//     to its mRID;
//   - a pecid-mRID identity from the battery query, whose BatteryUnit
//     type filter never matches a PhotovoltaicUnit, so its OPTIONAL
//     never binds and COALESCE falls back to ?pecid.
//
// Before this fix, dedupe keyed on that ?id value, so this ONE physical
// converter dedupe-collapsed to TWO registry entries (this is the exact
// shape of the live-run defect: 9 PECs minted 18 devices). After the
// fix, dedupe is keyed on ?pecid for every row regardless of which
// query produced it, so the registry must carry exactly ONE entry, keyed
// on the PEC's own mRID, never the unit's.
//
// This test deliberately does not call t.Parallel(): it uses
// captureLog, and per that helper's doc comment, callers must stay
// non-parallel to avoid interleaved log output from this package's
// other parallel bootstrapRegistry tests.
func TestBootstrapRegistryAnchorsIdentityOnPECNotUnit(t *testing.T) {
	const (
		pecMRID  = "PEC-MRID-1"
		unitMRID = "UNIT-MRID-1"
		name     = "Inverter 1"
	)

	requester := &kindRoutedMockCIMRequester{
		inverterResp: singleRowEnvelope(t, pecUnitBinding(unitMRID, pecMRID, name)),
		solarResp:    singleRowEnvelope(t, pecUnitBinding(unitMRID, pecMRID, name)),
		batteryResp:  singleRowEnvelope(t, pecUnitBinding(pecMRID, pecMRID, name)),
		countResp:    countEnvelope(t, 1),
	}
	client := cim.NewClient(requester)

	buf := captureLog(t)

	certDir := t.TempDir()
	reg, err := bootstrapRegistry(context.Background(), client, "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("bootstrapRegistry: %v", err)
	}

	if got := reg.Len(); got != 1 {
		t.Fatalf("registry.Len() = %d, want 1 (one PEC surfaced under a unit identity from inverter/solar and a pecid identity from battery; identity anchoring requires these to dedupe to one device)", got)
	}

	entry, ok := reg.Get(pecMRID)
	if !ok {
		t.Fatalf("registry has no entry keyed on the PEC's own mRID %q", pecMRID)
	}
	if entry.Name != name {
		t.Errorf("entry.Name = %q, want %q", entry.Name, name)
	}
	if entry.Placeholder {
		t.Errorf("entry.Placeholder = true, want false")
	}

	if _, ok := reg.Get(unitMRID); ok {
		t.Errorf("registry has a spurious entry keyed on the child PowerElectronicsUnit's mRID %q; identity must be PEC-anchored only", unitMRID)
	}

	logged := buf.String()
	if strings.Contains(logged, "bridge: WARNING") {
		t.Errorf("discovered(1) == projected(1) after the identity-anchoring fix; want no WARNING, got:\n%s", logged)
	}
	if !strings.Contains(logged, "no drops") {
		t.Errorf("expected the discover-vs-project log line to confirm the match; got:\n%s", logged)
	}
}

// TestQueryDevicesAnchorsMRIDOnPECIDAndPreservesUnitMRID is the
// unit-level VALUE assertion at the queryDevices layer:
// cimDevice.MRID must come from ?pecid (the PowerElectronicsConnection's
// own mRID), and the ?id binding queries.go COALESCEs (the unit mRID
// when a child Unit bound, the pecid again when it did not) must
// survive as cimDevice.UnitMRID rather than being discarded.
func TestQueryDevicesAnchorsMRIDOnPECIDAndPreservesUnitMRID(t *testing.T) {
	t.Parallel()

	fakeQuery := func(_ context.Context, _ string) (*cim.QueryDataResult, error) {
		return &cim.QueryDataResult{
			Results: cim.SPARQLResults{
				Bindings: []map[string]cim.Binding{
					{
						"id":    {Value: "UNIT-MRID-1"},
						"pecid": {Value: "PEC-MRID-1"},
						"name":  {Value: "Inverter 1"},
					},
				},
			},
		}, nil
	}

	devices, err := queryDevices(context.Background(), "inverter", fakeQuery, "_DEADBEEF-0000-0000-0000-000000000123")
	if err != nil {
		t.Fatalf("queryDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("queryDevices returned %d devices, want 1", len(devices))
	}

	got := devices[0]
	if got.MRID != "PEC-MRID-1" {
		t.Errorf("devices[0].MRID = %q, want %q (the PowerElectronicsConnection's own mRID)", got.MRID, "PEC-MRID-1")
	}
	if got.UnitMRID != "UNIT-MRID-1" {
		t.Errorf("devices[0].UnitMRID = %q, want %q (the ?id COALESCE result, preserved not discarded)", got.UnitMRID, "UNIT-MRID-1")
	}
}

// TestQueryDevicesSkipsRowsWithEmptyPECID confirms the skip
// condition is on ?pecid, not ?id: a row whose ?pecid binding is
// missing or empty is skipped, matching the prior behavior of
// skipping rows with a missing identity binding (previously ?id).
func TestQueryDevicesSkipsRowsWithEmptyPECID(t *testing.T) {
	t.Parallel()

	fakeQuery := func(_ context.Context, _ string) (*cim.QueryDataResult, error) {
		return &cim.QueryDataResult{
			Results: cim.SPARQLResults{
				Bindings: []map[string]cim.Binding{
					{
						"id":   {Value: "UNIT-MRID-1"},
						"name": {Value: "No PECID"},
						// pecid deliberately absent.
					},
					{
						"id":    {Value: "UNIT-MRID-2"},
						"pecid": {Value: "PEC-MRID-2"},
						"name":  {Value: "Has PECID"},
					},
				},
			},
		}, nil
	}

	devices, err := queryDevices(context.Background(), "inverter", fakeQuery, "_DEADBEEF-0000-0000-0000-000000000123")
	if err != nil {
		t.Fatalf("queryDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("queryDevices returned %d devices, want 1 (the row with an empty pecid must be skipped)", len(devices))
	}
	if devices[0].MRID != "PEC-MRID-2" {
		t.Errorf("devices[0].MRID = %q, want %q", devices[0].MRID, "PEC-MRID-2")
	}
}

// TestQueryDevicesBatteryPathStillAnchorsOnPECWhenBatteryUnitBound
// confirms PEC-identity anchoring does not regress a feeder that genuinely has
// BatteryUnit children: when the battery query's BatteryUnit type
// filter DOES match (unlike the PV-only-feeder case exercised by
// TestBootstrapRegistryAnchorsIdentityOnPECNotUnit, where it never
// binds), the row's ?id COALESCEs to the BatteryUnit's own mRID. That
// value must still be preserved as UnitMRID while MRID stays anchored
// on ?pecid, exactly like the PV path: only the identity anchor
// changed, not the battery-vs-PV binding shape itself.
func TestQueryDevicesBatteryPathStillAnchorsOnPECWhenBatteryUnitBound(t *testing.T) {
	t.Parallel()

	fakeQuery := func(_ context.Context, _ string) (*cim.QueryDataResult, error) {
		return &cim.QueryDataResult{
			Results: cim.SPARQLResults{
				Bindings: []map[string]cim.Binding{
					{
						"id":    {Value: "BATTERY-UNIT-MRID-1"},
						"pecid": {Value: "PEC-MRID-2"},
						"name":  {Value: "Battery 1"},
					},
				},
			},
		}, nil
	}

	devices, err := queryDevices(context.Background(), "battery", fakeQuery, "_DEADBEEF-0000-0000-0000-000000000123")
	if err != nil {
		t.Fatalf("queryDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("queryDevices returned %d devices, want 1", len(devices))
	}

	got := devices[0]
	if got.MRID != "PEC-MRID-2" {
		t.Errorf("devices[0].MRID = %q, want %q", got.MRID, "PEC-MRID-2")
	}
	if got.UnitMRID != "BATTERY-UNIT-MRID-1" {
		t.Errorf("devices[0].UnitMRID = %q, want %q", got.UnitMRID, "BATTERY-UNIT-MRID-1")
	}
	if got.Name != "Battery 1" {
		t.Errorf("devices[0].Name = %q, want %q", got.Name, "Battery 1")
	}
}

// TestBootstrapRegistryDerivesRealCertBackedIdentities is the
// data-invariants required VALUE assertion at the bootstrapRegistry
// layer: every registry entry's LFDI/SFDI must equal what sepTLS
// computes directly from the minted device certificate on disk, and
// Placeholder must be false. Not just "bootstrapRegistry did not
// error".
func TestBootstrapRegistryDerivesRealCertBackedIdentities(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	requester := &mockCIMRequester{resp: threeDeviceEnvelope(t)}
	client := cim.NewClient(requester)

	reg, err := bootstrapRegistry(context.Background(), client, "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("bootstrapRegistry: %v", err)
	}

	// Three distinct mRIDs came back identically from all three
	// queries; dedupe must collapse to exactly three registry entries,
	// not nine.
	if got := reg.Len(); got != 3 {
		t.Fatalf("registry.Len() = %d, want 3 (dedupe across inverter/solar/battery queries)", got)
	}

	for _, mrid := range []string{"mrid-inv-1", "mrid-bat-1", "mrid-sol-1"} {
		entry, ok := reg.Get(mrid)
		if !ok {
			t.Fatalf("registry missing entry for mRID %q", mrid)
		}
		if entry.Placeholder {
			t.Errorf("mRID %q: Placeholder = true, want false (certificate-derived identity retires the placeholder path)", mrid)
		}

		certFile := deviceCertFileForTest(t, certDir, mrid)
		certDER, err := os.ReadFile(certFile)
		if err != nil {
			t.Fatalf("mRID %q: ReadFile(%q): %v", mrid, certFile, err)
		}
		cert, err := x509.ParseCertificate(certDER)
		if err != nil {
			t.Fatalf("mRID %q: ParseCertificate: %v", mrid, err)
		}

		if want := sepTLS.LFDI(cert); entry.LFDI != want {
			t.Errorf("mRID %q: LFDI = %q, want %q (sepTLS.LFDI of the minted cert)", mrid, entry.LFDI, want)
		}
		if want := sepTLS.SFDI(cert); entry.SFDI != want {
			t.Errorf("mRID %q: SFDI = %q, want %q (sepTLS.SFDI of the minted cert)", mrid, entry.SFDI, want)
		}
	}
}

// deviceCertFileForTest locates the single minted device cert file
// under certDir/devices whose name starts with mrid's sanitized prefix
// (internal/sep2embed.deviceCertFileBase appends a hash suffix this
// test does not need to reproduce exactly; a glob on the safe prefix is
// sufficient and keeps this test decoupled from that suffix's exact
// derivation). Fails the test if zero or more than one file matches.
func deviceCertFileForTest(t *testing.T, certDir, mrid string) string {
	t.Helper()

	var b strings.Builder
	for _, r := range mrid {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if safe {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	// The glob matches only the DER certificate leaf: the sibling private
	// key is "<base>.pem" and the cert is "<base>-<hash>.x509", so a
	// ".x509" pattern cannot match the key file.
	pattern := filepath.Join(certDir, "devices", b.String()+"-*.x509")

	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("Glob(%q): %v", pattern, err)
	}
	if len(matches) != 1 {
		t.Fatalf("Glob(%q) matched %d certificate files, want exactly 1: %v", pattern, len(matches), matches)
	}
	return matches[0]
}

// TestBootstrapRegistryPreprovisionedMissingCertFailsClosed proves the
// fail-closed invariant at the bootstrapRegistry layer: a
// preprovisioned-mode run against a certDir with no operator-supplied
// device certs must return an error, and the registry it returns is
// nil, not partially populated.
func TestBootstrapRegistryPreprovisionedMissingCertFailsClosed(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	requester := &mockCIMRequester{resp: threeDeviceEnvelope(t)}
	client := cim.NewClient(requester)

	reg, err := bootstrapRegistry(context.Background(), client, "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModePreprovisioned)
	if err == nil {
		t.Fatal("bootstrapRegistry in Preprovisioned mode with no preprovisioned certs: want error, got nil")
	}
	if reg != nil {
		t.Errorf("bootstrapRegistry returned a non-nil registry alongside the error: %+v", reg)
	}
}

// TestQueryDevicesParsesOptionalMaxQ is the data-invariants required
// VALUE assertion at the queryDevices layer: a row carrying a maxQ
// literal binding must project to cimDevice.MaxQ with the exact parsed
// int64 magnitude, and a row with the OPTIONAL binding absent (as
// documented on the PEC SPARQL templates in internal/cim/queries.go)
// must project to a nil cimDevice.MaxQ, not a fabricated zero.
func TestQueryDevicesParsesOptionalMaxQ(t *testing.T) {
	t.Parallel()

	fakeQuery := func(_ context.Context, _ string) (*cim.QueryDataResult, error) {
		return &cim.QueryDataResult{
			Results: cim.SPARQLResults{
				Bindings: []map[string]cim.Binding{
					{
						"id":    {Value: "mrid-maxq-1"},
						"pecid": {Value: "mrid-maxq-1"},
						"name":  {Value: "Inverter MaxQ"},
						"maxQ":  {Value: "250000"},
					},
					{
						"id":    {Value: "mrid-nomaxq-1"},
						"pecid": {Value: "mrid-nomaxq-1"},
						"name":  {Value: "Inverter NoMaxQ"},
					},
				},
			},
		}, nil
	}

	devices, err := queryDevices(context.Background(), "inverter", fakeQuery, "_DEADBEEF-0000-0000-0000-000000000123")
	if err != nil {
		t.Fatalf("queryDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("queryDevices returned %d devices, want 2", len(devices))
	}

	if devices[0].MRID != "mrid-maxq-1" {
		t.Fatalf("devices[0].MRID = %q, want %q", devices[0].MRID, "mrid-maxq-1")
	}
	if devices[0].MaxQ == nil {
		t.Fatalf("devices[0].MaxQ is nil, want 250000")
	}
	if *devices[0].MaxQ != 250000 {
		t.Errorf("devices[0].MaxQ = %d, want 250000", *devices[0].MaxQ)
	}

	if devices[1].MRID != "mrid-nomaxq-1" {
		t.Fatalf("devices[1].MRID = %q, want %q", devices[1].MRID, "mrid-nomaxq-1")
	}
	if devices[1].MaxQ != nil {
		t.Errorf("devices[1].MaxQ = %v for a row with no maxQ binding, want nil", *devices[1].MaxQ)
	}
}

// TestQueryDevicesRejectsMalformedMaxQ confirms a present-but-unparsable
// maxQ binding is a hard error, not a silently dropped value.
func TestQueryDevicesRejectsMalformedMaxQ(t *testing.T) {
	t.Parallel()

	fakeQuery := func(_ context.Context, _ string) (*cim.QueryDataResult, error) {
		return &cim.QueryDataResult{
			Results: cim.SPARQLResults{
				Bindings: []map[string]cim.Binding{
					{
						"id":    {Value: "mrid-bad-1"},
						"pecid": {Value: "mrid-bad-1"},
						"name":  {Value: "Bad MaxQ"},
						"maxQ":  {Value: "not-a-number"},
					},
				},
			},
		}, nil
	}

	_, err := queryDevices(context.Background(), "inverter", fakeQuery, "_DEADBEEF-0000-0000-0000-000000000123")
	if err == nil {
		t.Fatal("queryDevices with a malformed maxQ binding: want error, got nil")
	}
	if !strings.Contains(err.Error(), "maxQ") {
		t.Errorf("queryDevices error = %q, want it to mention maxQ", err.Error())
	}
}

// TestQueryDevicesParsesFloatMaxQ is a regression test: live
// CIMHub CIM100 stores PowerElectronicsConnection.maxQ as a
// float-lexical string ("125000.0", not "125000"), and this bridge
// used to reject every such binding with strconv.ParseInt's "invalid
// syntax", aborting the whole bootstrap before any device was seeded.
// This is a table-driven data-invariants VALUE assertion per
// [[data-invariants]]: a float-lexical binding must project to the
// exact rounded int64 VAr magnitude (not merely "parses without
// error"), an integer-lexical binding must keep working exactly as
// before, an absent binding stays nil (not a fabricated zero), and a
// genuinely malformed binding is still a hard error, not a silently
// dropped value.
func TestQueryDevicesParsesFloatMaxQ(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantNil bool
		want    int64
		wantErr bool
	}{
		{name: "float-lexical maxQ (live CIMHub CIM100 shape)", raw: "125000.0", want: 125000},
		{name: "float-lexical maxQ, sub-VAr fraction rounds", raw: "5000.6", want: 5001},
		{name: "integer-lexical maxQ still parses", raw: "5000", want: 5000},
		{name: "empty binding stays nil, not a fabricated zero", raw: "", wantNil: true},
		{name: "malformed non-numeric binding is a hard error", raw: "abc", wantErr: true},
		{name: "NaN is rejected, not silently coerced via int64(NaN)", raw: "NaN", wantErr: true},
		{name: "+Inf is rejected, not silently coerced via int64(+Inf)", raw: "Inf", wantErr: true},
		{name: "-Inf is rejected, not silently coerced via int64(-Inf)", raw: "-Inf", wantErr: true},
		{name: "magnitude beyond int64 range is rejected, not silently overflowed", raw: "1e300", wantErr: true},
		{name: "exact 2^63 boundary is rejected, not silently wrapped to MinInt64 via int64 overflow", raw: "9223372036854775807", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			binding := map[string]cim.Binding{
				"id":    {Value: "mrid-floatmaxq-1"},
				"pecid": {Value: "mrid-floatmaxq-1"},
				"name":  {Value: "Inverter FloatMaxQ"},
			}
			if tt.raw != "" {
				binding["maxQ"] = cim.Binding{Value: tt.raw}
			}

			fakeQuery := func(_ context.Context, _ string) (*cim.QueryDataResult, error) {
				return &cim.QueryDataResult{
					Results: cim.SPARQLResults{
						Bindings: []map[string]cim.Binding{binding},
					},
				}, nil
			}

			devices, err := queryDevices(context.Background(), "inverter", fakeQuery, "_DEADBEEF-0000-0000-0000-000000000123")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("queryDevices with maxQ %q: want error, got nil", tt.raw)
				}
				if !strings.Contains(err.Error(), "maxQ") {
					t.Errorf("queryDevices error = %q, want it to mention maxQ", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("queryDevices with maxQ %q: %v", tt.raw, err)
			}
			if len(devices) != 1 {
				t.Fatalf("queryDevices with maxQ %q returned %d devices, want 1", tt.raw, len(devices))
			}

			got := devices[0].MaxQ
			if tt.wantNil {
				if got != nil {
					t.Errorf("devices[0].MaxQ = %d for an absent binding, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("devices[0].MaxQ is nil, want %d", tt.want)
			}
			if *got != tt.want {
				t.Errorf("devices[0].MaxQ = %d, want %d", *got, tt.want)
			}
		})
	}
}

// TestBootstrapRegistryThreadsMaxQIntoRegistryEntry confirms
// bootstrapRegistry carries the CIM-sourced MaxQ value from the SPARQL
// projection all the way into the registry.Entry the caller receives,
// for a device that has it, and leaves it nil for a device that
// doesn't (this envelope's other two devices carry no maxQ binding,
// matching threeDeviceBinding).
func TestBootstrapRegistryThreadsMaxQIntoRegistryEntry(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	data := map[string]any{
		"head": map[string]any{"vars": []string{"id", "name", "maxQ"}},
		"results": map[string]any{
			"bindings": []map[string]any{
				deviceBindingWithMaxQ("mrid-inv-1", "Inverter 1", "250000"),
				threeDeviceBinding("mrid-bat-1", "Battery 1"),
				threeDeviceBinding("mrid-sol-1", "Solar 1"),
			},
		},
	}
	env := map[string]any{"data": data, "responseComplete": true, "id": "x"}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	requester := &mockCIMRequester{resp: b}
	client := cim.NewClient(requester)

	reg, err := bootstrapRegistry(context.Background(), client, "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("bootstrapRegistry: %v", err)
	}

	withMaxQ, ok := reg.Get("mrid-inv-1")
	if !ok {
		t.Fatal("registry missing entry for mRID mrid-inv-1")
	}
	if withMaxQ.MaxQ == nil {
		t.Fatalf("entry mrid-inv-1: MaxQ is nil, want 250000")
	}
	if *withMaxQ.MaxQ != 250000 {
		t.Errorf("entry mrid-inv-1: MaxQ = %d, want 250000", *withMaxQ.MaxQ)
	}

	withoutMaxQ, ok := reg.Get("mrid-bat-1")
	if !ok {
		t.Fatal("registry missing entry for mRID mrid-bat-1")
	}
	if withoutMaxQ.MaxQ != nil {
		t.Errorf("entry mrid-bat-1: MaxQ = %v, want nil (no maxQ binding for this device)", *withoutMaxQ.MaxQ)
	}
}

func TestDeviceCertModeMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    sep2embed.DeviceCertMode
		wantErr bool
	}{
		{in: "dev-mint", want: sep2embed.DeviceCertModeDevMint},
		{in: "preprovisioned", want: sep2embed.DeviceCertModePreprovisioned},
		{in: "bogus", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			got, err := deviceCertMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("deviceCertMode(%q): want error, got nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("deviceCertMode(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("deviceCertMode(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
