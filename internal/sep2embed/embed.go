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

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
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

	// There is deliberately no bus, destination or simulation id here
	// (GAGO-121). This package serves the IEEE 2030.5 protocol and owns
	// the resource stores; it does not publish to the GridAPPS-D bus,
	// because a protocol request must never cause a platform-side send.
	// internal/telemetrypub reads DERStatusSnapshots on its own timer
	// and publishes from there. TestConfigCarriesNoBusPublishSurface
	// guards this.

	// DefaultControl is the DefaultDERControl seeded onto every
	// DERProgram's DefaultDERControlLink. As of the default-program work
	// this happens at New/seedStores time, for every registered device,
	// rather than at the first ApplyControlDelta: see DefaultProgram below
	// and seed.go's createDERProgram call. The zero value (every
	// DERControlBase field nil, including OpModConnect/OpModEnergize) is a
	// valid but degenerate configuration: a CSIP client would find a
	// well-formed but all-unset DefaultDERControl. Callers should source
	// this from sep2config.SEP2Policy.DefaultControl rather than leaving it
	// zero.
	DefaultControl sep2.DefaultDERControl

	// DefaultProgram is the DERProgram seeded for every registered device
	// at New time, the resource DefaultControl above hangs off.
	//
	// Seeding it, rather than creating it lazily on the first control
	// delta, is what makes both the program and its default control
	// reachable by a client that walks the tree once at startup. See
	// sep2config.DERProgramPolicy for the full rationale and the standards
	// citations behind the default values.
	//
	// The zero value serves primacy 0 and an absent description. Primacy 0
	// is a real value (the highest priority), not a stand-in for unset, so
	// callers should source this from
	// sep2config.SEP2Policy.DefaultProgram rather than leaving it zero and
	// getting a higher-priority program than they intended.
	DefaultProgram DERProgramSeed

	// DERControl is the temporal shape stamped onto every DERControl this
	// package issues from a control delta: the interval duration and the
	// randomizeDuration served with it. Callers should source it from
	// sep2config.SEP2Policy.DERControl.
	//
	// The zero value is NOT serviceable. A zero Duration produces an event
	// whose interval ends the instant it starts, which a conformant client
	// expires on arrival without ever actuating; that is the defect GAGO-131
	// fixed. ApplyControlDelta refuses it with ErrDERControlDurationUnset
	// rather than writing the control, so an unconfigured bridge fails
	// loudly at the first delta instead of serving controls that are
	// silently discarded.
	//
	// The refusal lives at the write site rather than in New because that is
	// where the invalid document would be produced: constructing a server
	// that never receives a control delta is not itself an error, and the
	// production path is already gated earlier by
	// sep2config.SEP2Policy.ValidateDERControl at boot.
	DERControl DERControlSeed

	// ModesSupported is the DERControlType bitmap GAGO-049 stamps into
	// the DERCapability New seeds for every registry entry (see
	// seedStores/seedOne). Typed as *sep2.DERControlType (IEEECORE-047),
	// matching sep2.DERCapability.ModesSupported's own field type
	// exactly. Nil (the zero value) leaves every seeded
	// DERCapability.ModesSupported nil: callers should source this from
	// sep2config.SEP2Policy.ModesSupported rather than fabricating a
	// bitmap here.
	ModesSupported *sep2.DERControlType

	// ResolveRegistrationPIN returns the operator-supplied registration
	// PIN for the device with the given canonical LFDI, and whether one is
	// configured. Callers should pass
	// sep2config.SEP2Policy.ResolveRegistrationPIN.
	//
	// There is deliberately no derived fallback: the PIN must not be
	// computable from device identity. Nil, or a resolver that answers
	// false for a device New is asked to seed, makes New fail with an
	// error naming that device, because sep.xsd:184 makes pIN minOccurs=1
	// in the Registration sequence and 0 is a meaningless schema-valid
	// value. Never logged.
	ResolveRegistrationPIN func(lfdi string) (uint32, bool)

	// ResolveRegistrationPollRate returns the polling interval, in
	// seconds, configured for the device with the given canonical LFDI,
	// and whether one is configured. It is stamped onto that device's
	// seeded Registration as the optional pollRate attribute
	// (sep.xsd:190). Callers should pass
	// sep2config.SEP2Policy.ResolvePollRate.
	//
	// Nil, or a resolver reporting false for a device, omits the
	// attribute so a client applies sep.xsd's own 900-second default
	// rather than a rate the bridge invented. Unlike
	// ResolveRegistrationPIN, an unresolved rate is NOT an error: pollRate
	// is optional in the schema, while pIN is minOccurs=1.
	//
	// A resolver rather than a scalar because the rate is a per-device
	// policy value whose per-device half is not wired yet; see
	// sep2config.SEP2Policy.PollRates. Seeding already knows each device's
	// LFDI, so passing it costs nothing today and means adding per-device
	// rates never touches this package.
	ResolveRegistrationPollRate func(lfdi string) (uint32, bool)

	// ResolvePostRate returns the posting interval, in seconds, configured
	// for the client with the given canonical LFDI, and whether one is
	// configured. It is threaded to core as the
	// metering.PostRateProvider behind POST /mup, where it is stamped onto
	// each MirrorUsagePoint at creation. Callers should pass
	// sep2config.SEP2Policy.ResolvePostRate.
	//
	// Nil, or a resolver reporting false, leaves the client's own postRate
	// untouched. This is NOT part of store seeding: the bridge creates no
	// MirrorUsagePoints at all (every one is created by a client via POST
	// /mup), so the only moment a server-side rate can reach a mirror is
	// at creation, inside core's handler.
	ResolvePostRate func(lfdi string) (uint32, bool)

	// Observer is the GAGO-090/GAGO-091 per-LFDI connection observer.
	// Nil (the zero value) disables observation entirely: New falls back
	// to delegating listener construction to sep2srv.New exactly as
	// before, and buildHandler wires a nil-safe pass-through in place of
	// the request-observation middleware. When non-nil, every
	// authenticated request is recorded via Observer.RecordRequest, and
	// (GCM/default listener only; see errObserverRequiresGCM) every mTLS
	// connection attempt that reaches certificate verification (i.e. the
	// client presented a certificate and chain-building ran), accepted
	// or rejected, is recorded via Observer.RecordHandshake; a connection
	// that fails before that point (no certificate presented, TLS
	// negotiation failure) is not recorded, per connobs's own package
	// doc comment. The caller (cmd/bridge) owns the Hook's lifetime and
	// reads it back via internal/adminui's /api/clients endpoint; this
	// package only ever writes to it.
	Observer *connobs.Hook
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
	policy   ControlPolicy
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

	policy := ControlPolicy{
		DefaultControl: cfg.DefaultControl,
		Program:        cfg.DefaultProgram,
		Control:        cfg.DERControl,
	}

	stores := newStores()
	seeding := seedPolicy{
		modesSupported:  cfg.ModesSupported,
		resolvePIN:      cfg.ResolveRegistrationPIN,
		resolvePollRate: cfg.ResolveRegistrationPollRate,
		control:         policy,
	}
	if err := seedStores(ctx, stores, reg, seeding); err != nil {
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

	// postRate reaches the wire through core's POST /mup handler, not
	// through seeding: this bridge creates no MirrorUsagePoints, so
	// creation-time stamping in core is the only point at which a
	// server-side rate can attach to a mirror. RouterConfig is how core
	// takes server-owned policy, so the resolver is projected onto it here
	// rather than at every buildHandler call below. cfg is a value
	// parameter, so this mutates only our copy.
	//
	// A nil ResolvePostRate leaves the field nil and core leaves every
	// client's postRate untouched.
	cfg.Router.PostRateProvider = cfg.ResolvePostRate

	// Observer wired: build the mTLS listener ourselves, with the
	// additive handshake-observation wrapper (see mtls.go's doc comment
	// for why core's sep2srv.New cannot be used for this path). Observer
	// unset (the common case today: cmd/bridge only wires it once
	// GAGO-091 lands): fall through unchanged to the pre-GAGO-090
	// sep2srv.New path below.
	if cfg.Observer != nil {
		if cfg.EnableCCM {
			return nil, errObserverRequiresGCM
		}

		listener, identity, err := newObservedMTLSListener(cfg.Addr, certFile, keyFile, caFile, cfg.ExtraClientCAs, cfg.Observer, reg)
		if err != nil {
			return nil, err
		}

		handler := buildHandler(cfg.Router, stores, reg, identity, notifier, cfg.Observer)

		shutdownTimeout := cfg.ShutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = sep2srv.DefaultShutdownTimeout
		}

		srv := &observedMTLSServer{
			identity: identity,
			listener: listener,
			httpSrv: &http.Server{
				Handler:           handler,
				ReadHeaderTimeout: sep2srv.DefaultReadHeaderTimeout,
				ReadTimeout:       sep2srv.DefaultReadTimeout,
				WriteTimeout:      sep2srv.DefaultWriteTimeout,
				IdleTimeout:       sep2srv.DefaultIdleTimeout,
			},
			shutdownTimeout: shutdownTimeout,
		}

		return &Embed{srv: srv, notifier: notifier, stores: stores, identity: identity, policy: policy}, nil
	}

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
		return buildHandler(cfg.Router, stores, reg, identity, notifier, cfg.Observer)
	}

	srv, err := sep2srv.New(opts, build)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: %w", err)
	}

	return &Embed{srv: srv, notifier: notifier, stores: stores, identity: srv.Identity, policy: policy}, nil
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
	return ApplyControlDelta(ctx, e.stores, e.notifier, reg, e.policy, delta)
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
