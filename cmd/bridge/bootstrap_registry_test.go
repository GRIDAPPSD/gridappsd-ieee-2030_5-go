package main

import (
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
// queryDevices expects: an "id" and a "name" field, matching what
// internal/cim.QueryDataResult.Results.Bindings decodes into. maxQ is
// omitted (OPTIONAL binding absent), matching the current threeDevice*
// fixtures' scope; see deviceBindingWithMaxQ for a row that carries it.
func threeDeviceBinding(mrid, name string) map[string]any {
	return map[string]any{
		"id":   map[string]string{"type": "literal", "value": mrid},
		"name": map[string]string{"type": "literal", "value": name},
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
			t.Errorf("mRID %q: Placeholder = true, want false (GAGO-033 retires the placeholder path)", mrid)
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
						"id":   {Value: "mrid-maxq-1"},
						"name": {Value: "Inverter MaxQ"},
						"maxQ": {Value: "250000"},
					},
					{
						"id":   {Value: "mrid-nomaxq-1"},
						"name": {Value: "Inverter NoMaxQ"},
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
						"id":   {Value: "mrid-bad-1"},
						"name": {Value: "Bad MaxQ"},
						"maxQ": {Value: "not-a-number"},
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

// TestQueryDevicesParsesFloatMaxQ is the GAGO-082 regression: live
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			binding := map[string]cim.Binding{
				"id":   {Value: "mrid-floatmaxq-1"},
				"name": {Value: "Inverter FloatMaxQ"},
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
