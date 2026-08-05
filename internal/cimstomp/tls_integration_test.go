//go:build integration

// TLS integration tests for cimstomp.Client and cimstomp.Publisher.
//
// Unlike client_integration_test.go, these tests do NOT require a live
// external broker. The in-process tlsTestServer below generates a fresh
// self-signed CA, server cert, and client cert in memory, listens on
// 127.0.0.1 with crypto/tls, and speaks just enough of STOMP to complete a
// CONNECT-CONNECTED handshake. That is sufficient to exercise both dial
// paths (TLS and plain) without dragging in an mTLS-configured ActiveMQ.

package cimstomp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// tlsTestServer is an in-process TLS listener that completes a STOMP
// CONNECT-CONNECTED handshake and then closes. It is sufficient to exercise
// the dial path through stomp.ConnectWithContext; full request/response is
// covered by the bare-broker integration tests.
type tlsTestServer struct {
	t        *testing.T
	listener net.Listener
	addr     string

	caPEM     []byte
	serverTLS *tls.Config
	clientTLS *tls.Config

	wg     sync.WaitGroup
	stop   chan struct{}
	stopMu sync.Mutex
	closed bool

	requireClientCert bool

	acceptErrs chan error
}

// startTLSTestServer generates a fresh CA plus server cert plus client cert,
// binds 127.0.0.1:0 with crypto/tls, and serves until Stop. If
// requireClientCert is true, the server enforces mTLS by setting
// ClientAuth to RequireAndVerifyClientCert and trusting the issued CA.
func startTLSTestServer(t *testing.T, requireClientCert bool) *tlsTestServer {
	t.Helper()

	caCert, caKey, caPEM := generateCA(t, "cimstomp-test-ca")
	serverCert := generateLeafCert(t, caCert, caKey, "127.0.0.1", true)
	clientCert := generateLeafCert(t, caCert, caKey, "cimstomp-test-client", false)

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	serverCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	}
	if requireClientCert {
		serverCfg.ClientAuth = tls.RequireAndVerifyClientCert
		serverCfg.ClientCAs = caPool
	}

	clientCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
		ServerName:   "127.0.0.1",
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}

	s := &tlsTestServer{
		t:                 t,
		listener:          ln,
		addr:              ln.Addr().String(),
		caPEM:             caPEM,
		serverTLS:         serverCfg,
		clientTLS:         clientCfg,
		requireClientCert: requireClientCert,
		stop:              make(chan struct{}),
		acceptErrs:        make(chan error, 8),
	}

	s.wg.Add(1)
	go s.serve()
	return s
}

func (s *tlsTestServer) Addr() string                 { return s.addr }
func (s *tlsTestServer) ClientTLSConfig() *tls.Config { return s.clientTLS.Clone() }
func (s *tlsTestServer) ServerCAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(s.caPEM)
	return pool
}

// Stop closes the listener and waits for serve/handleConn goroutines to
// exit, then drains any accept/handshake errors that accumulated in
// acceptErrs and logs them via t.Logf. The drain-and-log (rather than a
// bare drop) makes an unexpected accept-loop failure visible in a test's
// -v output instead of it silently vanishing when the buffered channel
// is garbage collected (Dutch M3 / Leon L4).
func (s *tlsTestServer) Stop() {
	s.stopMu.Lock()
	if s.closed {
		s.stopMu.Unlock()
		return
	}
	s.closed = true
	close(s.stop)
	_ = s.listener.Close()
	s.stopMu.Unlock()
	s.wg.Wait()

	for {
		select {
		case err := <-s.acceptErrs:
			if s.t != nil {
				s.t.Logf("tlsTestServer: accept/handshake error: %v", err)
			}
		default:
			return
		}
	}
}

