package sep2embed

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// mintDeviceIdentity mints a fresh device certificate signed by
// caCert/caKey and returns its certPEM/keyPEM plus its real,
// certificate-derived LFDI (spec section 6.3.4), computed the same way
// identityMiddleware computes a connecting client's LFDI at request
// time (sepTLS.LFDI on the verified peer leaf certificate). Seeding a
// registry.Entry with this exact LFDI is what makes the seeded
// EndDevice's LFDI match what the mTLS handshake will present when
// this cert's matching http.Client connects: the two-device ownership
// test below depends on that match, not on a coincidental shared id
// scheme.
func mintDeviceIdentity(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, serial string) (certPEM, keyPEM []byte, lfdi string) {
	t.Helper()

	certPEM, keyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: serial,
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert(%q): %v", serial, err)
	}
	leaf, err := sep2cert.ParseCertificatePEM(certPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM(%q): %v", serial, err)
	}
	return certPEM, keyPEM, sepTLS.LFDI(leaf)
}

// deviceClient builds an mTLS *http.Client presenting certPEM/keyPEM,
// trusting caCertPEM, mirroring mintTestDeviceClient's TLS config shape
// (embed_test.go) but for a specific, caller-supplied device identity
// rather than an arbitrary throwaway one.
func deviceClient(t *testing.T, certPEM, keyPEM, caCertPEM []byte) *http.Client {
	t.Helper()

	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(certPEM, keyPEM, caCertPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	// InsecureSkipVerify is safe here: the test dials by IP/port, not by
	// the server cert's SAN hostname, and RootCAs (set above) already
	// pins trust to the minted CA. Only the hostname match is skipped;
	// see embed_test.go's mintTestDeviceClient for the identical rationale.
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above; only hostname match is skipped

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   5 * time.Second,
	}
}

// TestACLTwoDeviceCrossAccessMatrix is the centerpiece test for
// GAGO-043: it boots a real embedded server over real mTLS with two
// seeded devices whose LFDIs are derived from their own certificates
// (not placeholders), and proves the full stack (identityMiddleware +
// aclMiddleware + storeOwnerResolver, wired exactly as buildHandler
// composes them) enforces per-device ownership end to end: a device
// can read and write its own resources, and is denied on the other
// device's resources, for both reads and writes.
func TestACLTwoDeviceCrossAccessMatrix(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	// Mint the server's own CA/leaf identity first (idempotent: New
	// below calls ensureServerIdentity again and finds all four files
	// already present, so it just loads them), so device certs below
	// can be signed against the same CA the embedded server will trust.
	_, _, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, caKeyFileName))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}

	certA, keyA, lfdiA := mintDeviceIdentity(t, caCert, caKey, "test-serial-device-a")
	certB, keyB, lfdiB := mintDeviceIdentity(t, caCert, caKey, "test-serial-device-b")
	if lfdiA == lfdiB {
		t.Fatalf("minted devices A and B produced the same LFDI %q; test fixture is broken", lfdiA)
	}

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-device-a", Name: "Device A", LFDI: lfdiA},
		{MRID: "mrid-device-b", Name: "Device B", LFDI: lfdiB},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	e, err := New(ctx, Config{
		Addr:            "127.0.0.1:0",
		CertDir:         certDir,
		ShutdownTimeout: time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned error after ctx cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not return within 3s of ctx cancel (goroutine leak or unbounded shutdown)")
		}
	})

	baseURL := "https://" + e.Addr()
	clientA := deviceClient(t, certA, keyA, caCertPEM)
	clientB := deviceClient(t, certB, keyB, caCertPEM)

	// Own-device reads succeed; cross-device reads are denied.

	assertStatus(t, clientA, http.MethodGet, baseURL+"/edev/"+lfdiA, nil, http.StatusOK,
		"device A reading its own EndDevice")
	assertStatus(t, clientA, http.MethodGet, baseURL+"/edev/"+lfdiB, nil, http.StatusForbidden,
		"device A reading device B's EndDevice (cross-device read)")
	assertStatus(t, clientB, http.MethodGet, baseURL+"/edev/"+lfdiB, nil, http.StatusOK,
		"device B reading its own EndDevice")
	assertStatus(t, clientB, http.MethodGet, baseURL+"/edev/"+lfdiA, nil, http.StatusForbidden,
		"device B reading device A's EndDevice (cross-device read)")

	// A nested own-vs-cross resource, not just the EndDevice singleton
	// itself: proves ownership scoping applies to the whole /edev/{id}
	// subtree, per the design's explicit sub-resource list.
	assertStatus(t, clientA, http.MethodGet, baseURL+"/edev/"+lfdiA+"/der", nil, http.StatusOK,
		"device A reading its own DER list")
	assertStatus(t, clientA, http.MethodGet, baseURL+"/edev/"+lfdiB+"/der", nil, http.StatusForbidden,
		"device A reading device B's DER list (cross-device read)")

	// Own-device writes reach the handler; cross-device writes are denied.

	psBody, err := xml.Marshal(&sep2.PowerStatus{})
	if err != nil {
		t.Fatalf("marshal empty PowerStatus: %v", err)
	}

	// PUT /edev/{id}/ps on A's own subtree must reach the handler (204
	// No Content on a valid upsert), not a 403 from the ACL.
	assertStatus(t, clientA, http.MethodPut, baseURL+"/edev/"+lfdiA+"/ps", psBody, http.StatusNoContent,
		"device A writing its own PowerStatus")

	// The same PUT against B's subtree, issued by A, must be denied by
	// the ACL before the handler ever sees it.
	assertStatus(t, clientA, http.MethodPut, baseURL+"/edev/"+lfdiB+"/ps", psBody, http.StatusForbidden,
		"device A writing device B's PowerStatus (cross-device write)")

	// And the reverse: B cannot write A's PowerStatus either.
	assertStatus(t, clientB, http.MethodPut, baseURL+"/edev/"+lfdiA+"/ps", psBody, http.StatusForbidden,
		"device B writing device A's PowerStatus (cross-device write)")

	// Common/global resources are reachable by any authenticated device.

	assertStatus(t, clientA, http.MethodGet, baseURL+"/dcap", nil, http.StatusOK,
		"device A reading the common /dcap resource")
	assertStatus(t, clientB, http.MethodGet, baseURL+"/dcap", nil, http.StatusOK,
		"device B reading the common /dcap resource")
	assertStatus(t, clientA, http.MethodGet, baseURL+"/edev", nil, http.StatusOK,
		"device A reading the common /edev list")

	// A read-only family rejects a write method with 405.

	assertStatus(t, clientA, http.MethodPost, baseURL+"/dcap", nil, http.StatusMethodNotAllowed,
		"device A POSTing to the read-only /dcap family")
}

// assertStatus issues method against url (with an optional body) using
// client, and fails the test if the response status does not equal
// wantStatus. desc names the scenario for a legible failure message.
func assertStatus(t *testing.T, client *http.Client, method, url string, body []byte, wantStatus int, desc string) {
	t.Helper()

	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatalf("%s: build request: %v", desc, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", desc, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		t.Errorf("%s: status = %d, want %d", desc, resp.StatusCode, wantStatus)
	}
}
