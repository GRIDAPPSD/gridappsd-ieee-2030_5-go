// Package sep2embed boots the IEEE 2030.5 protocol server in-process,
// inside the bridge's own binary. It consumes
// github.com/GRIDAPPSD/ieee-2030_5-server-go's public embed surface
// (pkg/sep2srv and pkg/sep2srv/assembly), which moved there from
// ieee-2030_5-core-go after core v0.14.1.
package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
)

// Default sizing for the subscription fan-out manager when Config leaves
// NotifyWorkers / NotifyQueueSize at zero or negative. Mirrors server-go's
// own NewManager zero-value fallback (workerCount<1 -> 2, queueSize<1 -> 100)
// with a slightly larger worker count sized for the bridge's expected
// device fleet (the 123pv feeder's 14+14+14 devices).
const (
	DefaultNotifyWorkers   = 4
	DefaultNotifyQueueSize = 100
)

// Defaults for the notification timeouts, which server-go keeps
// unexported; restated so the bridge's config and docs show them.
const (
	DefaultNotifyPostTimeout    = 30 * time.Second
	DefaultNotifyDialTimeout    = 30 * time.Second
	DefaultNotifyResolveTimeout = 5 * time.Second
)

// Config configures an Embed.
type Config struct {
	// Addr is the "host:port" the protocol listener binds. Required.
	// Use ":0" to let the OS assign a port; read it back via Embed.Addr.
	Addr string

	// CertDir is the directory holding (or receiving) the embedded
	// server's CA and leaf certificate/key material: ca.pem, ca-key.pem,
	// server.pem, server-key.pem. Required.
	//
	// serving-ca.pem and serving-ca-key.pem (#118) are an OPTIONAL second
	// pair: when present, they sign server.pem instead of ca.pem, so the
	// CA that authenticates devices (ca.pem, the device CA) can later be
	// replaced without stranding the certificate every client verifies
	// the bridge with. Absent, ca.pem fills both roles, unchanged from
	// before the split.
	//
	// New classifies it once, at startup, per ensureServerIdentity: a set
	// that is complete for DeviceCertMode is loaded as-is and nothing is
	// written; in dev-mint mode an empty writable directory receives
	// fresh material at 0600 (dir 0700); anything else is a fatal startup
	// error. No existing file is ever overwritten.
	//
	// In DeviceCertModePreprovisioned nothing under CertDir is written
	// ever, for any reason, so a read-only bind mount is the supported
	// shape for that mode.
	//
	// CertDir is never committed; the caller owns keeping it out of
	// version control.
	CertDir string

	// DeviceCertMode selects how device identity certificates are sourced
	// AND, because the two questions have one answer, which files the
	// embedded server's own identity set must contain.
	//
	// DeviceCertModeDevMint (the zero value) signs in this process: it
	// mints device certificates against the CA under CertDir, so the CA
	// private key must be present there. DeviceCertModePreprovisioned
	// never signs; it reads only the CA's public certificate, so
	// ca-key.pem is not required and should not be on the host at all
	// (least privilege: see loadDeviceSigningCA).
	//
	// Threading it here is load bearing, not cosmetic. While New
	// classified CertDir with no mode, it demanded all four files
	// unconditionally, so a correctly deployed non-signing bridge that
	// withheld the CA private key was treated as incomplete and had its
	// real server certificate and key replaced with self-signed
	// development material.
	//
	// The zero value is the stricter of the two required sets, so a
	// caller that forgets to set this fails loudly on a preprovisioned
	// directory rather than quietly minting over it.
	DeviceCertMode DeviceCertMode

	// ExtraClientCAs names additional client-CA bundles trusted
	// alongside the CertDir CA, for multi-root device trust.
	ExtraClientCAs []string

	// EnableCCM selects the CCM-8 mandatory cipher suite (IEEE
	// 2030.5-2018 section 6.7) via core's forked crypto/tls. Every
	// listener this package builds is CCM-8 only, so this no longer
	// changes the suite served: a client unable to offer CCM-8 is refused
	// the handshake, never served over GCM, whether or not this is set.
	// Mutually exclusive with Observer; see errObserverRequiresGCM.
	EnableCCM bool

	// ShutdownTimeout bounds Run's graceful drain after ctx is
	// cancelled. Zero uses sep2srv.DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// ReadHeaderTimeout, ReadTimeout, WriteTimeout and IdleTimeout set the
	// protocol listener's http.Server timeouts. Zero uses the matching
	// sep2srv.Default*Timeout. They apply only to the listener New builds
	// itself (Observer or EnableCCM); sep2srv.New owns its own, so a
	// value other than the default without either is refused rather than
	// ignored.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// ControlSweepInterval is how often ended DERControls are expired
	// fleet-wide. Zero uses DefaultControlSweepInterval.
	ControlSweepInterval time.Duration

	// Router carries the scalar time-zone/DST configuration for the /tm
	// resource, the PEN and the flow reservation deadline. The zero value
	// (UTC, no DST, server defaults) is a valid configuration.
	Router assembly.RouterConfig

	// NotifyWorkers and NotifyQueueSize size the subscription fan-out
	// manager's worker pool and bounded queue. Zero or negative uses
	// DefaultNotifyWorkers / DefaultNotifyQueueSize.
	NotifyWorkers   int
	NotifyQueueSize int

	// NotifyPostTimeout, NotifyDialTimeout and NotifyResolveTimeout bound
	// one notification POST, its dial, and the creation-time DNS check.
	// Zero keeps server-go's built-in value; server-go caps the dial at
	// the POST timeout.
	NotifyPostTimeout    time.Duration
	NotifyDialTimeout    time.Duration
	NotifyResolveTimeout time.Duration

	// CCMHandshakeTimeout bounds one inbound TLS handshake on the
	// Observer and EnableCCM listeners. Zero keeps core's default.
	CCMHandshakeTimeout time.Duration

	// NotifyAllowLoopback permits a subscription's notificationURI to
	// target loopback addresses (127.0.0.0/8, ::1), refused by default
	// (coresub.DestinationPolicy's zero value). Off by default because
	// enabling it lets any client that can create a subscription make the
	// server POST to any loopback destination on any port, reaching every
	// service this bridge's network namespace exposes there (its admin UI
	// listener, SEP2_ADMIN_UI_ADDR, among others; its Bearer auth does not
	// narrow this). Mirrors server-go's SEP2_NOTIFICATION_ALLOW_LOOPBACK;
	// see buildNotifier.
	NotifyAllowLoopback bool

	// There is deliberately no bus, destination or simulation id here.
	// This package serves the IEEE 2030.5 protocol and owns
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
	// expires on arrival without ever actuating. ApplyControlDelta
	// refuses it with ErrDERControlDurationUnset
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

	// ModesSupported is the DERControlType bitmap stamped into
	// the DERCapability New seeds for every registry entry (see
	// seedStores/seedOne). Typed as *sep2.DERControlType,
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
	// at creation, inside server-go's handler.
	ResolvePostRate func(lfdi string) (uint32, bool)

	// Observer is the per-LFDI connection observer.
	// Nil (the zero value) disables observation entirely. With EnableCCM
	// unset, New falls back to delegating listener construction to
	// sep2srv.New exactly as before, and buildHandler wires a nil-safe
	// pass-through in place of the request-observation middleware. With
	// EnableCCM set, Observer MUST be nil (New refuses the combination
	// otherwise, see errObserverRequiresGCM) and New instead builds the
	// CCM-only listener itself (newCCMOnlyListener), which has no
	// handshake-observation seam of its own. When non-nil, every
	// authenticated request is recorded via Observer.RecordRequest, and
	// (the observed listener path only, i.e. Observer non-nil; see
	// newObservedMTLSListener) every mTLS connection attempt that reaches
	// certificate verification (i.e. the
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
// server-go's pkg/sep2srv. Construct with New; start with Run.
type Embed struct {
	srv      protocolServer
	notifier *coresub.Manager
	stores   *assembly.Stores
	identity sep2srv.Identity
	policy   ControlPolicy

	// ended holds the identity of every DERControl this Embed has taken out
	// of service, for the retention window (see lifecycle.go). It is never
	// nil on an Embed built by New.
	ended *endedControlLedger

	// sweepInterval is the runControlSweep period. Zero (an Embed built
	// without New) uses DefaultControlSweepInterval.
	sweepInterval time.Duration
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
	// cmd/bridge validates this same bound at boot (SEP2Policy.ValidateDERControl),
	// but New is the exported constructor: a caller that reaches it without
	// going through that boot sequence must fail here rather than serve a
	// silently wrapped or narrowing-panicked randomizeDuration later (see
	// control.go's OneHourRange conversion).
	if !sep2config.RandomizeDurationInRange(cfg.DERControl.RandomizeDuration) {
		return nil, fmt.Errorf(
			"sep2embed: Config.DERControl.RandomizeDuration %d is outside the sep.xsd OneHourRangeType range of -%d to %d seconds",
			cfg.DERControl.RandomizeDuration, sep2config.MaxRandomizeSeconds, sep2config.MaxRandomizeSeconds)
	}
	// The boot validator also refuses a RandomizeDuration whose magnitude
	// is not smaller than Duration (policy.go: "a client applying an
	// offset that wide can reduce the interval to zero or less"). New
	// re-checks it here for the same reason it re-checks the range above:
	// a caller of the exported New that skips cmd/bridge's boot sequence
	// must fail here rather than serve a control whose effective window a
	// client can cancel or reverse. Skipped when Duration is 0: New does
	// not itself validate Duration, and ValidateDERControl never reaches
	// this comparison for a zero Duration either, since it refuses that
	// case first.
	if cfg.DERControl.Duration != 0 && sep2config.RandomizeDurationMagnitudeAtOrAboveDuration(cfg.DERControl.RandomizeDuration, cfg.DERControl.Duration) {
		return nil, fmt.Errorf(
			"sep2embed: Config.DERControl.RandomizeDuration %d is not smaller in magnitude than Config.DERControl.Duration %d; "+
				"a client applying an offset that wide can reduce the interval to zero or less",
			cfg.DERControl.RandomizeDuration, cfg.DERControl.Duration)
	}

	// The one and only read of the server's own certificate directory.
	// Everything downstream (the listener's TLS config, the identity
	// reported by Identity()) is built from what these paths hold at this
	// instant and is never refreshed: the CA certificate is the trust
	// anchor already-registered clients chained to, and the server key
	// backs live TLS sessions, so both are fixed for the process
	// lifetime. Per-device certificates are the material that IS
	// re-evaluated later; see ensureDeviceCert.
	certFile, keyFile, caFile, err := ensureServerIdentity(cfg.CertDir, cfg.DeviceCertMode)
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

	workers, queueSize := notifySizing(cfg)
	notifyTimeouts := coresub.NotificationTimeouts{
		Post:            cfg.NotifyPostTimeout,
		Dial:            cfg.NotifyDialTimeout,
		CreationResolve: cfg.NotifyResolveTimeout,
	}
	if err := notifyTimeouts.Validate(); err != nil {
		return nil, fmt.Errorf("sep2embed: %w", err)
	}
	notifier := newNotifier(stores.Subscriptions, workers, queueSize, cfg.NotifyAllowLoopback, notifyTimeouts)

	// postRate reaches the wire through server-go's POST /mup handler, not
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

	// Observer or EnableCCM: build the mTLS listener ourselves, either
	// with the additive handshake-observation wrapper (Observer) or
	// without one (EnableCCM alone). Both build the identical CCM-8
	// gotls.Config (buildCCMServerConfig, mtls.go) since core v0.20.0
	// dropped the separate GCM/default constructor, so both need the
	// same gotls http.Server wiring below. Neither set: fall through
	// unchanged to the sep2srv.New delegation, which builds that same
	// CCM-8 listener itself (server-go's own wrapMTLS).
	if cfg.Observer != nil || cfg.EnableCCM {
		if cfg.Observer != nil && cfg.EnableCCM {
			return nil, errObserverRequiresGCM
		}

		var (
			listener net.Listener
			identity sep2srv.Identity
			err      error
		)
		if cfg.Observer != nil {
			listener, identity, err = newObservedMTLSListener(cfg.Addr, certFile, keyFile, caFile, cfg.ExtraClientCAs, cfg.Observer, reg, cfg.CCMHandshakeTimeout)
		} else {
			listener, identity, err = newCCMOnlyListener(cfg.Addr, certFile, keyFile, caFile, cfg.ExtraClientCAs, cfg.CCMHandshakeTimeout)
		}
		if err != nil {
			return nil, err
		}

		handler := buildHandler(cfg.Router, stores, reg, identity, notifier, cfg.Observer)

		shutdownTimeout := cfg.ShutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = sep2srv.DefaultShutdownTimeout
		}
		readHeaderTimeout := durationOrDefault(cfg.ReadHeaderTimeout, sep2srv.DefaultReadHeaderTimeout)
		readTimeout := durationOrDefault(cfg.ReadTimeout, sep2srv.DefaultReadTimeout)
		writeTimeout := durationOrDefault(cfg.WriteTimeout, sep2srv.DefaultWriteTimeout)
		idleTimeout := durationOrDefault(cfg.IdleTimeout, sep2srv.DefaultIdleTimeout)

		httpSrv := &http.Server{
			// CCMIdentityMiddleware must wrap OUTERMOST: it populates
			// r.TLS from the gotls connection state SetupCCMServer below
			// threads through ConnContext, and every other middleware
			// (identityMiddleware included) reads r.TLS. This mirrors
			// server-go's own sep2srv.New CCM wiring exactly
			// (server.go: "sepTLS.CCMIdentityMiddleware(handler)").
			Handler:           sepTLS.CCMIdentityMiddleware(handler),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		}
		sepTLS.SetupCCMServer(httpSrv)

		srv := &observedMTLSServer{
			identity:        identity,
			listener:        listener,
			httpSrv:         httpSrv,
			shutdownTimeout: shutdownTimeout,
		}

		return &Embed{srv: srv, notifier: notifier, stores: stores, identity: identity, policy: policy, ended: newEndedControlLedger(), sweepInterval: cfg.ControlSweepInterval}, nil
	}

	// Neither Observer nor EnableCCM: delegate to server-go's sep2srv.New,
	// which builds the identical CCM-8-only listener (wrapMTLS) the
	// branch above builds by hand for the EnableCCM-alone case.
	opts := sep2srv.Options{
		Addr:            cfg.Addr,
		CertFile:        certFile,
		KeyFile:         keyFile,
		CAFile:          caFile,
		ExtraClientCAs:  cfg.ExtraClientCAs,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}

	build := func(identity sep2srv.Identity) http.Handler {
		return buildHandler(cfg.Router, stores, reg, identity, notifier, cfg.Observer)
	}

	if customListenerTimeouts(cfg) {
		return nil, errors.New("sep2embed: listener timeouts need Observer or EnableCCM: the default listener sets its own")
	}

	srv, err := sep2srv.New(opts, build)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: %w", err)
	}

	return &Embed{srv: srv, notifier: notifier, stores: stores, identity: srv.Identity, policy: policy, ended: newEndedControlLedger(), sweepInterval: cfg.ControlSweepInterval}, nil
}

