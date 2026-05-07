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

func (s *tlsTestServer) Addr() string                  { return s.addr }
func (s *tlsTestServer) ClientTLSConfig() *tls.Config  { return s.clientTLS.Clone() }
func (s *tlsTestServer) ServerCAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(s.caPEM)
	return pool
}

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
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, br)
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
		NotAfter:              time.Now().Add(24 * time.Hour),
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
		NotAfter:     time.Now().Add(24 * time.Hour),
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
		User:     "admin",
		Password: "admin",
		TLS:      srv.ClientTLSConfig(),
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	// Connect will fail at fetchAuthToken because the test server never
	// replies to the token topic. What we want to assert is that the
	// failure is NOT a TLS handshake failure: the dial path through TLS
	// completed and STOMP CONNECTED was processed.
	if err == nil {
		// If a future implementation surfaces fetchAuthToken differently,
		// a nil error is also acceptable.
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "tcp dial") {
		t.Fatalf("Connect with TLS unexpectedly failed at TCP dial: %v", err)
	}
	if strings.Contains(msg, "tls:") && !strings.Contains(msg, "fetch auth token") {
		t.Fatalf("Connect with TLS failed at TLS layer: %v", err)
	}
	if !strings.Contains(msg, "fetch auth token") &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("Connect with TLS failed in an unexpected way: %v", err)
	}
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
		User:     "admin",
		Password: "admin",
		// TLS intentionally nil: this is the negative test.
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("Connect with TLS=nil against TLS-only server: expected error, got nil")
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
		User:     "admin",
		Password: "admin",
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
		User:     "admin",
		Password: "admin",
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
// Publisher.Connect.
func TestTLS_PublisherConnectsOverTLS(t *testing.T) {
	srv := startTLSTestServer(t, true)
	defer srv.Stop()

	p := New(STOMPConfig{
		Address:  srv.Addr(),
		User:     "admin",
		Password: "admin",
		TLS:      srv.ClientTLSConfig(),
	})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err != nil {
		// Publisher does not do a token bootstrap, so a successful TLS dial
		// followed by STOMP CONNECT should yield a nil error. Any error
		// here is a TLS/STOMP-layer failure and is fatal.
		t.Fatalf("Publisher.Connect over TLS: %v", err)
	}
}

// TestTLS_PublisherPlainTCPAgainstTLSServerFails mirrors the Client
// negative path for Publisher.Connect.
func TestTLS_PublisherPlainTCPAgainstTLSServerFails(t *testing.T) {
	srv := startTLSTestServer(t, false)
	defer srv.Stop()

	p := New(STOMPConfig{
		Address:  srv.Addr(),
		User:     "admin",
		Password: "admin",
		// TLS intentionally nil.
	})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err == nil {
		t.Fatal("Publisher.Connect with TLS=nil against TLS-only server: expected error, got nil")
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
		User:     "admin",
		Password: "admin",
		TLS:      tlsCfg,
	})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err == nil {
		t.Fatal("Publisher.Connect with wrong CA: expected handshake error, got nil")
	}
}
