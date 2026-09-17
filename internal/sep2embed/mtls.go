package sep2embed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// errObserverRequiresGCM is returned by New when Config.Observer is set
// together with Config.EnableCCM. core's forked gotls listener (the CCM-8
// path) has its own, separate VerifyPeerCertificate/VerifyConnection
// plumbing (pkg/sep2tls/gotls), and this package's handshake-observation
// wrapper below only reconstructs the GCM/default listener core's
// sep2srv.New would otherwise build. Rather than silently skip handshake
// observation under CCM (an invisible gap), New fails closed and says so.
var errObserverRequiresGCM = errors.New("sep2embed: Config.Observer is not supported with Config.EnableCCM (the CCM-8 listener has no handshake-observation seam yet)")

// observedMTLSServer is a drop-in replacement for *sep2srv.Server (it
// satisfies the protocolServer interface embed.go defines) used only
// when a *connobs.Hook is supplied via Config.Observer: it builds the
// SAME mTLS tls.Config core's own sep2srv.New would build for the
// GCM/default path (via the same exported
// sepTLS.NewServerTLSConfigWithExtraCAs call, with the same cert/key/CA
// inputs), but wraps VerifyPeerCertificate to additively RECORD each
// connection attempt's accept/reject verdict, reason, and LFDI-match
// into hook before returning the verifier's own real result unchanged.
//
// Why this exists: core's pkg/sep2srv exposes no seam from outside the
// package for observing the mTLS handshake (no VerifyConnection or
// VerifyPeerCertificate hook on Options, no way to inject a pre-built
// tls.Config or net.Listener; wrapMTLS, which does the real
// construction, is unexported). Reproducing the identical GCM
// construction here, then layering an additive wrapper on top, is the
// only way to add handshake observation without a change to core. It
// deliberately reuses core's exported building blocks
// (sepTLS.NewServerTLSConfigWithExtraCAs for the tls.Config,
// sepTLS.LFDI/SFDI for identity derivation, and sep2srv's exported
// Default* timeout constants for the http.Server) rather than
// reimplementing any of their internal logic, so this stays a thin
// observation layer, not a fork.
type observedMTLSServer struct {
	identity        sep2srv.Identity
	listener        net.Listener
	httpSrv         *http.Server
	shutdownTimeout time.Duration
}

// newObservedMTLSListener builds the GCM/default mTLS listener with an
// additive VerifyPeerCertificate wrapper. innerVerify (the verifier
// sepTLS.NewServerTLSConfigWithExtraCAs already wires onto the returned
// tls.Config) is captured and called FIRST on every connection attempt;
// its return value is what actually decides accept or reject, and is
// also what this function returns unchanged. The wrapper only ever
// observes that decision after the fact: it can log an accept as a
// reject or vice versa, it cannot flip which one actually happens.
//
// reg is consulted (LFDI lookup only, never mutated) to populate each
// attempt's Known field: whether the presented certificate's LFDI
// matches an entry already in the bridge's mRID-to-LFDI registry. reg
// may be nil, in which case Known is always false.
func newObservedMTLSListener(addr, certFile, keyFile, caFile string, extraClientCAs []string, hook *connobs.Hook, reg *registry.Registry) (net.Listener, sep2srv.Identity, error) {
	tlsCfg, err := sepTLS.NewServerTLSConfigWithExtraCAs(certFile, keyFile, caFile, extraClientCAs)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: TLS config: %w", err)
	}

	innerVerify := tlsCfg.VerifyPeerCertificate
	if innerVerify == nil {
		return nil, sep2srv.Identity{}, errors.New("sep2embed: TLS config has no VerifyPeerCertificate to wrap (core API changed?)")
	}

	// GetConfigForClient is the only seam crypto/tls exposes with access
	// to the underlying net.Conn before certificate verification runs:
	// VerifyPeerCertificate itself receives only the raw certificate
	// bytes, never the connection, so there is no way to read
	// conn.RemoteAddr() from inside it directly. GetConfigForClient is
	// called once per incoming connection, after the ClientHello, and
	// its ClientHelloInfo carries .Conn; returning a per-connection
	// clone of tlsCfg whose VerifyPeerCertificate closure
	// (newRecordingVerifier below) has this connection's remote address
	// baked in is the standard way to thread that address through to the
	// verifier without altering verification itself: newRecordingVerifier
	// still calls innerVerify FIRST, unconditionally, and returns exactly
	// what it returns.
	tlsCfg.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		var remoteAddr string
		if chi.Conn != nil {
			remoteAddr = chi.Conn.RemoteAddr().String()
		}
		perConn := tlsCfg.Clone()
		perConn.GetConfigForClient = nil // must not recurse
		perConn.VerifyPeerCertificate = newRecordingVerifier(innerVerify, hook, reg, remoteAddr)
		return perConn, nil
	}

	if len(tlsCfg.Certificates) == 0 {
		return nil, sep2srv.Identity{}, errors.New("sep2embed: TLS config has no server certificate")
	}
	identity, err := deriveServerIdentity(tlsCfg.Certificates[0].Certificate)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: derive server identity: %w", err)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: listen: %w", err)
	}

	return tls.NewListener(listener, tlsCfg), identity, nil
}

