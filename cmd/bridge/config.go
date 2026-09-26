package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetrypub"
)

// config carries the runtime knobs the Stage 1 bridge needs. Only the
// GridAPPS-D side is wired here; IEEE 2030.5 server config arrives in
// a Stage 2 follow-up filed separately.
//
// All knobs are env-var driven with sensible gridappsd-docker defaults
// so a developer can run `go run ./cmd/bridge` against a freshly
// brought-up platform without ceremony. A single boolean flag
// (-publish-on-start) is exposed for the optional difference-message
// publish path because flipping it from the shell is more convenient
// than editing an env file.
type config struct {
	// STOMPAddr is the GridAPPS-D message bus address as host:port.
	// gridappsd-docker exposes 61613 on loopback; on this WSL2 box the
	// host-port mapping is sometimes flaky and the developer needs to
	// reach the broker via the container's bridge IP, hence the env
	// override.
	STOMPAddr string

	// STOMPUser, STOMPPassword authenticate against ActiveMQ. The
	// Python upstream's gridappsd-python defaults (system / manager)
	// are used, matching the gridappsd-docker default broker policy.
	STOMPUser     string
	STOMPPassword string

	// SimulationID identifies the running simulation for the output
	// topic the Pump subscribes to. May be empty in Stage 1; with no
	// simulation_id the subscribe uses an empty suffix and never
	// matches a real frame, but the connect-and-CIM-query path still
	// validates without one.
	SimulationID string

	// FeederMRID identifies the CIM feeder model to query for DERs.
	// Defaults to the IEEE 123-bus feeder shipped with gridappsd-docker.
	FeederMRID string

	// PublishOnStart, when true, publishes a small DifferenceBuilder
	// envelope to the simulation input topic right after registry
	// bootstrap. Useful as a smoke test of the publish path; defaults
	// off because it requires a SimulationID to land somewhere
	// observable.
	PublishOnStart bool

	// AllowPlaintext opts into a plain TCP dial to the GridAPPS-D
	// broker instead of TLS. Defaults false: gridappsd-go's
	// gridappsd.Config is fail-closed (nil TLSConfig plus
	// AllowPlaintext false dials TLS against the system trust store),
	// and this bridge's zero-value config preserves that default
	// rather than inverting it. Set true only against a broker known
	// to be plaintext, such as gridappsd-docker's dev stack on
	// 127.0.0.1:61613.
	AllowPlaintext bool

	// SEP2ServerAddr is the "host:port" the embedded IEEE 2030.5 mTLS
	// listener (internal/sep2embed) binds. Defaults to loopback only:
	// the embed has no per-device ACL yet, so a
	// loopback default keeps that unfinished access-control story from
	// being reachable off-box out of the box. Binding to a non-loopback
	// address is an explicit operator choice made by overriding
	// SEP2_SERVER_ADDR / -sep2-server-addr, mirroring AllowPlaintext's
	// explicit-opt-in shape.
	SEP2ServerAddr string

	// SEP2ServerCertDir is the directory holding the embedded server's
	// mTLS identity material. See sep2embed.Config.CertDir: when the
	// directory is missing or incomplete, fresh dev-mint material is
	// generated and written there; a production deployment points this
	// at a directory holding preprovisioned CA and server cert/key
	// material instead. Never committed; the operator owns keeping it
	// out of version control.
	SEP2ServerCertDir string

	// SEP2DeviceCertMode selects how bootstrapRegistry sources each
	// device's IEEE 2030.5 identity certificate (spec section 6.3.4
	// LFDI, section 6.3.3 SFDI; see internal/sep2embed.DeviceCertMode).
	// One of the two deviceCertMode* flag values below; loadConfig
	// rejects anything else. Defaults to "dev-mint": an operator must
	// set this explicitly to "preprovisioned" to get the fail-closed
	// production behavior, mirroring AllowPlaintext's explicit-opt-in
	// shape for the "this is production" signal.
	SEP2DeviceCertMode string

	// SEP2EnableCCM selects sep2embed.Config.EnableCCM, which builds the
	// embedded listener without the connection observer. As of core
	// v0.20.0 this changes nothing about which cipher suite is served:
	// core dropped the GCM/default constructor entirely, so every
	// embedded listener path (this flag set or not) now offers only
	// TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 (0xC0AE), the suite IEEE
	// 2030.5-2018 section 6.7 makes mandatory. Defaults false, mirroring
	// AllowPlaintext's explicit-opt-in shape; kept as configuration
	// surface pending a follow-up that retires this flag along with
	// SEP2CCMAllowNoObserver, since setting it now only trades away the
	// connection observer for no other effect.
	//
	// sep2embed.New still refuses Config.Observer and Config.EnableCCM
	// together (errObserverRequiresGCM; see sep2EmbedConfig's doc comment
	// for the wiring), but that refusal is configuration surface only now:
	// the CCM-8 listener has had a handshake-observation seam since this
	// package's mtls.go migration, and both listener builders share one
	// config call. validate below still refuses to start with this set
	// unless SEP2CCMAllowNoObserver is also set, so the operator chooses
	// to lose the rejected-device record explicitly rather than have it
	// implied by this flag alone; that choice, not a missing seam, is the
	// only remaining reason. See issue 127 for removing both flags.
	SEP2EnableCCM bool

	// SEP2CCMAllowNoObserver is the explicit second setting SEP2EnableCCM
	// requires: without it, validate refuses to start rather than
	// silently dropping the connection observer under CCM. Defaults
	// false, mirroring AllowPlaintext's explicit-opt-in shape. Setting it
	// accepts, kept as configuration surface pending issue 127 (which
	// removes this flag pair):
	//   - the loss of the rejected-device record, per-LFDI last-seen, and
	//     request counts and paths the observer would otherwise carry;
	//   - the admin UI's served-status table showing every served
	//     device's status as unknown, not connected or never connected
	//     (ConnectedClients.svelte's observationDisabled state);
	//   - the same panel's connected-clients and handshake-attempts
	//     tables showing no data rather than admitting they cannot tell,
	//     for the same reason;
	//   - a refused handshake (a client that cannot offer CCM-8) reaching
	//     the process log (sepTLS.WrapCCMListener) but never the panel.
	SEP2CCMAllowNoObserver bool

	// SEP2NotificationAllowLoopback lets subscription notificationURIs
	// target loopback addresses; refused by default. See
	// sep2embed.Config.NotifyAllowLoopback for why: the permission admits
	// any loopback destination on any port, reaching every service this
	// bridge's network namespace exposes there (its admin UI listener,
	// SEP2AdminUIAddr, among others), so enabling this mirrors
	// AllowPlaintext's explicit-opt-in shape for a switch that must not be
	// set in production.
	SEP2NotificationAllowLoopback bool

	// SEP2AdminUIAddr is the "host:port" the read only admin UI HTTP
	// listener (internal/adminui) binds. See adminui.Config.Addr:
	// loopback only unless SEP2AdminUIAllowNonLoopback is set. Defaults
	// to loopback so an operator opts in to any wider exposure
	// explicitly, mirroring SEP2ServerAddr's own default shape.
	SEP2AdminUIAddr string

	// SEP2AdminUIAllowNonLoopback must be explicitly set to bind
	// SEP2AdminUIAddr to a non-loopback host. See
	// adminui.Config.AllowNonLoopback.
	SEP2AdminUIAllowNonLoopback bool

	// SEP2AdminUIKey is the Bearer token the admin UI requires on every
	// request. Deliberately NOT validated as required by config.validate:
	// an empty key is the intentional "admin UI disabled" state per
	// adminui.New's fail closed ErrDisabled contract. An
	// operator opts in to the admin UI by setting this explicitly.
	SEP2AdminUIKey string

	// SEP2AdminUIAllowedHosts is an additional, comma separated set of
	// Host header values the admin UI's host allowlist middleware
	// accepts, beyond its own built in defaults (localhost, 127.0.0.1,
	// ::1). See adminui.Config.AllowedHosts.
	SEP2AdminUIAllowedHosts []string

	// SEP2AdminUISORLink is an optional, operator supplied URL to a
	// server of record dashboard, exposed read only via the admin UI's
	// /api/health endpoint. Not a credential: unlike
	// SEP2AdminUIKey, this value is safe to return in an API response
	// and is never scrubbed from the environment or logged specially.
	// Empty means unset: no link, no error, no admin UI behavior
	// change.
	SEP2AdminUISORLink string

	// SEP2RegistrationPIN is the optional fleet-wide fallback IEEE
	// 2030.5 registration PIN (see sep2config.SEP2Policy's
	// DefaultRegistrationPIN doc comment for why a fleet-wide value is
	// a dev/interop fallback, not a production shape). Pointer-typed
	// because 0 is itself a schema-legal PIN (sep.xsd's PINType has no
	// range floor above 0), so a *uint32 is the only way to distinguish
	// "operator configured 0" from "operator configured nothing"; nil
	// means the flag was not set. Populated from -sep2-registration-pin
	// only: this value is never sourced from an env var, so it never
	// needs the credential flags' "empty flag default, resolve after
	// Parse" dance.
	SEP2RegistrationPIN *uint32

	// SEP2RegistrationPINs is the optional per-device IEEE 2030.5
	// registration PIN map, loaded verbatim from the JSON file at
	// -sep2-registration-pin-file (device LFDI to PIN). Nil means the
	// flag was not set: zero devices configured this way is
	// indistinguishable from "flag absent" for this field, unlike
	// SEP2RegistrationPIN's zero-value ambiguity, because an existing
	// but empty PIN file is rejected outright by
	// loadRegistrationPINFile rather than producing an empty map here.
	SEP2RegistrationPINs map[string]uint32

	// SEP2BatteryLegs is the optional, explicit list of utility battery
	// leg EnergyConsumer mRIDs, loaded verbatim from the plain-text file
	// at -sep2-battery-leg-list-file (one mRID per line, blank lines and
	// "#" comments ignored). The CIM model carries no type or link that
	// marks a battery leg the way c:House.EnergyConsumer marks a house
	// load (see internal/cim's sparqlQueryEnergyConsumers doc comment),
	// so these must be named rather than discovered; bootstrapRegistry
	// checks every entry against the live model at boot (an
	// EnergyConsumer on the configured feeder, not also a house load)
	// before registering it. Nil means the flag was not set: zero legs
	// configured is indistinguishable from "flag absent" here, matching
	// SEP2RegistrationPINs's shape, because loadBatteryLegListFile
	// rejects an existing but empty file outright rather than producing
	// an empty slice.
	SEP2BatteryLegs []string

	// SEP2PollRate and SEP2PostRate are the optional fleet-wide IEEE
	// 2030.5 poll and post intervals, in seconds, from -sep2-poll-rate and
	// -sep2-post-rate (env SEP2_POLL_RATE / SEP2_POST_RATE). They become
	// sep2config.SEP2Policy's DefaultPollRate and DefaultPostRate.
	//
	// Pointer-typed because nil (flag absent) is a distinct, load-bearing
	// state and not merely an absent number: nil means the bridge
	// advertises no rate at all, so a Registration omits its optional
	// pollRate attribute and a MirrorUsagePoint keeps whatever postRate its
	// creating client supplied. That is precisely today's behavior, which
	// is why these carry no compiled-in default: adding one would start
	// advertising a value to every existing deployment on upgrade.
	//
	// sep2config.RecommendedPollRate and RecommendedPostRate carry the
	// values an operator should reach for, with the reasoning.
	SEP2PollRate *uint32
	SEP2PostRate *uint32

	// SEP2ProgramPrimacy and SEP2ProgramDescription override the seeded
	// default DERProgram's primacy and description. They resolve from
	// -sep2-program-file (a JSON object an admin UI can rewrite), then from
	// -sep2-program-primacy / -sep2-program-description, and become
	// sep2config.SEP2Policy.DefaultProgram.
	//
	// Pointer-typed because both have a compiled-in default that nil must
	// not clobber: sep2config.DefaultPolicy sets primacy to
	// PrimacyContractedServiceProvider (1) and a non-empty description, and
	// nil here means "leave the compiled-in default alone". A non-pointer
	// uint8 could not express that, because 0 is a legal primacy (the
	// highest priority) and would silently promote every deployment's
	// program above any other on upgrade. The empty string is likewise a
	// legal description (marshals as absent), so it cannot double as the
	// unset sentinel either.
	//
	// The IEEE 2030.5 domain checks (String32 bound, reserved primacy
	// ranges) live in sep2config.SEP2Policy.ValidateDefaultProgram, called
	// from buildSEP2Policy, not here: this layer parses, that layer judges.
	SEP2ProgramPrimacy     *uint8
	SEP2ProgramDescription *string

	// SEP2ControlDuration and SEP2ControlRandomizeDuration override the
	// interval duration and the randomizeDuration served on every DERControl
	// this bridge issues, from -sep2-control-duration and
	// -sep2-control-randomize-duration. They become
	// sep2config.SEP2Policy.DERControl.
	//
	// Pointer-typed for the same reason the program fields above are: both
	// have a compiled-in default that nil must not clobber
	// (sep2config.DefaultDERControlDuration, and 0 randomization), and 0 is
	// a legal-looking value for each. For the randomization 0 is not merely
	// legal but the shipped default, so it cannot double as an unset
	// sentinel; for the duration 0 is illegal, and letting a bare uint32
	// carry it would turn "operator said nothing" into the exact
	// zero-length-interval defect these knobs exist to prevent.
	//
	// The IEEE 2030.5 domain checks (nonzero duration, the OneHourRangeType
	// bound, and the two knobs' relationship) live in
	// sep2config.SEP2Policy.ValidateDERControl, called from buildSEP2Policy,
	// not here: this layer parses, that layer judges.
	SEP2ControlDuration          *uint32
	SEP2ControlRandomizeDuration *int32

	// SEP2DefaultControlBase carries the operator's DefaultDERControl
	// overrides, from -sep2-default-control-file. It becomes
	// sep2config.SEP2Policy.DefaultControl's DERControlBase.
	//
	// Each member is a pointer, and an absent member leaves the compiled-in
	// value alone rather than resetting it: the file is a set of overrides,
	// not a replacement document. That is why the two booleans are pointers
	// rather than bare bools (false is a meaningful setting for both and
	// could not otherwise be told from absent), and why OpModMaxLimW is a
	// pointer too: 0 is a meaningful percent (full curtailment).
	//
	// The shipped default sets NONE of the three, which is what makes it
	// command nothing; see sep2config.DefaultPolicy.
	SEP2DefaultControlOpModConnect  *bool
	SEP2DefaultControlOpModEnergize *bool
	SEP2DefaultControlOpModMaxLimW  *uint16

	// SEP2TelemetryInterval is the period of the DERStatus telemetry
	// publisher (internal/telemetrypub), from -sep2-telemetry-interval
	// (env SEP2_TELEMETRY_INTERVAL). It is a Go duration string ("15s",
	// "1m"), not a bare number of seconds, because a bare number is
	// ambiguous about its unit at exactly the moment an operator is
	// changing it under pressure.
	//
	// Unlike the poll/post rates above this one DOES carry a compiled-in
	// default (telemetrypub.DefaultInterval, 15 seconds, matching the
	// Python upstream's publish_interval_seconds), because the publisher
	// always runs when a simulation id is configured and there is no
	// "advertise nothing" state for it to be in.
	SEP2TelemetryInterval time.Duration

	// SEP2TelemetryPublishUnchanged turns OFF suppression of unchanged
	// devices, from -sep2-telemetry-publish-unchanged (env
	// SEP2_TELEMETRY_PUBLISH_UNCHANGED).
	//
	// THIS IS THE SUPPRESSION SWITCH. False (the default) publishes only
	// devices whose mapped values moved since their last successful
	// publish. True restores full-snapshot semantics: every device with
	// a stored DERStatus, every interval, like the Python upstream. Flip
	// it if the eventual subscriber turns out to treat each message as a
	// complete state snapshot rather than a set of incremental updates,
	// because suppression would then silently age out every device that
	// has not moved.
	SEP2TelemetryPublishUnchanged bool
}

