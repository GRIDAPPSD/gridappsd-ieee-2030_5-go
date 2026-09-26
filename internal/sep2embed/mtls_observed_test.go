package sep2embed

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// ccmObservedTestServer starts an Embed with Config.Observer set and
// Config.EnableCCM left false: the path cmd/bridge actually runs by
// default (cmd/bridge/main.go's connHook is never nil; sep2EmbedConfig
// only clears it when SEP2EnableCCM is set). This is distinct from
// ccmTestServer above, whose EnableCCM=false case leaves Observer nil and
// so exercises the DELEGATED sep2srv.New path, never
// newObservedMTLSListener. Returns the address, a device cert/key/CA pool
// minted from the same run's CA, and the Hook so a test can inspect what
// was recorded. t.Cleanup tears the server down.
func ccmObservedTestServer(t *testing.T) (addr string, caPool *x509.CertPool, deviceCert gotls.Certificate, hook *connobs.Hook) {
	t.Helper()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	hook = &connobs.Hook{}
	certDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		Observer:               hook,
	}, reg)
	if err != nil {
		t.Fatalf("New(Observer set): %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	caCertPEM, err := os.ReadFile(filepath.Join(certDir, caCertFileName))
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
	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-observed-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}

	caPool = x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("failed to parse CA cert into pool")
	}
	deviceCert, err = gotls.X509KeyPair(devCertPEM, devKeyPEM)
	if err != nil {
		t.Fatalf("gotls.X509KeyPair: %v", err)
	}

	return e.Addr(), caPool, deviceCert, hook
}

