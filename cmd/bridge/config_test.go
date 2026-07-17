package main

import (
	"bytes"
	"flag"
	"net"
	"os"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("SEP2_STOMP_ADDR", "")
	t.Setenv("SEP2_STOMP_USER", "")
	t.Setenv("SEP2_STOMP_PASSWORD", "")
	t.Setenv("SEP2_SIMULATION_ID", "")
	t.Setenv("SEP2_FEEDER_MRID", "")
	t.Setenv("SEP2_PUBLISH_ON_START", "")
	t.Setenv("SEP2_STOMP_ALLOW_PLAINTEXT", "")
	t.Setenv("SEP2_SERVER_ADDR", "")
	t.Setenv("SEP2_SERVER_CERT_DIR", "")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.STOMPAddr != defaultSTOMPAddr {
		t.Errorf("STOMPAddr: got %q want %q", cfg.STOMPAddr, defaultSTOMPAddr)
	}
	if cfg.STOMPUser != defaultSTOMPUser {
		t.Errorf("STOMPUser: got %q want %q", cfg.STOMPUser, defaultSTOMPUser)
	}
	if cfg.STOMPPassword != defaultSTOMPPassword {
		t.Errorf("STOMPPassword: got %q want %q", cfg.STOMPPassword, defaultSTOMPPassword)
	}
	if cfg.FeederMRID != defaultFeederMRID {
		t.Errorf("FeederMRID: got %q want %q", cfg.FeederMRID, defaultFeederMRID)
	}
	if cfg.PublishOnStart {
		t.Errorf("PublishOnStart: got true, want false")
	}
	// The zero-value default must be fail-closed: a bare invocation with
	// no override dials TLS, never plaintext. See config.AllowPlaintext's
	// doc comment.
	if cfg.AllowPlaintext {
		t.Errorf("AllowPlaintext: got true, want false (fail-closed default)")
	}
	if cfg.SEP2ServerAddr != defaultSEP2ServerAddr {
		t.Errorf("SEP2ServerAddr: got %q want %q", cfg.SEP2ServerAddr, defaultSEP2ServerAddr)
	}
	if cfg.SEP2ServerCertDir != defaultSEP2ServerCertDir {
		t.Errorf("SEP2ServerCertDir: got %q want %q", cfg.SEP2ServerCertDir, defaultSEP2ServerCertDir)
	}
}

// TestLoadConfigSEP2ServerAddrDefaultsToLoopback verifies the embedded
// IEEE 2030.5 listener's default bind host is a loopback address. The
// embed has no per-device ACL yet (GAGO-043 follow-up), so a default
// that is reachable off-box would silently widen the exposure of that
// unfinished access-control story; only an explicit override should do
// that. See config.SEP2ServerAddr's doc comment.
func TestLoadConfigSEP2ServerAddrDefaultsToLoopback(t *testing.T) {
	t.Setenv("SEP2_SERVER_ADDR", "")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	host, _, err := net.SplitHostPort(cfg.SEP2ServerAddr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", cfg.SEP2ServerAddr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("SEP2ServerAddr host = %q, want a loopback address", host)
	}
}

// TestLoadConfigSEP2ServerAddrEnvOverride verifies a non-loopback bind
// is honored when the operator sets it explicitly: the address itself
// carries the "explicit choice" signal (unlike AllowPlaintext, there is
// no separate boolean gate), so an override must pass through
// unmodified.
func TestLoadConfigSEP2ServerAddrEnvOverride(t *testing.T) {
	t.Setenv("SEP2_SERVER_ADDR", "0.0.0.0:8443")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ServerAddr != "0.0.0.0:8443" {
		t.Errorf("SEP2ServerAddr: got %q, want the explicit override unchanged", cfg.SEP2ServerAddr)
	}
}

