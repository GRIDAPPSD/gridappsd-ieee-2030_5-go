// Package adminui is the bridge's read only operator HTTP API (GAGO-058,
// GAGO-059). It serves a small set of GET only, Bearer gated JSON
// endpoints over the bridge's own in process state (the registry, the
// embedded IEEE 2030.5 server's snapshot accessors, and the GAGO-057
// control flow observation hook). It never mutates bridge state: every
// handler is a pure reader, and the mux this package builds rejects any
// non GET method before a handler ever runs.
//
// This server is off by default: New returns ErrDisabled when
// Config.Key is empty, so cmd/bridge only starts a listener when an
// operator has explicitly set SEP2_ADMIN_UI_KEY. This is deliberate fail
// closed behavior, not an oversight: an admin UI with no token
// configured must never silently serve on an unauthenticated port.
//
// The intended consumer is a future embedded SPA (built separately);
// this package supplies only the JSON read layer it will call.
package adminui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// ErrDisabled is returned by New when Config.Key is empty. This is the
// fail closed "admin UI is off" state: cmd/bridge should treat this
// error as "do not start a runner", not as a startup failure.
var ErrDisabled = errors.New("adminui: SEP2_ADMIN_UI_KEY unset, admin UI disabled")

// defaultShutdownTimeout bounds Run's graceful drain after ctx is
// cancelled, mirroring sep2embed's own shutdown timeout pattern.
const defaultShutdownTimeout = 5 * time.Second

// HTTP server timeouts. This is a read only, low traffic local API, so
// the values are generous rather than tight, but every one of them is
// still bounded: an http.Server with no timeouts set is exposed to a
// slow client (deliberate or not) holding a connection open
// indefinitely, which matters once Config.AllowNonLoopback lets this
// server bind somewhere reachable off the local host.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 10 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// Config configures a Server.
type Config struct {
	// Addr is the "host:port" the admin HTTP listener binds. Required.
	// The caller (cmd/bridge) is responsible for supplying the
	// SEP2_ADMIN_UI_ADDR default (127.0.0.1:8444); this package makes no
	// assumption about what "the default" is, only that a loopback Addr
	// is required unless AllowNonLoopback is set.
	Addr string

	// AllowNonLoopback must be explicitly set to bind Addr to a
	// non-loopback host. Without it, New refuses to start (fail
	// closed) when Addr's host is not recognized as loopback
	// (127.0.0.1, ::1, localhost). This is the "explicit opt-in for
	// non-loopback" posture requirement.
	AllowNonLoopback bool

	// Key is the Bearer token every request must present in its
	// Authorization header. Empty means the admin UI is disabled: New
	// returns ErrDisabled rather than starting an unauthenticated
	// listener.
	Key string

	// AllowedHosts is an additional set of Host header values accepted
	// by the host allowlist middleware, beyond the built in defaults
	// (localhost, 127.0.0.1, ::1). Comparison is case insensitive and
	// ignores any port suffix on the incoming Host header.
	AllowedHosts []string

	// FeederMRID is the CIM feeder mRID the bridge was configured to
	// enumerate DERs from (config.FeederMRID). Plain display data: not
	// validated or defaulted by this package. Empty means the caller
	// has none configured.
	FeederMRID string

	// SimulationID is the GridAPPS-D simulation_id the bridge was
	// configured with (config.SimulationID). Plain display data, same
	// posture as FeederMRID. Empty means no simulation is configured.
	SimulationID string

	// SORLink is an optional, operator supplied URL to a server of
	// record dashboard for this bridge (GAGO-075, SEP2_ADMIN_UI_SOR_LINK).
	// It is a URL, not a secret, and is safe to expose over /api/health
	// unlike Key. Empty means unset: no link, no error.
	SORLink string
}

// RegistrySource is the minimal read surface Server needs from
// *registry.Registry. Defined at the consumer per the workspace Go
// standard (interfaces belong where they are used), so handlers_test.go
// can exercise the registry endpoint against a fake with no real
// registry state.
type RegistrySource interface {
	Snapshot() []registry.Entry
}

// EndDeviceSource is the minimal read surface Server needs from
// *sep2embed.Embed for the served-EndDevice and DER endpoints.
type EndDeviceSource interface {
	EndDevices(ctx context.Context) ([]sep2embed.EndDeviceSnapshot, error)
}

// DERProgramSource is the minimal read surface Server needs from
// *sep2embed.Embed for the served-DERProgram endpoint.
type DERProgramSource interface {
	DERPrograms(ctx context.Context, edevID string) ([]sep2embed.DERProgramSnapshot, error)
}

// ControlFlowSource is the minimal read surface Server needs from
// *controlobs.Hook for the control flow endpoint.
type ControlFlowSource interface {
	Snapshot() controlobs.Snapshot
}

// IdentitySource is the minimal read surface Server needs from
// *sep2embed.Embed for the health endpoint's server identity and mTLS
// listener address fields (GAGO-074). *sep2embed.Embed already exposes
// both methods publicly, so this interface needs no changes on that
// side; it exists here, at the consumer, per the workspace Go standard.
type IdentitySource interface {
	Identity() sep2srv.Identity
	Addr() string
}

