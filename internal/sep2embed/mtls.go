package sep2embed

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	gotls "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls/gotls"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// errObserverRequiresGCM is returned by New when Config.Observer is set
// together with Config.EnableCCM.
//
// The technical reason this error used to describe (the CCM-8 path had no
// handshake-observation seam) no longer holds: core v0.20.0 dropped the
// GCM/default *tls.Config constructor entirely, so both listener builders
// below now build a *gotls.Config from the same core call
// (buildCCMServerConfig), and newObservedMTLSListener's wrapping works on
// either. The refusal is kept for now as configuration surface only,
// pending a follow-up that retires Config.EnableCCM (and cmd/bridge's
// SEP2_ENABLE_CCM/SEP2_CCM_ALLOW_NO_OBSERVER) now that setting it changes
// nothing observable.
var errObserverRequiresGCM = errors.New("sep2embed: Config.Observer is not supported with Config.EnableCCM (kept as configuration surface pending removal; see buildCCMServerConfig)")

// observedMTLSServer is a drop-in replacement for *sep2srv.Server (it
// satisfies the protocolServer interface embed.go defines). Built by
// newObservedMTLSListener below when Config.Observer is set: it builds the
// SAME CCM-8 gotls.Config core's own sep2srv.New would build (via
// buildCCMServerConfig, the shared call both this file's listener
// constructors now use), but wraps VerifyPeerCertificate to additively
// RECORD each connection attempt's accept/reject verdict, reason, and
// LFDI-match into hook before returning the verifier's own real result
// unchanged. The CCM-only path (newCCMOnlyListener) reuses this same
// struct as a plain listener/httpSrv container: no hook, no recording
// wrapper.
//
// Why this exists: core's pkg/sep2srv exposes no seam from outside the
// package for observing the mTLS handshake (no VerifyConnection or
// VerifyPeerCertificate hook on Options, no way to inject a pre-built
// gotls.Config or net.Listener; wrapMTLS, which does the real
// construction, is unexported). Reproducing the identical CCM
// construction here, then layering an additive wrapper on top, is the
// only way to add handshake observation without a change to core. It
// deliberately reuses core's exported building blocks
// (sepTLS.NewCCMServerConfigWithExtraCAs for the gotls.Config,
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

