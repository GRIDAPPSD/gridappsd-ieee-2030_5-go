package sep2embed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
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
// order, comes away with CCM_8, not GCM. This is the assertion server-go
// issue #603 finds missing on the shared library's own layer; this is the
// bridge's half, proving the flag reaches a running listener through
// sep2embed.New and gridappsd-ieee-2030_5-go's actual production
// construction (New -> the pinned sep2srv.New -> wrapMTLS -> gotls, with
// no handshake-observation wrapper in the way: Observer is nil here,
// mirroring cmd/bridge's own choice when EnableCCM is set).
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
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP, trust pinned via RootCAs above
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

// TestDefaultListenerNegotiatesGCM is TestCCMFlagNegotiatesCCM8Suite's
// sibling: with Config.EnableCCM left at its zero value (false), the
// listener is the plain stdlib crypto/tls GCM path, which has no CCM_8
// cipher suite to offer at all. A listener that always served one thing
// would pass both tests; this one is what tells them apart, and it is
// what a bridge deployed with today's default config actually serves.
func TestDefaultListenerNegotiatesGCM(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert := ccmTestServer(t, false)

	tlsCert := tls.Certificate{Certificate: deviceCert.Certificate, PrivateKey: deviceCert.PrivateKey}
	cfg := &tls.Config{
		RootCAs:            caPool,
		Certificates:       []tls.Certificate{tlsCert},
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP, trust pinned via RootCAs above
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	if state.CipherSuite != tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
		t.Errorf("negotiated cipher = %#04x, want %#04x (TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, the default fallback)", state.CipherSuite, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
	}
}