// newRecordingVerifier builds the additive VerifyPeerCertificate closure
// for exactly one connection attempt: it calls innerVerify FIRST,
// unconditionally, and returns exactly what innerVerify returns; nothing
// in this function can change accept to reject or reject to accept.
// remoteAddr is baked in via closure capture (the caller derives it from
// that one connection's ClientHelloInfo.Conn; see
// newObservedMTLSListener's GetConfigForClient wiring) so every attempt
// this closure records carries the real peer address.
//
// reg is consulted (LFDI lookup only, never mutated) to populate the
// recorded attempt's Known field. reg may be nil, in which case Known is
// always false: this is DISTINCT from a presented certificate whose LFDI
// is simply absent from a non-nil registry (Known false there means "not
// this device", Known false here means "no registry was even
// consulted"); both surface as Known == false on the recorded attempt,
// but the caller can tell them apart because reg == nil is a config
// choice, not a lookup result.
func newRecordingVerifier(innerVerify func([][]byte, [][]*x509.Certificate) error, hook *connobs.Hook, reg *registry.Registry, remoteAddr string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		verifyErr := innerVerify(rawCerts, verifiedChains)

		attempt := connobs.HandshakeAttempt{Accepted: verifyErr == nil, RemoteAddr: remoteAddr}
		if verifyErr != nil {
			attempt.Reason = verifyErr.Error()
		}
		if len(rawCerts) > 0 {
			if leaf, parseErr := x509.ParseCertificate(rawCerts[0]); parseErr == nil {
				attempt.LFDI = sepTLS.LFDI(leaf)
				if reg != nil {
					_, attempt.Known = reg.MRID(attempt.LFDI)
				}
			}
			// A leaf that fails to parse leaves attempt.LFDI empty and
			// attempt.Known false: there is no identity to report, and
			// verifyErr (from innerVerify, which parses the same bytes
			// and would itself have already failed) carries the real
			// reason.
		}
		hook.RecordHandshake(attempt)

		return verifyErr
	}
}

// deriveServerIdentity parses the leaf certificate from a raw DER chain
// and returns the server SFDI/LFDI. This mirrors sep2srv's own
// unexported deriveIdentity exactly (same sepTLS.SFDI/LFDI calls over the
// same rawChain[0] leaf): reproduced here because core does not export
// it, and this package needs the identical Identity value sep2srv.New
// would have produced for the same server leaf certificate.
func deriveServerIdentity(rawChain [][]byte) (sep2srv.Identity, error) {
	if len(rawChain) == 0 {
		return sep2srv.Identity{}, errors.New("empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(rawChain[0])
	if err != nil {
		return sep2srv.Identity{}, fmt.Errorf("parse server leaf: %w", err)
	}
	return sep2srv.Identity{
		SFDI: sepTLS.SFDI(leaf),
		LFDI: sepTLS.LFDI(leaf),
	}, nil
}

// Addr implements protocolServer.
func (s *observedMTLSServer) Addr() string {
	return s.listener.Addr().String()
}

// Run implements protocolServer. It mirrors sep2srv.Server.Run's
// shutdown contract exactly, on both of its two exit paths: a clean
// ctx-triggered shutdown (the ctx.Done branch below) returns nil, and
// the listener goroutine always exits before Run returns on every path.
// The OTHER exit path, Serve returning on its own via errCh (e.g.
// something outside this Run call closed the listener or called
// s.httpSrv.Close/Shutdown without ctx ever being cancelled), returns
// that error UNCHANGED, including a bare http.ErrServerClosed: this is
// not "shutdown failed", it is Run faithfully reporting that Serve
// exited via that path rather than the ctx-driven one, exactly as
// sep2srv.Server.Run's own errCh branch does. Callers that only ever
// drive shutdown via ctx cancellation (the only path this package
// itself uses) will never observe this branch's return value at all.
func (s *observedMTLSServer) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.httpSrv.Serve(s.listener) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			<-errCh
			return fmt.Errorf("sep2embed: shutdown: %w", err)
		}
		<-errCh // discard the expected http.ErrServerClosed
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("sep2embed: serve: %w", err)
		}
		return err
	}
}