// deviceCertMode* are the only two values config.validate accepts for
// SEP2DeviceCertMode / SEP2_DEVICE_CERT_MODE.
const (
	deviceCertModeDevMintFlag        = "dev-mint"
	deviceCertModePreprovisionedFlag = "preprovisioned"
)

// errVersionRequested is loadConfig's sentinel for "-version was
// passed": main checks for it with errors.Is, the same pattern it
// already uses for flag.ErrHelp, and prints the build version instead
// of treating the return as a usage error.
var errVersionRequested = errors.New("version requested")

// envDefaults are the gridappsd-docker dev-stack defaults. They are
// safe to bake into the binary because:
//
//   - The credentials are public (documented in the gridappsd-docker
//     README) and only useful against a localhost dev stack.
//   - The feeder mRID is the IEEE 123-bus feeder included with the
//     standard gridappsd-docker model catalog; it is a stable string
//     and not a secret.
//
// A production deployment overrides every value via the env.
const (
	defaultSTOMPAddr     = "127.0.0.1:61613"
	defaultSTOMPUser     = "system"
	defaultSTOMPPassword = "manager"
	defaultFeederMRID    = "_C1C3E687-6FFD-C753-582B-632A27E28507"

	// defaultSEP2ServerAddr binds the embedded IEEE 2030.5 mTLS listener
	// to loopback only by default; see config.SEP2ServerAddr's doc
	// comment for why: no per-device ACL yet.
	defaultSEP2ServerAddr = "127.0.0.1:8443"

	// defaultSEP2ServerCertDir is where dev-mint mTLS material is
	// written when no preprovisioned CA/server cert pair exists yet
	// (see sep2embed.Config.CertDir). Relative to the process's working
	// directory so a bare `go run ./cmd/bridge` boots the embed with no
	// extra setup; a production deployment overrides this via the env.
	defaultSEP2ServerCertDir = "./sep2-certs"

	// defaultSEP2DeviceCertMode is "dev-mint": a bare `go run
	// ./cmd/bridge` derives working device identities with no extra
	// setup. A production deployment must opt into "preprovisioned"
	// explicitly; see config.SEP2DeviceCertMode's doc comment.
	defaultSEP2DeviceCertMode = deviceCertModeDevMintFlag

	// defaultSEP2AdminUIAddr binds the admin UI listener to loopback
	// only by default; see config.SEP2AdminUIAddr's doc comment.
	// SEP2AdminUIKey has no compiled-in default (and no fallback
	// constant here): its zero value, the empty string, is the
	// intentional "admin UI disabled" state.
	defaultSEP2AdminUIAddr = "127.0.0.1:8444"
)

