package sep2embed

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// dialPinnedVersion dials addr over TLS with MinVersion and MaxVersion
// both set to version, so the handshake only succeeds if the server
// accepts exactly that version. On success it returns the version the
// server actually negotiated, read back from the live connection
// (crypto/tls.ConnectionState), not asserted from the dialer's own
// config.
func dialPinnedVersion(t *testing.T, addr string, certPEM, keyPEM, caPEM []byte, version uint16) (negotiated uint16, dialErr error) {
	t.Helper()

	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(certPEM, keyPEM, caPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above; only hostname match is skipped, same posture as mtls_test.go's dialWithClientCert
	tlsCfg.MinVersion = version
	tlsCfg.MaxVersion = version
	clientCert := tlsCfg.Certificates[0]
	tlsCfg.Certificates = nil
	tlsCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &clientCert, nil
	}

	conn, dialErr := tls.Dial("tcp", addr, tlsCfg)
	if dialErr != nil {
		return 0, dialErr
	}
	defer conn.Close()
	return conn.ConnectionState().Version, nil
}

// TestNewObservedMTLSListenerCapsAtTLS12 pins the version range on the
// PRODUCTION observer-listener path (newObservedMTLSListener, the
// constructor Config.Observer wires): core v0.17.0 dropped the
// listener's own MaxVersion from TLS 1.3 to TLS 1.2, and nothing in this
// package sets a version range of its own, so this is the only test
// that would notice a future core bump reopening TLS 1.3.
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

	reg := registry.New()
	var hook connobs.Hook
	listener, _, err := newObservedMTLSListener("127.0.0.1:0", certFile, keyFile, caFile, nil, &hook, reg)
	if err != nil {
		t.Fatalf("newObservedMTLSListener: %v", err)
	}
	defer listener.Close()
	go serveOneRequest(listener)

	if _, dialErr := dialPinnedVersion(t, listener.Addr().String(), devCertPEM, devKeyPEM, caCertPEM, tls.VersionTLS13); dialErr == nil {
		t.Fatal("TLS 1.3-only client: want a handshake error, got nil")
	}

	negotiated, dialErr := dialPinnedVersion(t, listener.Addr().String(), devCertPEM, devKeyPEM, caCertPEM, tls.VersionTLS12)
	if dialErr != nil {
		t.Fatalf("TLS 1.2 client: dial: %v", dialErr)
	}
	if negotiated != tls.VersionTLS12 {
		t.Errorf("negotiated version = %#x, want %#x (tls.VersionTLS12)", negotiated, tls.VersionTLS12)
	}
}
