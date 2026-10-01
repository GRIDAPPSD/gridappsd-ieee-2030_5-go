// Package adminui serves the bridge's admin listener: the server's admin
// UI and admin API (pkg/sep2adminplane) as the root handler, with the
// bridge's own read-only views registered as six gridappsd-* panels that
// the server's shell renders after its own tabs.
//
// Two bridge JSON routes, /api/health and /api/clients, stay beside the
// plane under the bridge's own Bearer, Host and GET-only gates, because
// the mTLS conformance harness reads them.
//
// The listener is off by default: New returns ErrDisabled when Config.Key
// is blank or shorter than sep2adminplane.MinAdminKeyLength, so cmd/bridge
// opens no listener rather than serving an unauthenticated or weakly
// keyed admin plane.
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

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2adminplane"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// ErrDisabled is returned by New when Config.Key is blank or too short
// for the admin plane. It is the fail closed "admin UI is off" state:
// cmd/bridge treats it as "do not start a runner", not as a startup
// failure. A short key wraps the plane's own refusal as well.
var ErrDisabled = errors.New("adminui: SEP2_ADMIN_UI_KEY unset or too short, admin UI disabled")

// defaultShutdownTimeout bounds Run's graceful drain after ctx is
// cancelled, mirroring sep2embed's own shutdown timeout pattern.
const defaultShutdownTimeout = 5 * time.Second

// HTTP server timeouts. Every one is bounded: an http.Server with no
// timeouts is exposed to a slow client holding a connection open, which
// matters once Config.AllowNonLoopback binds somewhere reachable.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 10 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// Config configures a Server.
type Config struct {
	// Addr is the "host:port" the admin HTTP listener binds. Required.
	// A loopback Addr is required unless AllowNonLoopback is set.
	Addr string

	// AllowNonLoopback must be explicitly set to bind Addr to a
	// non-loopback host. Without it, New refuses to start when Addr's
	// host is not loopback (127.0.0.1, ::1, localhost).
	AllowNonLoopback bool

	// Key is the admin credential: the Bearer token and the plane's login
	// password. Blank or shorter than sep2adminplane.MinAdminKeyLength
	// disables the admin UI (ErrDisabled).
	Key string

	// AllowedHosts are Host header values accepted beyond the loopback
	// names (localhost, 127.0.0.1, ::1), by the plane and by the bridge
	// JSON routes alike.
	AllowedHosts []string

	// FeederMRID and SimulationID are the bridge's configured values,
	// shown as-is. Empty means unconfigured, not an error.
	FeederMRID   string
	SimulationID string

	// SORLink is an optional, operator supplied URL to a server of record
	// dashboard (SEP2_ADMIN_UI_SOR_LINK). It is not a secret.
	SORLink string

	// ObservationDisabled is true when the connection observer is not
	// wired to the listener (SEP2_ENABLE_CCM), so an empty client
	// snapshot can be told apart from "nothing connected yet".
	ObservationDisabled bool
}

// RegistrySource is the read surface Server needs from *registry.Registry.
type RegistrySource interface {
	Snapshot() []registry.Entry
}

// EndDeviceSource is the read surface Server needs from *sep2embed.Embed
// for the served EndDevice and DER views.
type EndDeviceSource interface {
	EndDevices(ctx context.Context) ([]sep2embed.EndDeviceSnapshot, error)
}

// DERProgramSource is the read surface Server needs from *sep2embed.Embed
// for the served DERProgram view.
type DERProgramSource interface {
	DERPrograms(ctx context.Context, edevID string) ([]sep2embed.DERProgramSnapshot, error)
}

// ControlFlowSource is the read surface Server needs from *controlobs.Hook.
type ControlFlowSource interface {
	Snapshot() controlobs.Snapshot
}

// IdentitySource is the read surface Server needs from *sep2embed.Embed
// for the health view's server identity and mTLS listener address.
type IdentitySource interface {
	Identity() sep2srv.Identity
	Addr() string
}

// StompSource is the read surface Server needs from the message bus.
type StompSource interface {
	IsConnected() bool
}

// ClientObserverSource is the read surface Server needs from
// *connobs.Hook: per-LFDI request activity and the mTLS handshake log.
type ClientObserverSource interface {
	Snapshot() connobs.Snapshot
}