// loadConfig reads bridge config from env vars and the command-line
// flags. The two sources are merged: flags shadow envs, envs shadow
// the compiled-in defaults. Returns a usage error wrapped with the
// missing/invalid field name when the resulting config cannot drive a
// connect.
//
// Credential-bearing flags (-stomp-user, -stomp-password) deliberately
// register an empty string as their flag default so flag.PrintDefaults
// (triggered by -h or any parse error) never prints a real credential
// value. Env-var or compiled-in defaults are folded in after Parse, in
// the same precedence as the non-credential flags.
func loadConfig(args []string) (config, error) {
	cfg := config{
		STOMPAddr:               getenvDefault("SEP2_STOMP_ADDR", defaultSTOMPAddr),
		SimulationID:            os.Getenv("SEP2_SIMULATION_ID"),
		FeederMRID:              getenvDefault("SEP2_FEEDER_MRID", defaultFeederMRID),
		SEP2ServerAddr:          getenvDefault("SEP2_SERVER_ADDR", defaultSEP2ServerAddr),
		SEP2ServerCertDir:       getenvDefault("SEP2_SERVER_CERT_DIR", defaultSEP2ServerCertDir),
		SEP2DeviceCertMode:      getenvDefault("SEP2_DEVICE_CERT_MODE", defaultSEP2DeviceCertMode),
		SEP2AdminUIAddr:         getenvDefault("SEP2_ADMIN_UI_ADDR", defaultSEP2AdminUIAddr),
		SEP2AdminUIAllowedHosts: getenvList("SEP2_ADMIN_UI_ALLOWED_HOSTS"),
		SEP2AdminUISORLink:      getenvDefault("SEP2_ADMIN_UI_SOR_LINK", ""),
	}
	pubFromEnv, err := getenvBool("SEP2_PUBLISH_ON_START", false)
	if err != nil {
		return config{}, err
	}
	cfg.PublishOnStart = pubFromEnv

	// AllowPlaintext defaults false (fail-closed): see the field's doc
	// comment on config. Only an explicit env or flag override flips
	// it on.
	plaintextFromEnv, err := getenvBool("SEP2_STOMP_ALLOW_PLAINTEXT", false)
	if err != nil {
		return config{}, err
	}
	cfg.AllowPlaintext = plaintextFromEnv

	// SEP2AdminUIAllowNonLoopback mirrors AllowPlaintext's explicit
	// opt-in shape: defaults false, and only an explicit env or flag
	// override flips it on.
	adminUINonLoopbackFromEnv, err := getenvBool("SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK", false)
	if err != nil {
		return config{}, err
	}
	cfg.SEP2AdminUIAllowNonLoopback = adminUINonLoopbackFromEnv

	// SEP2EnableCCM mirrors AllowPlaintext's explicit opt-in shape:
	// defaults false, and only an explicit env or flag override flips it
	// on. See the field's doc comment for the observer trade-off this
	// makes.
	enableCCMFromEnv, err := getenvBool("SEP2_ENABLE_CCM", false)
	if err != nil {
		return config{}, err
	}
	cfg.SEP2EnableCCM = enableCCMFromEnv

	// SEP2CCMAllowNoObserver mirrors AllowPlaintext's explicit opt-in
	// shape: defaults false, and only an explicit env or flag override
	// flips it on. See the field's doc comment for what setting it
	// accepts.
	ccmAllowNoObserverFromEnv, err := getenvBool("SEP2_CCM_ALLOW_NO_OBSERVER", false)
	if err != nil {
		return config{}, err
	}
	cfg.SEP2CCMAllowNoObserver = ccmAllowNoObserverFromEnv

	// SEP2NotificationAllowLoopback mirrors AllowPlaintext's explicit
	// opt-in shape: defaults false, and only an explicit env or flag
	// override flips it on.
	notificationAllowLoopbackFromEnv, err := getenvBool("SEP2_NOTIFICATION_ALLOW_LOOPBACK", false)
	if err != nil {
		return config{}, err
	}
	cfg.SEP2NotificationAllowLoopback = notificationAllowLoopbackFromEnv

	// SEP2TelemetryPublishUnchanged defaults false: unchanged devices
	// are suppressed unless an operator explicitly asks for
	// full-snapshot semantics. See the field's doc comment.
	publishUnchangedFromEnv, err := getenvBool("SEP2_TELEMETRY_PUBLISH_UNCHANGED", false)
	if err != nil {
		return config{}, err
	}
	cfg.SEP2TelemetryPublishUnchanged = publishUnchangedFromEnv

	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.StringVar(&cfg.STOMPAddr, "stomp-addr", cfg.STOMPAddr, "GridAPPS-D STOMP broker host:port")
	// User and password flags register with an empty default so the
	// usage banner never echoes a real credential. The precedence merge
	// (flag, then env, then built-in default) happens below after Parse.
	var stompUserFlag, stompPasswordFlag string
	fs.StringVar(&stompUserFlag, "stomp-user", "", "STOMP login user (env: SEP2_STOMP_USER)")
	fs.StringVar(&stompPasswordFlag, "stomp-password", "", "STOMP login password (env: SEP2_STOMP_PASSWORD)")
	fs.StringVar(&cfg.SimulationID, "simulation-id", cfg.SimulationID, "GridAPPS-D simulation_id (empty disables sim subscribe)")
	fs.StringVar(&cfg.FeederMRID, "feeder-mrid", cfg.FeederMRID, "CIM feeder mRID to enumerate DERs from")
	fs.BoolVar(&cfg.PublishOnStart, "publish-on-start", cfg.PublishOnStart, "publish a smoke-test DifferenceBuilder envelope after registry bootstrap")
	fs.BoolVar(&cfg.AllowPlaintext, "stomp-allow-plaintext", cfg.AllowPlaintext, "dial the GridAPPS-D broker over plain TCP instead of TLS (dev-only; default false)")
	fs.StringVar(&cfg.SEP2ServerAddr, "sep2-server-addr", cfg.SEP2ServerAddr, "embedded IEEE 2030.5 mTLS listener host:port (defaults to loopback only)")
	fs.StringVar(&cfg.SEP2ServerCertDir, "sep2-server-cert-dir", cfg.SEP2ServerCertDir, "directory holding (or receiving dev-mint) the embedded server's CA/leaf cert material")
	fs.StringVar(&cfg.SEP2DeviceCertMode, "sep2-device-cert-mode", cfg.SEP2DeviceCertMode, `device identity certificate source: "dev-mint" (default) or "preprovisioned"`)
	fs.BoolVar(&cfg.SEP2EnableCCM, "sep2-enable-ccm", cfg.SEP2EnableCCM,
		"serve ONLY the mandatory TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8 suite (a client unable to offer it is refused, not served over GCM); requires -sep2-ccm-allow-no-observer (default false)")
	fs.BoolVar(&cfg.SEP2CCMAllowNoObserver, "sep2-ccm-allow-no-observer", cfg.SEP2CCMAllowNoObserver,
		"accept running -sep2-enable-ccm with the connection observer disabled, losing the rejected-device record (kept pending issue 127; default false)")
	fs.StringVar(&cfg.SEP2AdminUIAddr, "admin-ui-addr", cfg.SEP2AdminUIAddr, "admin UI read only HTTP listener host:port (defaults to loopback only)")
	fs.BoolVar(&cfg.SEP2AdminUIAllowNonLoopback, "admin-ui-allow-non-loopback", cfg.SEP2AdminUIAllowNonLoopback, "bind the admin UI listener to a non-loopback host (dev-only; default false)")
	// admin-ui-key registers with an empty default so flag.PrintDefaults
	// never echoes a real token, matching -stomp-user / -stomp-password
	// above. The precedence merge happens below after Parse.
	var adminUIKeyFlag string
	fs.StringVar(&adminUIKeyFlag, "admin-ui-key", "", "admin UI Bearer token; unset disables the admin UI entirely (env: SEP2_ADMIN_UI_KEY)")
	fs.StringVar(&cfg.SEP2AdminUISORLink, "admin-ui-sor-link", cfg.SEP2AdminUISORLink, "optional server of record dashboard URL exposed via the admin UI (env: SEP2_ADMIN_UI_SOR_LINK)")
	fs.BoolVar(&cfg.SEP2NotificationAllowLoopback, "sep2-notification-allow-loopback", cfg.SEP2NotificationAllowLoopback,
		"allow subscription notificationURIs to target any loopback destination on the host (dev/test-only; default false)")

	// sep2-registration-pin and sep2-registration-pin-file register with
	// an empty string default, then are parsed and validated by hand
	// after Parse below. A registered numeric flag default
	// cannot represent "unset" here, because 0 is itself a schema-legal
	// PIN (see SEP2RegistrationPIN's doc comment above); the empty
	// string sentinel resolves that ambiguity the same way the
	// credential flags above resolve theirs.
	var registrationPINFlag, registrationPINFileFlag string
	fs.StringVar(&registrationPINFlag, "sep2-registration-pin", "",
		"fleet-wide fallback IEEE 2030.5 registration PIN, 0-999999 with a valid section 6.3.5 check digit; unset means no fleet-wide fallback")
	fs.StringVar(&registrationPINFileFlag, "sep2-registration-pin-file", "",
		"path to a JSON object mapping device LFDI to that device's IEEE 2030.5 registration PIN; unset means no per-device PINs are configured")

	// sep2-battery-leg-list-file registers with an empty string default
	// and is resolved (flag, then env) after Parse below, matching the
	// poll/post rate flags' precedence dance rather than the PIN flags'
	// flag-only one: an operator running the bridge against a fixed
	// co-simulation feeder wants this set once in the environment, not
	// repeated on every invocation's command line.
	var batteryLegListFileFlag string
	fs.StringVar(&batteryLegListFileFlag, "sep2-battery-leg-list-file", "",
		"path to a plain-text file of utility battery leg EnergyConsumer mRIDs, one per line, in the model's form (upper case, no leading underscore); blank lines and lines starting with # are ignored (env: SEP2_BATTERY_LEG_LIST_FILE); unset means no utility battery legs are registered")

	// sep2-poll-rate and sep2-post-rate register an empty-string default
	// for the same reason the PIN flags above do: a numeric flag default
	// cannot represent "unset", and unset is a distinct, load-bearing
	// state here. Unset means the bridge advertises no rate at all, which
	// is exactly today's behavior; a registered numeric default would
	// silently start advertising a value to every existing deployment.
	var pollRateFlag, postRateFlag string
	fs.StringVar(&pollRateFlag, "sep2-poll-rate", "",
		"fleet-wide Registration pollRate in seconds, advertised to every device; unset advertises nothing and clients apply the sep.xsd default of 900")
	fs.StringVar(&postRateFlag, "sep2-post-rate", "",
		"fleet-wide MirrorUsagePoint postRate in seconds, stamped on every mirror a client creates; unset advertises nothing and leaves the client's own value untouched")

	// The default DERProgram's operator surface. -sep2-program-file is the
	// primary path and the one an admin UI is expected to write: a single
	// JSON object holding every field of this settings group, so the UI
	// rewrites one file and the bridge restarts, rather than the UI having
	// to synthesize a command line. The two scalar flags are the
	// dev-and-interop convenience path and override the file, matching the
	// flag-beats-file precedence the PIN flags already use.
	//
	// All three register an empty-string default for the reason the flags
	// above do: 0 is a legal primacy and "" is a legal description, so
	// neither can serve as its own unset sentinel.
	var programFileFlag, programPrimacyFlag, programDescriptionFlag string
	fs.StringVar(&programFileFlag, "sep2-program-file", "",
		"path to a JSON object configuring the seeded default DERProgram: {\"primacy\": 1, \"description\": \"...\"}; unset uses the compiled-in defaults")
	fs.StringVar(&programPrimacyFlag, "sep2-program-primacy", "",
		"primacy of the seeded default DERProgram, 0-2 or 65-191 (lower is higher priority); "+
			"unset uses 1, contracted premises service provider, which suits a co-simulation but NOT a field deployment, "+
			"where 1 outranks a DSO program in the 65-191 band; set it per the interconnection agreement")
	fs.StringVar(&programDescriptionFlag, "sep2-program-description", "",
		"description of the seeded default DERProgram, at most 32 characters (sep.xsd String32); unset uses the compiled-in default")

	// The issued-DERControl temporal surface. Both register an
	// empty-string default for the reason every flag above does: 0 is the
	// shipped randomization value and cannot be its own unset sentinel, and
	// a registered numeric duration default would mask the compiled-in one.
	var controlDurationFlag, controlRandomizeDurationFlag string
	fs.StringVar(&controlDurationFlag, "sep2-control-duration", "",
		"interval duration in seconds of every issued DERControl, after which the device falls back to DefaultDERControl; "+
			"unset uses 1800, twice the sep.xsd default poll rate; set it above the configured poll rate or controls expire between polls")
	fs.StringVar(&controlRandomizeDurationFlag, "sep2-control-randomize-duration", "",
		"randomizeDuration in seconds served on every issued DERControl, -3600 to 3600, staggering when devices revert to DefaultDERControl; "+
			"unset uses 0 for reproducible co-simulation runs; a field deployment should set a non-zero value")

	// The DefaultDERControl operator surface. File-only, with no scalar
	// convenience flags: unlike the program's primacy and description, these
	// are the values a device applies when nothing else is commanding it, so
	// the deliberate friction of writing a file is proportionate. It is also
	// the shape an admin UI rewrites.
	var defaultControlFileFlag string
	fs.StringVar(&defaultControlFileFlag, "sep2-default-control-file", "",
		"path to a JSON object configuring the seeded DefaultDERControl, e.g. {\"opModConnect\": true, \"opModMaxLimW\": 10000}; "+
			"unset ships a control that commands nothing, leaving each DER on its own IEEE 1547 autonomous behavior")

	// sep2-telemetry-interval registers with an empty-string default and
	// is parsed by hand after Parse, matching the PIN and rate flags
	// above: the empty string is the "operator said nothing" sentinel,
	// which is what lets the env fallback and the compiled-in default
	// resolve in that order without a registered default masking either.
	var telemetryIntervalFlag string
	fs.StringVar(&telemetryIntervalFlag, "sep2-telemetry-interval", "",
		"period of the DERStatus telemetry publisher as a Go duration (default 15s, matching the Python upstream)")
	fs.BoolVar(&cfg.SEP2TelemetryPublishUnchanged, "sep2-telemetry-publish-unchanged", cfg.SEP2TelemetryPublishUnchanged,
		"publish every device every interval instead of only those whose values changed (full-snapshot semantics; default false)")

	var versionFlag bool
	fs.BoolVar(&versionFlag, "version", false, "print the build version and exit")

	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("parse flags: %w", err)
	}

	// -version is a pure query flag: return before resolveCred's env
	// scrub and before validate's required-field checks run, so passing
	// -version alone (with none of the other flags/env set) always
	// succeeds and never has any side effect beyond reporting the
	// version. main checks for errVersionRequested with errors.Is, the
	// same pattern it already uses for flag.ErrHelp.
	if versionFlag {
		return config{}, errVersionRequested
	}

	// Resolve credential precedence: flag wins if non-empty, else env,
	// else the compiled-in default. The flag value comes through as
	// empty when the user did not pass -stomp-user / -stomp-password,
	// in which case the env-or-default is the right answer.
	cfg.STOMPUser = resolveCred(stompUserFlag, "SEP2_STOMP_USER", defaultSTOMPUser)
	cfg.STOMPPassword = resolveCred(stompPasswordFlag, "SEP2_STOMP_PASSWORD", defaultSTOMPPassword)

	// SEP2AdminUIKey has no compiled-in fallback: an empty result here
	// (no flag, no env) is the intentional "admin UI disabled" state,
	// not a missing-required-field error. resolveCred's empty-string
	// fallback argument encodes exactly that.
	cfg.SEP2AdminUIKey = resolveCred(adminUIKeyFlag, "SEP2_ADMIN_UI_KEY", "")

	// registrationPINFlag / registrationPINFileFlag are resolved here,
	// before validate, so a malformed value stops the bridge at config
	// load, before any network I/O (connectClient has not run yet: see
	// main's call order). Domain validation (0-999999 range, section
	// 6.3.5 check digit) is deliberately NOT duplicated here: it lives
	// in sep2config.SEP2Policy.ValidateRegistrationPIN, the single place
	// that logic already lives, and buildSEP2Policy in main.go calls it
	// on both of these fields before the bridge serves anything.
	if registrationPINFlag != "" {
		pin, err := parseRegistrationPINFlag(registrationPINFlag)
		if err != nil {
			return config{}, err
		}
		cfg.SEP2RegistrationPIN = &pin
	}
	if registrationPINFileFlag != "" {
		pins, err := loadRegistrationPINFile(registrationPINFileFlag)
		if err != nil {
			return config{}, err
		}
		cfg.SEP2RegistrationPINs = pins
	}

	// batteryLegListFileFlag resolves here for the same reason the PIN
	// fields do: a malformed file must stop the bridge at config load,
	// before any network I/O. Flag wins over env, matching every other
	// non-credential field's precedence in this loader.
	if batteryLegListFileFlag == "" {
		batteryLegListFileFlag = os.Getenv("SEP2_BATTERY_LEG_LIST_FILE")
	}
	if batteryLegListFileFlag != "" {
		legs, err := loadBatteryLegListFile(batteryLegListFileFlag)
		if err != nil {
			return config{}, err
		}
		cfg.SEP2BatteryLegs = legs
	}

	// Rate flags resolve here alongside the PIN flags, and for the same
	// reason: a malformed value must stop the bridge at config load,
	// before any network I/O. Syntax only; the domain rule (a rate must
	// be at least 1 second) lives in
	// sep2config.SEP2Policy.ValidateRates, called from buildSEP2Policy,
	// so it is stated once and applies equally to the per-device rates
	// that config layer will carry later.
	//
	// The env fallbacks match the flag names' existing SEP2_ convention.
	// Flags shadow envs, as everywhere else in this loader.
	if pollRateFlag == "" {
		pollRateFlag = os.Getenv("SEP2_POLL_RATE")
	}
	if postRateFlag == "" {
		postRateFlag = os.Getenv("SEP2_POST_RATE")
	}
	if pollRateFlag != "" {
		rate, err := parseRateFlag(pollRateFlag, "-sep2-poll-rate")
		if err != nil {
			return config{}, err
		}
		cfg.SEP2PollRate = &rate
	}
	if postRateFlag != "" {
		rate, err := parseRateFlag(postRateFlag, "-sep2-post-rate")
		if err != nil {
			return config{}, err
		}
		cfg.SEP2PostRate = &rate
	}

	// The default DERProgram resolves file first, then the scalar flags,
	// so a flag overrides the file rather than the other way round.
	if programFileFlag != "" {
		primacy, description, err := loadProgramFile(programFileFlag)
		if err != nil {
			return config{}, err
		}
		cfg.SEP2ProgramPrimacy = primacy
		cfg.SEP2ProgramDescription = description
	}
	if programPrimacyFlag != "" {
		v, err := strconv.ParseUint(programPrimacyFlag, 10, 8)
		if err != nil {
			return config{}, fmt.Errorf(
				"config: -sep2-program-primacy value %q must be a base-10 integer in [0, 255]", programPrimacyFlag)
		}
		primacy := uint8(v)
		cfg.SEP2ProgramPrimacy = &primacy
	}
	if programDescriptionFlag != "" {
		// Length is not checked here: the String32 bound is an IEEE 2030.5
		// domain rule and lives in ValidateDefaultProgram with the rest of
		// them, so there is one place an operator's value is judged.
		cfg.SEP2ProgramDescription = &programDescriptionFlag
	}

	// Only representability is judged here, exactly as for the primacy flag
	// above: whether the digits fit the wire type. Whether the VALUE is
	// usable (a nonzero duration, a randomization inside OneHourRangeType
	// and narrower than the duration) is an IEEE 2030.5 domain question and
	// lives in ValidateDERControl, so an operator gets one message for it
	// wherever the value came from.
	if controlDurationFlag != "" {
		v, err := strconv.ParseUint(controlDurationFlag, 10, 32)
		if err != nil {
			return config{}, fmt.Errorf(
				"config: -sep2-control-duration value %q must be a base-10 integer in [0, 4294967295] seconds", controlDurationFlag)
		}
		duration := uint32(v)
		cfg.SEP2ControlDuration = &duration
	}
	if controlRandomizeDurationFlag != "" {
		v, err := strconv.ParseInt(controlRandomizeDurationFlag, 10, 32)
		if err != nil {
			return config{}, fmt.Errorf(
				"config: -sep2-control-randomize-duration value %q must be a base-10 integer in seconds, negative permitted", controlRandomizeDurationFlag)
		}
		randomize := int32(v)
		cfg.SEP2ControlRandomizeDuration = &randomize
	}

	if defaultControlFileFlag != "" {
		connect, energize, maxLimW, err := loadDefaultControlFile(defaultControlFileFlag)
		if err != nil {
			return config{}, err
		}
		cfg.SEP2DefaultControlOpModConnect = connect
		cfg.SEP2DefaultControlOpModEnergize = energize
		cfg.SEP2DefaultControlOpModMaxLimW = maxLimW
	}

	// The telemetry interval resolves flag, then env, then the
	// compiled-in default, and is validated here so an unusable value
	// stops the bridge at config load rather than at the publisher's
	// first tick (time.NewTicker panics on a non-positive period).
	if telemetryIntervalFlag == "" {
		telemetryIntervalFlag = os.Getenv("SEP2_TELEMETRY_INTERVAL")
	}
	if telemetryIntervalFlag == "" {
		cfg.SEP2TelemetryInterval = telemetrypub.DefaultInterval
	} else {
		interval, err := time.ParseDuration(telemetryIntervalFlag)
		if err != nil {
			return config{}, fmt.Errorf(
				"config: -sep2-telemetry-interval / SEP2_TELEMETRY_INTERVAL value %q must be a Go duration such as \"15s\" or \"1m\"",
				telemetryIntervalFlag)
		}
		if interval <= 0 {
			return config{}, fmt.Errorf(
				"config: -sep2-telemetry-interval / SEP2_TELEMETRY_INTERVAL value %q must be greater than zero", telemetryIntervalFlag)
		}
		cfg.SEP2TelemetryInterval = interval
	}

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// parseRateFlag parses a poll or post rate flag's raw string into a uint32,
// rejecting anything that is not a base-10 non-negative integer fitting in
// 32 bits. flagName is embedded in the error so an operator is told which
// knob to fix rather than being handed a bare parse failure.
//
// Unlike parseRegistrationPINFlag, the offending value IS echoed: a rate is
// operational configuration, not a shared secret, and seeing what was
// actually parsed is what makes a typo obvious.
//
// The lower bound (a rate must be at least 1) is deliberately NOT enforced
// here. It is a domain rule about what the rate MEANS, it applies identically
// to per-device rates this flag does not carry, and duplicating it would
// create a second place to update. sep2config.SEP2Policy.ValidateRates owns
// it, and buildSEP2Policy runs that before the bridge dials anything.
// loadProgramFile reads -sep2-program-file: a single JSON object holding
// the seeded default DERProgram's operator-settable fields.
//
//	{"primacy": 1, "description": "GridAPPS-D DER program"}
//
// This is the shape an admin UI is expected to write, which is why it is a
// whole-object file rather than a flag: a UI can rewrite it and the operator
// restarts the bridge, with no command line to synthesize. Both members are
// optional, and each returns nil when absent so the compiled-in default
// survives. That is the reason for the pointer returns rather than an
// (uint8, string) pair: a file setting only description must not silently
// reset primacy to 0, which is a legal and higher-priority value.
//
// Only syntax and JSON type are judged here. The IEEE 2030.5 domain rules
// (String32 bound, reserved primacy bands) live in
// sep2config.SEP2Policy.ValidateDefaultProgram, so an operator sees the same
// message whether the value came from this file or from a flag.
func loadProgramFile(path string) (*uint8, *string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("config: -sep2-program-file %q does not exist", path)
		}
		return nil, nil, fmt.Errorf("config: -sep2-program-file %q is not readable: %w", path, err)
	}

	// Decode into map[string]any with UseNumber and type-check each member
	// by hand, rather than into a struct with json.Number fields.
	// encoding/json's Number silently accepts a QUOTED numeric string
	// ("1") as though it were a bare JSON number, so a struct decode would
	// let a value of the wrong JSON type through. Same reasoning, and the
	// same hazard, as loadRegistrationPINFile above.
	var members map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&members); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, nil, fmt.Errorf(
				"config: -sep2-program-file %q is not a JSON object", path)
		}
		return nil, nil, fmt.Errorf("config: -sep2-program-file %q is not valid JSON: %w", path, err)
	}

	// An unrecognized member is an error, not a silently ignored setting.
	// An operator who writes "primacy_value" and sees the bridge come up on
	// the default has no way to tell the setting was dropped; that is the
	// class of config bug only ever noticed on the wire, days later.
	for k := range members {
		if k != "primacy" && k != "description" {
			return nil, nil, fmt.Errorf(
				"config: -sep2-program-file %q: unknown member %q (want \"primacy\" or \"description\")", path, k)
		}
	}

	var primacy *uint8
	if v, ok := members["primacy"]; ok {
		num, ok := v.(json.Number)
		if !ok {
			return nil, nil, fmt.Errorf(
				"config: -sep2-program-file %q: \"primacy\" is not a JSON number", path)
		}
		i, err := num.Int64()
		if err != nil {
			return nil, nil, fmt.Errorf(
				"config: -sep2-program-file %q: \"primacy\" must be an integer", path)
		}
		// Bounded here, not in ValidateDefaultProgram, because this is a
		// representability question rather than an IEEE 2030.5 one: primacy
		// is a UInt8 on the wire, and a value outside that range cannot be
		// carried at all. Refusing beats the silent truncation a bare
		// uint8(i) conversion would do.
		if i < 0 || i > 255 {
			return nil, nil, fmt.Errorf(
				"config: -sep2-program-file %q: \"primacy\" %d is outside the PrimacyType range [0, 255]", path, i)
		}
		u := uint8(i)
		primacy = &u
	}

	var description *string
	if v, ok := members["description"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, nil, fmt.Errorf(
				"config: -sep2-program-file %q: \"description\" is not a JSON string", path)
		}
		description = &s
	}
	return primacy, description, nil
}

