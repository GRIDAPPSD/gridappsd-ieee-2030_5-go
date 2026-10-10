// Package adminui serves the bridge's admin listener: the server's admin
// UI and admin API (pkg/sep2adminplane) as the root handler, with the
// bridge's own views registered as gridappsd-* panels that the server's
// shell renders after its own tabs: five read-only views, the bus
// monitor when Sources.Monitor is set, and the bus sender when
// Sources.Sender is set.
//
// Three bridge JSON routes, /api/health, /api/clients and /api/registry,
// stay beside the plane under the bridge's own Bearer, Host and GET-only
// gates, because the mTLS conformance harness and the client config
// generator read them.
//
// The listener is off by default: New returns ErrDisabled when Config.Key
// is unset or blank, so cmd/bridge opens no listener rather than serving
// an unauthenticated admin plane. A key that is set but too short fails
// start instead. Config.InsecureNoKey is the explicit opt-out: the server
// holds a generated key and authenticates every request with it.
package adminui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2adminplane"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/activity"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sender"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// ErrDisabled is returned by New when Config.Key is unset or blank. It is
// the fail closed "admin UI is off" state: cmd/bridge treats it as "do not
// start a runner", not as a startup failure.
var ErrDisabled = errors.New("adminui: SEP2_ADMIN_UI_KEY unset or blank, admin UI disabled")

// timeouts bound the listener. Every one is set: an http.Server with no
// timeouts is exposed to a slow client holding a connection open, which
// matters once Config.AllowNonLoopback binds somewhere reachable. Tests
// shorten them.
type timeouts struct {
	readHeader, read, write, idle time.Duration
	// shutdown bounds Run's drain after ctx is cancelled.
	shutdown time.Duration
}

var defaultTimeouts = timeouts{
	readHeader: 5 * time.Second,
	read:       10 * time.Second,
	write:      10 * time.Second,
	idle:       60 * time.Second,
	shutdown:   5 * time.Second,
}

// DefaultClientIdleAfter is how long a client may go unseen before the
// panel and /api/clients call it idle. Five minutes is several poll
// periods of a typical 2030.5 client, so a quiet but live client is not
// flagged, while a stopped one clears within a few minutes.
const DefaultClientIdleAfter = 5 * time.Minute

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
	// password. Unset or blank disables the admin UI (ErrDisabled); set, it
	// must be at least sep2adminplane.MinAdminKeyLength characters.
	Key string

	// InsecureNoKey runs the UI with no credential for the operator
	// (SEP2_ADMIN_UI_INSECURE_NO_KEY). The server generates a random key,
	// never shown, and sets it as the Bearer on every request. Setting Key
	// as well is a start failure.
	InsecureNoKey bool

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

	// Settings are the server's admin-plane values, parsed once at startup
	// by cmd/bridge. Only the deadline and grace reach the plane, which
	// validates them; the read-only plane reads neither. Edition is always
	// 2018 here (loadConfig refuses 2023), and PEN is read only by the
	// plane's write routes, which the bridge does not mount.
	Settings sep2adminplane.Settings

	// ObservationDisabled is true when the connection observer is not
	// wired to the listener (SEP2_ENABLE_CCM), so an empty client
	// snapshot can be told apart from "nothing connected yet".
	ObservationDisabled bool

	// ReadHeaderTimeout, ReadTimeout, WriteTimeout, IdleTimeout and
	// ShutdownTimeout override the listener timeouts. Zero keeps the
	// default.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	// ClientIdleAfter is the age past which a client that was seen before
	// reads as idle instead of connected. Zero keeps
	// DefaultClientIdleAfter.
	ClientIdleAfter time.Duration
}

// DefaultTimeouts reports the listener timeouts used when Config leaves
// them zero, for callers that surface them as defaults.
func DefaultTimeouts() (readHeader, read, write, idle, shutdown time.Duration) {
	d := defaultTimeouts
	return d.readHeader, d.read, d.write, d.idle, d.shutdown
}

