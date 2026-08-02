package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
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
	// the embed has no per-device ACL yet (GAGO-043 follow-up), so a
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
	// adminui.New's fail closed ErrDisabled contract (GAGO-058). An
	// operator opts in to the admin UI by setting this explicitly.
	SEP2AdminUIKey string

	// SEP2AdminUIAllowedHosts is an additional, comma separated set of
	// Host header values the admin UI's host allowlist middleware
	// accepts, beyond its own built in defaults (localhost, 127.0.0.1,
	// ::1). See adminui.Config.AllowedHosts.
	SEP2AdminUIAllowedHosts []string

	// SEP2AdminUISORLink is an optional, operator supplied URL to a
	// server of record dashboard, exposed read only via the admin UI's
	// /api/health endpoint (GAGO-075). Not a credential: unlike
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
	// comment for why (GAGO-043, no per-device ACL yet).
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
	fs.StringVar(&cfg.SEP2AdminUIAddr, "admin-ui-addr", cfg.SEP2AdminUIAddr, "admin UI read only HTTP listener host:port (defaults to loopback only)")
	fs.BoolVar(&cfg.SEP2AdminUIAllowNonLoopback, "admin-ui-allow-non-loopback", cfg.SEP2AdminUIAllowNonLoopback, "bind the admin UI listener to a non-loopback host (dev-only; default false)")
	// admin-ui-key registers with an empty default so flag.PrintDefaults
	// never echoes a real token, matching -stomp-user / -stomp-password
	// above. The precedence merge happens below after Parse.
	var adminUIKeyFlag string
	fs.StringVar(&adminUIKeyFlag, "admin-ui-key", "", "admin UI Bearer token; unset disables the admin UI entirely (env: SEP2_ADMIN_UI_KEY)")
	fs.StringVar(&cfg.SEP2AdminUISORLink, "admin-ui-sor-link", cfg.SEP2AdminUISORLink, "optional server of record dashboard URL exposed via the admin UI (env: SEP2_ADMIN_UI_SOR_LINK)")

	// sep2-registration-pin and sep2-registration-pin-file register with
	// an empty string default, then are parsed and validated by hand
	// after Parse below (GAGO-PIN). A registered numeric flag default
	// cannot represent "unset" here, because 0 is itself a schema-legal
	// PIN (see SEP2RegistrationPIN's doc comment above); the empty
	// string sentinel resolves that ambiguity the same way the
	// credential flags above resolve theirs.
	var registrationPINFlag, registrationPINFileFlag string
	fs.StringVar(&registrationPINFlag, "sep2-registration-pin", "",
		"fleet-wide fallback IEEE 2030.5 registration PIN, 0-999999 with a valid section 6.3.5 check digit; unset means no fleet-wide fallback")
	fs.StringVar(&registrationPINFileFlag, "sep2-registration-pin-file", "",
		"path to a JSON object mapping device LFDI to that device's IEEE 2030.5 registration PIN; unset means no per-device PINs are configured")

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

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
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
// "admin UI disabled" state (GAGO-058's fail closed contract), not a
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
