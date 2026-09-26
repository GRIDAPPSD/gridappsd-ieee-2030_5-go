// Command bridge is the GridAPPS-D side of the IEEE 2030.5 to
// GridAPPS-D bridge. It connects to the GridAPPS-D message bus,
// queries the CIM feeder for inverter / solar / battery DERs, populates
// an in-memory mRID-to-LFDI registry, boots an in-process IEEE 2030.5
// mTLS server (internal/sep2embed) seeded from that registry, and (when
// a SimulationID is configured) subscribes to the simulation output
// topic and logs each MeasurementFrame.
//
// The embedded IEEE 2030.5 server speaks real mTLS with real,
// certificate-derived device identity: the LFDI on every EndDevice is
// per spec section 6.3.4 and the SFDI is per section 6.3.3, both
// derived from each device's own certificate rather than a placeholder
// hash of the CIM mRID. The listener still has no per-device ACL,
// which is why it binds to loopback by default. Bidirectional control
// flow (device writes reaching the CIM side) is a further follow-up.
//
// The GridAPPS-D connection rides github.com/GRIDAPPSD/gridappsd-go's
// fieldbus.MessageBus, adapted to this bridge's own
// internal/cim.Requester and internal/cim/sim.SubscribeClient
// interfaces via internal/gridappsdclient. internal/cimstomp, this
// repo's own STOMP implementation, stays in the tree for its
// Publisher and DifferenceBuilder types but no longer backs the
// bridge's connection.
//
// Lifecycle: the process runs until SIGINT or SIGTERM. Cancellation
// flows through a single context.Context root: the sep2embed server,
// the fieldbus.MessageBus, and any sim.Pump goroutines all exit on ctx
// cancel (or, for the sep2embed/STOMP pair, when either side exits on
// its own so neither is left running orphaned); the bridge then
// disconnects the bus and returns.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-go/gridappsd"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/buildinfo"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetrypub"
)

// connectTimeout bounds the initial STOMP dial plus auth-token
// bootstrap. The platform's broker normally responds in well under a
// second; 15s leaves comfortable headroom for slow CI machines.
const connectTimeout = 15 * time.Second

// queryTimeout bounds a single CIM SPARQL request. The 123-bus feeder
// query returns in ~100 ms locally; 30 s tolerates a busy platform.
const queryTimeout = 30 * time.Second

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		// -version and -h / -help both return before validate ever
		// runs, so neither reaches safeFatal below and neither has
		// any side effect (no ctx, no listener, no CIM query, no
		// bus connect): handleVersionFlag prints the version and
		// exits 0 first; flag.ErrHelp's usage banner is already
		// printed by flag.NewFlagSet, so that path also just exits 0
		// so shell redirection of `bridge -h` for docs works.
		if handleVersionFlag(os.Stdout, err) {
			os.Exit(0)
		}
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		safeFatal(cfg, "bridge: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Printf("bridge %s starting", buildinfo.Version)
	if err := run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		safeFatal(cfg, "bridge: %v", err)
	}
	log.Printf("bridge: shutdown complete")
}

// safeFatal formats msg the same as log.Fatalf, then strips any
// credential-shaped substring before logging it and exiting (Leon
// L3). Neither cfg.STOMPPassword nor cfg.SEP2AdminUIKey is
// expected to appear in a formatted error today (connectClient's own
// wrap at "connect %s: %w" only interpolates cfg.STOMPAddr, never the
// credential fields), but nothing upstream guarantees that stays true
// as new error wraps are added, and an accidentally-interpolated
// credential landing in a log line is exactly the kind of silent,
// hard-to-notice leak this defense-in-depth check exists to catch
// before it reaches stderr. cfg is passed by value (its zero-cost
// struct copy), so this call never mutates the caller's config.
func safeFatal(cfg config, format string, args ...any) {
	log.Fatal(redactCreds(cfg, fmt.Sprintf(format, args...)))
}

// redactCreds replaces any occurrence of cfg's credential-shaped fields
// in msg with a fixed placeholder: the raw STOMPPassword and
// SEP2AdminUIKey values, and also the base64(STOMPUser:STOMPPassword)
// blob the GOSS auth-token bootstrap sends over the wire (see
// internal/cimstomp's fetchAuthToken and gridappsd-go's internal/auth),
// since that encoded form shares no substring with the raw password and
// would otherwise slip past the two checks above undetected. This is
// still not exhaustive: any credential-shaped value
// that is not cfg.STOMPPassword, cfg.SEP2AdminUIKey, or their base64
// pairing is out of scope, so this remains a defense-in-depth catch, not
// a guarantee that no credential can ever appear in a log line. Split
// out from safeFatal so a test can assert the exact redacted output
// without going through log.Fatal's os.Exit. An empty credential field
// is never redacted against (an empty STOMPPassword would otherwise
// match, and replace, every empty substring in msg).
func redactCreds(cfg config, msg string) string {
	if cfg.STOMPPassword != "" {
		msg = strings.ReplaceAll(msg, cfg.STOMPPassword, "[REDACTED]")
	}
	if cfg.SEP2AdminUIKey != "" {
		msg = strings.ReplaceAll(msg, cfg.SEP2AdminUIKey, "[REDACTED]")
	}
	if cfg.STOMPUser != "" && cfg.STOMPPassword != "" {
		authBlob := base64.StdEncoding.EncodeToString([]byte(cfg.STOMPUser + ":" + cfg.STOMPPassword))
		msg = strings.ReplaceAll(msg, authBlob, "[REDACTED]")
	}
	return msg
}

// handleVersionFlag reports whether err is loadConfig's
// errVersionRequested sentinel (set when -version was passed), and if
// so writes the build version to w. Split out as a pure helper (no
// os.Exit inside it) so a test can drive it directly and assert the
// printed value without exec-ing a subprocess or exercising main's
// os.Exit call.
func handleVersionFlag(w io.Writer, err error) bool {
	if !errors.Is(err, errVersionRequested) {
		return false
	}
	fmt.Fprintf(w, "bridge %s\n", buildinfo.Version)
	return true
}