// customListenerTimeouts reports whether cfg asks for a listener timeout
// other than the default the sep2srv-built listener already uses.
func customListenerTimeouts(cfg Config) bool {
	for _, c := range []struct{ got, def time.Duration }{
		{cfg.ReadHeaderTimeout, sep2srv.DefaultReadHeaderTimeout},
		{cfg.ReadTimeout, sep2srv.DefaultReadTimeout},
		{cfg.WriteTimeout, sep2srv.DefaultWriteTimeout},
		{cfg.IdleTimeout, sep2srv.DefaultIdleTimeout},
		{cfg.CCMHandshakeTimeout, sepTLS.DefaultCCMHandshakeTimeout},
	} {
		if c.got != 0 && c.got != c.def {
			return true
		}
	}
	return false
}

// notifySizing resolves the notifier's worker count and queue length,
// substituting the default for a zero or negative value.
func notifySizing(cfg Config) (workers, queueSize int) {
	workers, queueSize = cfg.NotifyWorkers, cfg.NotifyQueueSize
	if workers <= 0 {
		workers = DefaultNotifyWorkers
	}
	if queueSize <= 0 {
		queueSize = DefaultNotifyQueueSize
	}
	return workers, queueSize
}

func durationOrDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
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

// Stores returns the store set the protocol listener serves, for the
// admin plane: it must read and write the same instances a device does.
func (e *Embed) Stores() *assembly.Stores {
	return e.stores
}

