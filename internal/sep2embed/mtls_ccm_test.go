package sep2embed

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// ccmTestServer starts an Embed (Observer nil, matching cmd/bridge's own
// choice when EnableCCM is set; see sep2EmbedConfig) with the given
// EnableCCM value, and returns its address plus a device cert/key/CA
// pool minted from the same run's CA, for a client to dial with. t.Cleanup
// tears the server down.
func ccmTestServer(t *testing.T, enableCCM bool) (addr string, caPool *x509.CertPool, deviceCert gotls.Certificate) {
	t.Helper()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	certDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		EnableCCM:              enableCCM,
	}, reg)
	if err != nil {
		t.Fatalf("New(EnableCCM=%v): %v", enableCCM, err)
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
		HWSerialNum: "test-serial-ccm-001",
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

	return e.Addr(), caPool, deviceCert
}

// TestCCMFlagNegotiatesCCM8Suite proves Config.EnableCCM actually produces
// a listener that negotiates TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 (0xC0AE):
// a client offering both CCM_8 and the GCM fallback, in that preference
// order, comes away with CCM_8, not GCM. Proves the flag reaches a running
// listener through sep2embed.New's CCM-only construction
// (newCCMOnlyListener, mtls.go), with no handshake-observation wrapper in
// the way: Observer is nil here, mirroring cmd/bridge's own choice when
// EnableCCM is set.
func TestCCMFlagNegotiatesCCM8Suite(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert := ccmTestServer(t, true)

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
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8, gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; this skips server cert verification entirely (RootCAs above is unused), which is fine here: only the negotiated suite is asserted
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	if err := conn.HandshakeContext(hsCtx); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	state := conn.ConnectionState()
	if state.CipherSuite != gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 {
		t.Errorf("negotiated cipher = %#04x, want %#04x (TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8)", state.CipherSuite, gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8)
	}
	if state.Version != gotls.VersionTLS12 {
		t.Errorf("negotiated version = %#04x, want TLS 1.2 (%#04x)", state.Version, gotls.VersionTLS12)
	}
}

// TestCCMListenerRefusesGCMOnlyClient is the operator's revised
// requirement for issue 82: with the flag on, the listener must serve
// TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 and nothing else, so a client unable
// to offer it fails the handshake rather than being served over GCM.
// This is the negative case TestCCMFlagNegotiatesCCM8Suite cannot cover
// (that test's client offers CCM_8 first, so it would pass even if GCM
// were still available as a fallback): here the client offers ONLY GCM,
// which the un-fixed listener (core's
// NewCCMServerConfigWithExtraCAs, CipherSuites unmodified) would still
// accept.
func TestCCMListenerRefusesGCMOnlyClient(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert := ccmTestServer(t, true)

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
		t.Fatal("handshake from a GCM-only client against the CCM listener: want an error, got nil (the listener must not fall back to GCM)")
	}
	// PR 108 review LOW: asserting only that the handshake errs lets a
	// future change break it for an unrelated reason and still pass.
	// "handshake failure" is the alert the client actually receives for
	// a cipher-suite mismatch (the server's more specific reason, "no
	// cipher suite supported by both client and server", is logged
	// server side only, never sent over the wire).
	if !strings.Contains(err.Error(), "handshake failure") {
		t.Errorf("handshake error = %q, want a cipher-suite-mismatch message", err.Error())
	}
}

// TestDefaultListenerNegotiatesCCM8 is TestCCMFlagNegotiatesCCM8Suite's
// sibling for the delegated path: with Config.EnableCCM left at its zero
// value (false) and Observer nil, New falls through to server-go's
// sep2srv.New (embed.go), which as of core v0.20.0 also builds a CCM-8
// only listener (wrapMTLS -> sepTLS.NewCCMServerConfigWithExtraCAs), the
// same core call newCCMOnlyListener uses. Before that bump this path
// served the plain stdlib crypto/tls GCM suite instead; a client
// offering only GCM now gets exactly TestCCMListenerRefusesGCMOnlyClient's
// refusal, proven separately below rather than re-run here.
func TestDefaultListenerNegotiatesCCM8(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert := ccmTestServer(t, false)

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
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; this skips server cert verification entirely (RootCAs above is unused), which is fine here: only the negotiated suite is asserted
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	if err := conn.HandshakeContext(hsCtx); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	if state.CipherSuite != gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 {
		t.Errorf("negotiated cipher = %#04x, want %#04x (TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8, the only suite core v0.20.0 offers)", state.CipherSuite, gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8)
	}
}

