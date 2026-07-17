// Package sep2embed boots the IEEE 2030.5 protocol server in-process,
// inside the bridge's own binary. It consumes core's public embed
// surface (github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv and
// .../pkg/sep2srv/assembly) rather than promoting anything from the
// server-of-record: per Noor's 2026-07-02 assessment, the reusable
// surface already lives in core, so no server-side promotion is
// required to stand up a working embedded server.
//
// GAGO-030 builds this package; GAGO-031 wires it into cmd/bridge.
package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// Default sizing for the subscription fan-out manager when Config leaves
// NotifyWorkers / NotifyQueueSize at zero or negative. Mirrors core's own
// NewManager zero-value fallback (workerCount<1 -> 2, queueSize<1 -> 100)
// with a slightly larger worker count sized for the bridge's expected
// device fleet (the 123pv feeder's 14+14+14 devices).
const (
	DefaultNotifyWorkers   = 4
	DefaultNotifyQueueSize = 100
)

// Config configures an Embed.
type Config struct {
	// Addr is the "host:port" the protocol listener binds. Required.
	// Use ":0" to let the OS assign a port; read it back via Embed.Addr.
	Addr string

	// CertDir is the directory holding (or receiving) the embedded
	// server's CA and leaf certificate/key material: ca.pem, ca-key.pem,
	// server.pem, server-key.pem. When all four files already exist they
	// are loaded as-is (the production, preprovisioned-material path).
	// When any is missing, fresh dev-mint material is generated and
	// written here with 0600 (files) / 0700 (dir) permissions. Required.
	// CertDir is never committed; the caller owns keeping it out of
	// version control.
	CertDir string

	// ExtraClientCAs names additional client-CA bundles trusted
	// alongside the CertDir CA, for multi-root device trust.
	ExtraClientCAs []string

	// EnableCCM selects the CCM-8 mandatory cipher suite (IEEE
	// 2030.5-2018 section 6.7) via core's forked crypto/tls. False (the
	// default) serves the stdlib GCM fallback, which is still mTLS: this
	// knob selects the cipher suite, not whether TLS is required.
	EnableCCM bool

	// ShutdownTimeout bounds Run's graceful drain after ctx is
	// cancelled. Zero uses sep2srv.DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// Router carries the scalar time-zone/DST configuration for the /tm
	// resource. The zero value (UTC, no DST) is a valid configuration.
	Router assembly.RouterConfig

	// NotifyWorkers and NotifyQueueSize size the subscription fan-out
	// manager's worker pool and bounded queue. Zero or negative uses
	// DefaultNotifyWorkers / DefaultNotifyQueueSize.
	NotifyWorkers   int
	NotifyQueueSize int

	// Bus is the optional GridAPPS-D message-bus publisher used for the
	// GAGO-034 UP-path telemetry relay: each successful PUT of an
	// owning device's DERStatus is mapped to a diff.Message (see
	// telemetry.go) and sent over Bus to TelemetryDestination. Nil (the
	// zero value) disables the relay entirely: DERStatus PUT still
	// succeeds and is stored exactly as before, it is just not echoed
	// to the bus. See BusPublisher's doc comment for why this is not
	// internal/cimstomp.Publisher.
	Bus BusPublisher

	// TelemetryDestination is the bus destination the relay publishes
	// to (typically internal/cim/sim.InputTopic(simID)). Empty disables
	// the relay.
	TelemetryDestination string

	// TelemetrySimulationID is stamped into the outgoing diff.Message
	// envelope's simulation_id field. Empty disables the relay.
	TelemetrySimulationID string
}

// protocolServer is the minimal surface Run needs from the embedded mTLS
// listener. *sep2srv.Server satisfies it. Defined here at the consumer
// (Pike rule: interfaces at the consumer, not the producer) so Run's
// notifier-teardown behavior is unit-testable against a fake that
// returns from Run independent of ctx cancellation, without standing up
// a real TCP listener. See embed_test.go's fakeProtocolServer.
type protocolServer interface {
	Run(ctx context.Context) error
	Addr() string
}

// Embed is the in-process IEEE 2030.5 protocol server: seeded resource
// stores, the subscription fan-out manager, and the mTLS listener from
// core's pkg/sep2srv. Construct with New; start with Run.
type Embed struct {
	srv      protocolServer
	notifier *coresub.Manager
	stores   *assembly.Stores
	identity sep2srv.Identity
}