// maxOpModMaxLimW is PerCent's own XSD range ceiling (IEEE 2030.5-2018
// Annex B.2.3.4, "PerCent object (UInt16)": 0 to 10000, hundredths of a
// percent), restated here because loadDefaultControlFile judges
// representability before core's own sep2.PerCent ever sees the value.
const maxOpModMaxLimW = 10000

// loadDefaultControlFile reads -sep2-default-control-file: a single JSON
// object holding the operator's overrides for the seeded DefaultDERControl,
// the control a device applies when no DERControl is active.
//
//	{"opModConnect": true, "opModEnergize": true, "opModMaxLimW": 5000}
//
// All three members are optional and each returns nil when absent, so an
// unmentioned member keeps the compiled-in value rather than being reset.
// That is why the two booleans return pointers rather than bare bools
// (false is a meaningful setting for both, so the zero value cannot double
// as "absent") and why opModMaxLimW returns a pointer too (0 is a
// meaningful percent: full curtailment).
//
// WHY opModMaxLimW BUT NOT THE OTHER POWER FIELDS. All three are
// power-related, but only a cap is exposed here:
//
//   - opModMaxLimW is a ceiling on generation (IEEE 2030.5-2018 Annex
//     B.2.22), not a fixed dispatch: the device still regulates
//     autonomously below it, so 1547-2018 clause 5.3's mutual-exclusivity
//     rule for FIXED settings does not reach it.
//   - opModTargetW and opModTargetVar remain unexposed. Setting either in
//     the FALLBACK puts the device into a fixed-power mode whenever
//     nothing else is commanding it, which disables its own autonomous
//     volt-var and curtailment behavior (clause 5.3). A fallback that
//     suppresses the device's autonomy is not a fallback.
//   - setGradW and the setES* family remain unexposed. sep.xsd:3306 and
//     sep.xsd:3271 say each SHALL update the corresponding DERSettings
//     value, which is an installer-owned persistent write to the device's
//     commissioned configuration, not a control-channel default.
//
// An unrecognized member is therefore an error rather than a silently
// ignored setting, the same as in loadProgramFile: an operator who writes a
// member this bridge does not honor should be told, not left to discover it
// on the wire.
//
// Only syntax and JSON type are judged here.
func loadDefaultControlFile(path string) (opModConnect, opModEnergize *bool, opModMaxLimW *uint16, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, fmt.Errorf("config: -sep2-default-control-file %q does not exist", path)
		}
		return nil, nil, nil, fmt.Errorf("config: -sep2-default-control-file %q is not readable: %w", path, err)
	}

	var members map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&members); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, nil, nil, fmt.Errorf(
				"config: -sep2-default-control-file %q is not a JSON object", path)
		}
		return nil, nil, nil, fmt.Errorf("config: -sep2-default-control-file %q is not valid JSON: %w", path, err)
	}

	for k := range members {
		if k != "opModConnect" && k != "opModEnergize" && k != "opModMaxLimW" {
			return nil, nil, nil, fmt.Errorf(
				"config: -sep2-default-control-file %q: unknown member %q (want \"opModConnect\", \"opModEnergize\" or \"opModMaxLimW\"); "+
					"power targets and the setGradW/setES settings are deliberately not configurable here", path, k)
		}
	}

	for _, m := range []struct {
		name string
		out  **bool
	}{
		{"opModConnect", &opModConnect},
		{"opModEnergize", &opModEnergize},
	} {
		v, ok := members[m.name]
		if !ok {
			continue
		}
		b, ok := v.(bool)
		if !ok {
			return nil, nil, nil, fmt.Errorf(
				"config: -sep2-default-control-file %q: %q is not a JSON boolean", path, m.name)
		}
		*m.out = &b
	}

	if v, ok := members["opModMaxLimW"]; ok {
		n, ok := v.(json.Number)
		if !ok {
			return nil, nil, nil, fmt.Errorf(
				"config: -sep2-default-control-file %q: %q is not a JSON integer", path, "opModMaxLimW")
		}
		i, err := n.Int64()
		if err != nil {
			return nil, nil, nil, fmt.Errorf(
				"config: -sep2-default-control-file %q: %q value %q is not a base-10 integer", path, "opModMaxLimW", n.String())
		}
		if i < 0 || i > maxOpModMaxLimW {
			return nil, nil, nil, fmt.Errorf(
				"config: -sep2-default-control-file %q: %q value %d out of range [0, %d]", path, "opModMaxLimW", i, maxOpModMaxLimW)
		}
		u := uint16(i)
		opModMaxLimW = &u
	}

	return opModConnect, opModEnergize, opModMaxLimW, nil
}

