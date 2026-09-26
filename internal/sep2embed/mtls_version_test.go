package sep2embed

import (
	"context"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// TestNewObservedMTLSListenerCapsAtTLS12 pins the version range on the
// PRODUCTION observer-listener path (newObservedMTLSListener, the
// constructor Config.Observer wires): core v0.17.0 dropped the
// listener's own MaxVersion from TLS 1.3 to TLS 1.2, and nothing in this
// package sets a version range of its own, so this is the only test
// that would notice a future core bump reopening TLS 1.3. Dials through
// gotls/dialCCMPinnedVersion below: since core v0.20.0 this listener
// serves CCM-8 only, a suite stdlib crypto/tls cannot negotiate.
func TestNewObservedMTLSListenerCapsAtTLS12(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	certFile, keyFile, caFile, err := ensureServerIdentity(certDir, DeviceCertModeDevMint)
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
	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-tls-version-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	deviceCert, err := gotls.X509KeyPair(devCertPEM, devKeyPEM)
	if err != nil {
		t.Fatalf("gotls.X509KeyPair: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("failed to parse CA cert into pool")
	}

	reg := registry.New()
	var hook connobs.Hook
	listener, _, err := newObservedMTLSListener("127.0.0.1:0", certFile, keyFile, caFile, nil, &hook, reg)
	if err != nil {
		t.Fatalf("newObservedMTLSListener: %v", err)
	}
	defer listener.Close()
	go serveOneRequest(listener)

	if _, dialErr := dialCCMPinnedVersion(t, listener.Addr().String(), caPool, deviceCert, gotls.VersionTLS13); dialErr == nil {
		t.Fatal("TLS 1.3-only client: want a handshake error, got nil")
	}

	negotiated, dialErr := dialCCMPinnedVersion(t, listener.Addr().String(), caPool, deviceCert, gotls.VersionTLS12)
	if dialErr != nil {
		t.Fatalf("TLS 1.2 client: dial: %v", dialErr)
	}
	if negotiated != gotls.VersionTLS12 {
		t.Errorf("negotiated version = %#x, want %#x (gotls.VersionTLS12)", negotiated, gotls.VersionTLS12)
	}
}

// dialCCMPinnedVersion is dialPinnedVersion for the gotls/CCM-8 stack:
// it offers only TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 (a stdlib crypto/tls
// client has no CCM-8 suite to offer at all, so this must dial through
// gotls, matching mtls_ccm_test.go's own clients) with MinVersion and
// MaxVersion both pinned to version.
func dialCCMPinnedVersion(t *testing.T, addr string, caPool *x509.CertPool, deviceCert gotls.Certificate, version uint16) (negotiated uint16, dialErr error) {
	t.Helper()

	raw, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	cfg := &gotls.Config{
		RootCAs:          caPool,
		MinVersion:       version,
		MaxVersion:       version,
		CipherSuites:     []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8},
		CurvePreferences: []gotls.CurveID{gotls.CurveP256},
		GetClientCertificate: func(*gotls.CertificateRequestInfo) (*gotls.Certificate, error) {
			return &deviceCert, nil
		},
		InsecureSkipVerify: true, //nolint:gosec // only the negotiated version is under test
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if dialErr = conn.HandshakeContext(hsCtx); dialErr != nil {
		_ = raw.Close()
		return 0, dialErr
	}
	defer conn.Close()
	return conn.ConnectionState().Version, nil
}

// TestCCMOnlyListenerCapsAtTLS12 is
// TestNewObservedMTLSListenerCapsAtTLS12's sibling for the CCM-only
// production path (newCCMOnlyListener): PR 108 review MEDIUM (round 2)
// found no test covering this listener's own version range. Mutation,
// applied and diffed before trusting it: adding
// `cfg.MaxVersion = gotls.VersionTLS13` after the CipherSuites overwrite
// in newCCMOnlyListener left the whole suite green; against that mutant a
// TLS 1.2-1.3 client offering no CCM-8 suite was ACCEPTED at TLS 1.3,
// where CipherSuites is not consulted. This test is what would notice
// that regression.
func TestCCMOnlyListenerCapsAtTLS12(t *testing.T) {
	t.Parallel()

	addr, caPool, deviceCert := ccmTestServer(t, true)

	if _, dialErr := dialCCMPinnedVersion(t, addr, caPool, deviceCert, gotls.VersionTLS13); dialErr == nil {
		t.Fatal("TLS 1.3-only client: want a handshake error, got nil")
	}

	negotiated, dialErr := dialCCMPinnedVersion(t, addr, caPool, deviceCert, gotls.VersionTLS12)
	if dialErr != nil {
		t.Fatalf("TLS 1.2 client: dial: %v", dialErr)
	}
	if negotiated != gotls.VersionTLS12 {
		t.Errorf("negotiated version = %#x, want %#x (gotls.VersionTLS12)", negotiated, gotls.VersionTLS12)
	}
}
