package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
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
// internal/cim.QueryDataResult.Results.Bindings decodes into.
func threeDeviceBinding(mrid, name string) map[string]any {
	return map[string]any{
		"id":   map[string]string{"type": "literal", "value": mrid},
		"name": map[string]string{"type": "literal", "value": name},
	}
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

	reg, err := bootstrapRegistry(context.Background(), client, "_FEEDER123", certDir, sep2embed.DeviceCertModeDevMint)
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
		certPEM, err := os.ReadFile(certFile)
		if err != nil {
			t.Fatalf("mRID %q: ReadFile(%q): %v", mrid, certFile, err)
		}
		cert, err := sep2cert.ParseCertificatePEM(certPEM)
		if err != nil {
			t.Fatalf("mRID %q: ParseCertificatePEM: %v", mrid, err)
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
	pattern := filepath.Join(certDir, "devices", b.String()+"-*.pem")

	all, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("Glob(%q): %v", pattern, err)
	}
	// Exclude the sibling "-key.pem" private-key file DevMint mode also
	// writes: it matches the same glob (its name is "<base>-key.pem",
	// which itself ends in ".pem"), but this helper wants the leaf
	// certificate only.
	var matches []string
	for _, m := range all {
		if !strings.HasSuffix(m, "-key.pem") {
			matches = append(matches, m)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("Glob(%q) matched %d certificate files (excluding -key.pem), want exactly 1: %v", pattern, len(matches), matches)
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

	reg, err := bootstrapRegistry(context.Background(), client, "_FEEDER123", certDir, sep2embed.DeviceCertModePreprovisioned)
	if err == nil {
		t.Fatal("bootstrapRegistry in Preprovisioned mode with no preprovisioned certs: want error, got nil")
	}
	if reg != nil {
		t.Errorf("bootstrapRegistry returned a non-nil registry alongside the error: %+v", reg)
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