func parseRateFlag(raw, flagName string) (uint32, error) {
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf(
			"config: %s value %q must be a base-10, non-negative integer that fits in 32 bits", flagName, raw)
	}
	return uint32(v), nil
}

// parseRegistrationPINFlag parses -sep2-registration-pin's raw string
// into a uint32. It only rejects syntax: a non-numeric string, a
// negative number, or a value too large for 32 bits. The IEEE 2030.5
// domain checks (0-999999 range, section 6.3.5 check digit) are left to
// sep2config.SEP2Policy.ValidateRegistrationPIN, called from
// buildSEP2Policy in main.go.
//
// The returned error deliberately never echoes raw: even syntactically
// invalid input may be an operator's mistyped PIN, and the PIN is a
// shared secret in the registration flow that must never appear in a
// log or error message (see SEP2Policy.RegistrationPINs's doc comment
// in internal/sep2config/policy.go).
func parseRegistrationPINFlag(raw string) (uint32, error) {
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, errors.New("config: -sep2-registration-pin must be a base-10, non-negative integer that fits in 32 bits")
	}
	return uint32(v), nil
}

// loadRegistrationPINFile reads and validates the JSON file at path,
// which must be a flat object mapping device LFDI to that device's
// IEEE 2030.5 registration PIN (e.g. {"F0FA1AC6...": 123455}). Keys are
// stored exactly as they appear in the file: ResolveRegistrationPIN
// normalizes case at lookup time, so this loader does not need to.
//
// Every failure mode below returns a distinct, named error and this
// function never falls back to any other data source on a bad file: an
// operator's path typo or a malformed entry must produce a fail-closed
// boot, not a bridge that quietly boots with the fleet default (or with
// no PIN at all) and then rejects every device at seeding time with a
// confusing, hard to trace error.
//
// Only file shape and JSON-number-vs-integer syntax are checked here.
// The IEEE 2030.5 domain checks (0-999999 range, section 6.3.5 check
// digit) are deliberately NOT duplicated here: they live in
// sep2config.SEP2Policy.ValidateRegistrationPIN, called from
// buildSEP2Policy in main.go before the bridge serves anything.
//
// No parsed PIN value is ever included in a returned error: only the
// path and, for a per-entry problem, the LFDI (which is not secret, see
// SEP2Policy.RegistrationPINs's doc comment) are named.
func loadRegistrationPINFile(path string) (map[string]uint32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config: -sep2-registration-pin-file %q does not exist", path)
		}
		return nil, fmt.Errorf("config: -sep2-registration-pin-file %q is not readable: %w", path, err)
	}

	// Decode into map[string]interface{} with UseNumber, then type-check
	// each value by hand, rather than decoding straight into
	// map[string]json.Number: encoding/json's Number type silently
	// accepts a quoted numeric string ("123455") as if it were a bare
	// JSON number, which would let a value of the wrong JSON type pass
	// as "a flat object of string to number" when it is not one.
	var entries map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&entries); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			// The bytes parsed as JSON but the top-level value is not
			// an object (e.g. an array, a bare number, or a string):
			// a distinct case from a syntax error, so it gets its own
			// message.
			return nil, fmt.Errorf("config: -sep2-registration-pin-file %q is not a flat JSON object of LFDI to PIN", path)
		}
		return nil, fmt.Errorf("config: -sep2-registration-pin-file %q is not valid JSON: %w", path, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("config: -sep2-registration-pin-file %q contains no entries", path)
	}

	pins := make(map[string]uint32, len(entries))
	for lfdi, val := range entries {
		num, ok := val.(json.Number)
		if !ok {
			return nil, fmt.Errorf(
				"config: -sep2-registration-pin-file %q: entry %q is not a JSON number",
				path, lfdi)
		}
		i, err := num.Int64()
		if err != nil {
			return nil, fmt.Errorf("config: -sep2-registration-pin-file %q: entry %q is not an integer", path, lfdi)
		}
		if i < 0 || i > int64(sep2config.MaxRegistrationPIN) {
			return nil, fmt.Errorf(
				"config: -sep2-registration-pin-file %q: entry %q is out of the IEEE 2030.5 PIN range [0, %d]",
				path, lfdi, sep2config.MaxRegistrationPIN)
		}
		pins[lfdi] = uint32(i)
	}
	return pins, nil
}

