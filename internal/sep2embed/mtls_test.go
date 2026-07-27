package sep2embed

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// serveOneRequest runs a minimal HTTP server on listener until listener
// is closed by the caller. It answers every request with 200 and an
// empty body; the tests in this file only care about the TLS handshake
// outcome (accept/reject, and what newObservedMTLSListener's wrapper
// recorded about it), never about any HTTP response body. Runs in its
// own goroutine; returns (via http.Serve returning its own error) once
// listener.Close() unblocks Accept.
func serveOneRequest(listener net.Listener) {
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// dialWithClientCert dials addr over TLS using certPEM/keyPEM as the
// client's own certificate, trusting trustedCACertPEM as the sole root.
// It returns whatever error the handshake itself produced (nil on
// success), and always closes a successful connection before returning.
//
// GetClientCertificate (not the plain Certificates field) is used so the
// client presents certPEM/keyPEM unconditionally, even when the server's
// CertificateRequest advertises an acceptable-CA list the client's own
// certificate does not chain to (the rogue-CA reject-path test's exact
// case): crypto/tls's stdlib client silently sends an empty certificate
// when Certificates does not match that list, which would make the
// server observe "no certificate presented" rather than the "wrong
// signer" rejection this test needs to drive.
func dialWithClientCert(t *testing.T, addr string, certPEM, keyPEM, trustedCACertPEM []byte) error {
	t.Helper()

	tlsCfg, err := sepTLS.NewClientTLSConfigFromPEM(certPEM, keyPEM, trustedCACertPEM)
	if err != nil {
		t.Fatalf("NewClientTLSConfigFromPEM: %v", err)
	}
	tlsCfg.InsecureSkipVerify = true //nolint:gosec // trust pinned via RootCAs above; only hostname match is skipped, same posture as embed_test.go's mintTestDeviceClient
	clientCert := tlsCfg.Certificates[0]
	tlsCfg.Certificates = nil
	tlsCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &clientCert, nil
	}

	conn, dialErr := tls.Dial("tcp", addr, tlsCfg)
	if dialErr != nil {
		return dialErr
	}
	defer conn.Close()
	return nil
}