// Notifier returns the subscription fan-out the protocol listener uses,
// so a write through the admin plane notifies subscribers as a protocol
// write does.
func (e *Embed) Notifier() assembly.ResourceNotifier {
	return e.notifier
}

// ApplyControlDelta maps one GridAPPS-D control delta onto this Embed's
// own resource stores and subscription notifier (the DOWN path).
// See the package-level ApplyControlDelta function for the full
// contract (owner scoping, field mapping, supersede semantics); this
// method is the entry point a caller holding an *Embed (rather than the
// package-private stores/notifier fields) uses, e.g. a future
// GridAPPS-D control-delta subscriber in cmd/bridge.
//
// Controls whose maximum Effective Scheduled Period has already closed are
// swept out BEFORE the delta is applied. Sweeping here as well as
// on Run's timer is what keeps the exposure at a live delta cadence rather
// than at the control sweep interval, and it also keeps the supersession pass
// honest: an event that is out of service is not a predecessor for the
// incoming control to mark.
func (e *Embed) ApplyControlDelta(ctx context.Context, reg *registry.Registry, delta ControlDelta) error {
	// ONE clock read for the sweep and the write together, for the same
	// reason ApplyControlDelta reads the clock once for creationTime,
	// interval.start and EventStatus.dateTime: two reads could land on
	// different seconds, and an event issued at a later instant than the one
	// the sweep judged its predecessors by is a state nothing downstream can
	// interpret. pinnedClockPolicy hands the same instant to both.
	nowUnix := e.policy.Control.now().UTC().Unix()

	if _, err := e.expireEndedControlsAt(ctx, nowUnix); err != nil {
		return fmt.Errorf("sep2embed: control delta: %w", err)
	}
	return ApplyControlDelta(ctx, e.stores, e.notifier, reg, pinnedClockPolicy(e.policy, nowUnix), delta)
}