// batteryLegMRIDPattern is an ALLOWLIST for the shape of a battery-leg
// mRID line in a -sep2-battery-leg-list-file: the model's bare,
// uppercase-hex-and-dash mRID form (e.g.
// "CA0A0024-DA79-4395-9B05-6A7B9DE0AED9"), the same shape
// internal/cim's queries store c:IdentifiedObject.mRID in. Rejecting
// anything outside [0-9A-F-] is what "upper case, no leading
// underscore" in the file-format contract means in practice: a
// lowercase or underscore-prefixed entry (the form some spreadsheet
// exports carry, see craigpnnl/EPRI_Client#62) is a format error here,
// not a value this loader normalizes.
var batteryLegMRIDPattern = regexp.MustCompile(`^[0-9A-F-]{8,}$`)

// loadBatteryLegListFile reads and validates the plain-text file at
// path: one EnergyConsumer mRID per line, in the model's form (upper
// case, no leading underscore; see batteryLegMRIDPattern), with blank
// lines and lines starting with "#" ignored. It rejects a malformed
// line and a mRID repeated within the file, each by name, and never
// falls back to an empty or partial list on a bad file: an operator's
// path typo or a malformed entry must fail the boot outright, mirroring
// loadRegistrationPINFile's fail-closed shape.
//
// This function checks only the file's own syntax and internal
// consistency. Whether each mRID actually names an EnergyConsumer on
// the configured feeder, and whether it is also a house load, is a
// model-dependent question this loader cannot answer without a network
// round trip; bootstrapRegistry checks both against the live CIM model
// at boot, per issue #115.
func loadBatteryLegListFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config: -sep2-battery-leg-list-file %q does not exist", path)
		}
		return nil, fmt.Errorf("config: -sep2-battery-leg-list-file %q is not readable: %w", path, err)
	}

	seen := make(map[string]struct{})
	var legs []string
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !batteryLegMRIDPattern.MatchString(line) {
			return nil, fmt.Errorf(
				"config: -sep2-battery-leg-list-file %q: line %d: %q is not an mRID in the model's form (upper case, no leading underscore)",
				path, i+1, line)
		}
		if _, dup := seen[line]; dup {
			return nil, fmt.Errorf("config: -sep2-battery-leg-list-file %q: mRID %q is duplicated", path, line)
		}
		seen[line] = struct{}{}
		legs = append(legs, line)
	}
	if len(legs) == 0 {
		return nil, fmt.Errorf("config: -sep2-battery-leg-list-file %q contains no mRIDs", path)
	}
	return legs, nil
}