// TestObservedListenerRefusesGCMOnlyClient is TestCCMListenerRefusesGCMOnlyClient's
// sibling for the path cmd/bridge actually runs by default: Config.Observer
// set (newObservedMTLSListener), not Config.EnableCCM (newCCMOnlyListener).
// No prior test dialed this path at all: PR 126's coverage and security
// review lanes both reproduced a GCM-only client ACCEPTED at 0xc02b
// (TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256) by appending that suite to the
// per-connection clone newPerConnectionConfig builds
// (newObservedMTLSListener's GetConfigForClient), with every package test
// still passing. This is the test that catches it.
func TestObservedListenerRefusesGCMOnlyClient(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert, _ := ccmObservedTestServer(t)

	raw, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer func() { _ = raw.Close() }()

	cfg := &gotls.Config{
		RootCAs:            caPool,
		Certificates:       []gotls.Certificate{deviceCert},
		MinVersion:         gotls.VersionTLS12,
		MaxVersion:         gotls.VersionTLS12,
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; the point under test is the server's refusal of a GCM-only client offer
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	err = conn.HandshakeContext(hsCtx)
	if err == nil {
		t.Fatal("handshake from a GCM-only client against the observed listener: want an error, got nil (Config.Observer must not widen the served suite set)")
	}
	if !strings.Contains(err.Error(), "handshake failure") {
		t.Errorf("handshake error = %q, want a cipher-suite-mismatch message", err.Error())
	}
}

// TestObservedListenerServesAuthenticatedRequest is the coverage gap PR
// 126's review found (P8): no test served a request through an Embed with
// Config.Observer set; the only test that sets Observer
// (TestNewFailsClosedWhenObserverSetWithCCMEnabled) asserts New fails.
// This drives one real GET /dcap over a genuinely CCM-8-negotiated
// connection on the observed path and asserts the response and that the
// hook recorded exactly one accepted handshake, mirroring
// TestCCMListenerServesAuthenticatedRequest's shape for the CCM-only path.
func TestObservedListenerServesAuthenticatedRequest(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert, hook := ccmObservedTestServer(t)

	client := gotlsHTTPClient(&gotls.Config{
		RootCAs:            caPool,
		Certificates:       []gotls.Certificate{deviceCert},
		MinVersion:         gotls.VersionTLS12,
		MaxVersion:         gotls.VersionTLS12,
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; only the request/response over the observed connection is under test
	})

	resp, err := client.Get("https://" + addr + "/dcap")
	if err != nil {
		t.Fatalf("GET /dcap over the observed listener: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /dcap over the observed listener: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	attempt := waitForHandshake(t, hook, 1)
	if !attempt.Accepted {
		t.Errorf("recorded handshake Accepted = false, want true: Reason = %q", attempt.Reason)
	}
}

// TestObservedListenerRejectsForeignCertificate is
// TestObservedListenerServesAuthenticatedRequest's refusal-side pairing
// (PR 126 review risk area: cert acceptance proven on the wire with a
// paired accept and refusal), mirroring
// TestCCMListenerRejectsForeignCertificate's shape for the CCM-only path:
// a device certificate from a CA the observed listener never loaded must
// still be refused at the network level.
func TestObservedListenerRejectsForeignCertificate(t *testing.T) {
	t.Parallel()

	addr, caPool, _, hook := ccmObservedTestServer(t)

	rogueCACert, rogueCAKey, _ := genTestCA(t, "rogue-ca-observed")
	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(rogueCACert, rogueCAKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-observed-rogue",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert (rogue CA): %v", err)
	}
	rogueCert, err := gotls.X509KeyPair(devCertPEM, devKeyPEM)
	if err != nil {
		t.Fatalf("gotls.X509KeyPair: %v", err)
	}

	raw, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer func() { _ = raw.Close() }()

	cfg := &gotls.Config{
		RootCAs:          caPool,
		MinVersion:       gotls.VersionTLS12,
		MaxVersion:       gotls.VersionTLS12,
		CipherSuites:     []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		CurvePreferences: []gotls.CurveID{gotls.CurveP256},
		// GetClientCertificate, not a static Certificates slice: with
		// Certificates set directly, gotls (mirroring stdlib crypto/tls)
		// filters against the server's CertificateRequest acceptable-CA
		// list and silently sends NO certificate at all when the rogue
		// cert matches none of them, which the server then rejects for
		// "client didn't provide a certificate" rather than for the
		// untrusted signature this test means to exercise.
		// GetClientCertificate bypasses that filtering and forces the
		// rogue cert to be presented, matching dialCCMPinnedVersion's
		// pattern in mtls_version_test.go.
		GetClientCertificate: func(*gotls.CertificateRequestInfo) (*gotls.Certificate, error) {
			return &rogueCert, nil
		},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; the point under test is the server's rejection of the client cert
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	if err := conn.HandshakeContext(hsCtx); err == nil {
		t.Fatal("handshake with a rogue-CA-signed client cert over the observed listener: want an error, got nil")
	}

	attempt := waitForHandshake(t, hook, 1)
	if attempt.Accepted {
		t.Error("recorded handshake Accepted = true, want false for a rogue-CA-signed certificate")
	}
	if attempt.Reason == "" {
		t.Error("recorded handshake Reason is empty, want the verifier's real rejection reason")
	}
}

// TestPerConnectionConfigPreservesCipherSuites is a white-box unit test on
// newPerConnectionConfig, the per-connection clone
// newObservedMTLSListener's GetConfigForClient returns for every
// connection: it asserts equality with the base config's CipherSuites
// rather than checking for the absence of any one suite, so it catches a
// future clone that widens the served set by adding ANY suite, not only
// the specific TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 mutation PR 126's
// review reproduced. No network dial: the property under test is entirely
// in the clone, not on the wire.
func TestPerConnectionConfigPreservesCipherSuites(t *testing.T) {
	t.Parallel()

	base := &gotls.Config{
		CipherSuites: []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		ClientAuth:   gotls.RequireAnyClientCert,
		GetConfigForClient: func(*gotls.ClientHelloInfo) (*gotls.Config, error) {
			return nil, nil // never invoked; only its presence on base, and absence on the clone, is asserted
		},
	}
	innerVerify := func([][]byte, [][]*x509.Certificate) error { return nil }

	got := newPerConnectionConfig(base, innerVerify, nil, nil, "203.0.113.1:1234")

	if !slices.Equal(got.CipherSuites, base.CipherSuites) {
		t.Errorf("per-connection CipherSuites = %#04x, want unchanged from base %#04x", got.CipherSuites, base.CipherSuites)
	}
	if got.ClientAuth != base.ClientAuth {
		t.Errorf("per-connection ClientAuth = %v, want unchanged from base %v", got.ClientAuth, base.ClientAuth)
	}
	if got.GetConfigForClient != nil {
		t.Error("per-connection GetConfigForClient must be nil (must not recurse)")
	}
	if got.VerifyPeerCertificate == nil {
		t.Error("per-connection VerifyPeerCertificate must be set (the recording wrapper)")
	}
}

// TestNewRecordingVerifierNilHookDoesNotPanic is the regression test for
// the nil guard added to newRecordingVerifier's hook.RecordHandshake call
// (PR 126 review MEDIUM, P6): newObservedMTLSListener's one caller never
// passes a nil hook today, so this guard is defense in depth rather than a
// currently reachable bug, but nothing else in this package proved it
// safe. Calls the returned verifier directly, bypassing the network, with
// hook == nil.
func TestNewRecordingVerifierNilHookDoesNotPanic(t *testing.T) {
	t.Parallel()

	innerVerify := func([][]byte, [][]*x509.Certificate) error { return nil }
	verify := newRecordingVerifier(innerVerify, nil, nil, "203.0.113.1:1234")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("newRecordingVerifier with hook == nil panicked: %v", r)
		}
	}()
	if err := verify(nil, nil); err != nil {
		t.Errorf("verify(nil, nil) = %v, want nil (innerVerify returns nil)", err)
	}
}
