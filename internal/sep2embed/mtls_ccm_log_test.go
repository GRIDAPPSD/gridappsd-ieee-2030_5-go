package sep2embed

import (
	"bytes"
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
)

// syncBuffer guards a bytes.Buffer written by WrapCCMListener's own
// acceptLoop goroutine (ccmserver.go) while a test goroutine polls it,
// the same shape internal/gridappsdclient/supervisor_test.go uses for a
// background goroutine logging while a test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestCCMListenerRefusalReachesLog pins newCCMOnlyListener's
// sepTLS.WrapCCMListener wrap (mtls.go:177): a rejected handshake on the
// CCM-8-only listener must reach the process log. PR 108 round 3
// review MEDIUM: nothing in the suite asserted this, so a mutant
// reverting mtls.go:177 to the round-2 form (gotls.NewListener(listener,
// cfg), no wrap) left every package passing.
//
// Not parallel: it reads the standard logger's global output, and
// captureLog's reasoning in cmd/bridge/empty_fleet_guard_test.go applies
// here too (Go's test driver runs every non-parallel top-level test to
// completion before any parallel-declared test in this package resumes).
func TestCCMListenerRefusalReachesLog(t *testing.T) {
	buf := &syncBuffer{}
	origOutput := log.Writer()
	log.SetOutput(buf)
	defer log.SetOutput(origOutput)

	addr, caPool, deviceCert := ccmTestServer(t, true)

	raw, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer func() { _ = raw.Close() }()

	// A GCM-only client offer, the same refusal trigger
	// TestCCMListenerRefusesGCMOnlyClient uses: the CCM-only listener
	// must reject it, and this test additionally requires that
	// rejection to reach the log.
	cfg := &gotls.Config{
		RootCAs:            caPool,
		Certificates:       []gotls.Certificate{deviceCert},
		MinVersion:         gotls.VersionTLS12,
		MaxVersion:         gotls.VersionTLS12,
		CipherSuites:       []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		CurvePreferences:   []gotls.CurveID{gotls.CurveP256},
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; the point under test is whether the refusal reaches the log, not certificate trust
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	if err := conn.HandshakeContext(hsCtx); err == nil {
		t.Fatal("handshake from a GCM-only client against the CCM listener: want an error, got nil")
	}

	// The server-side handshake and log write run in WrapCCMListener's
	// own acceptLoop goroutine, not synchronously with the client's
	// HandshakeContext return, so a short poll avoids a flaky race
	// against that goroutine (same shape as mtls_test.go's
	// waitForHandshake).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "TLS handshake error") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no refusal reached the process log within timeout; log = %q", buf.String())
}

// TestObservedListenerLogsRefusedHandshake is
// TestCCMListenerRefusalReachesLog's sibling for the path cmd/bridge
// actually runs by default: Config.Observer set (newObservedMTLSListener),
// not Config.EnableCCM (newCCMOnlyListener). PR 126 review found that
// removing sepTLS.WrapCCMListener from newObservedMTLSListener (serving
// the raw gotls.NewListener directly) left every existing test green,
// because a GCM-only client still fails its OWN handshake either way.
// What WrapCCMListener buys, and what this test is the only check on for
// the observed path, is that the refusal also reaches the process log:
// without it the failure happens lazily inside net/http's per-connection
// read (gotls.Conn is not *tls.Conn, so net/http's own "TLS handshake
// error" special case never fires there), and the review's fallback claim
// that the process log survives as the detector for a cipher-mismatch
// refusal (which connobs never records: VerifyPeerCertificate, and so
// RecordHandshake, only runs once a certificate has been presented) would
// be false.
func TestObservedListenerLogsRefusedHandshake(t *testing.T) {
	buf := &syncBuffer{}
	origOutput := log.Writer()
	log.SetOutput(buf)
	defer log.SetOutput(origOutput)

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
		InsecureSkipVerify: true, //nolint:gosec // test dials by IP; the point under test is whether the refusal reaches the log, not certificate trust
	}
	conn := gotls.Client(raw, cfg)
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hsCancel()
	if err := conn.HandshakeContext(hsCtx); err == nil {
		t.Fatal("handshake from a GCM-only client against the observed listener: want an error, got nil")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "TLS handshake error") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no refusal reached the process log within timeout; log = %q", buf.String())
}