// resolveCred returns the first non-empty value among the parsed flag,
// the named env var, and the compiled-in fallback. Used for credential
// fields whose flag defaults are intentionally registered as empty so
// flag.PrintDefaults never echoes a real value.
//
// As a side effect, the env var is unset after the read so it does not
// remain visible via /proc/<pid>/environ for the rest of process
// lifetime. The resolved value still lives on the config struct (and
// thus in heap memory) but is no longer reachable to anything that
// only reads the process environment.
func resolveCred(flagVal, envKey, fallback string) string {
	if flagVal != "" {
		// Even when the flag wins, scrub the env var so a leftover
		// export does not surface to /proc/<pid>/environ.
		os.Unsetenv(envKey)
		return flagVal
	}
	v, ok := os.LookupEnv(envKey)
	os.Unsetenv(envKey)
	if ok && v != "" {
		return v
	}
	return fallback
}

// validate enforces the minimum field set the run loop assumes. Empty
// SimulationID is allowed: the bridge still validates connect plus CIM
// query plus registry; the Pump will just sit idle. SEP2AdminUIKey is
// deliberately NOT checked here: an empty key is the intentional
// "admin UI disabled" state (adminui.New's fail closed contract), not a
// missing-required-field error.
func (c config) validate() error {
	if c.STOMPAddr == "" {
		return errors.New("config: SEP2_STOMP_ADDR / -stomp-addr is required")
	}
	if c.STOMPUser == "" {
		return errors.New("config: SEP2_STOMP_USER / -stomp-user is required")
	}
	if c.FeederMRID == "" {
		return errors.New("config: SEP2_FEEDER_MRID / -feeder-mrid is required")
	}
	if c.PublishOnStart && c.SimulationID == "" {
		return errors.New("config: -publish-on-start requires SEP2_SIMULATION_ID")
	}
	if c.SEP2ServerAddr == "" {
		return errors.New("config: SEP2_SERVER_ADDR / -sep2-server-addr is required")
	}
	if c.SEP2ServerCertDir == "" {
		return errors.New("config: SEP2_SERVER_CERT_DIR / -sep2-server-cert-dir is required")
	}
	if c.SEP2DeviceCertMode != deviceCertModeDevMintFlag && c.SEP2DeviceCertMode != deviceCertModePreprovisionedFlag {
		return fmt.Errorf("config: SEP2_DEVICE_CERT_MODE / -sep2-device-cert-mode must be %q or %q, got %q",
			deviceCertModeDevMintFlag, deviceCertModePreprovisionedFlag, c.SEP2DeviceCertMode)
	}
	// cmd/bridge forces the connection observer off whenever SEP2EnableCCM
	// is set (ccmObservationDisabled, main.go), even though the CCM-8
	// listener has had a handshake-observation seam since this package's
	// mtls.go migration: SEP2EnableCCM and SEP2CCMAllowNoObserver are kept
	// as configuration surface pending issue 127, which removes both. So
	// SEP2EnableCCM alone would silently drop the connection observer: no
	// rejected-device record, no per-LFDI last-seen, no request counts or
	// paths, and nothing in the admin UI or the logs to show it happened.
	// Refuse to start rather than let that loss be implied; the operator
	// must choose it explicitly via SEP2CCMAllowNoObserver, or it is not
	// chosen at all.
	if c.SEP2EnableCCM && !c.SEP2CCMAllowNoObserver {
		return fmt.Errorf(
			"config: SEP2_ENABLE_CCM / -sep2-enable-ccm is set without SEP2_CCM_ALLOW_NO_OBSERVER / -sep2-ccm-allow-no-observer: " +
				"this flag pair is kept as configuration surface pending issue 127 (both settings are scheduled for removal), and setting it drops the connection observer, " +
				"losing the rejected-device record, per-LFDI last-seen, and request counts and paths; " +
				"the admin UI panel's served-status table would show every served device's connection status as unknown, not connected or disconnected; " +
				"its connected-clients table would show no clients rather than admitting it cannot tell; " +
				"its handshake-attempts table would show no handshakes rather than admitting it cannot tell; " +
				"and a client unable to offer CCM-8 would be refused with the refusal reaching the process log but never the panel; " +
				"set SEP2_CCM_ALLOW_NO_OBSERVER / -sep2-ccm-allow-no-observer=true to accept those losses, or leave SEP2_ENABLE_CCM unset")
	}
	return nil
}

// getenvDefault returns the env var if set and non-empty, else fallback.
func getenvDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getenvBool parses a boolean env var. Empty / unset returns fallback.
// Anything strconv.ParseBool accepts is honored.
func getenvBool(key string, fallback bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s is not a boolean (%q): %w", key, v, err)
	}
	return parsed, nil
}

// getenvList reads a comma separated env var into a string slice,
// trimming surrounding whitespace from each entry and dropping empty
// entries (so a trailing comma or repeated commas do not produce a
// blank allowlist entry that could accidentally match an empty Host
// header). Unset or empty returns nil, not an empty non-nil slice: the
// admin UI's own AllowedHosts zero value already means "no extra
// hosts", so there is no meaningful distinction here between nil and
// empty for this field.
func getenvList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