// pinnedClockPolicy returns a copy of policy whose control clock reports
// nowUnix on every read.
//
// ControlPolicy is a value, so this mutates nothing the Embed holds.
func pinnedClockPolicy(policy ControlPolicy, nowUnix int64) ControlPolicy {
	policy.Control.Now = func() time.Time { return time.Unix(nowUnix, 0).UTC() }
	return policy
}

// expireEndedControls runs one lifecycle sweep over this Embed's fleet at the
// policy clock's current instant, and returns how many controls it removed.
//
// The clock is DERControlSeed.Now, the same seam ApplyControlDelta stamps
// creationTime and interval.start from. Reading the end of an event's window
// from a different clock than the one that wrote its start would make the two
// disagree under a test that pins one of them, and would be indefensible in
// production for the same reason.
func (e *Embed) expireEndedControls(ctx context.Context) (int, error) {
	return e.expireEndedControlsAt(ctx, e.policy.Control.now().UTC().Unix())
}

// expireEndedControlsAt is expireEndedControls with the instant supplied by
// the caller, for the delta path, which has already read the clock.
func (e *Embed) expireEndedControlsAt(ctx context.Context, nowUnix int64) (int, error) {
	return expireEndedControls(ctx, e.stores, e.notifier, e.ended, servedEventEdition, nowUnix)
}