// run is the bridge lifecycle. It is split out from main so tests can
// drive a synthetic ctx and config without exec-ing a subprocess.
//
// Steady-state errors (handler errors, mid-run subscription drops) are
// logged and the loop continues. Startup errors (connect, CIM query,
// registry populate) are fatal: the bridge has nothing useful to do
// without them.
func run(ctx context.Context, cfg config) error {
	// policy is built and validated first, before connectClient below
	// ever dials the GridAPPS-D broker. buildSEP2Policy is a pure
	// function of cfg (matching this bridge's other config sources), so
	// running it first means an operator-supplied registration PIN that
	// is out of range or fails the IEEE 2030.5 section 6.3.5 check digit
	// (-sep2-registration-pin, -sep2-registration-pin-file)
	// stops the bridge immediately: before it dials anything, not merely
	// before it seeds or serves a device. buildSEP2Policy fills
	// policy.DefaultControl and policy.ModesSupported, both threaded
	// through newSEP2Embed below; DefaultPolicy leaves ModesSupported
	// nil, so the DERCapability it seeds is still nil-safe until a real
	// policy value is configured.
	policy, err := buildSEP2Policy(cfg)
	if err != nil {
		return err
	}
	// The registration PIN is deliberately absent from this line and must
	// stay absent: it is a shared secret in the registration flow, and
	// this log is written on every boot. Log whether it is configured, if
	// that is ever needed, never what it is.
	log.Printf("bridge: sep2 policy loaded modesSupported=%s pollRate=%s postRate=%s",
		fmtDERControlTypePtr(policy.ModesSupported), fmtU32Ptr(policy.DefaultPollRate), fmtU32Ptr(policy.DefaultPostRate))

	bus, err := connectClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := bus.Disconnect(); cerr != nil {
			log.Printf("bridge: bus disconnect: %v", cerr)
		}
	}()

	cimClient := cim.NewClient(gridappsdclient.NewRequester(bus))

	mode, err := deviceCertMode(cfg.SEP2DeviceCertMode)
	if err != nil {
		return err
	}
	reg, err := bootstrapRegistry(ctx, cimClient, cfg.FeederMRID, cfg.SEP2ServerCertDir, mode, cfg.SEP2BatteryLegs)
	if err != nil {
		return err
	}

	if cfg.PublishOnStart {
		// The publish smoke test wants to send a DifferenceBuilder
		// envelope to /topic/goss.gridappsd.simulation.input.<sim_id>.
		// fieldbus.MessageBus.Send can carry an arbitrary body, but
		// building the DifferenceBuilder envelope itself and wiring
		// it through here is a deliberate Stage 2 follow-up, not part
		// of this connection swap. The flag is wired so callers can
		// opt in once that follow-up lands; for now we log and
		// proceed.
		log.Printf("bridge: -publish-on-start requested; DifferenceBuilder envelope publish is filed as Stage 2 follow-up; skipping")
	}

	// connHook is a read-only observation point over the embedded mTLS
	// listener's connection surface: every authenticated request's LFDI
	// (sep2embed's connObserveMiddleware) and every mTLS handshake
	// attempt, accepted or rejected (sep2embed's additive
	// VerifyPeerCertificate wrapper), is recorded here. Must be
	// constructed before newSEP2Embed below, since sep2EmbedConfig
	// threads its address into sep2embed.Config.Observer for New to wire
	// into the listener it builds, UNLESS cfg.SEP2EnableCCM is set (see
	// sep2EmbedConfig's doc comment): connHook is still passed to
	// adminui.New below either way, so its /api/clients endpoint stays a
	// valid, merely empty, reader rather than a nil one. The admin UI
	// /api/clients endpoint is its only reader.
	var connHook connobs.Hook

	logCCMObserverDisabledChoice(cfg)

	// The embed seeds its EndDevice/DER stores from reg, so it must be
	// built after bootstrapRegistry above, not before.
	embed, err := newSEP2Embed(ctx, cfg, reg, policy, &connHook, mode)
	if err != nil {
		return fmt.Errorf("sep2 embed: %w", err)
	}
	id := embed.Identity()
	log.Printf("bridge: sep2 embed listening addr=%s sfdi=%s lfdi=%s",
		embed.Addr(), id.SFDI, id.LFDI)

	// controlHook is a read-only observation point over the
	// control-delta down path (and the sim-output/control-input topic
	// pair): runSimSide's two loops are its only writers, and the admin
	// UI controlflow endpoint is its only reader. It is safe to
	// construct unconditionally, even when SimulationID is empty and
	// stompRun never touches it: the zero value is a valid, all-empty
	// observation state.
	var controlHook controlobs.Hook

	// stompRun adapts the SimulationID branch (idle-wait, or the
	// measurement pump plus the control-delta subscriber) to
	// the func(context.Context) error shape runEmbedAndStomp expects
	// for its second seam.
	stompRun := func(runCtx context.Context) error {
		if cfg.SimulationID == "" {
			log.Printf("bridge: no SEP2_SIMULATION_ID set; skipping simulation subscribe; idling until shutdown")
			<-runCtx.Done()
			return runCtx.Err()
		}
		// runCtx is the pump's (and the control subscriber's) root.
		//
		// gridappsd-go's router still does not surface a broker-teardown
		// signal to fieldbus.MessageBus callers (an upstream gap;
		// see the relay doc comment in
		// internal/gridappsdclient/subscriber.go), so a mid-run broker
		// disconnect does NOT independently wake either loop. That is
		// why the subscribe path goes through a Supervisor
		// rather than a bare Subscriber. The Supervisor polls the bus
		// for liveness, and on a dead connection reconnects it (which
		// re-runs the GOSS token bootstrap) and resubscribes BOTH
		// simulation destinations, loudly, instead of leaving the
		// bridge alive-but-deaf.
		//
		// The probe destination is the per-simulation log topic: a
		// sibling of the output and input topics subscribed below, so
		// it carries no ACL risk the bridge is not already taking, and
		// nothing this bridge cares about is lost by churning a
		// subscription on it. See WithProbeDestination for why a
		// destination the broker would reject must not be used.
		subs := gridappsdclient.NewSupervisor(bus,
			gridappsdclient.WithProbeDestination(sim.LogTopic(cfg.SimulationID)))

		return runSimSide(runCtx, subs, embed, reg, cfg.SimulationID, &controlHook)
	}

	// adminSrv is the read only operator HTTP API. It is off by
	// default: adminui.New returns ErrDisabled when
	// SEP2_ADMIN_UI_KEY is unset, in which case no listener is opened and
	// no runner goroutine is started at all, matching the "off by
	// default" hard rule. Any other error from New (an invalid Addr, or
	// a non-loopback Addr without the explicit opt-in) is a genuine
	// startup failure, not the disabled state.
	// telemetryRun is the UP path: an independent timer-driven
	// publisher that reads the embed's DERStatus store and sends one
	// aggregate per interval. It is a peer of the embed and the admin UI,
	// not a hook inside the protocol request path, which is the whole
	// point of this layering: a 2030.5 PUT stores and returns, and nothing on
	// the platform side can make it fail, block, or slow down.
	//
	// Gated on SimulationID for the same reason the pump is: with no
	// simulation id there is no destination to publish to, and inventing
	// one would put frames on a topic nobody asked for. Nil then, exactly
	// like a disabled admin UI, so no goroutine is started at all.
	var telemetryRun func(context.Context) error
	if cfg.SimulationID == "" {
		log.Printf("bridge: no SEP2_SIMULATION_ID set; DERStatus telemetry publisher disabled")
	} else {
		pub, perr := telemetrypub.New(telemetryPublisherConfig(cfg, embed, bus))
		if perr != nil {
			return fmt.Errorf("telemetry publisher: %w", perr)
		}
		telemetryRun = pub.Run
	}

	var adminUIRun func(context.Context) error
	adminSrv, err := adminui.New(adminUIConfig(cfg), reg, embed, embed, &controlHook, embed, bus, &connHook)
	switch {
	case errors.Is(err, adminui.ErrDisabled):
		log.Printf("bridge: admin UI disabled, SEP2_ADMIN_UI_KEY unset")
	case err != nil:
		return fmt.Errorf("admin ui: %w", err)
	default:
		log.Printf("bridge: admin UI listening addr=%s", adminSrv.Addr())
		adminUIRun = adminSrv.Run
	}

	return runBridgeRunners(ctx, embed.Run, stompRun, adminUIRun, telemetryRun)
}

// runEmbedAndStomp runs the embedded IEEE 2030.5 server (embedRun) and
// the STOMP-side pump/idle loop (stompRun) concurrently under a single
// ctx derived from the caller's shutdown root, and blocks until both
// have finished.
//
// embedRun and stompRun are injectable seams, each shaped
// func(context.Context) error: run() passes embed.Run and a closure
// wrapping the SimulationID branch, but a unit test can pass stub
// runners to drive every branch below without a live broker or a real
// mTLS listener.
//
// runCtx is a child of ctx: SIGINT/SIGTERM cancelling ctx propagates to
// runCtx automatically. In addition, each side's goroutine cancels
// runCtx itself on exit (the embedRun goroutine's deferred cancelRun,
// and the explicit cancelRun call after stompRun returns below), so an
// early, independent failure on either side (a Serve error in the
// embed, a broker drop reaching the pump) tears the other down too
// rather than leaving it running orphaned until the next signal.
//
// Error precedence: a stompRun error is returned as-is unless embedRun
// also failed with something other than a graceful (context.Canceled)
// shutdown. If only embedRun failed, that error is returned wrapped. If
// both failed independently (neither error is a graceful shutdown),
// both are preserved via errors.Join rather than discarding one, so
// errors.Is/errors.As against either failure still matches.
func runEmbedAndStomp(ctx context.Context, embedRun, stompRun func(context.Context) error) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	embedErr := make(chan error, 1)
	go func() {
		defer cancelRun()
		embedErr <- embedRun(runCtx)
	}()

	stompErr := stompRun(runCtx)
	// Ask the embed side to stop even when stompRun returned on its own
	// (rather than via runCtx cancellation), so this function never
	// returns while the embed side is still active.
	cancelRun()

	// Wait for embedRun to actually finish (for the real embed: its own
	// listener Serve goroutine plus its subscription notifier's worker
	// pool; see sep2embed.Embed.Run) before this function returns, so
	// the caller never observes "runEmbedAndStomp returned" while the
	// embed is still tearing down.
	eerr := <-embedErr
	if eerr == nil || errors.Is(eerr, context.Canceled) {
		return stompErr
	}

	wrappedEmbedErr := fmt.Errorf("sep2 embed: %w", eerr)
	if stompErr == nil || errors.Is(stompErr, context.Canceled) {
		return wrappedEmbedErr
	}
	return errors.Join(stompErr, wrappedEmbedErr)
}