// buildCCMServerConfig builds and validates the gotls.Config shared by
// newObservedMTLSListener and newCCMOnlyListener. Core's own
// NewCCMServerConfigWithExtraCAs already narrows CipherSuites to CCM-8
// only (ccmserver.go): since core v0.20.0 dropped the separate
// GCM/default *tls.Config constructor, both listeners in this file now
// build the identical base config and differ only in whether
// VerifyPeerCertificate is wrapped for observation.
func buildCCMServerConfig(certFile, keyFile, caFile string, extraClientCAs []string) (*gotls.Config, error) {
	cfg, err := sepTLS.NewCCMServerConfigWithExtraCAs(certFile, keyFile, caFile, extraClientCAs)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: CCM TLS config: %w", err)
	}
	if err := requireCCMVerification(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// newObservedMTLSListener builds the CCM-8 mTLS listener with an
// additive VerifyPeerCertificate wrapper. innerVerify (the verifier
// buildCCMServerConfig already wires onto the returned gotls.Config) is
// captured and called FIRST on every connection attempt; its return
// value is what actually decides accept or reject, and is also what this
// function returns unchanged. The wrapper only ever observes that
// decision after the fact: it can log an accept as a reject or vice
// versa, it cannot flip which one actually happens.
//
// reg is consulted (LFDI lookup only, never mutated) to populate each
// attempt's Known field: whether the presented certificate's LFDI
// matches an entry already in the bridge's mRID-to-LFDI registry. reg
// may be nil, in which case Known is always false.
func newObservedMTLSListener(addr, certFile, keyFile, caFile string, extraClientCAs []string, hook *connobs.Hook, reg *registry.Registry, handshakeTimeout time.Duration) (net.Listener, sep2srv.Identity, error) {
	cfg, err := buildCCMServerConfig(certFile, keyFile, caFile, extraClientCAs)
	if err != nil {
		return nil, sep2srv.Identity{}, err
	}

	innerVerify := cfg.VerifyPeerCertificate

	// GetConfigForClient is the only seam gotls exposes (mirroring
	// stdlib crypto/tls) with access to the underlying net.Conn before
	// certificate verification runs: VerifyPeerCertificate itself
	// receives only the raw certificate bytes, never the connection, so
	// there is no way to read conn.RemoteAddr() from inside it directly.
	// GetConfigForClient is called once per incoming connection, after
	// the ClientHello, and its ClientHelloInfo carries .Conn; returning a
	// per-connection clone of cfg whose VerifyPeerCertificate closure
	// (newRecordingVerifier below) has this connection's remote address
	// baked in is the standard way to thread that address through to the
	// verifier without altering verification itself: newRecordingVerifier
	// still calls innerVerify FIRST, unconditionally, and returns exactly
	// what it returns.
	cfg.GetConfigForClient = func(chi *gotls.ClientHelloInfo) (*gotls.Config, error) {
		var remoteAddr string
		if chi.Conn != nil {
			remoteAddr = chi.Conn.RemoteAddr().String()
		}
		return newPerConnectionConfig(cfg, innerVerify, hook, reg, remoteAddr), nil
	}

	if len(cfg.Certificates) == 0 {
		return nil, sep2srv.Identity{}, errors.New("sep2embed: TLS config has no server certificate")
	}
	identity, err := deriveServerIdentity(cfg.Certificates[0].Certificate)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: derive server identity: %w", err)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: listen: %w", err)
	}

	// WrapCCMListener forces the handshake eagerly and logs a failure the
	// way net/http logs one for *tls.Conn; that case never fires for the
	// forked *gotls.Conn type net.Listen/gotls.NewListener returns on
	// their own. A nil errorLog logs through the standard logger,
	// matching this package's unset http.Server.ErrorLog on this path.
	return wrapCCM(listener, cfg, handshakeTimeout, identity)
}

// newPerConnectionConfig builds the *gotls.Config newObservedMTLSListener's
// GetConfigForClient returns for one connection: base cloned with
// GetConfigForClient cleared (must not recurse) and VerifyPeerCertificate
// replaced by a recording wrapper. Split out as its own function so a test
// can assert, directly and without a network dial, that every OTHER field
// on the clone (CipherSuites included) is untouched: the CCM-8 exclusivity
// this guards is a property of the whole suite set copied from base, not
// of any one suite a future edit here happens to add or drop, so the test
// checks equality with base rather than enumerating suites.
func newPerConnectionConfig(base *gotls.Config, innerVerify func([][]byte, [][]*x509.Certificate) error, hook *connobs.Hook, reg *registry.Registry, remoteAddr string) *gotls.Config {
	perConn := base.Clone()
	perConn.GetConfigForClient = nil // must not recurse
	perConn.VerifyPeerCertificate = newRecordingVerifier(innerVerify, hook, reg, remoteAddr)
	return perConn
}

