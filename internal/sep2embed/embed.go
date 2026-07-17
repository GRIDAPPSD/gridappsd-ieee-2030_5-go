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
}

// Embed is the in-process IEEE 2030.5 protocol server: seeded resource
// stores, the subscription fan-out manager, and the mTLS listener from
// core's pkg/sep2srv. Construct with New; start with Run.
type Embed struct {
	srv      *sep2srv.Server
	notifier *coresub.Manager
	stores   *assembly.Stores
}

// New builds the resource stores, seeds EndDevices and DERs from reg,
// wires the subscription notifier, and binds the mTLS listener. It does
// NOT start serving; call Run to do that. On any error, New leaves no
// bound listener behind (sep2srv.New closes any listener it opened on
// its own error paths).
func New(cfg Config, reg *registry.Registry) (*Embed, error) {
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
	if err := seedStores(context.Background(), stores, reg); err != nil {
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

	build := func(identity sep2srv.Identity) http.Handler {
		return buildHandler(cfg.Router, stores, identity, notifier)
	}

	srv, err := sep2srv.New(opts, build)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: %w", err)
	}

	return &Embed{srv: srv, notifier: notifier, stores: stores}, nil
}

// Addr returns the listener's actual bound address. Useful when
// Config.Addr used ":0" and the caller needs the OS-assigned port.
func (e *Embed) Addr() string {
	return e.srv.Addr()
}

// Identity returns the server's SFDI/LFDI, derived from its leaf
// certificate during New.
func (e *Embed) Identity() sep2srv.Identity {
	return e.srv.Identity
}

// Run starts the subscription notifier's worker pool and serves the
// protocol listener until ctx is cancelled or the listener fails, then
// shuts both down gracefully. Run blocks until BOTH the listener's
// Serve goroutine and the notifier's worker pool have exited, so a
// caller that observes Run return knows there is no goroutine left
// running: everything ties to the single ctx root passed in here.
func (e *Embed) Run(ctx context.Context) error {
	notifierDone := make(chan struct{})
	go func() {
		defer close(notifierDone)
		e.notifier.Start(ctx)
	}()

	err := e.srv.Run(ctx)

	<-notifierDone

	return err
}