// runBridgeRunners runs the embedded IEEE 2030.5 server (embedRun) and
// the STOMP side (stompRun) concurrently, exactly as runEmbedAndStomp
// does, and additionally runs the read only admin UI (adminUIRun)
// concurrently with both when it is non-nil.
//
// adminUIRun is nil when the admin UI is disabled (SEP2_ADMIN_UI_KEY
// unset, see adminui.ErrDisabled handling in run()): in that case no
// third goroutine is started at all, and this function delegates
// entirely to runEmbedAndStomp, so the disabled path exercises the exact
// same, already tested two way combinator with no behavior change.
//
// When adminUIRun is supplied, an admin UI failure (Run returning a non
// graceful error) tears down both the embed and stomp sides the same way
// an embed or stomp failure already tears down the other, and a clean
// shutdown of embedRun/stompRun tears the admin UI down too: all three
// share one derived context, following the same cancel on any exit,
// join errors on independent failure pattern as runEmbedAndStomp.
// telemetryRun is nil when no simulation id is configured, in
// which case no publisher goroutine is started at all, exactly as a nil
// adminUIRun starts no admin goroutine. When supplied it is a peer of
// the other three: one shared derived context, cancel on any exit, join
// errors on independent failure.
func runBridgeRunners(ctx context.Context, embedRun, stompRun, adminUIRun, telemetryRun func(context.Context) error) error {
	if telemetryRun == nil {
		return runEmbedStompAdmin(ctx, embedRun, stompRun, adminUIRun)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	telErr := make(chan error, 1)
	go func() {
		defer cancelRun()
		telErr <- telemetryRun(runCtx)
	}()

	coreErr := runEmbedStompAdmin(runCtx, embedRun, stompRun, adminUIRun)
	// Ask the publisher to stop even when the other sides returned on
	// their own, so this function never returns while it is still
	// publishing.
	cancelRun()

	terr := <-telErr
	if terr == nil || errors.Is(terr, context.Canceled) {
		return coreErr
	}

	wrappedTelemetryErr := fmt.Errorf("telemetry publisher: %w", terr)
	if coreErr == nil || errors.Is(coreErr, context.Canceled) {
		return wrappedTelemetryErr
	}
	return errors.Join(coreErr, wrappedTelemetryErr)
}

// runEmbedStompAdmin is the embed plus stomp plus admin UI combinator
// runBridgeRunners builds on. See runBridgeRunners' doc comment for the
// admin UI's nil-disabled contract and the shared teardown semantics.
func runEmbedStompAdmin(ctx context.Context, embedRun, stompRun, adminUIRun func(context.Context) error) error {
	if adminUIRun == nil {
		return runEmbedAndStomp(ctx, embedRun, stompRun)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	adminErr := make(chan error, 1)
	go func() {
		defer cancelRun()
		adminErr <- adminUIRun(runCtx)
	}()

	coreErr := runEmbedAndStomp(runCtx, embedRun, stompRun)
	// Ask the admin UI to stop even when the embed/stomp side returned
	// on its own, so this function never returns while the admin UI is
	// still serving.
	cancelRun()

	aerr := <-adminErr
	if aerr == nil || errors.Is(aerr, context.Canceled) {
		return coreErr
	}

	wrappedAdminErr := fmt.Errorf("admin ui: %w", aerr)
	if coreErr == nil || errors.Is(coreErr, context.Canceled) {
		return wrappedAdminErr
	}
	return errors.Join(coreErr, wrappedAdminErr)
}

// buildSEP2Policy assembles the runtime SEP2Policy from the compiled-in
// defaults plus any operator-supplied registration PIN configuration,
// then validates every configured PIN before returning.
//
// cfg.SEP2RegistrationPIN (from -sep2-registration-pin) becomes the
// fleet-wide DefaultRegistrationPIN fallback; cfg.SEP2RegistrationPINs
// (from -sep2-registration-pin-file) becomes the per-device
// RegistrationPINs map. Both may be set together: SEP2Policy's own
// ResolveRegistrationPIN (unchanged by this function) already implements
// the precedence, a per-device entry wins over the fleet default, so
// this function does not reimplement it. Neither flag set is exactly
// sep2config.DefaultPolicy(): both fields stay nil/unset, so the bridge
// still refuses to seed any device with no PIN configured, matching
// today's fail-closed default.
//
// This is the single point where cfg's PIN fields reach the policy
// ResolveRegistrationPIN reads at seeding time. It is called from run,
// before connectClient, before bootstrapRegistry, and before
// newSEP2Embed, so a PIN failing ValidateRegistrationPIN's range or
// IEEE 2030.5 section 6.3.5 check-digit rule stops the bridge before it
// dials anything, let alone before it serves a device: not lazily at
// first device-seeding time.
// cfg.SEP2PollRate and cfg.SEP2PostRate (from -sep2-poll-rate and
// -sep2-post-rate) become the fleet-wide DefaultPollRate and
// DefaultPostRate. Both stay nil when their flag is absent, which leaves
// the bridge advertising no rate at all: a seeded Registration omits its
// optional pollRate attribute and a client-created MirrorUsagePoint keeps
// whatever postRate the client sent. The per-device PollRates/PostRates
// maps are deliberately left nil here: no operator surface populates them
// yet, and SEP2Policy's resolvers already implement the precedence, so
// wiring them later is a change to this function and nothing downstream.
//
// ValidateRates runs alongside ValidateRegistrationPIN, in the same
// before-we-dial-anything window, so a 0-second rate stops the bridge at
// boot rather than surfacing as a client hammering the server.
func buildSEP2Policy(cfg config) (sep2config.SEP2Policy, error) {
	policy := sep2config.DefaultPolicy()
	policy.DefaultRegistrationPIN = cfg.SEP2RegistrationPIN
	policy.RegistrationPINs = cfg.SEP2RegistrationPINs
	policy.DefaultPollRate = cfg.SEP2PollRate
	policy.DefaultPostRate = cfg.SEP2PostRate

	// Assigned only when configured, so an absent flag or file member
	// leaves DefaultPolicy's compiled-in value in place. Both are legal
	// values in their own right (primacy 0 is the highest priority, an
	// empty description marshals as absent), which is why nil rather than
	// the zero value is the "operator said nothing" signal.
	if cfg.SEP2ProgramPrimacy != nil {
		policy.DefaultProgram.Primacy = *cfg.SEP2ProgramPrimacy
	}
	if cfg.SEP2ProgramDescription != nil {
		policy.DefaultProgram.Description = *cfg.SEP2ProgramDescription
	}

	// Same assigned-only-when-configured discipline, for the same reason: 0
	// is the shipped randomizeDuration, so a bare value could not be told
	// from "operator said nothing" and would silently look configured.
	if cfg.SEP2ControlDuration != nil {
		policy.DERControl.Duration = *cfg.SEP2ControlDuration
	}
	if cfg.SEP2ControlRandomizeDuration != nil {
		policy.DERControl.RandomizeDuration = *cfg.SEP2ControlRandomizeDuration
	}

	// The DefaultDERControl is FILLED, not replaced. Each configured member
	// is written onto the compiled-in control's own DERControlBase, so a
	// file naming one member leaves the other at its shipped value instead
	// of clearing it. DefaultPolicy always supplies a non-nil base
	// (DERControlBase is minOccurs=1 on DefaultDERControl), but this checks
	// rather than assumes: a nil base here would panic on the first
	// assignment, and the failure would be a crash at boot for an operator
	// who did nothing wrong.
	if cfg.SEP2DefaultControlOpModConnect != nil || cfg.SEP2DefaultControlOpModEnergize != nil || cfg.SEP2DefaultControlOpModMaxLimW != nil {
		if policy.DefaultControl.DERControlBase == nil {
			policy.DefaultControl.DERControlBase = &sep2.DERControlBase{}
		}
		if cfg.SEP2DefaultControlOpModConnect != nil {
			policy.DefaultControl.DERControlBase.OpModConnect = cfg.SEP2DefaultControlOpModConnect
		}
		if cfg.SEP2DefaultControlOpModEnergize != nil {
			policy.DefaultControl.DERControlBase.OpModEnergize = cfg.SEP2DefaultControlOpModEnergize
		}
		if cfg.SEP2DefaultControlOpModMaxLimW != nil {
			maxLimW := sep2.PerCent(*cfg.SEP2DefaultControlOpModMaxLimW)
			policy.DefaultControl.DERControlBase.OpModMaxLimW = &maxLimW
		}
	}

	if err := policy.ValidateRegistrationPIN(); err != nil {
		return sep2config.SEP2Policy{}, err
	}
	if err := policy.ValidateRates(); err != nil {
		return sep2config.SEP2Policy{}, err
	}
	// Same before-we-dial-anything window as the two above: an over-length
	// description or a reserved primacy stops the bridge at boot rather
	// than reaching a client as a document it refuses to parse.
	if err := policy.ValidateDefaultProgram(); err != nil {
		return sep2config.SEP2Policy{}, err
	}
	// Same window again. An unusable interval policy must stop the bridge
	// here rather than at the first control delta, because a control served
	// with a zero-length interval is discarded by the client silently: it
	// still fetches, parses and acknowledges, so nothing downstream reports
	// a failure and the only symptom is a device that never moves.
	if err := policy.ValidateDERControl(); err != nil {
		return sep2config.SEP2Policy{}, err
	}
	return policy, nil
}

// ccmObservationDisabled reports whether the connection observer is
// disabled for cfg: today exactly cfg.SEP2EnableCCM, but named as its
// own function so sep2EmbedConfig's decision to pass a nil Observer and
// adminUIConfig's ObservationDisabled field can never answer this
// question differently. With each caller asserting cfg.SEP2EnableCCM
// independently, a second disabling condition added to sep2EmbedConfig
// alone would leave the full suite green; deriving both from this one
// function closes that by construction, and
// TestSEP2EmbedConfigObservationAgreesWithAdminUI below pins the two
// projections' agreement directly as well.
func ccmObservationDisabled(cfg config) bool {
	return cfg.SEP2EnableCCM
}

// logCCMObserverDisabledChoice logs the operator's explicit choice to
// run SEP2_ENABLE_CCM with the connection observer disabled. Split out
// from run() so its content, and the condition that gates it, can be
// asserted by a unit test without starting a bridge, the same pattern
// sep2EmbedConfig and adminUIConfig follow above. A no-op when
// ccmObservationDisabled(cfg) is false. cfg.validate (called from
// loadConfig, before run ever starts) already refused to reach this
// call unless SEP2CCMAllowNoObserver is also set: this log records
// that the operator made that choice explicitly, not that the bridge
// made it for them.
func logCCMObserverDisabledChoice(cfg config) {
	if !ccmObservationDisabled(cfg) {
		return
	}
	log.Printf("bridge: SEP2_ENABLE_CCM is set with SEP2_CCM_ALLOW_NO_OBSERVER: serving ONLY TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 (a client unable to offer it is refused, not served over GCM, and the refusal reaches this log but not the admin UI), with the connection observer disabled as explicitly accepted (the admin UI panel's served-status table will show every served device's status as unknown, not connected or disconnected, and its connected-clients and handshake-attempts tables will show no data rather than admit they cannot tell, until GRIDAPPSD/ieee-2030_5-server-go#583 adds handshake observation for the CCM-8 listener)")
}

// sep2EmbedConfig projects the bridge's config onto sep2embed.Config.
// Split out from newSEP2Embed so the address/cert-dir mapping can be
// asserted by a unit test without minting real certificate material or
// binding a listener.
//
// No bus, destination or simulation id is threaded here. The
// embedded 2030.5 server stores DERStatus and stops there; the
// GridAPPS-D publish is internal/telemetrypub's, driven by its own
// timer off that store. See telemetryPublisher below for that wiring.
//
// policy is threaded through as sep2embed.Config.DefaultControl:
// the fallback DefaultDERControl this bridge seeds onto
// every DERProgram is sourced from policy.DefaultControl, never
// hardcoded at this layer. policy.ModesSupported is threaded through the
// same way: the DERControlType bitmap seeded onto every
// device's DERCapability is sourced from policy, never hardcoded here;
// DefaultPolicy leaves it nil, so seeding is nil-safe until a real
// policy value is configured.
//
// policy.ResolveRegistrationPIN and policy.DefaultPollRate are threaded
// through as ResolveRegistrationPIN and RegistrationPollRate, the two
// policy inputs to
// the Registration resource seeded for every device. Both are nil under
// DefaultPolicy: a nil PIN selects the stable per-device value derived
// from that device's own LFDI (the production default), and a nil poll
// rate omits the optional pollRate attribute so the client applies the
// schema's own 900-second default. Neither is hardcoded at this layer,
// and the PIN is never logged.
//
// connHook is threaded through as sep2embed.Config.Observer:
// a non-nil connHook opts this bridge into the
// additive request- and handshake-observation path sep2embed.New
// builds when Config.Observer is set. cmd/bridge passes its own
// non-nil *connobs.Hook (connHook) UNLESS cfg.SEP2EnableCCM is set: New
// still refuses Config.Observer and Config.EnableCCM together
// (errObserverRequiresGCM), but only as configuration surface now, not
// because the CCM-8 listener lacks a seam; see issue 127, which removes
// both SEP2EnableCCM and this refusal. By the time this runs,
// cfg has already passed validate (called from loadConfig, before run
// ever starts), so SEP2EnableCCM true here implies
// SEP2CCMAllowNoObserver is also true: validate refuses to start
// otherwise, rather than let the observer's loss be implied by
// SEP2EnableCCM alone. run logs the operator's explicit choice whenever
// it takes this branch. A nil connHook is also exercised directly by
// sep2embed's own tests that leave Config.Observer unset.
func sep2EmbedConfig(cfg config, policy sep2config.SEP2Policy, connHook *connobs.Hook, mode sep2embed.DeviceCertMode) sep2embed.Config {
	observer := connHook
	if ccmObservationDisabled(cfg) {
		observer = nil
	}
	return sep2embed.Config{
		Addr:    cfg.SEP2ServerAddr,
		CertDir: cfg.SEP2ServerCertDir,
		// The SAME mode bootstrapRegistry sourced device certs with, by
		// construction: run parses it once and passes that one value to
		// both. It decides which files the server's own identity set must
		// contain, and a preprovisioned deployment correctly withholds the
		// CA private key, so passing DevMint here against such a directory
		// would make sep2embed.New refuse to start (it is the stricter
		// required set). Threading the real mode is what keeps a
		// preprovisioned bridge startable and its material untouched.
		DeviceCertMode: mode,
		DefaultControl: policy.DefaultControl,
		// Projected field by field rather than by struct conversion: the
		// two types are deliberately separate (sep2config knows nothing
		// about stores), and naming the fields keeps that a real boundary
		// instead of one that silently depends on declaration order.
		DefaultProgram: sep2embed.DERProgramSeed{
			Primacy:     policy.DefaultProgram.Primacy,
			Description: policy.DefaultProgram.Description,
		},
		// Field by field for the same reason, and Now is left nil so the
		// bridge reads the real clock; only tests supply one.
		DERControl: sep2embed.DERControlSeed{
			Duration:          policy.DERControl.Duration,
			RandomizeDuration: policy.DERControl.RandomizeDuration,
		},
		ModesSupported:         policy.ModesSupported,
		ResolveRegistrationPIN: policy.ResolveRegistrationPIN,
		// Resolvers, not the bare DefaultPollRate/DefaultPostRate fields:
		// the consumer must not be able to tell a fleet default from a
		// per-device override, so populating SEP2Policy's per-device maps
		// later changes nothing here or downstream of here.
		ResolveRegistrationPollRate: policy.ResolvePollRate,
		ResolvePostRate:             policy.ResolvePostRate,
		Observer:                    observer,
		EnableCCM:                   cfg.SEP2EnableCCM,
		NotifyAllowLoopback:         cfg.SEP2NotificationAllowLoopback,
	}
}

// adminUIConfig projects the bridge's config onto adminui.Config. Split
// out from run() so the field mapping can be asserted by a unit test
// with no listener bound and no admin token required.
//
// ObservationDisabled is ccmObservationDisabled(cfg), the SAME function
// sep2EmbedConfig's "observer = nil" branch calls above: that is the one
// and only branch where the connection observer never reaches the
// listener, so /api/clients can never report a real client either.
func adminUIConfig(cfg config) adminui.Config {
	return adminui.Config{
		Addr:                cfg.SEP2AdminUIAddr,
		AllowNonLoopback:    cfg.SEP2AdminUIAllowNonLoopback,
		Key:                 cfg.SEP2AdminUIKey,
		AllowedHosts:        cfg.SEP2AdminUIAllowedHosts,
		FeederMRID:          cfg.FeederMRID,
		SimulationID:        cfg.SimulationID,
		SORLink:             cfg.SEP2AdminUISORLink,
		ObservationDisabled: ccmObservationDisabled(cfg),
	}
}

// newSEP2Embed builds, seeds, and binds the in-process IEEE 2030.5
// protocol server from the bridge's registry. It does not start
// serving; the caller starts embed.Run once this returns successfully.
func newSEP2Embed(ctx context.Context, cfg config, reg *registry.Registry, policy sep2config.SEP2Policy, connHook *connobs.Hook, mode sep2embed.DeviceCertMode) (*sep2embed.Embed, error) {
	return sep2embed.New(ctx, sep2EmbedConfig(cfg, policy, connHook, mode), reg)
}

// telemetryPublisherConfig projects the bridge's config onto
// telemetrypub.Config: the timer-driven GridAPPS-D publisher that reads
// the embedded 2030.5 server's DERStatus store and sends one aggregate
// per interval. Split out from run() so the field mapping can
// be asserted by a unit test with no broker and no listener.
//
// Destination is the ONLY place this bridge names the telemetry topic.
// It reuses internal/cim/sim.InputTopic, exactly as the removed per-PUT
// relay did, which is a synthetic simulation id supplied by the
// operator, never a real platform simulation and never a
// goss.gridappsd.process.* destination. The agreed eventual target is an
// application output topic; changing it is this one line, because
// nothing inside telemetrypub derives or inspects the destination.
//
// Build likewise names the wire shape in exactly one place. It is the
// diff envelope today (identical per device to what the per-PUT relay
// published); the agreed eventual target is CIM AnalogValue, which is a
// different MessageBuilder passed here and nothing else.
//
// PublishUnchanged comes straight from -sep2-telemetry-publish-unchanged
// and is the single switch that turns unchanged-device suppression off.
func telemetryPublisherConfig(cfg config, src telemetrypub.StatusSource, bus telemetrypub.BusPublisher) telemetrypub.Config {
	return telemetrypub.Config{
		Source:           src,
		Bus:              bus,
		Destination:      sim.InputTopic(cfg.SimulationID),
		Build:            telemetrypub.DiffMessageBuilder(cfg.SimulationID),
		Interval:         cfg.SEP2TelemetryInterval,
		PublishUnchanged: cfg.SEP2TelemetryPublishUnchanged,
	}
}

// busConfig projects the bridge's config onto gridappsd-go's connection
// config. Split out from connectClient so the mapping, in particular
// the plaintext opt-in, can be asserted by a unit test without dialing
// a broker.
func busConfig(cfg config) gridappsd.Config {
	return gridappsd.Config{
		Address:        cfg.STOMPAddr,
		User:           cfg.STOMPUser,
		Password:       cfg.STOMPPassword,
		AllowPlaintext: cfg.AllowPlaintext,
	}
}

// connectClient dials the GridAPPS-D broker, runs the two-step GOSS
// token bootstrap, and returns a connected fieldbus.MessageBus. The
// connect uses its own timeout so a stuck platform fails fast rather
// than hanging on the caller's parent ctx.
//
// Transport selection is entirely cfg.AllowPlaintext's call: the zero
// value dials TLS against the system trust store (gridappsd.Config's
// fail-closed default), matching this bridge's own config default.
// cfg.AllowPlaintext must be set explicitly to reach a plaintext
// broker such as gridappsd-docker's dev stack.
func connectClient(ctx context.Context, cfg config) (fieldbus.MessageBus, error) {
	log.Printf("bridge: connecting to %s as %s", cfg.STOMPAddr, cfg.STOMPUser)

	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	bus := fieldbus.New(busConfig(cfg))
	if err := bus.Connect(cctx); err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.STOMPAddr, err)
	}
	log.Printf("bridge: connected; auth token bootstrapped")
	return bus, nil
}