// TestCCMListenerRejectsForeignCertificate is TestCCMFlagNegotiatesCCM8Suite's
// negative sibling (PR 108 review MEDIUM 3): both existing CCM tests dial
// with a certificate the listener's own CA signed, so neither would catch
// RequireAnyClientCert or VerifyPeerCertificate being weakened on the CCM
// path specifically. This mints a device certificate from a SEPARATE,
// untrusted CA (genTestCA, mirroring
// TestNewObservedMTLSListenerRecordsRejectedHandshakeWithRealReason's GCM
// equivalent) and asserts the CCM listener still refuses it at the
// network level.
func TestCCMListenerRejectsForeignCertificate(t *testing.T) {
	t.Parallel()

	addr, caPool, _ := ccmTestServer(t, true)

	rogueCACert, rogueCAKey, _ := genTestCA(t, "rogue-ca-ccm")
	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(rogueCACert, rogueCAKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-ccm-rogue",
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
		RootCAs:            caPool,
		Certificates:       []gotls.Certificate{rogueCert},
		MinVersion:         gotls.VersionTLS12,
		MaxVersion:         gotls.VersionTLS12,
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; the point under test is the server's rejection of the client cert, not the client's trust of the server
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	if err := conn.HandshakeContext(hsCtx); err == nil {
		t.Fatal("handshake with a rogue-CA-signed client cert over CCM: want an error, got nil")
	}
}

// gotlsHTTPClient returns an *http.Client that dials over the gotls fork
// (the CCM-8 listener's stack: plain crypto/tls has no CCM cipher suite
// to offer) via DialTLSContext, so http.Client's own request/response
// machinery drives the actual protocol traffic under test rather than a
// hand-rolled read of the raw connection.
func gotlsHTTPClient(cfg *gotls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				conn := gotls.Client(raw, cfg)
				if err := conn.HandshakeContext(ctx); err != nil {
					_ = raw.Close()
					return nil, err
				}
				return conn, nil
			},
		},
		Timeout: 5 * time.Second,
	}
}

// TestCCMListenerServesAuthenticatedRequest is the coverage gap the
// security lane's LOW named: the two suite-negotiation tests above
// handshake, assert, and close, so a regression in identity extraction
// on the forked CCM connection type would pass both today. This drives
// one real GET /dcap over a genuinely CCM-8-negotiated connection and
// asserts the response, mirroring embed_test.go's plain-GCM
// TestEmbedServesSeededDevicesOverMTLS shape for the CCM path.
func TestCCMListenerServesAuthenticatedRequest(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert := ccmTestServer(t, true)

	client := gotlsHTTPClient(&gotls.Config{
		RootCAs:            caPool,
		Certificates:       []gotls.Certificate{deviceCert},
		MinVersion:         gotls.VersionTLS12,
		MaxVersion:         gotls.VersionTLS12,
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; only the request/response over the CCM connection is under test
	})

	resp, err := client.Get("https://" + addr + "/dcap")
	if err != nil {
		t.Fatalf("GET /dcap over CCM: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /dcap over CCM: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestRequireCCMVerification is the guard newCCMOnlyListener now runs
// (PR 108 review MEDIUM 3): newObservedMTLSListener's sibling check at
// mtls.go:81 only wraps an existing VerifyPeerCertificate it does not
// own; this constructor mutates a *gotls.Config another module builds,
// so it needs its own check that a core change has not silently dropped
// verification. Table-driven over the config shape a future core bump
// could plausibly return, proving each case actually trips the guard.
//
// The CipherSuites cases (PR 126 review HIGH, P4) cover only the
// CONSTRUCTION-time config buildCCMServerConfig returns, before
// newObservedMTLSListener's GetConfigForClient ever clones it: they do
// NOT reach a mutation to that per-connection clone, which is
// TestPerConnectionConfigPreservesCipherSuites's job instead.
func TestRequireCCMVerification(t *testing.T) {
	t.Parallel()

	valid := func() *gotls.Config {
		return &gotls.Config{
			ClientAuth:            gotls.RequireAnyClientCert,
			VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { return nil },
			CipherSuites:          []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*gotls.Config)
		wantErr bool
	}{
		{"unmodified config", func(*gotls.Config) {}, false},
		{"VerifyPeerCertificate nil", func(c *gotls.Config) { c.VerifyPeerCertificate = nil }, true},
		{"ClientAuth relaxed to NoClientCert", func(c *gotls.Config) { c.ClientAuth = gotls.NoClientCert }, true},
		{"ClientAuth relaxed to RequestClientCert", func(c *gotls.Config) { c.ClientAuth = gotls.RequestClientCert }, true},
		{"CipherSuites widened to add GCM", func(c *gotls.Config) {
			c.CipherSuites = append(c.CipherSuites, gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
		}, true},
		{"CipherSuites narrowed to empty", func(c *gotls.Config) { c.CipherSuites = nil }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			err := requireCCMVerification(cfg)
			if tc.wantErr && err == nil {
				t.Error("requireCCMVerification: want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("requireCCMVerification: want nil, got %v", err)
			}
		})
	}
}