// EndedControl resolves the mRID of a DERControl this server has taken out of
// service to the record it retained for it, if the retention window is still
// open.
//
// It is the server-side half of the removal in lifecycle.go: a client's
// Response names the event by mRID (Response.subject), and 2018 Table 27 p.75
// places the status 3 (Event completed) POST at EffectiveEndTime, which is
// the same instant the event stops being served. Without this, a server that
// removed the resource would be unable to say which control a late
// EventCompleted reported on. See eventRetentionSeconds for how long the
// record is kept and why that window is our decision rather than the
// standard's.
func (e *Embed) EndedControl(mrid string) (EndedControl, bool) {
	return e.ended.lookup(mrid)
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
//
// The DERControl lifecycle sweep runs on the same lifetime and
// under the same discipline: it is started here, it is cancelled by the same
// explicit cancel, and Run waits for it before returning, so no sweep is left
// writing to the stores after the listener has stopped.
func (e *Embed) Run(ctx context.Context) error {
	notifyCtx, cancelNotify := context.WithCancel(ctx)
	defer cancelNotify() // backstop: guarantees cancellation even if a future edit adds an early return above the explicit call below

	notifierDone := make(chan struct{})
	go func() {
		defer close(notifierDone)
		e.notifier.Start(notifyCtx)
	}()

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		e.runControlSweep(notifyCtx)
	}()

	err := e.srv.Run(ctx)

	// Explicit cancel here (not just the deferred one) is what actually
	// unblocks the notifier before we wait on it: the defer only fires
	// after this function returns, which is too late to unblock the
	// very `<-notifierDone` line below it.
	cancelNotify()

	<-notifierDone
	<-sweepDone

	return err
}