// deviceCertMode maps the bridge's SEP2DeviceCertMode config string onto
// sep2embed.DeviceCertMode. config.validate already restricts the
// stored string to deviceCertModeDevMintFlag or
// deviceCertModePreprovisionedFlag, so the default branch below should
// be unreachable once loadConfig has run; it still returns an error
// rather than silently picking a mode, matching the fail-closed
// posture the preprovisioned mode itself is for.
// fmtU32Ptr renders a *uint32 as its decimal value, or "unset" for nil.
// sep2config.SEP2Policy's DefaultPollRate/DefaultPostRate are
// pointer-typed precisely so a real 0 is distinguishable from unset;
// this keeps that distinction visible in the boot log too.
func fmtU32Ptr(v *uint32) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *v)
}

// fmtDERControlTypePtr renders a *sep2.DERControlType (the core's
// hexBinary bitmap family) as hex, or "unset" for nil, matching how the
// value actually appears on the wire (sep2.HexBinary32.MarshalXML's
// uppercase, minimal even-padded hex form) rather than decimal. This is
// a debug log line only: it is not itself part of any wire encoding, but
// printing it in decimal would make it harder to cross-reference against
// a served DERCapability document while debugging.
func fmtDERControlTypePtr(v *sep2.DERControlType) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%X", uint32(*v))
}

