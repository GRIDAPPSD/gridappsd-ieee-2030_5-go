// Command bridge is the GridAPPS-D side of the IEEE 2030.5 to
// GridAPPS-D bridge. It connects to the GridAPPS-D message bus,
// queries the CIM feeder for inverter / solar / battery DERs, populates
// an in-memory mRID-to-LFDI registry, boots an in-process IEEE 2030.5
// mTLS server (internal/sep2embed) seeded from that registry, and (when
// a SimulationID is configured) subscribes to the simulation output
// topic and logs each MeasurementFrame.
//
// The embedded IEEE 2030.5 server speaks real mTLS with real,
// certificate-derived device identity (GAGO-033): the LFDI on every
// EndDevice is per spec section 6.3.4 and the SFDI is per section
// 6.3.3, both derived from each device's own certificate rather than a
// placeholder hash of the CIM mRID. The listener still has no
// per-device ACL (GAGO-043 follow-up), which is why it binds to
// loopback by default. Bidirectional control flow (device writes
// reaching the CIM side) is a further follow-up.
//
// The GridAPPS-D connection rides github.com/GRIDAPPSD/gridappsd-go's
// fieldbus.MessageBus (GAGO-039), adapted to this bridge's own
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
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/buildinfo"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
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
// credential-shaped substring before logging it and exiting (GAGO-028
// Leon L3). Neither cfg.STOMPPassword nor cfg.SEP2AdminUIKey is
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
// would otherwise slip past the two checks above undetected (GAGO-028
// follow-up). This is still not exhaustive: any credential-shaped value
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
	reg, err := bootstrapRegistry(ctx, cimClient, cfg.FeederMRID, cfg.SEP2ServerCertDir, mode)
	if err != nil {
		return err
	}

	// policy is loaded once, here at boot, matching this bridge's other
	// config sources. GAGO-050 consumes policy.DefaultControl and
	// GAGO-049 consumes policy.ModesSupported, both threaded through
	// newSEP2Embed below. DefaultPolicy leaves ModesSupported nil, so the
	// DERCapability GAGO-049 seeds is still nil-safe until a real policy
	// value is configured.
	policy := sep2config.DefaultPolicy()
	log.Printf("bridge: sep2 policy loaded modesSupported=%s pollRate=%s postRate=%s",
		fmtU32Ptr(policy.ModesSupported), fmtU32Ptr(policy.DefaultPollRate), fmtU32Ptr(policy.DefaultPostRate))

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

	// The embed seeds its EndDevice/DER stores from reg, so it must be
	// built after bootstrapRegistry above, not before.
	embed, err := newSEP2Embed(ctx, cfg, reg, bus, policy)
	if err != nil {
		return fmt.Errorf("sep2 embed: %w", err)
	}
	id := embed.Identity()
	log.Printf("bridge: sep2 embed listening addr=%s sfdi=%s lfdi=%s",
		embed.Addr(), id.SFDI, id.LFDI)

	// controlHook is the GAGO-057 read-only observation point over the
	// control-delta down path (and the sim-output/control-input topic
	// pair): runSimSide's two loops are its only writers, and the future
	// admin UI controlflow endpoint (GAGO-059) is its only reader. It is
	// safe to construct unconditionally, even when SimulationID is
	// empty and stompRun never touches it: the zero value is a valid,
	// all-empty observation state.
	var controlHook controlobs.Hook

	// stompRun adapts the SimulationID branch (idle-wait, or the
	// measurement pump plus the GAGO-034 control-delta subscriber) to
	// the func(context.Context) error shape runEmbedAndStomp expects
	// for its second seam.
	stompRun := func(runCtx context.Context) error {
		if cfg.SimulationID == "" {
			log.Printf("bridge: no SEP2_SIMULATION_ID set; skipping simulation subscribe; idling until shutdown")
			<-runCtx.Done()
			return runCtx.Err()
		}
		// runCtx is the pump's (and the control subscriber's) root.
		// gridappsd-go's router does not yet surface a broker-teardown
		// signal to fieldbus.MessageBus callers (upstream gap GAG-009;
		// see the relay doc comment in
		// internal/gridappsdclient/subscriber.go), so a mid-run broker
		// disconnect does NOT independently wake either loop: only
		// runCtx's cancellation (SIGINT/SIGTERM, or the embed side
		// exiting) does.
		return runSimSide(runCtx, bus, embed, reg, cfg.SimulationID, &controlHook)
	}

	// adminSrv is the GAGO-058/GAGO-059 read only operator HTTP API. It
	// is off by default: adminui.New returns ErrDisabled when
	// SEP2_ADMIN_UI_KEY is unset, in which case no listener is opened and
	// no runner goroutine is started at all, matching the "off by
	// default" hard rule. Any other error from New (an invalid Addr, or
	// a non-loopback Addr without the explicit opt-in) is a genuine
	// startup failure, not the disabled state.
	var adminUIRun func(context.Context) error
	adminSrv, err := adminui.New(adminUIConfig(cfg), reg, embed, embed, &controlHook, embed, bus)
	switch {
	case errors.Is(err, adminui.ErrDisabled):
		log.Printf("bridge: admin UI disabled, SEP2_ADMIN_UI_KEY unset")
	case err != nil:
		return fmt.Errorf("admin ui: %w", err)
	default:
		log.Printf("bridge: admin UI listening addr=%s", adminSrv.Addr())
		adminUIRun = adminSrv.Run
	}

	return runBridgeRunners(ctx, embed.Run, stompRun, adminUIRun)
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
func runBridgeRunners(ctx context.Context, embedRun, stompRun, adminUIRun func(context.Context) error) error {
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

// sep2EmbedConfig projects the bridge's config onto sep2embed.Config.
// Split out from newSEP2Embed so the address/cert-dir mapping can be
// asserted by a unit test without minting real certificate material or
// binding a listener.
//
// bus is threaded through as sep2embed.Config.Bus for the GAGO-034
// UP-path telemetry relay (SEP2 DERStatus -> GridAPPS-D bus). A nil bus
// (or an empty cfg.SimulationID) disables the relay: see
// sep2embed.Config.Bus's doc comment. TelemetryDestination reuses
// internal/cim/sim.InputTopic, the same simulation-input destination
// this bridge's own -publish-on-start smoke test already documents as
// the outgoing-difference channel.
//
// policy is threaded through as sep2embed.Config.DefaultControl
// (GAGO-050): the fallback DefaultDERControl this bridge seeds onto
// every DERProgram is sourced from policy.DefaultControl, never
// hardcoded at this layer. policy.ModesSupported is threaded through the
// same way (GAGO-049): the DERControlType bitmap seeded onto every
// device's DERCapability is sourced from policy, never hardcoded here;
// DefaultPolicy leaves it nil, so seeding is nil-safe until a real
// policy value is configured.
func sep2EmbedConfig(cfg config, bus sep2embed.BusPublisher, policy sep2config.SEP2Policy) sep2embed.Config {
	dest := ""
	if cfg.SimulationID != "" {
		dest = sim.InputTopic(cfg.SimulationID)
	}
	return sep2embed.Config{
		Addr:                  cfg.SEP2ServerAddr,
		CertDir:               cfg.SEP2ServerCertDir,
		Bus:                   bus,
		TelemetryDestination:  dest,
		TelemetrySimulationID: cfg.SimulationID,
		DefaultControl:        policy.DefaultControl,
		ModesSupported:        policy.ModesSupported,
	}
}

// adminUIConfig projects the bridge's config onto adminui.Config. Split
// out from run() so the field mapping can be asserted by a unit test
// with no listener bound and no admin token required.
func adminUIConfig(cfg config) adminui.Config {
	return adminui.Config{
		Addr:             cfg.SEP2AdminUIAddr,
		AllowNonLoopback: cfg.SEP2AdminUIAllowNonLoopback,
		Key:              cfg.SEP2AdminUIKey,
		AllowedHosts:     cfg.SEP2AdminUIAllowedHosts,
		FeederMRID:       cfg.FeederMRID,
		SimulationID:     cfg.SimulationID,
		SORLink:          cfg.SEP2AdminUISORLink,
	}
}

// newSEP2Embed builds, seeds, and binds the in-process IEEE 2030.5
// protocol server from the bridge's registry. It does not start
// serving; the caller starts embed.Run once this returns successfully.
func newSEP2Embed(ctx context.Context, cfg config, reg *registry.Registry, bus sep2embed.BusPublisher, policy sep2config.SEP2Policy) (*sep2embed.Embed, error) {
	return sep2embed.New(ctx, sep2EmbedConfig(cfg, bus, policy), reg)
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
// sep2config.SEP2Policy's ModesSupported/DefaultPollRate/DefaultPostRate
// are pointer-typed precisely so a real 0 is distinguishable from unset;
// this keeps that distinction visible in the boot log too.
func fmtU32Ptr(v *uint32) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *v)
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

// pecCountLogLine renders the GAGO-051 discover-vs-project drop-visibility
// message for feederMRID, given the QueryPECCount discovery result
// (discovered, discoveredOK) and the post-dedupe projected device count
// projected. It returns the message text and whether the caller should
// log it at WARNING level.
//
//   - discoveredOK && discovered <= projected: counts-match path; INFO-level,
//     no "WARNING" text. discovered < projected is not a valid drop
//     (the enumeration queries cannot project more devices than truly
//     exist), so it is treated the same as an exact match rather than
//     surfaced as a nonsensical negative drop count: the two queries
//     ran against a moving CIM dataset (queryTimeout apart, see
//     bootstrapRegistry), so a discovered count that lags the projected
//     count is a dataset-changed-mid-query artifact, not evidence of a
//     drop.
//   - discoveredOK && discovered > projected: some PECs were silently
//     dropped by the enumeration queries' mandatory attribute joins;
//     WARNING-level, states both counts and the drop count.
//   - !discoveredOK: the true discovered count could not be determined
//     (QueryPECCount failed, or returned an unparsable/missing count);
//     WARNING-level, states the limitation rather than fabricating a
//     drop count of 0.
func pecCountLogLine(feederMRID string, discovered int, discoveredOK bool, projected int) (string, bool) {
	if !discoveredOK {
		return fmt.Sprintf(
			"feeder %s: could not determine the true discovered PowerElectronicsConnection count; "+
				"projected %d device(s) from the enumeration queries, but cannot confirm whether any were dropped by their mandatory attribute joins",
			feederMRID, projected), true
	}
	if discovered <= projected {
		return fmt.Sprintf(
			"feeder %s: discovered %d PowerElectronicsConnection object(s), projected %d device(s); no drops",
			feederMRID, discovered, projected), false
	}
	dropped := discovered - projected
	return fmt.Sprintf(
		"feeder %s: discovered %d PowerElectronicsConnection object(s) but only %d were fully projected as devices (%d dropped for missing mandatory attributes)",
		feederMRID, discovered, projected, dropped), true
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

// bootstrapRegistry runs the three CIM enumeration queries against the
// feeder, dedupes by mRID (a single device may surface in multiple
// queries when the upstream filter is open), derives each device's real
// IEEE 2030.5 identity from its certificate (GAGO-033, spec sections
// 6.3.4 LFDI / 6.3.3 SFDI, via sep2embed.EnsureDeviceIdentities), and
// populates a fresh registry from the result. Returns the populated
// registry; the caller does not need a separate add step.
//
// certDir and mode are threaded straight through to
// EnsureDeviceIdentities: certDir is cfg.SEP2ServerCertDir, the SAME
// directory the embedded server's own CA and leaf material live under
// (device certs are signed by that same CA; see
// sep2embed.EnsureDeviceIdentities's doc comment for why this must run
// before sep2embed.New's own load-or-create call against the same
// dir). mode selects dev-mint vs fail-closed preprovisioned sourcing.
func bootstrapRegistry(ctx context.Context, c *cim.Client, feederMRID, certDir string, mode sep2embed.DeviceCertMode) (*registry.Registry, error) {
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

	// Dedupe across the three lists. A PhotovoltaicUnit shows up under
	// QueryInverter (open filter) and QuerySolar; the registry must
	// only carry one entry per mRID.
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

	// GAGO-028 Dutch M4: without this line, a reader sees the two log
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

	// GAGO-051: the empty-fleet fail-loud guard. Model-only device
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

	// GAGO-051: discover-vs-project drop visibility (Cyrus's finding).
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

// cimDevice is the slim projection of a SPARQL binding row this bridge
// needs at Stage 1: identity, name, and now MaxQ (GAGO-049 follow-up),
// the one PowerElectronicsConnection rated-maximum value that has a
// model-correct target in the vendored core library's DERCapability
// type (RTGMaxVar). Other richer attributes (ratedS, ratedU, phases)
// stay in the raw QueryDataResult and can be lifted into typed structs
// when downstream code consumes them.
type cimDevice struct {
	MRID string
	Name string
	MaxQ *int64 // CIM PowerElectronicsConnection.maxQ, base VAr, rounded from the CIM float; nil when the binding is absent
}

// queryDevices runs one of the cim.Client Query* wrappers, projects
// each binding row down to a cimDevice, and skips rows whose ?id binding
// is missing or empty. The kind argument is only used for log
// readability. The query argument is the bound method on *cim.Client;
// passing it as a value lets the three call sites share this projection
// without a type switch.
//
// The ?maxQ binding is OPTIONAL in every PEC-rooted SPARQL template
// (internal/cim/queries.go), so a row can legitimately carry an empty
// Binding.Value for it: that is treated as "absent", not "zero", and
// leaves cimDevice.MaxQ nil. Live CIMHub CIM100 stores
// PowerElectronicsConnection.maxQ as CIM ReactivePower (xsd:float), so
// a present binding is lexically "125000.0", not "125000" (GAGO-082);
// the same is true of the SPARQL-adjacent PEC attributes ratedS,
// ratedU, p, and q, which are also xsd:float in the CIM100 profile
// (those stay unparsed in the raw QueryDataResult today, per this
// function's own doc comment above, but confirm maxQ is not an
// isolated case: the float-lexical shape is the CIM norm for this
// class of attribute, not an anomaly). maxQ is parsed here with
// strconv.ParseFloat and then rounded to the nearest whole base VAr for
// cimDevice.MaxQ's int64 storage (see the rounding note below). A
// present binding that fails to parse as a float at all is a hard
// error rather than a silently-dropped value, matching the
// no-fabricated-fallback discipline internal/sep2embed's
// decodeMultiplierValue already applies to the wire-side ReactivePower
// shape.
//
// Rounding: cimDevice.MaxQ (and registry.Entry.MaxQ, and
// sep2.ReactivePower.Value downstream in buildRTGMaxVar) is int64
// because the vendored core sep2.ReactivePower wire type stores its
// Value as an integer scaled by an int8 Multiplier, and this bridge
// always writes Multiplier 0 (base units, see buildRTGMaxVar's doc
// comment), so there is no scaling step left to absorb a fractional
// remainder. math.Round to the nearest whole VAr before the int64
// conversion is therefore the correct (and only) place to resolve the
// float-to-int64 boundary: a sub-VAr fraction is below the resolution
// IEEE 2030.5 RTGMaxVar can represent at this multiplier, so rounding
// (rather than truncating, which would silently discard up to 1 VAr in
// the low direction every time) is the least lossy choice.
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
		mrid := row["id"].Value
		if mrid == "" {
			continue
		}
		d := cimDevice{
			MRID: mrid,
			Name: row["name"].Value,
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
			if rounded < math.MinInt64 || rounded > math.MaxInt64 {
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
func runPump(ctx context.Context, bus fieldbus.MessageBus, reg *registry.Registry, simID string) error {
	dest := sim.OutputTopic(simID)
	log.Printf("bridge: subscribing to %s", dest)

	pump := sim.NewPump(gridappsdclient.NewSubscriber(bus), simID)

	// seen dedupes the per-mRID lookup log so a 1Hz simulation does not
	// reprint the same line every timestep. Plain map plus mutex; the
	// pump handler is invoked serially so the mutex is cheap insurance
	// against a future parallel-handler change rather than current need.
	//
	// maxSeenMRIDs bounds the map (GAGO-028 Dutch L1). Without a bound,
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

// runSimSide runs the measurement pump (runPump) and the GAGO-034
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
// hook is the GAGO-057 read-only observation point (see controlHook's
// doc comment in run): runSimSide records both subscription
// destinations on it up front, before either loop starts, since both
// destinations are known unconditionally from simID and recording them
// does not depend on either loop actually receiving a frame. hook may be
// nil (tests that do not care about observation can omit it); every
// call below guards for that.
func runSimSide(ctx context.Context, bus fieldbus.MessageBus, embed *sep2embed.Embed, reg *registry.Registry, simID string, hook *controlobs.Hook) error {
	if hook != nil {
		hook.SetTopics(sim.OutputTopic(simID), sim.InputTopic(simID))
	}

	pumpErr := make(chan error, 1)
	go func() { pumpErr <- runPump(ctx, bus, reg, simID) }()

	ctrlErr := runControlSubscriber(ctx, bus, embed, reg, simID, hook)

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
// telemetry relay (internal/sep2embed's Config.TelemetryDestination)
// already publish to (internal/cim/sim.InputTopic), decodes each frame
// as a diff.Message, and applies every forward difference to embed via
// sep2embed.Embed.ApplyControlDelta.
//
// Topic-convention caveat (GAGO-034 follow-up): the Python upstream
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
// LOAD-BEARING INVARIANT (Leon INFO / Pike LOW, GAGO-034 PR #9 review):
// this DOWN-path subscriber and the UP-path telemetry relay
// (internal/sep2embed's telemetryMiddleware, which also publishes to
// this same destination) are safe to share sim.InputTopic ONLY because
// their attribute namespaces never overlap: ApplyControlDelta acts
// exclusively on "DERControl.DERControlBase."-prefixed attributes
// (derControlAttributePrefix), and the telemetry relay publishes
// exclusively "DERStatus."-prefixed attributes
// (derStatusAttributePrefix). This bridge's own DERStatus echoes are
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
// hook, when non-nil, is the GAGO-057 read-only observation point: this
// function is the down path's only writer, so it is the only place that
// calls hook.Applied / hook.Skipped. A malformed frame that never
// resolves to a delta is not counted at all (there is no delta to
// report skipping); only a decoded delta that ApplyControlDelta accepts
// or rejects is counted.
func runControlSubscriber(ctx context.Context, bus fieldbus.MessageBus, embed *sep2embed.Embed, reg *registry.Registry, simID string, hook *controlobs.Hook) error {
	dest := sim.InputTopic(simID)
	log.Printf("bridge: subscribing to %s for control deltas", dest)

	sub, err := gridappsdclient.NewSubscriber(bus).Subscribe(ctx, dest)
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
