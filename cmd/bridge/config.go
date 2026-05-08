package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
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
}

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
		STOMPAddr:    getenvDefault("SEP2_STOMP_ADDR", defaultSTOMPAddr),
		SimulationID: os.Getenv("SEP2_SIMULATION_ID"),
		FeederMRID:   getenvDefault("SEP2_FEEDER_MRID", defaultFeederMRID),
	}
	pubFromEnv, err := getenvBool("SEP2_PUBLISH_ON_START", false)
	if err != nil {
		return config{}, err
	}
	cfg.PublishOnStart = pubFromEnv

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

	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("parse flags: %w", err)
	}

	// Resolve credential precedence: flag wins if non-empty, else env,
	// else the compiled-in default. The flag value comes through as
	// empty when the user did not pass -stomp-user / -stomp-password,
	// in which case the env-or-default is the right answer.
	cfg.STOMPUser = resolveCred(stompUserFlag, "SEP2_STOMP_USER", defaultSTOMPUser)
	cfg.STOMPPassword = resolveCred(stompPasswordFlag, "SEP2_STOMP_PASSWORD", defaultSTOMPPassword)

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// resolveCred returns the first non-empty value among the parsed flag,
// the named env var, and the compiled-in fallback. Used for credential
// fields whose flag defaults are intentionally registered as empty so
// flag.PrintDefaults never echoes a real value.
func resolveCred(flagVal, envKey, fallback string) string {
	if flagVal != "" {
		return flagVal
	}
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		return v
	}
	return fallback
}

// validate enforces the minimum field set the run loop assumes. Empty
// SimulationID is allowed: the bridge still validates connect plus CIM
// query plus registry; the Pump will just sit idle.
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