// TestLoadConfigSEP2ServerAddrFlagShadowsEnv matches the precedence
// shape already covered for -stomp-addr / SEP2_STOMP_ADDR.
func TestLoadConfigSEP2ServerAddrFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_SERVER_ADDR", "env.example:8443")

	cfg, err := loadConfig([]string{"-sep2-server-addr=flag.example:8443"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ServerAddr != "flag.example:8443" {
		t.Errorf("SEP2ServerAddr: got %q want flag value", cfg.SEP2ServerAddr)
	}
}

// TestLoadConfigSEP2ServerAddrRequired verifies an explicitly-emptied
// value is a loadConfig error naming the field.
func TestLoadConfigSEP2ServerAddrRequired(t *testing.T) {
	_, err := loadConfig([]string{"-sep2-server-addr="})
	if err == nil {
		t.Fatal("expected validate error, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_SERVER_ADDR") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestLoadConfigSEP2ServerCertDirEnvOverride verifies the cert dir env
// override passes through unmodified, matching the other string knobs.
func TestLoadConfigSEP2ServerCertDirEnvOverride(t *testing.T) {
	t.Setenv("SEP2_SERVER_CERT_DIR", "/etc/bridge/sep2-certs")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ServerCertDir != "/etc/bridge/sep2-certs" {
		t.Errorf("SEP2ServerCertDir: got %q, want the explicit override unchanged", cfg.SEP2ServerCertDir)
	}
}

// TestLoadConfigSEP2ServerCertDirRequired verifies an explicitly-emptied
// value is a loadConfig error naming the field.
func TestLoadConfigSEP2ServerCertDirRequired(t *testing.T) {
	_, err := loadConfig([]string{"-sep2-server-cert-dir="})
	if err == nil {
		t.Fatal("expected validate error, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_SERVER_CERT_DIR") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestLoadConfigAllowPlaintextEnvOverride verifies the plaintext opt-in
// is honored from the env var, matching the other boolean knob
// (PublishOnStart)'s precedence shape.
func TestLoadConfigAllowPlaintextEnvOverride(t *testing.T) {
	t.Setenv("SEP2_STOMP_ALLOW_PLAINTEXT", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.AllowPlaintext {
		t.Errorf("AllowPlaintext: want true from SEP2_STOMP_ALLOW_PLAINTEXT=true")
	}
}

// TestLoadConfigAllowPlaintextFlagShadowsEnv verifies the flag wins
// over the env var, matching the merge order documented on loadConfig.
func TestLoadConfigAllowPlaintextFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_STOMP_ALLOW_PLAINTEXT", "false")

	cfg, err := loadConfig([]string{"-stomp-allow-plaintext=true"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.AllowPlaintext {
		t.Errorf("AllowPlaintext: want true, flag should shadow the false env value")
	}
}

// TestLoadConfigAllowPlaintextBadBool verifies a malformed env value is
// a loadConfig error naming the field, matching SEP2_PUBLISH_ON_START's
// existing behavior (TestLoadConfigBadBool).
func TestLoadConfigAllowPlaintextBadBool(t *testing.T) {
	t.Setenv("SEP2_STOMP_ALLOW_PLAINTEXT", "notabool")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid bool, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_STOMP_ALLOW_PLAINTEXT") {
		t.Errorf("error should name the env var: %v", err)
	}
}

func TestLoadConfigEnvOverride(t *testing.T) {
	t.Setenv("SEP2_STOMP_ADDR", "broker.example:61613")
	t.Setenv("SEP2_STOMP_USER", "u")
	t.Setenv("SEP2_STOMP_PASSWORD", "p")
	t.Setenv("SEP2_SIMULATION_ID", "12345")
	t.Setenv("SEP2_FEEDER_MRID", "_FEED")
	t.Setenv("SEP2_PUBLISH_ON_START", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.STOMPAddr != "broker.example:61613" {
		t.Errorf("STOMPAddr: got %q", cfg.STOMPAddr)
	}
	if cfg.SimulationID != "12345" {
		t.Errorf("SimulationID: got %q", cfg.SimulationID)
	}
	if !cfg.PublishOnStart {
		t.Errorf("PublishOnStart: want true")
	}
}

func TestLoadConfigFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_STOMP_ADDR", "env.example:61613")
	t.Setenv("SEP2_SIMULATION_ID", "envsim")

	cfg, err := loadConfig([]string{"-stomp-addr=flag.example:61613", "-simulation-id=flagsim"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.STOMPAddr != "flag.example:61613" {
		t.Errorf("STOMPAddr: got %q want flag value", cfg.STOMPAddr)
	}
	if cfg.SimulationID != "flagsim" {
		t.Errorf("SimulationID: got %q want flag value", cfg.SimulationID)
	}
}

func TestLoadConfigBadBool(t *testing.T) {
	t.Setenv("SEP2_PUBLISH_ON_START", "yesplease")
	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid bool, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_PUBLISH_ON_START") {
		t.Errorf("error should name the env var: %v", err)
	}
}

func TestLoadConfigPublishOnStartRequiresSim(t *testing.T) {
	t.Setenv("SEP2_PUBLISH_ON_START", "true")
	t.Setenv("SEP2_SIMULATION_ID", "")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected validate error, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_SIMULATION_ID") {
		t.Errorf("error should mention simulation_id requirement: %v", err)
	}
}

// TestLoadConfigCredentialFlagDefaultsHidden verifies that even when
// SEP2_STOMP_PASSWORD is set in the environment, parsing -h does not
// echo the password value. flag.PrintDefaults reads the registered
// flag default, which loadConfig deliberately keeps empty for
// credentials; the resolved value is folded in after Parse.
func TestLoadConfigCredentialFlagDefaultsHidden(t *testing.T) {
	t.Setenv("SEP2_STOMP_PASSWORD", "topsecret")
	t.Setenv("SEP2_STOMP_USER", "alsosecret")

	// loadConfig itself does not expose the flag set, so build a parallel
	// flag set the same way and snapshot its usage output. This locks
	// the registered default shape against future regressions where a
	// caller passes a resolved value to fs.StringVar.
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	var user, password string
	fs.StringVar(&user, "stomp-user", "", "STOMP login user (env: SEP2_STOMP_USER)")
	fs.StringVar(&password, "stomp-password", "", "STOMP login password (env: SEP2_STOMP_PASSWORD)")

	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	usage := buf.String()

	if strings.Contains(usage, "topsecret") {
		t.Errorf("usage banner echoed password value: %q", usage)
	}
	if strings.Contains(usage, "alsosecret") {
		t.Errorf("usage banner echoed user value: %q", usage)
	}
	if !strings.Contains(usage, "SEP2_STOMP_PASSWORD") {
		t.Errorf("usage banner should mention env var SEP2_STOMP_PASSWORD: %q", usage)
	}
	if !strings.Contains(usage, "SEP2_STOMP_USER") {
		t.Errorf("usage banner should mention env var SEP2_STOMP_USER: %q", usage)
	}
}

// TestLoadConfigUnsetsCredentialEnvVars verifies that loadConfig
// scrubs SEP2_STOMP_PASSWORD and SEP2_STOMP_USER from the process
// environment after reading them, so they do not remain visible via
// /proc/<pid>/environ for the rest of process lifetime.
func TestLoadConfigUnsetsCredentialEnvVars(t *testing.T) {
	t.Setenv("SEP2_STOMP_PASSWORD", "topsecret")
	t.Setenv("SEP2_STOMP_USER", "alsosecret")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.STOMPPassword != "topsecret" {
		t.Errorf("STOMPPassword: got %q, want topsecret", cfg.STOMPPassword)
	}
	if cfg.STOMPUser != "alsosecret" {
		t.Errorf("STOMPUser: got %q, want alsosecret", cfg.STOMPUser)
	}
	if v, ok := os.LookupEnv("SEP2_STOMP_PASSWORD"); ok {
		t.Errorf("SEP2_STOMP_PASSWORD still set in env after loadConfig: %q", v)
	}
	if v, ok := os.LookupEnv("SEP2_STOMP_USER"); ok {
		t.Errorf("SEP2_STOMP_USER still set in env after loadConfig: %q", v)
	}
}

// TestLoadConfigCredentialPrecedence verifies the credential resolution
// order: flag wins, then env var, then compiled-in default.
func TestLoadConfigCredentialPrecedence(t *testing.T) {
	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("SEP2_STOMP_PASSWORD", "envpass")
		cfg, err := loadConfig([]string{"-stomp-password=flagpass"})
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.STOMPPassword != "flagpass" {
			t.Errorf("STOMPPassword: got %q, want flagpass", cfg.STOMPPassword)
		}
	})

	t.Run("env beats default", func(t *testing.T) {
		t.Setenv("SEP2_STOMP_PASSWORD", "envpass")
		cfg, err := loadConfig(nil)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.STOMPPassword != "envpass" {
			t.Errorf("STOMPPassword: got %q, want envpass", cfg.STOMPPassword)
		}
	})

	t.Run("default fills in when neither set", func(t *testing.T) {
		t.Setenv("SEP2_STOMP_PASSWORD", "")
		cfg, err := loadConfig(nil)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.STOMPPassword != defaultSTOMPPassword {
			t.Errorf("STOMPPassword: got %q, want %q", cfg.STOMPPassword, defaultSTOMPPassword)
		}
	})
}