func (s *tlsTestServer) serve() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
			}
			select {
			case s.acceptErrs <- err:
			default:
			}
			return
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn performs a minimal STOMP CONNECT-CONNECTED handshake. We do
// NOT attempt to be a real STOMP broker; we only need the client side of
// stomp.ConnectWithContext to consider the handshake successful, at which
// point Client.Connect proceeds to fetchAuthToken (which will then fail or
// hang). Tests that need a successful Connect call use a context deadline
// or examine intermediate state.
func (s *tlsTestServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Force the TLS handshake so handshake errors surface here rather than
	// on first Read.
	if tc, ok := conn.(*tls.Conn); ok {
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		if err := tc.Handshake(); err != nil {
			select {
			case s.acceptErrs <- fmt.Errorf("server handshake: %w", err):
			default:
			}
			return
		}
		_ = tc.SetDeadline(time.Time{})
	}

	br := bufio.NewReader(conn)

	// Read the CONNECT (or STOMP) frame: command line, then headers until
	// blank line, then body until NUL.
	cmd, err := br.ReadString('\n')
	if err != nil {
		return
	}
	cmd = strings.TrimRight(cmd, "\r\n")
	if cmd != "CONNECT" && cmd != "STOMP" {
		return
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\n" || line == "\r\n" {
			break
		}
	}
	// Discard body up to NUL.
	if _, err := br.ReadString('\x00'); err != nil {
		return
	}

	// Send CONNECTED frame. Heartbeat 0,0 keeps the test simple; the
	// client sent its own preference but go-stomp accepts the server's.
	resp := "CONNECTED\nversion:1.2\nheart-beat:0,0\nserver:cimstomp-tls-test\n\n\x00"
	if _, err := io.WriteString(conn, resp); err != nil {
		return
	}

	// Hold the connection open until the client disconnects or stop fires.
	// Dropping the conn here would cause go-stomp to surface a read error
	// immediately after CONNECTED, which makes Connect look like it failed
	// even though the handshake succeeded.
	//
	// We watch for a STOMP DISCONNECT frame and reply with a RECEIPT so
	// the client's Close completes promptly instead of waiting for its
	// own disconnect timeout (default ~15s in go-stomp).
	//
	// This reader goroutine is tracked on s.wg (not just the outer
	// handleConn) so that Stop's wg.Wait() cannot return while this
	// goroutine is still reading from conn: without the extra Add/Done,
	// the outer select below can take the <-s.stop branch and return
	// (releasing handleConn's own wg slot) while this inner goroutine is
	// still blocked in br.ReadString on a conn Stop is about to close
	// out from under it, which is exactly the kind of racy conn access
	// -race is built to catch (Leon L2).
	done := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimRight(line, "\r\n")
			// Read headers up to blank line.
			receipt := ""
			for {
				h, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if h == "\n" || h == "\r\n" {
					break
				}
				if strings.HasPrefix(h, "receipt:") {
					receipt = strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(h, "\r\n"), "receipt:"))
				}
			}
			// Read body up to NUL.
			if _, err := br.ReadString('\x00'); err != nil {
				return
			}
			if cmd == "DISCONNECT" {
				if receipt != "" {
					resp := fmt.Sprintf("RECEIPT\nreceipt-id:%s\n\n\x00", receipt)
					_, _ = io.WriteString(conn, resp)
				}
				return
			}
		}
	}()
	select {
	case <-s.stop:
	case <-done:
	}
}

// generateCA returns a self-signed CA cert plus its key plus a PEM
// encoding of the cert (suitable for x509.CertPool.AppendCertsFromPEM).
func generateCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CA create: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("CA parse: %v", err)
	}
	pemBytes := pemEncodeCertificate(der)
	return cert, key, pemBytes
}

// generateLeafCert returns a tls.Certificate signed by the given CA. If
// isServer is true, the cert is issued for 127.0.0.1 with both DNS and IP
// SAN entries so it works regardless of how the client phrases ServerName.
func generateLeafCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, isServer bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"127.0.0.1", "localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf create: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        mustParseCert(t, der),
	}
}