// New builds the resource stores, seeds EndDevices and DERs from reg,
// wires the subscription notifier, and binds the mTLS listener. It does
// NOT start serving; call Run to do that. On any error, New leaves no
// bound listener behind (sep2srv.New closes any listener it opened on
// its own error paths).
//
// ctx scopes the seeding writes only (New's own store.Create calls);
// it is not retained. Run below takes its own ctx, which is the one
// that governs the listener and notifier lifecycle. Two separate ctx
// parameters rather than one stored on the struct: per the workspace Go
// standards, a Context is never stored in a struct and is passed
// explicitly through the call chain that needs it.
func New(ctx context.Context, cfg Config, reg *registry.Registry) (*Embed, error) {
	if cfg.Addr == "" {
		return nil, errors.New("sep2embed: Config.Addr is required")
	}
	if cfg.CertDir == "" {
		return nil, errors.New("sep2embed: Config.CertDir is required")
	}
	if reg == nil {
		return nil, errors.New("sep2embed: registry is required")
	}

	certFile, keyFile, caFile, err := ensureServerIdentity(cfg.CertDir)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: server identity: %w", err)
	}

	stores := newStores()
	if err := seedStores(ctx, stores, reg); err != nil {
		return nil, fmt.Errorf("sep2embed: seed stores: %w", err)
	}

	workers := cfg.NotifyWorkers
	if workers <= 0 {
		workers = DefaultNotifyWorkers
	}
	queueSize := cfg.NotifyQueueSize
	if queueSize <= 0 {
		queueSize = DefaultNotifyQueueSize
	}
	notifier := coresub.NewManager(stores.Subscriptions, workers, queueSize)

	opts := sep2srv.Options{
		Addr:            cfg.Addr,
		CertFile:        certFile,
		KeyFile:         keyFile,
		CAFile:          caFile,
		ExtraClientCAs:  cfg.ExtraClientCAs,
		EnableCCM:       cfg.EnableCCM,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}

	telemetry := telemetryConfig{
		bus:   cfg.Bus,
		reg:   reg,
		dest:  cfg.TelemetryDestination,
		simID: cfg.TelemetrySimulationID,
	}

	build := func(identity sep2srv.Identity) http.Handler {
		return buildHandler(cfg.Router, stores, identity, notifier, telemetry)
	}

	srv, err := sep2srv.New(opts, build)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: %w", err)
	}

	return &Embed{srv: srv, notifier: notifier, stores: stores, identity: srv.Identity}, nil
}

// Addr returns the listener's actual bound address. Useful when
// Config.Addr used ":0" and the caller needs the OS-assigned port.
func (e *Embed) Addr() string {
	return e.srv.Addr()
}

// Identity returns the server's SFDI/LFDI, derived from its leaf
// certificate during New.
func (e *Embed) Identity() sep2srv.Identity {
	return e.identity
}

// ApplyControlDelta maps one GridAPPS-D control delta onto this Embed's
// own resource stores and subscription notifier (GAGO-034 DOWN path).
// See the package-level ApplyControlDelta function for the full
// contract (owner scoping, field mapping, supersede semantics); this
// method is the entry point a caller holding an *Embed (rather than the
// package-private stores/notifier fields) uses, e.g. a future
// GridAPPS-D control-delta subscriber in cmd/bridge.
func (e *Embed) ApplyControlDelta(ctx context.Context, reg *registry.Registry, delta ControlDelta) error {
	return ApplyControlDelta(ctx, e.stores, e.notifier, reg, delta)
}

// Run starts the subscription notifier's worker pool and serves the
// protocol listener until ctx is cancelled or the listener fails on its
// own (e.g. an accept error unrelated to shutdown), then shuts both down
// gracefully. Run blocks until BOTH the listener's Serve goroutine and
// the notifier's worker pool have exited, on EVERY exit path: the
// notifier runs under notifyCtx, a child of ctx that is cancelled
// immediately after srv.Run returns, regardless of which of srv.Run's
// two internal branches (ctx.Done, or its own errCh) produced that
// return. Without that explicit cancel, the errCh branch would return
// without ever cancelling ctx, and the notifier's `<-ctx.Done()` inside
// Start would block forever, deadlocking this function on
// `<-notifierDone`. A caller that observes Run return therefore knows
// there is no goroutine left running on every path, not just the
// ctx-cancel path.
func (e *Embed) Run(ctx context.Context) error {
	notifyCtx, cancelNotify := context.WithCancel(ctx)
	defer cancelNotify() // backstop: guarantees cancellation even if a future edit adds an early return above the explicit call below

	notifierDone := make(chan struct{})
	go func() {
		defer close(notifierDone)
		e.notifier.Start(notifyCtx)
	}()

	err := e.srv.Run(ctx)

	// Explicit cancel here (not just the deferred one) is what actually
	// unblocks the notifier before we wait on it: the defer only fires
	// after this function returns, which is too late to unblock the
	// very `<-notifierDone` line below it.
	cancelNotify()

	<-notifierDone

	return err
}