// withOverrides returns t with each non-zero cfg timeout applied.
func (t timeouts) withOverrides(cfg Config) timeouts {
	for _, o := range []struct {
		dst *time.Duration
		v   time.Duration
	}{
		{&t.readHeader, cfg.ReadHeaderTimeout},
		{&t.read, cfg.ReadTimeout},
		{&t.write, cfg.WriteTimeout},
		{&t.idle, cfg.IdleTimeout},
		{&t.shutdown, cfg.ShutdownTimeout},
	} {
		if o.v > 0 {
			*o.dst = o.v
		}
	}
	return t
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

// HistorySource is the read surface Server needs from
// *telemetryhistory.Store for the graph panels.
type HistorySource interface {
	Snapshot() []telemetryhistory.SeriesSnapshot
	Series(key telemetryhistory.SeriesKey) ([]telemetryhistory.Sample, bool)
	Evictions() uint64
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
	// History holds the device status samples, read by the graph panel.
	History HistorySource

	// Mirror, when set, serves the posted mirror readings as series at
	// /apps/soc/api/mirror. That route takes no credential by decision;
	// leaving Mirror nil leaves it unmounted.
	Mirror MirrorSource

	// Monitor and Sender are the optional fields: each, when set, adds its
	// panel.
	Monitor MonitorSource
	Sender  *sender.Sender
	// Control, when set, mounts the watts control routes.
	Control ControlSource

	// Activity is the recorder the protocol router was given in
	// assembly.RouterConfig.Activity. It feeds the Devices comms columns
	// and the last-seen, count and age that /api/clients reports. Nil
	// leaves the Devices tab at Unknown and /api/clients on the connobs
	// values.
	Activity *activity.Recorder
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
	activity *activity.Recorder
	history  HistorySource
	mirror   MirrorSource
	monitor  *monitorPanel
	sender   *senderPanel
	control  ControlSource
	// controlLimit bounds the watts control send route.
	controlLimit *tokenBucket
	// controls remembers the sends the status route answers for.
	controls *controlLedger

	// mirrorBuilds holds a slot for each mirror series build in flight.
	mirrorBuilds chan struct{}
	// outputBuilds is the same gate for the output series route.
	outputBuilds chan struct{}

	// idleAfter is Config.ClientIdleAfter with its default applied.
	idleAfter time.Duration

	startedAt time.Time
	// now is the clock the graph panel ages samples against.
	now      func() time.Time
	timeouts timeouts

	ln      net.Listener
	handler http.Handler
	plane   *sep2adminplane.Plane

	// planePatterns are the routes the admin plane mounted, as
	// sep2adminplane.Plane.Patterns reports them.
	planePatterns []string
}

// New validates cfg, builds the admin plane with the bridge's panels, and
// binds the listener. It does NOT start serving; call Run to do that.
//
// It returns ErrDisabled, not a general error, for an unset or blank key,
// and an error naming the length rule for a key that is set but short.
// A non-loopback Addr without AllowNonLoopback is refused before any
// socket opens.
func New(cfg Config, src Sources) (*Server, error) {
	if cfg.InsecureNoKey {
		if strings.TrimSpace(cfg.Key) != "" {
			return nil, errors.New("adminui: SEP2_ADMIN_UI_INSECURE_NO_KEY and SEP2_ADMIN_UI_KEY are both set; unset one")
		}
		generated, err := generateKey()
		if err != nil {
			return nil, fmt.Errorf("adminui: generate session key: %w", err)
		}
		cfg.Key = generated
	} else if cfg.Key == "" {
		return nil, ErrDisabled
	}
	if cfg.Addr == "" {
		return nil, errors.New("adminui: Config.Addr is required")
	}
	if src.Registry == nil || src.Devices == nil || src.Programs == nil || src.Flow == nil ||
		src.Identity == nil || src.Stomp == nil || src.Clients == nil || src.Protocol == nil || src.History == nil {
		return nil, errors.New("adminui: every Sources field is required")
	}

	loopback, err := isLoopbackHost(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("adminui: %w", err)
	}
	if !loopback && !cfg.AllowNonLoopback {
		return nil, fmt.Errorf("adminui: Addr %q is not loopback; set Config.AllowNonLoopback to bind a non-loopback address", cfg.Addr)
	}

	idleAfter := cfg.ClientIdleAfter
	if idleAfter <= 0 {
		idleAfter = DefaultClientIdleAfter
	}
	s := &Server{
		cfg:       cfg,
		idleAfter: idleAfter,
		registry:  src.Registry,
		devices:   src.Devices,
		programs:  src.Programs,
		flow:      src.Flow,
		identity:  src.Identity,
		stomp:     src.Stomp,
		clients:   src.Clients,
		activity:  src.Activity,
		history:   src.History,
		mirror:    src.Mirror,
		control:   src.Control,
		controls:  newControlLedger(),
		startedAt: time.Now(),
		now:       time.Now,
		timeouts:  defaultTimeouts.withOverrides(cfg),

		mirrorBuilds: make(chan struct{}, maxMirrorBuilds),
		outputBuilds: make(chan struct{}, maxOutputBuilds),
	}
	s.controlLimit = newTokenBucket(s.now())
	if src.Monitor != nil {
		s.monitor = newMonitorPanel(src.Monitor, s.startedAt)
	}
	if src.Sender != nil {
		s.sender = &senderPanel{s: src.Sender}
	}

	plane, err := sep2adminplane.New(sep2adminplane.Config{
		Stores:       src.Protocol.Stores(),
		AdminKey:     cfg.Key,
		AllowedHosts: s.allowedHosts(),
		Notifier:     src.Protocol.Notifier(),

		// One recorder and one threshold for the Devices tab and
		// /api/clients, so the two never disagree about a device.
		Activity:          src.Activity,
		CommsOfflineAfter: idleAfter,

		FlowReservationDeadline: cfg.Settings.FlowReservationDeadline,
		RetentionGrace:          cfg.Settings.RetentionGrace,
		// The bridge has always required a credential on loopback, and it
		// seeds and writes these stores itself: an admin write (an FSA
		// assignment, a new EndDevice, a DER control) would be a second
		// writer behind its back, so the plane mounts no write route.
		// Panel actions write no store: the sender publishes to the bus,
		// and the control subscriber stays the only store writer.
		LoopbackBypass: false,
		ControlWrites:  false,
		ReadOnly:       true,
		PanelActions:   true,
		Panels:         s.panels(),
		DeviceColumns:  []sep2admin.DeviceColumnSource{&deviceColumns{registry: s.registry, devices: s.devices}},
	})
	switch {
	case errors.Is(err, sep2adminplane.ErrNoCredential):
		return nil, fmt.Errorf("%w: %w", ErrDisabled, err)
	case errors.Is(err, sep2adminplane.ErrShortCredential):
		// The plane's error never carries the key, and neither does this one.
		return nil, fmt.Errorf("adminui: SEP2_ADMIN_UI_KEY must be at least %d characters: %w", sep2adminplane.MinAdminKeyLength, err)
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
	s.plane = plane
	s.planePatterns = plane.Patterns()
	s.handler = s.buildHandler(plane.Handler())
	if cfg.InsecureNoKey {
		log.Print(s.noKeyWarning())
	}
	return s, nil
}

// generateKey returns 32 random bytes as hex, the key the server holds
// for itself in no-key mode.
func generateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// noKeyWarning is the line logged from New in no-key mode, after the listener binds and before Run serves.
func (s *Server) noKeyWarning() string {
	addr := s.Addr()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			addr = "all interfaces (" + addr + ")"
		}
	}
	return fmt.Sprintf("adminui: WARNING admin UI running WITHOUT A KEY (SEP2_ADMIN_UI_INSECURE_NO_KEY=true) on %s; accepted Host values: %s; anyone who can reach this address can read every panel and, when publishing is switched on, send control messages",
		addr, strings.Join(s.allowedHosts(), ", "))
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
// shuts down within the shutdown timeout. A graceful shutdown returns
// nil; a serve failure independent of ctx, or a shutdown that does not
// finish in time, returns an error, matching cmd/bridge's runner contract.
func (s *Server) Run(ctx context.Context) error {
	// Shutdown waits for handlers but never cancels them, so an open event
	// stream would hold it to its deadline. Every request context derives
	// from base, which shutdown cancels first.
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	httpSrv := &http.Server{
		Handler:           s.handler,
		BaseContext:       func(net.Listener) context.Context { return base },
		ReadHeaderTimeout: s.timeouts.readHeader,
		ReadTimeout:       s.timeouts.read,
		WriteTimeout:      s.timeouts.write,
		IdleTimeout:       s.timeouts.idle,
	}
	httpSrv.RegisterOnShutdown(cancelBase)

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(s.ln) }()

	select {
	case <-ctx.Done():
		// One shutdown bound covers both steps. The plane closes first, so
		// each panel stream gets its final status event rather than a cut
		// connection, and running panel actions are told to stop and
		// waited for, for at most half the bound.
		deadline := time.Now().Add(s.timeouts.shutdown)
		if n := s.plane.Close(s.timeouts.shutdown / 2); n > 0 {
			log.Printf("adminui: %d panel action(s) still running at shutdown; they may still take effect", n)
		}
		shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
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