// ProtocolSource is what the admin plane needs from *sep2embed.Embed: the
// store set and notifier the protocol listener serves, so an admin read
// sees what a device sees and an admin write notifies its subscribers.
type ProtocolSource interface {
	Stores() *assembly.Stores
	Notifier() assembly.ResourceNotifier
}

// Sources are the readers New builds the listener from. Every field is
// required.
type Sources struct {
	Registry RegistrySource
	Devices  EndDeviceSource
	Programs DERProgramSource
	Flow     ControlFlowSource
	Identity IdentitySource
	Stomp    StompSource
	Clients  ClientObserverSource
	Protocol ProtocolSource
}

// Server is the admin listener: a bound, not yet serving listener and
// the handler built from Sources. Construct with New; serve with Run.
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

	// planePatterns are the routes the admin plane mounted, as
	// sep2adminplane.Plane.Patterns reports them.
	planePatterns []string
}

// New validates cfg, builds the admin plane with the bridge's panels, and
// binds the listener. It does NOT start serving; call Run to do that.
//
// It returns ErrDisabled, not a general error, for a blank or short key.
// A non-loopback Addr without AllowNonLoopback is refused before any
// socket opens.
func New(cfg Config, src Sources) (*Server, error) {
	if cfg.Key == "" {
		return nil, ErrDisabled
	}
	if cfg.Addr == "" {
		return nil, errors.New("adminui: Config.Addr is required")
	}
	if src.Registry == nil || src.Devices == nil || src.Programs == nil || src.Flow == nil ||
		src.Identity == nil || src.Stomp == nil || src.Clients == nil || src.Protocol == nil {
		return nil, errors.New("adminui: every Sources field is required")
	}

	loopback, err := isLoopbackHost(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("adminui: %w", err)
	}
	if !loopback && !cfg.AllowNonLoopback {
		return nil, fmt.Errorf("adminui: Addr %q is not loopback; set Config.AllowNonLoopback to bind a non-loopback address", cfg.Addr)
	}

	s := &Server{
		cfg:       cfg,
		registry:  src.Registry,
		devices:   src.Devices,
		programs:  src.Programs,
		flow:      src.Flow,
		identity:  src.Identity,
		stomp:     src.Stomp,
		clients:   src.Clients,
		startedAt: time.Now(),
	}

	plane, err := sep2adminplane.New(sep2adminplane.Config{
		Stores:       src.Protocol.Stores(),
		AdminKey:     cfg.Key,
		AllowedHosts: s.allowedHosts(),
		Notifier:     src.Protocol.Notifier(),
		// The bridge has always required a credential on loopback, and it
		// seeds and writes these stores itself: an admin write (an FSA
		// assignment, a new EndDevice, a DER control) would be a second
		// writer behind its back, so the plane mounts no write route.
		LoopbackBypass: false,
		ControlWrites:  false,
		ReadOnly:       true,
		Panels:         s.panels(),
	})
	switch {
	case errors.Is(err, sep2adminplane.ErrNoCredential), errors.Is(err, sep2adminplane.ErrShortCredential):
		return nil, fmt.Errorf("%w: %w", ErrDisabled, err)
	case err != nil:
		return nil, fmt.Errorf("adminui: %w", err)
	}

	if !loopback {
		log.Printf("adminui: WARNING binding non-loopback address %q; the admin UI will be reachable from outside this host", cfg.Addr)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("adminui: listen: %w", err)
	}
	s.ln = ln
	s.planePatterns = plane.Patterns()
	s.handler = s.buildHandler(plane.Handler())
	return s, nil
}

// allowedHosts is the loopback names plus Config.AllowedHosts, blanks
// dropped: the plane refuses a blank entry outright.
func (s *Server) allowedHosts() []string {
	hosts := append([]string(nil), defaultAllowedHosts...)
	for _, h := range s.cfg.AllowedHosts {
		if strings.TrimSpace(h) != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// Addr returns the listener's bound address, useful when Config.Addr
// used a ":0" port.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Handler returns the full handler with no listener involved, for tests
// that drive it through httptest.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Run serves on the listener bound by New until ctx is cancelled, then
// shuts down within defaultShutdownTimeout. A graceful shutdown returns
// nil; a serve failure independent of ctx, or a shutdown that does not
// finish in time, returns an error, matching cmd/bridge's runner contract.
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