func mustParseCert(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

func pemEncodeCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestTLS_ClientConnectsOverTLS exercises the positive path: when
// STOMPConfig.TLS is non-nil, Client.Connect dials with crypto/tls and the
// STOMP CONNECTED frame arrives. The test passes if the handshake succeeds;
// we do not require fetchAuthToken to complete because that needs a token
// responder. We rely on a short context deadline and assert that the
// failure (if any) is a token-fetch failure, NOT a TLS or STOMP-handshake
// failure.
func TestTLS_ClientConnectsOverTLS(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	c := NewClient(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		TLS:      srv.ClientTLSConfig(),
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	assertTLSDialSucceededTokenFetchFailed(t, "Connect", err)
}

// assertTLSDialSucceededTokenFetchFailed encodes the shared assertion used
// by both TestTLS_ClientConnectsOverTLS and TestTLS_ClientReconnectOverTLS
// (Dutch M3): the test server never replies to the token topic, so
// both Connect and Reconnect are expected to fail at fetchAuthToken. What
// each test wants to prove is that the failure is NOT a TLS handshake
// failure: the dial path through TLS completed and STOMP CONNECTED was
// processed, on both the first dial (Connect) and any subsequent dial
// (Reconnect).
func assertTLSDialSucceededTokenFetchFailed(t *testing.T, op string, err error) {
	t.Helper()
	if err == nil {
		// If a future implementation surfaces fetchAuthToken differently,
		// a nil error is also acceptable, but log it: a silent early
		// return here would otherwise hide the fact that the call
		// succeeded (which the caller's doc comment says it does not
		// expect) from -v output (Dutch L1).
		t.Logf("%s over TLS returned nil error (fetchAuthToken unexpectedly succeeded or was bypassed)", op)
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "tcp dial") || strings.Contains(msg, "tls dial") {
		t.Fatalf("%s with TLS unexpectedly failed at TCP/TLS dial: %v", op, err)
	}
	if strings.Contains(msg, "tls:") && !strings.Contains(msg, "fetch auth token") {
		t.Fatalf("%s with TLS failed at TLS layer: %v", op, err)
	}
	if !strings.Contains(msg, "fetch auth token") &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("%s with TLS failed in an unexpected way: %v", op, err)
	}
}

// TestTLS_ClientReconnectOverTLS closes a gap: none of
// the existing TLS tests exercise Client.Reconnect. This proves Reconnect
// re-dials through the same TLS path Connect used (a second TLS handshake
// against the same tlsTestServer, which its serve() Accept loop already
// supports for multiple sequential connections), rather than Reconnect's
// TLS handling only ever having been exercised transitively via Connect.
//
// The test does not require Connect to fully succeed first: Reconnect only
// requires the Client be not-yet-Closed (see Reconnect's doc comment), and
// exercising it after a Connect that itself only got as far as
// fetchAuthToken (same token-bootstrap limitation every other test in this
// file works around) still proves the TLS re-dial path.
func TestTLS_ClientReconnectOverTLS(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	c := NewClient(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		TLS:      srv.ClientTLSConfig(),
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	assertTLSDialSucceededTokenFetchFailed(t, "Connect", err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel2()
	err2 := c.Reconnect(ctx2)
	assertTLSDialSucceededTokenFetchFailed(t, "Reconnect", err2)
}

// TestTLS_ClientPlainTCPAgainstTLSServerFails proves the negative path:
// a Client with TLS=nil cannot speak to a TLS-only listener. The dial may
// succeed (TCP three-way handshake completes), but the STOMP CONNECT frame
// will be parsed as TLS-record garbage by the server, and the client will
// see a closed conn or a timeout. Either way the result must NOT be a
// successful Connect.
func TestTLS_ClientPlainTCPAgainstTLSServerFails(t *testing.T) {
	srv := startTLSTestServer(t, false)
	defer srv.Stop()

	c := NewClient(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		// TLS intentionally nil: this is the negative test.
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("Connect with TLS=nil against TLS-only server: expected error, got nil")
	}
	// Soft substring check (Dutch M4): the plain-TCP client
	// should fail either at the STOMP-frame layer (the server reads TLS
	// record bytes as garbage and never sends CONNECTED) or via context
	// deadline; a bare assertion of "any non-nil error" cannot tell a
	// deliberate handshake mismatch apart from an unrelated flake, so we
	// log when the message doesn't obviously implicate one of those,
	// without failing the test over wording that can vary by Go version.
	msg := strings.ToLower(err.Error())
	if !errors.Is(err, context.DeadlineExceeded) &&
		!strings.Contains(msg, "eof") &&
		!strings.Contains(msg, "connect") &&
		!strings.Contains(msg, "closed") &&
		!strings.Contains(msg, "reset") {
		t.Logf("error did not obviously identify a STOMP-handshake or timeout failure (acceptable): %v", err)
	}
}

// TestTLS_ClientWrongCAFails proves that mTLS is actually validated: a
// client whose RootCAs does not include the server's CA must fail at the
// TLS handshake, not silently downgrade or proceed.
func TestTLS_ClientWrongCAFails(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	// Build a client TLS config with an empty RootCAs pool. The server's
	// cert is signed by a CA we do not trust, so verification must fail.
	tlsCfg := &tls.Config{
		Certificates: srv.ClientTLSConfig().Certificates,
		RootCAs:      x509.NewCertPool(),
		MinVersion:   tls.VersionTLS12,
		ServerName:   "127.0.0.1",
	}

	c := NewClient(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		TLS:      tlsCfg,
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("Connect with wrong CA: expected handshake error, got nil")
	}
	// Sanity check: the error message should mention TLS or certificate;
	// we do not pin the exact wording because crypto/tls phrasing may vary
	// across Go versions.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "tls") &&
		!strings.Contains(msg, "certificate") &&
		!strings.Contains(msg, "x509") &&
		!strings.Contains(msg, "verify") &&
		!strings.Contains(msg, "authority") {
		t.Logf("error did not obviously identify TLS layer (acceptable): %v", err)
	}
}

// TestTLS_ClientMissingClientCertFails proves the mTLS path enforces the
// client cert: a Client with valid RootCAs but no client cert must fail
// the handshake when the server requires one.
func TestTLS_ClientMissingClientCertFails(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	tlsCfg := &tls.Config{
		// No Certificates: the client cannot satisfy the server's
		// RequireAndVerifyClientCert.
		RootCAs:    srv.ServerCAPool(),
		MinVersion: tls.VersionTLS12,
		ServerName: "127.0.0.1",
	}

	c := NewClient(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		TLS:      tlsCfg,
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("Connect without client cert against mTLS server: expected error, got nil")
	}
}

// TestTLS_PublisherConnectsOverTLS mirrors the Client positive path for
// Publisher.Connect, and additionally exercises Publisher.Close on the
// happy path: a successful TLS Connect followed by Close must complete
// the STOMP DISCONNECT/RECEIPT round trip cleanly rather than only being
// covered indirectly via a deferred cleanup call whose result nothing
// checks (Dutch M5).
func TestTLS_PublisherConnectsOverTLS(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	p := New(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		TLS:      srv.ClientTLSConfig(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err != nil {
		// Publisher does not do a token bootstrap, so a successful TLS dial
		// followed by STOMP CONNECT should yield a nil error. Any error
		// here is a TLS/STOMP-layer failure and is fatal.
		t.Fatalf("Publisher.Connect over TLS: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Publisher.Close after successful TLS Connect: %v", err)
	}
}

// TestTLS_PublisherPlainTCPAgainstTLSServerFails mirrors the Client
// negative path for Publisher.Connect.
func TestTLS_PublisherPlainTCPAgainstTLSServerFails(t *testing.T) {
	srv := startTLSTestServer(t, false)
	defer srv.Stop()

	p := New(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		// TLS intentionally nil.
	})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.Connect(ctx)
	if err == nil {
		t.Fatal("Publisher.Connect with TLS=nil against TLS-only server: expected error, got nil")
	}
	// Soft substring check mirroring the Client negative test above
	// (Dutch M4).
	msg := strings.ToLower(err.Error())
	if !errors.Is(err, context.DeadlineExceeded) &&
		!strings.Contains(msg, "eof") &&
		!strings.Contains(msg, "connect") &&
		!strings.Contains(msg, "closed") &&
		!strings.Contains(msg, "reset") {
		t.Logf("error did not obviously identify a STOMP-handshake or timeout failure (acceptable): %v", err)
	}
}

// TestTLS_PublisherWrongCAFails mirrors the Client wrong-CA negative test
// for Publisher.
func TestTLS_PublisherWrongCAFails(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	tlsCfg := &tls.Config{
		Certificates: srv.ClientTLSConfig().Certificates,
		RootCAs:      x509.NewCertPool(),
		MinVersion:   tls.VersionTLS12,
		ServerName:   "127.0.0.1",
	}

	p := New(STOMPConfig{
		Address:  srv.Addr(),
		User:     "system",
		Password: "manager",
		TLS:      tlsCfg,
	})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err == nil {
		t.Fatal("Publisher.Connect with wrong CA: expected handshake error, got nil")
	}
}