// newCCMOnlyListener builds a CCM-8-only mTLS listener with no
// handshake-observation wrapper: the plain sibling of
// newObservedMTLSListener, sharing the same buildCCMServerConfig base
// and the same WrapCCMListener/gotls.NewListener wiring for a refused
// handshake to reach the process log the way net/http logs one for
// *tls.Conn (mirrors server-go's own wrapMTLS, pkg/sep2srv/server.go).
//
// Identity is derived the same way newObservedMTLSListener's does
// (deriveServerIdentity), since sep2srv's own deriveIdentity is
// unexported. The caller must additionally wire
// sepTLS.SetupCCMServer(httpSrv) and sepTLS.CCMIdentityMiddleware
// (outermost), matching sep2srv.New's own CCM wiring in server.go, since
// the standard identity middleware reads r.TLS, which crypto/tls
// populates automatically but the gotls fork does not.
func newCCMOnlyListener(addr, certFile, keyFile, caFile string, extraClientCAs []string, handshakeTimeout time.Duration) (net.Listener, sep2srv.Identity, error) {
	cfg, err := buildCCMServerConfig(certFile, keyFile, caFile, extraClientCAs)
	if err != nil {
		return nil, sep2srv.Identity{}, err
	}

	if len(cfg.Certificates) == 0 {
		return nil, sep2srv.Identity{}, errors.New("sep2embed: CCM TLS config has no server certificate")
	}
	identity, err := deriveServerIdentity(cfg.Certificates[0].Certificate)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: derive server identity (CCM): %w", err)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: listen: %w", err)
	}

	// nil errorLog here must stay in sync with newObservedMTLSListener's
	// identical nil above: both log through the standard logger, matching
	// this package's unset http.Server.ErrorLog on both paths.
	return wrapCCM(listener, cfg, handshakeTimeout, identity)
}

// requireCCMVerification refuses a CCM config that no longer enforces
// client-certificate verification, or that no longer restricts the
// listener to CCM-8 exclusively. Unlike newObservedMTLSListener above,
// which only WRAPS an existing VerifyPeerCertificate it does not own,
// buildCCMServerConfig's caller mutates nothing but must not trust a
// *gotls.Config another module builds (NewCCMServerConfigWithExtraCAs)
// blindly: this guards against a future core change weakening ClientAuth,
// dropping VerifyPeerCertificate, or widening CipherSuites, so it cannot
// silently serve an unverified or GCM-reachable listener.
//
// The CipherSuites check runs once, here, against the base config
// buildCCMServerConfig returns; it does NOT reach a mutation to the
// per-connection clone newPerConnectionConfig builds inside
// newObservedMTLSListener's GetConfigForClient, since that clone is made
// fresh per connection, after this check has already run. That gap is
// covered separately (newPerConnectionConfig's own equality-with-base
// test), not by construction-time validation alone.
func requireCCMVerification(cfg *gotls.Config) error {
	if cfg.VerifyPeerCertificate == nil {
		return errors.New("sep2embed: CCM TLS config has no VerifyPeerCertificate (core API changed?)")
	}
	if cfg.ClientAuth != gotls.RequireAnyClientCert {
		return fmt.Errorf("sep2embed: CCM TLS config ClientAuth = %v, want RequireAnyClientCert (core API changed?)", cfg.ClientAuth)
	}
	wantSuites := []uint16{gotls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8}
	if !slices.Equal(cfg.CipherSuites, wantSuites) {
		return fmt.Errorf("sep2embed: CCM TLS config CipherSuites = %#04x, want %#04x only (core API changed?)", cfg.CipherSuites, wantSuites)
	}
	return nil
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
		// hook is never nil from this function's one caller today
		// (newObservedMTLSListener only builds this closure when
		// Config.Observer, threaded through as hook, is non-nil), but a
		// method call on a nil *Hook would panic inside RecordHandshake's
		// own mutex lock; guarding here treats hook the same as the
		// already-nil-safe reg above rather than trusting a caller
		// invariant this function cannot enforce.
		if hook != nil {
			hook.RecordHandshake(attempt)
		}

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

// wrapCCMListener is a seam: the core listener records its handshake
// bound in a field only core's own tests can read.
var wrapCCMListener = sepTLS.WrapCCMListenerWithTimeout

// wrapCCM puts the eager-handshake wrapper on a TLS listener. Zero
// handshakeTimeout keeps core's default; a negative one is refused and the
// listener closed.
func wrapCCM(listener net.Listener, cfg *gotls.Config, handshakeTimeout time.Duration, identity sep2srv.Identity) (net.Listener, sep2srv.Identity, error) {
	l, err := wrapCCMListener(gotls.NewListener(listener, cfg), nil, handshakeTimeout)
	if err != nil {
		_ = listener.Close()
		return nil, sep2srv.Identity{}, fmt.Errorf("sep2embed: %w", err)
	}
	return l, identity, nil
}