// StompSource is the minimal read surface Server needs from
// fieldbus.MessageBus for the health endpoint's connectivity field
// (GAGO-074). Defined narrowly at the consumer rather than importing
// the full fieldbus.MessageBus interface at every call site.
type StompSource interface {
	IsConnected() bool
}

// ClientObserverSource is the minimal read surface Server needs from
// *connobs.Hook for the GAGO-091 /api/clients endpoint: the per-LFDI
// connection activity and mTLS handshake log GAGO-090 records.
type ClientObserverSource interface {
	Snapshot() connobs.Snapshot
}

// Server is the admin UI's HTTP server: a bound, not yet serving
// listener, plus the read only handler chain built from the injected
// sources above. Construct with New; start serving with Run.
type Server struct {
	cfg      Config
	registry RegistrySource
	devices  EndDeviceSource
	programs DERProgramSource
	flow     ControlFlowSource
	identity IdentitySource
	stomp    StompSource
	clients  ClientObserverSource

	startedAt time.Time

	ln      net.Listener
	handler http.Handler
}

// New validates cfg, binds the listener, and builds the read only
// handler chain. It does NOT start serving; call Run to do that.
//
// New returns ErrDisabled (not a general error) when cfg.Key is empty:
// callers should check for this specific sentinel with errors.Is and
// skip starting a runner entirely, rather than treating it as a
// startup failure.
//
// A non-loopback cfg.Addr without cfg.AllowNonLoopback is rejected here,
// before any socket is opened: fail closed on the loopback posture
// check, exactly as fail closed applies to the missing-token case.
func New(cfg Config, reg RegistrySource, devices EndDeviceSource, programs DERProgramSource, flow ControlFlowSource, identity IdentitySource, stomp StompSource, clients ClientObserverSource) (*Server, error) {
	if cfg.Key == "" {
		return nil, ErrDisabled
	}
	if cfg.Addr == "" {
		return nil, errors.New("adminui: Config.Addr is required")
	}
	if reg == nil || devices == nil || programs == nil || flow == nil || identity == nil || stomp == nil || clients == nil {
		return nil, errors.New("adminui: registry, devices, programs, flow, identity, stomp, and clients sources are all required")
	}

	loopback, err := isLoopbackHost(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("adminui: %w", err)
	}
	if !loopback && !cfg.AllowNonLoopback {
		return nil, fmt.Errorf("adminui: Addr %q is not loopback; set Config.AllowNonLoopback to bind a non-loopback address", cfg.Addr)
	}
	if !loopback && cfg.AllowNonLoopback {
		log.Printf("adminui: WARNING binding non-loopback address %q; the admin UI will be reachable from outside this host", cfg.Addr)
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("adminui: listen: %w", err)
	}

	s := &Server{
		cfg:       cfg,
		registry:  reg,
		devices:   devices,
		programs:  programs,
		flow:      flow,
		identity:  identity,
		stomp:     stomp,
		clients:   clients,
		startedAt: time.Now(),
		ln:        ln,
	}
	s.handler = s.buildHandler()
	return s, nil
}

// Addr returns the listener's actual bound address. Useful when
// Config.Addr used a ":0" style port and the caller needs the
// OS-assigned port, e.g. in tests.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Handler returns the fully built, middleware wrapped read only
// handler, with no listener involved. Tests exercise this directly via
// httptest, so the GAGO-059 endpoints are testable against injected
// fakes with no live broker and no live listener.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Run serves on the listener bound by New until ctx is cancelled, then
// shuts down gracefully within defaultShutdownTimeout. A graceful,
// ctx-driven shutdown returns nil: only a genuine listen/serve failure
// (independent of ctx cancellation) or a shutdown that does not
// complete within the timeout returns a non-nil error, matching the
// bridge's existing runner contract (see cmd/bridge's runEmbedAndStomp
// doc comment) so a future three-way combinator can treat this runner
// identically to embed.Run and stompRun.
func (s *Server) Run(ctx context.Context) error {
	httpSrv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(s.ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			<-serveErr
			return fmt.Errorf("adminui: shutdown: %w", err)
		}
		<-serveErr // discard the expected http.ErrServerClosed
		return nil
	case err := <-serveErr:
		return fmt.Errorf("adminui: serve: %w", err)
	}
}

// isLoopbackHost reports whether addr's host component (a "host:port"
// string) is recognized as loopback: literal "localhost", or an IP
// address for which net.IP.IsLoopback reports true. An empty host (the
// ":8444" shorthand, which net.Listen binds to ALL interfaces) is
// deliberately treated as NOT loopback: it is the opposite of
// loopback-only, even though it is easy to mistake for a safe default.
// An unrecognized, non-IP hostname is also treated as NOT loopback:
// this function fails closed rather than assuming a name it cannot
// verify resolves to loopback.
func isLoopbackHost(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("parse Addr %q: %w", addr, err)
	}
	if host == "" {
		return false, nil
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false, nil
	}
	return ip.IsLoopback(), nil
}