// parsePECCount extracts the single "count" binding QueryPECCount's
// SPARQL template returns. It returns (0, false) for every shape other
// than exactly one row with a parsable integer "count" binding: a nil
// result, zero rows, a missing binding, or an unparsable value. Zero
// itself is a valid, present count and returns (0, true); callers must
// use the ok return, not a zero check, to distinguish "the feeder has
// zero PECs" from "the count could not be determined".
func parsePECCount(res *cim.QueryDataResult) (int, bool) {
	if res == nil || len(res.Results.Bindings) != 1 {
		return 0, false
	}
	binding, ok := res.Results.Bindings[0]["count"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(binding.Value)
	if err != nil {
		return 0, false
	}
	return n, true
}

// pecCountLogLine renders the discover-vs-project drop-visibility
// message for feederMRID, given the QueryPECCount discovery result
// (discovered, discoveredOK) and the post-dedupe projected device count
// projected. It returns the message text and whether the caller should
// log it at WARNING level.
//
//   - discoveredOK && discovered == projected: counts match; INFO-level,
//     "no drops" text, no WARNING.
//   - discoveredOK && discovered > projected: some PECs were silently
//     dropped by the enumeration queries' mandatory attribute joins;
//     WARNING-level, states both counts and the drop count.
//   - discoveredOK && discovered < projected: some PowerElectronicsConnection
//     objects were counted MORE than once by the enumeration queries
//     (a PowerElectronicsUnit child bound under a different mRID than
//     its own parent PowerElectronicsConnection produces a second,
//     spurious device identity for the same physical converter);
//     WARNING-level, states both counts and the over-projection count.
//     This branch used to be silently folded into the counts-match path,
//     which let a 9-PEC feeder mint 18 devices while logging "no drops."
//     Over-projection is now surfaced, not clamped away.
//   - !discoveredOK: the true discovered count could not be determined
//     (QueryPECCount failed, or returned an unparsable/missing count);
//     WARNING-level, states the limitation rather than fabricating a
//     drop or over-projection count of 0.
func pecCountLogLine(feederMRID string, discovered int, discoveredOK bool, projected int) (string, bool) {
	if !discoveredOK {
		return fmt.Sprintf(
			"feeder %s: could not determine the true discovered PowerElectronicsConnection count; "+
				"projected %d device(s) from the enumeration queries, but cannot confirm whether any were dropped or over-projected",
			feederMRID, projected), true
	}
	switch {
	case discovered == projected:
		return fmt.Sprintf(
			"feeder %s: discovered %d PowerElectronicsConnection object(s), projected %d device(s); no drops",
			feederMRID, discovered, projected), false
	case discovered > projected:
		dropped := discovered - projected
		return fmt.Sprintf(
			"feeder %s: discovered %d PowerElectronicsConnection object(s) but only %d were fully projected as devices (%d dropped for missing mandatory attributes)",
			feederMRID, discovered, projected, dropped), true
	default:
		over := projected - discovered
		return fmt.Sprintf(
			"feeder %s: discovered %d PowerElectronicsConnection object(s) but %d were projected as devices (%d over-projected; a PowerElectronicsConnection likely surfaced under more than one identity)",
			feederMRID, discovered, projected, over), true
	}
}

func deviceCertMode(s string) (sep2embed.DeviceCertMode, error) {
	switch s {
	case deviceCertModeDevMintFlag:
		return sep2embed.DeviceCertModeDevMint, nil
	case deviceCertModePreprovisionedFlag:
		return sep2embed.DeviceCertModePreprovisioned, nil
	default:
		return 0, fmt.Errorf("config: unknown SEP2_DEVICE_CERT_MODE %q (want %q or %q)",
			s, deviceCertModeDevMintFlag, deviceCertModePreprovisionedFlag)
	}
}

// bootstrapRegistry runs the three PowerElectronicsConnection (PEC)
// enumeration queries against the feeder, dedupes by mRID (a single
// device may surface in multiple queries when the upstream filter is
// open), then (issue #115) queries the feeder's EnergyConsumers to add
// house loads (found structurally, by their c:House link) and
// caller-supplied utility battery legs (found by name nowhere; each
// entry in batteryLegs is checked against this same EnergyConsumer
// query before it is trusted). It derives every device's real IEEE
// 2030.5 identity from its certificate (spec sections 6.3.4 LFDI /
// 6.3.3 SFDI, via sep2embed.EnsureDeviceIdentities) and populates a
// fresh registry from the result. Returns the populated registry; the
// caller does not need a separate add step.
//
// batteryLegs is cfg.SEP2BatteryLegs: nil or empty means no utility
// battery legs are configured, in which case this function adds only
// whatever house loads the model itself carries (see
// projectEnergyConsumers and resolveBatteryLegs below for the model
// checks each entry must pass).
//
// certDir and mode are threaded straight through to
// EnsureDeviceIdentities: certDir is cfg.SEP2ServerCertDir, the SAME
// directory the embedded server's own CA and leaf material live under
// (device certs are signed by that same CA; see
// sep2embed.EnsureDeviceIdentities's doc comment for why this must run
// before sep2embed.New's own load-or-create call against the same
// dir). mode selects dev-mint vs fail-closed preprovisioned sourcing.
func bootstrapRegistry(ctx context.Context, c *cim.Client, feederMRID, certDir string, mode sep2embed.DeviceCertMode, batteryLegs []string) (*registry.Registry, error) {
	log.Printf("bridge: querying CIM feeder %s", feederMRID)

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	inverters, err := queryDevices(qctx, "inverter", c.QueryInverter, feederMRID)
	if err != nil {
		return nil, err
	}
	solar, err := queryDevices(qctx, "solar", c.QuerySolar, feederMRID)
	if err != nil {
		return nil, err
	}
	battery, err := queryDevices(qctx, "battery", c.QueryBattery, feederMRID)
	if err != nil {
		return nil, err
	}

	log.Printf("bridge: queried CIM feeder %s; %d inverters / %d solar / %d battery",
		feederMRID, len(inverters), len(solar), len(battery))

	// Dedupe across the three lists, keyed on d.MRID (the
	// PowerElectronicsConnection's own ?pecid). A single PEC
	// shows up under QueryInverter (open filter) and QuerySolar because
	// both bind the same child PhotovoltaicUnit; the registry must only
	// carry one entry per PEC. Anchoring d.MRID on ?pecid rather than the
	// COALESCE(unitID, pecid) ?id binding is what makes this collapse
	// correct: a PEC with a bound child Unit otherwise produces a
	// unit-mRID identity from the inverter/solar rows and a separate
	// pecid-mRID identity from the battery row (whose Unit-type filter
	// never matched a PhotovoltaicUnit), so the same physical converter
	// dedupe-collapsed to TWO registry entries instead of one.
	seen := make(map[string]struct{})
	var devices []cimDevice
	for _, src := range [][]cimDevice{inverters, solar, battery} {
		for _, d := range src {
			if d.MRID == "" {
				continue
			}
			if _, dup := seen[d.MRID]; dup {
				continue
			}
			seen[d.MRID] = struct{}{}
			devices = append(devices, d)
		}
	}

	// Without this line, a reader sees the two log
	// lines "N inverters / N solar / N battery" above and "registry
	// populated: N entries" below and does the (wrong) math of summing
	// the three counts, expecting 3N. The three queries use open
	// filters, so the same PEC can legitimately appear in more than one
	// list (e.g. a PhotovoltaicUnit satisfies both QueryInverter's and
	// QuerySolar's filter); dedupe above collapses those overlaps down
	// to one entry per unique mRID. Spell that out here so the two
	// surrounding counts read as consistent rather than contradictory.
	log.Printf("bridge: deduped by mRID across inverter/solar/battery queries (open filters can overlap); %d unique devices",
		len(devices))

	// This is the empty-fleet fail-loud guard. Model-only device
	// discovery (queryDevices above) finds devices exclusively via
	// PowerElectronicsConnection (PEC) CIM objects. A load-modeled
	// feeder (DERs represented as named EnergyConsumer loads instead of
	// PECs) returns zero rows from every one of the three queries above
	// with NO error: the SPARQL succeeds, it just has nothing to bind.
	// Left unchecked, that flows straight through dedupe to an empty
	// devices slice, an empty registry, and a bridge that boots and
	// serves an empty /edev list looking perfectly healthy. Fail here,
	// loudly, naming the actual feeder queried and the actual finding
	// (zero PECs), so an operator pointed at a load-modeled feeder sees
	// why immediately instead of debugging a silently-empty fleet. This
	// deliberately does not fall back to an EnergyConsumer-regex scan;
	// that fleet-discovery path was excluded by design and this guard
	// only makes the model-only consequence visible, not reversed.
	if len(devices) == 0 {
		return nil, fmt.Errorf(
			"bridge: feeder %s: found zero PowerElectronicsConnection objects across the inverter/solar/battery queries; "+
				"this bridge discovers its DER fleet exclusively via PowerElectronicsConnection CIM objects, "+
				"so a feeder with DERs modeled as EnergyConsumer loads instead of PowerElectronicsConnection objects "+
				"(a load-modeled feeder) will boot with an empty fleet and no other error; "+
				"confirm the feeder's DER modeling shape if this is unexpected",
			feederMRID)
	}

	// Discover-vs-project drop visibility (Cyrus's finding).
	// The three device-enumeration queries above use several mandatory
	// (INNER-join-equivalent) attribute triples (ratedS, ratedU,
	// maxIFault, p, q, plus the Terminal/ConnectivityNode bus lookup);
	// any PEC missing one of those attributes is silently dropped from
	// their row sets rather than surfaced as a partial row. QueryPECCount
	// runs a second, minimal SPARQL count (identity plus feeder-membership
	// triples only, see internal/cim/queries.go) so the true discovered
	// count is available independent of that INNER-join risk; comparing
	// it against the post-dedupe projected count surfaces exactly what
	// those joins silently dropped. This is a genuinely extra query
	// (there is no way to derive a reliable pre-join count from the
	// enumeration queries' own results, since those results are the
	// thing with the drops); it is kept minimal by design so its own
	// query cost is negligible next to the three enumeration queries
	// already issued above. A failure to run it degrades to a WARN
	// noting the comparison is unavailable rather than failing the
	// whole bootstrap: the enumeration queries above already succeeded
	// and produced a non-empty, usable fleet, so a problem with this
	// purely-diagnostic second query should not block boot.
	//
	// QueryPECCount shares qctx (and its remaining queryTimeout budget)
	// with the three enumeration queries above rather than getting its
	// own fresh timeout window; a slow broker can leave this fourth,
	// purely-diagnostic query starved of time. Accepted as-is: a
	// dedicated budget would need its own constant and context, which
	// is more machinery than a diagnostic-only query warrants.
	pecCountRes, pecCountErr := c.QueryPECCount(qctx, feederMRID)
	discovered, discoveredOK := parsePECCount(pecCountRes)
	if pecCountErr != nil {
		discoveredOK = false
		log.Printf("bridge: WARNING: feeder %s: QueryPECCount failed, discover-vs-project drop visibility unavailable: %v",
			feederMRID, pecCountErr)
	}
	if msg, warn := pecCountLogLine(feederMRID, discovered, discoveredOK, len(devices)); warn {
		log.Printf("bridge: WARNING: %s", msg)
	} else {
		log.Printf("bridge: %s", msg)
	}

	// EnergyConsumer devices (issue #115): the PEC-only discovery above
	// finds neither the feeder's house loads nor its utility battery
	// legs, since both are modeled as EnergyConsumer, not
	// PowerElectronicsConnection. This runs, and can add house devices,
	// even when batteryLegs is empty: houses are found by model
	// structure, not by the caller-supplied list. It runs strictly after
	// the PEC drop-check above so that check's len(devices) stays
	// PEC-sourced only, per issue #115's done-when.
	ecRes, err := c.QueryEnergyConsumers(qctx, feederMRID)
	if err != nil {
		return nil, fmt.Errorf("query energy consumers: %w", err)
	}
	houseDevices, feederECs, err := projectEnergyConsumers(ecRes)
	if err != nil {
		return nil, err
	}
	legDevices, err := resolveBatteryLegs(batteryLegs, feederECs)
	if err != nil {
		return nil, fmt.Errorf("bridge: feeder %s: %w", feederMRID, err)
	}
	log.Printf("bridge: feeder %s: %d house load(s), %d utility battery leg(s) from EnergyConsumer discovery",
		feederMRID, len(houseDevices), len(legDevices))
	devices = append(devices, houseDevices...)
	devices = append(devices, legDevices...)

	mrids := make([]string, len(devices))
	for i, d := range devices {
		mrids[i] = d.MRID
	}

	identities, err := sep2embed.EnsureDeviceIdentities(certDir, mode, mrids)
	if err != nil {
		return nil, fmt.Errorf("device identities: %w", err)
	}

	entries := make([]registry.Entry, 0, len(devices))
	for _, d := range devices {
		id, ok := identities[d.MRID]
		if !ok {
			// EnsureDeviceIdentities is contracted to return an identity
			// for every mRID it was given, or a single overall error;
			// this branch should be unreachable, but fail loudly rather
			// than silently seeding a device with an empty LFDI (which
			// registry.AddBatch would reject anyway, with a far less
			// actionable error than this one).
			return nil, fmt.Errorf("device identities: no identity returned for mRID %q", d.MRID)
		}
		entries = append(entries, registry.Entry{
			MRID:        d.MRID,
			Name:        d.Name,
			LFDI:        id.LFDI,
			SFDI:        id.SFDI,
			Placeholder: false,
			MaxQ:        d.MaxQ,
		})
		log.Printf("bridge: device mrid=%s lfdi=%s sfdi=%s (certificate-derived)", d.MRID, id.LFDI, id.SFDI)
	}

	reg := registry.New()
	if err := reg.AddBatch(entries); err != nil {
		return nil, fmt.Errorf("registry populate: %w", err)
	}
	log.Printf("bridge: registry populated: %d entries (LFDI/SFDI certificate-derived per spec 6.3.4/6.3.3)",
		reg.Len())
	return reg, nil
}

// ecRow is one EnergyConsumer's projection from
// cim.Client.QueryEnergyConsumers: its display name and whether it is a
// house load, per that query's "house" binding.
type ecRow struct {
	Name    string
	IsHouse bool
}

// projectEnergyConsumers projects res (a QueryEnergyConsumers result)
// into the feeder's house-load devices and a lookup of every
// EnergyConsumer the query found, keyed by mRID. houses is deduped by
// construction: QueryEnergyConsumers already GROUPs by ?ecid, so each
// mRID here binds at most once, and a row with an empty mRID (a
// malformed or missing ?ecid binding) is skipped the same way
// queryDevices skips an empty ?pecid. A repeated mRID across rows (the
// model binding the same EC under two different names) is a data error
// this function refuses rather than silently picking one.
func projectEnergyConsumers(res *cim.QueryDataResult) (houses []cimDevice, ecs map[string]ecRow, err error) {
	ecs = make(map[string]ecRow)
	if res == nil {
		return nil, ecs, nil
	}
	for _, row := range res.Results.Bindings {
		mrid := row["ecid"].Value
		if mrid == "" {
			continue
		}
		if _, dup := ecs[mrid]; dup {
			return nil, nil, fmt.Errorf("query energy consumers: mRID %q returned more than one row", mrid)
		}
		r := ecRow{Name: row["ecname"].Value, IsHouse: row["house"].Value != ""}
		ecs[mrid] = r
		if r.IsHouse {
			houses = append(houses, cimDevice{MRID: mrid, Name: r.Name})
		}
	}
	return houses, ecs, nil
}

// resolveBatteryLegs checks every mRID in legs against feederECs (the
// feeder's EnergyConsumers, from projectEnergyConsumers) and returns
// one cimDevice per leg, in legs' own order. A leg mRID absent from
// feederECs is not an EnergyConsumer on the configured feeder; a leg
// mRID present but flagged IsHouse is also a house load. Either stops
// boot, naming the offending mRID, per issue #115's done-when. legs is
// assumed already deduplicated against itself: loadBatteryLegListFile
// rejects a repeated line at config load, so no duplicate check runs
// here.
func resolveBatteryLegs(legs []string, feederECs map[string]ecRow) ([]cimDevice, error) {
	if len(legs) == 0 {
		return nil, nil
	}
	out := make([]cimDevice, 0, len(legs))
	for _, mrid := range legs {
		r, ok := feederECs[mrid]
		if !ok {
			return nil, fmt.Errorf("battery leg mRID %q is not an EnergyConsumer on the configured feeder", mrid)
		}
		if r.IsHouse {
			return nil, fmt.Errorf("battery leg mRID %q is also a house load", mrid)
		}
		out = append(out, cimDevice{MRID: mrid, Name: r.Name})
	}
	return out, nil
}

// cimDevice is the slim projection of a SPARQL binding row this bridge
// needs at Stage 1: identity, name, and MaxQ,
// the one PowerElectronicsConnection rated-maximum value that has a
// model-correct target in the vendored core library's DERCapability
// type (RTGMaxVar). Other richer attributes (ratedS, ratedU, phases)
// stay in the raw QueryDataResult and can be lifted into typed structs
// when downstream code consumes them.
//
// MRID is anchored on ?pecid, the PowerElectronicsConnection's
// own mRID, never on the optional child PowerElectronicsUnit's mRID. A
// PowerElectronicsUnit (PhotovoltaicUnit, BatteryUnit) has no Terminal
// and no ConnectivityNode attachment in CIM100: it is not the
// ConductingEquipment GridAPPS-D applies p/q setpoints to and it cannot
// execute an IEEE 2030.5 opModConnect or DERControlBase target. The
// PowerElectronicsConnection is. Anchoring identity there is what makes
// dedupe (see bootstrapRegistry) collapse a PEC's inverter/solar/battery
// query rows down to exactly one device, regardless of whether an
// OPTIONAL child-Unit block happened to bind on that row.
type cimDevice struct {
	MRID string // CIM PowerElectronicsConnection.mRID (?pecid): the IEEE 2030.5 device identity anchor and the dedupe key
	// UnitMRID is the raw ?id binding (COALESCE(unitID, pecid)) queries.go
	// selects: the child PowerElectronicsUnit's mRID when one bound on
	// this row, or MRID again when it did not. Retained as a
	// display/reference label only; it is never used for identity,
	// dedupe, or certificate minting.
	UnitMRID string
	Name     string
	MaxQ     *int64 // CIM PowerElectronicsConnection.maxQ, base VAr, rounded from the CIM float; nil when the binding is absent
}

// queryDevices runs one of the cim.Client Query* wrappers, projects each
// binding row down to a cimDevice, and skips rows whose ?pecid binding
// is missing or empty (identity is anchored on the
// PowerElectronicsConnection's own mRID, not the optional child
// PowerElectronicsUnit's; see cimDevice's doc comment). kind is used
// only for log/error readability; query is the bound *cim.Client
// method, passed as a value so the three call sites share this
// projection without a type switch.
//
// ?maxQ is OPTIONAL: an empty Binding.Value means "absent" and leaves
// cimDevice.MaxQ nil, never a fabricated zero. Live CIMHub CIM100 stores
// PowerElectronicsConnection.maxQ as CIM ReactivePower (xsd:float), so a
// present binding is lexically "125000.0", not "125000"; other
// PEC attributes (ratedS, ratedU, p, q) share that float-lexical shape
// but stay unparsed in the raw QueryDataResult today.
//
// maxQ is parsed with strconv.ParseFloat, rejected outright if NaN,
// +/-Inf, or out of int64 range, then rounded to the nearest whole base
// VAr: cimDevice.MaxQ (and its downstream sep2.ReactivePower.Value) is
// int64 because this bridge always writes Multiplier 0, leaving no
// scaling step to absorb a fractional remainder. Any parse or range
// failure is a hard error, never a silently-dropped or coerced value,
// matching internal/sep2embed's decodeMultiplierValue discipline.
func queryDevices(
	ctx context.Context,
	kind string,
	query func(context.Context, string) (*cim.QueryDataResult, error),
	feederMRID string,
) ([]cimDevice, error) {
	res, err := query(ctx, feederMRID)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", kind, err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]cimDevice, 0, len(res.Results.Bindings))
	for _, row := range res.Results.Bindings {
		mrid := row["pecid"].Value
		if mrid == "" {
			continue
		}
		d := cimDevice{
			MRID:     mrid,
			UnitMRID: row["id"].Value,
			Name:     row["name"].Value,
		}
		if raw := row["maxQ"].Value; raw != "" {
			maxQF, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("query %s: mRID %q: parse maxQ %q: %w", kind, mrid, raw, err)
			}
			if math.IsNaN(maxQF) || math.IsInf(maxQF, 0) {
				return nil, fmt.Errorf("query %s: mRID %q: maxQ %q is not a finite reactive-power magnitude", kind, mrid, raw)
			}
			rounded := math.Round(maxQF)
			// math.MinInt64 (-2^63) is exactly representable in float64, so
			// the lower bound below is safe as written. math.MaxInt64
			// (2^63-1) is NOT exactly representable: converting it to
			// float64 rounds UP to 2^63, so a naive "rounded > MaxInt64"
			// comparison lets rounded == 2^63 through, and int64(2^63)
			// overflows (implementation-defined; wraps to MinInt64 on
			// amd64). Compare against 2^63 with >= instead, so the int64
			// conversion below can never see an out-of-range value.
			const maxInt64Boundary = float64(1 << 63) // == 2^63, exactly representable
			if rounded < math.MinInt64 || rounded >= maxInt64Boundary {
				return nil, fmt.Errorf("query %s: mRID %q: maxQ %q rounds to %g, out of int64 range", kind, mrid, raw, rounded)
			}
			maxQ := int64(rounded)
			d.MaxQ = &maxQ
		}
		out = append(out, d)
	}
	return out, nil
}