// DefaultControlSweepInterval is how often runControlSweep re-checks the fleet for
// DERControls whose maximum Effective Scheduled Period has closed.
//
// It bounds the residual exposure this lifecycle exists to remove: between an
// event's end and the next sweep, that event is still served with a past
// interval. Ten seconds against a default 1800-second control window means a
// client can observe an ended event for at most about half a percent of the
// window it was served in, and only when no control delta arrives in the
// meantime, since Embed.ApplyControlDelta sweeps before it writes and a live
// federation's delta cadence closes the gap further.
//
// Operators may lengthen it, but a long value widens that exposure in
// proportion, so the default stays at ten seconds.
const DefaultControlSweepInterval = 10 * time.Second

// Seams for tests that assert the configured values reach the ticker and
// the notifier, which expose neither.
var (
	newSweepTicker = time.NewTicker
	newNotifier    = buildNotifier
)

func (e *Embed) controlSweepInterval() time.Duration {
	return durationOrDefault(e.sweepInterval, DefaultControlSweepInterval)
}

// runControlSweep expires ended DERControls on a fixed timer until ctx is
// cancelled. It is the fleet-wide half of the lifecycle; the per-delta half
// is in Embed.ApplyControlDelta.
//
// It has to exist independently of the delta path because a device's events
// end on their own schedule whether or not another delta ever arrives. A
// bridge whose platform side goes quiet must still take its last event out of
// service at the end of that event's window, or it serves a past interval
// until the process restarts, which is precisely the condition observed live
// on 2026-08-03.
//
// A sweep error is logged and the loop continues. Stopping would leave every
// subsequent event in service forever on the strength of one bad record, and
// the error is surfaced rather than swallowed so an operator sees it.
func (e *Embed) runControlSweep(ctx context.Context) {
	ticker := newSweepTicker(e.controlSweepInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := e.expireEndedControls(ctx); err != nil {
				log.Printf("sep2embed: control lifecycle sweep: %v", err)
			}
		}
	}
}