// waitForHandshake polls hook.Snapshot() until at least want handshake
// attempts have been recorded, or fails the test after a short timeout.
// The additive wrapper runs on the TLS server goroutine spawned by the
// standard library's own Accept/handshake machinery, so a client Dial
// returning is not itself synchronized with RecordHandshake having
// already run; a short poll avoids a flaky race against that goroutine.
func waitForHandshake(t *testing.T, hook *connobs.Hook, want int) connobs.HandshakeAttempt {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := hook.Snapshot()
		if len(snap.Handshakes) >= want {
			return snap.Handshakes[want-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForHandshake: no handshake recorded within timeout (want >= %d)", want)
	return connobs.HandshakeAttempt{}
}

// TestNewObservedMTLSListenerRecordsAcceptedHandshakeWithMatchingLFDI
// drives one successful mTLS handshake, from a device certificate signed
// by the SAME CA the listener trusts, and asserts the recorded
// HandshakeAttempt's exact field values: Accepted true, Reason empty,
// LFDI equal to the value sepTLS.LFDI itself derives from that same
// certificate, and Known true because the registry carries that LFDI.
func TestNewObservedMTLSListenerRecordsAcceptedHandshakeWithMatchingLFDI(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	certFile, keyFile, caFile, err := ensureServerIdentity(certDir)
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
		HWSerialNum: "test-serial-accept-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}
	devLeaf, err := x509.ParseCertificate(devCertDER)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	wantLFDI := sepTLS.LFDI(devLeaf)

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-accept-1", Name: "Accepted Device", LFDI: wantLFDI, Placeholder: true},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	var hook connobs.Hook
	listener, _, err := newObservedMTLSListener("127.0.0.1:0", certFile, keyFile, caFile, nil, &hook, reg)
	if err != nil {
		t.Fatalf("newObservedMTLSListener: %v", err)
	}
	defer listener.Close()
	go serveOneRequest(listener)

	if dialErr := dialWithClientCert(t, listener.Addr().String(), devCertPEM, devKeyPEM, caCertPEM); dialErr != nil {
		t.Fatalf("dialWithClientCert: %v", dialErr)
	}

	attempt := waitForHandshake(t, &hook, 1)

	if !attempt.Accepted {
		t.Errorf("Handshakes[0].Accepted = false, want true")
	}
	if attempt.Reason != "" {
		t.Errorf("Handshakes[0].Reason = %q, want empty on an accepted attempt", attempt.Reason)
	}
	if attempt.LFDI != wantLFDI {
		t.Errorf("Handshakes[0].LFDI = %q, want %q (sepTLS.LFDI of the presented leaf)", attempt.LFDI, wantLFDI)
	}
	if !attempt.Known {
		t.Errorf("Handshakes[0].Known = false, want true: LFDI %q is in the registry", wantLFDI)
	}
	if attempt.At.IsZero() {
		t.Error("Handshakes[0].At is zero, want a real timestamp")
	}
}

// TestNewObservedMTLSListenerRecordsRejectedHandshakeWithRealReason mints
// a device certificate signed by a ROGUE CA, distinct from the one the
// listener trusts (mirroring devicecert_test.go's
// TestEnsureDeviceIdentitiesPreprovisionedRejectsWrongSignerCert
// pattern), and asserts the additive wrapper both (a) still rejects the
// handshake at the network level (the dial itself must fail: this is the
// "never record-and-swallow" proof) and (b) records the real verifier
// error as Reason, with Known false since the rogue LFDI was never added
// to the registry.
func TestNewObservedMTLSListenerRecordsRejectedHandshakeWithRealReason(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()
	certFile, keyFile, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}

	rogueCACert, rogueCAKey, _ := genTestCA(t, "rogue-ca")
	devCertPEM, devKeyPEM, err := sep2cert.GenerateDeviceCert(rogueCACert, rogueCAKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-reject-001",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert (rogue CA): %v", err)
	}
	devCertDER, err := sep2cert.CertificateDER(devCertPEM)
	if err != nil {
		t.Fatalf("CertificateDER: %v", err)
	}
	devLeaf, err := x509.ParseCertificate(devCertDER)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	rogueLFDI := sepTLS.LFDI(devLeaf)

	reg := registry.New() // deliberately empty: the rogue LFDI is not a known device

	var hook connobs.Hook
	listener, _, err := newObservedMTLSListener("127.0.0.1:0", certFile, keyFile, caFile, nil, &hook, reg)
	if err != nil {
		t.Fatalf("newObservedMTLSListener: %v", err)
	}
	defer listener.Close()
	go serveOneRequest(listener)

	// The client trusts the SERVER's real CA (caCertPEM), exactly as a
	// legitimate client would: this test's failure must come from the
	// SERVER's rejection of the client's rogue-signed chain, not from the
	// client itself refusing to trust the server.
	dialErr := dialWithClientCert(t, listener.Addr().String(), devCertPEM, devKeyPEM, caCertPEM)
	if dialErr == nil {
		t.Fatal("dialWithClientCert with a rogue-CA-signed client cert: want a handshake error, got nil (additive wrapper must never turn a reject into an accept)")
	}

	attempt := waitForHandshake(t, &hook, 1)

	if attempt.Accepted {
		t.Error("Handshakes[0].Accepted = true, want false: the wrapper must record the real verdict, not swallow it")
	}
	if attempt.Reason == "" {
		t.Error("Handshakes[0].Reason is empty, want the real verifier error")
	}
	if attempt.LFDI != rogueLFDI {
		t.Errorf("Handshakes[0].LFDI = %q, want %q (sepTLS.LFDI of the presented leaf, recorded even on reject)", attempt.LFDI, rogueLFDI)
	}
	if attempt.Known {
		t.Error("Handshakes[0].Known = true, want false: the rogue LFDI was never added to the registry")
	}
}