// runPump subscribes to the simulation output topic and runs the Pump
// until ctx is cancelled or the subscription closes. The handler logs a
// one-liner per frame and then walks each measurement, performing an
// mRID-to-LFDI lookup against reg. The first time a given measurement
// mRID is seen, the handler logs the lookup result; subsequent frames
// carrying the same mRID are deduplicated to avoid log spam.
//
// At Stage 1, measurement mRIDs (per-point identifiers in the platform
// frame) are not the same as the device mRIDs the registry is keyed on,
// so the lookup typically misses. The point of plumbing reg into the
// handler is to demonstrate the seam: a future revision can change the
// lookup key (e.g., to the parent ConductingEquipment mRID) without
// reworking the pump glue. Richer downstream consumption (IEEE 2030.5
// MirrorMeterReading mapping) is Stage 2.
func runPump(ctx context.Context, subs sim.SubscribeClient, reg *registry.Registry, simID string) error {
	dest := sim.OutputTopic(simID)
	log.Printf("bridge: subscribing to %s", dest)

	pump := sim.NewPump(subs, simID)

	// seen dedupes the per-mRID lookup log so a 1Hz simulation does not
	// reprint the same line every timestep. Plain map plus mutex; the
	// pump handler is invoked serially so the mutex is cheap insurance
	// against a future parallel-handler change rather than current need.
	//
	// maxSeenMRIDs bounds the map. Without a bound,
	// seen grows for the lifetime of the simulation: at Stage 1 the
	// measurement mRIDs are a stable per-point set from the platform's
	// fixed device fleet, so in the common case this never approaches
	// the cap, but nothing upstream guarantees that mRID set is finite
	// or stable across a long-running simulation. Once the cap is hit,
	// seen is cleared: the tradeoff is a handful of duplicate log lines
	// immediately after the reset, which is the same class of log noise
	// this map exists to reduce, not a correctness issue (the LFDI
	// lookup itself is unaffected, only whether a still-registered mRID
	// is logged again).
	const maxSeenMRIDs = 10000
	var (
		seenMu sync.Mutex
		seen   = make(map[string]struct{})
	)

	err := pump.Run(ctx, func(f sim.MeasurementFrame) error {
		log.Printf("bridge: frame received simulation_id=%s timestamp=%d measurements=%d",
			f.SimulationID, f.Message.Timestamp, len(f.Message.Measurements))
		for mrid := range f.Message.Measurements {
			seenMu.Lock()
			_, dup := seen[mrid]
			if !dup {
				if len(seen) >= maxSeenMRIDs {
					seen = make(map[string]struct{})
				}
				seen[mrid] = struct{}{}
			}
			seenMu.Unlock()
			if dup {
				continue
			}
			if lfdi, ok := reg.LFDI(mrid); ok {
				log.Printf("bridge: frame for mrid=%s lfdi=%s", mrid, lfdi)
			} else {
				log.Printf("bridge: frame for mrid=%s lfdi=<not registered>", mrid)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("pump: %w", err)
	}
	return nil
}

// runSimSide runs the measurement pump (runPump) and the
// control-delta subscriber (runControlSubscriber) concurrently under
// ctx, and returns once BOTH have finished. Splitting the two loops out
// as a pair (rather than folding control consumption into runPump
// itself) keeps runPump's existing MeasurementFrame contract untouched;
// they are independent subscriptions on independent (if, for now,
// identically-named) destinations.
//
// Error precedence mirrors runEmbedAndStomp: if only one side failed
// with something other than a graceful ctx cancellation, that error is
// returned; if both failed independently, both are preserved via
// errors.Join.
//
// hook is a read-only observation point (see controlHook's
// doc comment in run): runSimSide records both subscription
// destinations on it up front, before either loop starts, since both
// destinations are known unconditionally from simID and recording them
// does not depend on either loop actually receiving a frame. hook may be
// nil (tests that do not care about observation can omit it); every
// call below guards for that.
func runSimSide(ctx context.Context, subs sim.SubscribeClient, embed *sep2embed.Embed, reg *registry.Registry, simID string, hook *controlobs.Hook) error {
	if hook != nil {
		hook.SetTopics(sim.OutputTopic(simID), sim.InputTopic(simID))
	}

	pumpErr := make(chan error, 1)
	go func() { pumpErr <- runPump(ctx, subs, reg, simID) }()

	ctrlErr := runControlSubscriber(ctx, subs, embed, reg, simID, hook)

	perr := <-pumpErr
	pGraceful := perr == nil || errors.Is(perr, context.Canceled)
	cGraceful := ctrlErr == nil || errors.Is(ctrlErr, context.Canceled)

	switch {
	case pGraceful && cGraceful:
		return nil
	case pGraceful:
		return ctrlErr
	case cGraceful:
		return perr
	default:
		return errors.Join(perr, ctrlErr)
	}
}

// runControlSubscriber subscribes to the same differences destination
// this bridge's own -publish-on-start smoke test and the UP-path
// telemetry publisher (telemetryPublisherConfig's Destination)
// already publish to (internal/cim/sim.InputTopic), decodes each frame
// as a diff.Message, and applies every forward difference to embed via
// sep2embed.Embed.ApplyControlDelta.
//
// Topic-convention caveat: the Python upstream
// reference this bridge reproduces
// (ieee_2030_5/adapters/gridappsd_adapter.py:_input_detected, in the
// gridappsd-2030_5 project) subscribes to a dedicated
// application-input topic (topics.application_input_topic), not the
// shared simulation-input topic used here. This bridge has no Go
// equivalent of that helper yet, and reusing sim.InputTopic is a
// deliberate, documented interim choice rather than an invented
// convention: it is the only "differences" destination this codebase
// already has. Confirming the production topic convention (shared
// sim-input vs. a dedicated per-app input queue) is left to a follow-up
// card; this loop is written so only the destination string need change
// once that is settled.
//
// LOAD-BEARING INVARIANT (Leon INFO / Pike LOW, PR #9 review):
// this DOWN-path subscriber and the UP-path telemetry publisher
// (internal/telemetrypub, which publishes its aggregates to this same
// destination) are safe to share sim.InputTopic ONLY because
// their attribute namespaces never overlap: ApplyControlDelta acts
// exclusively on "DERControl.DERControlBase."-prefixed attributes
// (derControlAttributePrefix), and the telemetry publisher emits
// exclusively "DERStatus."-prefixed attributes
// (telemetrypub's derStatusAttributePrefix). This bridge's own DERStatus echoes are
// therefore ignored here, not misapplied as controls, purely because
// the two prefixes never collide. THIS IS A GUARD, NOT A DESIGN: any
// future field added under a THIRD shared prefix (or, worse, under
// "DERControl." without the DERControlBase suffix, or under
// "DERStatus." on the DOWN side) silently regresses this invariant and
// reopens a self-echo/misapply bug. Give any new UP- or DOWN-path
// attribute family its own distinct, non-overlapping prefix, or split
// the two directions onto separate topics (the shared-topic choice
// itself is not re-litigated by this comment; only the prefix
// discipline that currently makes it safe is).
//
// Decode and per-delta apply errors are logged and skipped; the loop
// continues, matching runPump's resilience style (a malformed or
// inapplicable frame must not take down the whole subscriber).
//
// hook, when non-nil, is a read-only observation point: this
// function is the down path's only writer, so it is the only place that
// calls hook.Applied / hook.Skipped. A malformed frame that never
// resolves to a delta is not counted at all (there is no delta to
// report skipping); only a decoded delta that ApplyControlDelta accepts
// or rejects is counted.
func runControlSubscriber(ctx context.Context, subs sim.SubscribeClient, embed *sep2embed.Embed, reg *registry.Registry, simID string, hook *controlobs.Hook) error {
	dest := sim.InputTopic(simID)
	log.Printf("bridge: subscribing to %s for control deltas", dest)

	sub, err := subs.Subscribe(ctx, dest)
	if err != nil {
		return fmt.Errorf("control subscriber: subscribe %s: %w", dest, err)
	}

	for msg := range sub.Messages() {
		var envelope diff.Message
		if derr := json.Unmarshal(msg.Body, &envelope); derr != nil {
			log.Printf("control subscriber: skip malformed frame on %s: %v", dest, derr)
			continue
		}
		for _, delta := range envelope.Input.Message.ForwardDifferences {
			if aerr := embed.ApplyControlDelta(ctx, reg, delta); aerr != nil {
				log.Printf("control subscriber: skip delta object=%q attribute=%q: %v",
					delta.Object, delta.Attribute, aerr)
				if hook != nil {
					hook.Skipped()
				}
				continue
			}
			if hook != nil {
				hook.Applied(delta)
			}
		}
	}

	endErr := sub.Err()
	if endErr == nil {
		return nil
	}
	if errors.Is(endErr, context.Canceled) || errors.Is(endErr, context.DeadlineExceeded) {
		return endErr
	}
	return fmt.Errorf("control subscriber: subscription ended: %w", endErr)
}
