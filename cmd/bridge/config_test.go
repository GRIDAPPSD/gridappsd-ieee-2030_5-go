package main

import (
	"bytes"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	// t.Setenv(key, "") clears each override for this test (Dutch
	// L3), not because t.Setenv treats an empty value specially,
	// but because loadConfig itself reads os.Getenv and branches on
	// `== ""` to decide "not set" (see e.g. getenvList in config.go):
	// there is no distinction in this codebase between "unset" and "set
	// to the empty string". Setting to "" here is equivalent to
	// t.Setenv-and-Unsetenv, and is used instead because t.Cleanup-based
	// unset is what t.Setenv already gives us for free.
	t.Setenv("SEP2_STOMP_ADDR", "")
	t.Setenv("SEP2_STOMP_USER", "")
	t.Setenv("SEP2_STOMP_PASSWORD", "")
	t.Setenv("SEP2_SIMULATION_ID", "")
	t.Setenv("SEP2_FEEDER_MRID", "")
	t.Setenv("SEP2_PUBLISH_ON_START", "")
	t.Setenv("SEP2_STOMP_ALLOW_PLAINTEXT", "")
	t.Setenv("SEP2_SERVER_ADDR", "")
	t.Setenv("SEP2_SERVER_CERT_DIR", "")
	t.Setenv("SEP2_DEVICE_CERT_MODE", "")

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
	// The zero-value default must be dev-mint, not preprovisioned: a bare
	// `go run ./cmd/bridge` must derive working device identities with
	// no extra setup. See config.SEP2DeviceCertMode's doc comment.
	if cfg.SEP2DeviceCertMode != defaultSEP2DeviceCertMode {
		t.Errorf("SEP2DeviceCertMode: got %q want %q", cfg.SEP2DeviceCertMode, defaultSEP2DeviceCertMode)
	}
}

// TestLoadConfigSEP2DeviceCertModeEnvOverride verifies the device-cert
// mode env override passes through unmodified.
func TestLoadConfigSEP2DeviceCertModeEnvOverride(t *testing.T) {
	t.Setenv("SEP2_DEVICE_CERT_MODE", "preprovisioned")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2DeviceCertMode != "preprovisioned" {
		t.Errorf("SEP2DeviceCertMode: got %q, want the explicit override unchanged", cfg.SEP2DeviceCertMode)
	}
}

// TestLoadConfigSEP2DeviceCertModeFlagShadowsEnv matches the precedence
// shape already covered for -sep2-server-addr / SEP2_SERVER_ADDR.
func TestLoadConfigSEP2DeviceCertModeFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_DEVICE_CERT_MODE", "preprovisioned")

	cfg, err := loadConfig([]string{"-sep2-device-cert-mode=dev-mint"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2DeviceCertMode != "dev-mint" {
		t.Errorf("SEP2DeviceCertMode: got %q want flag value", cfg.SEP2DeviceCertMode)
	}
}

// TestLoadConfigSEP2DeviceCertModeRejectsUnknownValue verifies the
// fail-closed contract: an unrecognized mode string is a loadConfig
// error naming the field, not a silent fallback to dev-mint.
func TestLoadConfigSEP2DeviceCertModeRejectsUnknownValue(t *testing.T) {
	_, err := loadConfig([]string{"-sep2-device-cert-mode=bogus"})
	if err == nil {
		t.Fatal("expected validate error for an unknown SEP2_DEVICE_CERT_MODE value, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_DEVICE_CERT_MODE") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestLoadConfigSEP2ServerAddrDefaultsToLoopback verifies the embedded
// IEEE 2030.5 listener's default bind host is a loopback address. The
// embed has no per-device ACL yet, so a default
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

// TestLoadConfigSEP2AdminUIDisabledByDefault is the "admin UI
// disabled by default" acceptance test at the config layer: with no
// SEP2_ADMIN_UI_KEY override, the resolved key is empty (the intentional
// disabled state adminui.New's ErrDisabled contract reads), the addr
// defaults to loopback, and AllowNonLoopback defaults false.
func TestLoadConfigSEP2AdminUIDisabledByDefault(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_KEY", "")
	t.Setenv("SEP2_ADMIN_UI_ADDR", "")
	t.Setenv("SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK", "")
	t.Setenv("SEP2_ADMIN_UI_ALLOWED_HOSTS", "")
	t.Setenv("SEP2_ADMIN_UI_SOR_LINK", "")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUIKey != "" {
		t.Errorf("SEP2AdminUIKey: got %q, want empty (admin UI disabled by default)", cfg.SEP2AdminUIKey)
	}
	if cfg.SEP2AdminUIAddr != defaultSEP2AdminUIAddr {
		t.Errorf("SEP2AdminUIAddr: got %q want %q", cfg.SEP2AdminUIAddr, defaultSEP2AdminUIAddr)
	}
	host, _, err := net.SplitHostPort(cfg.SEP2AdminUIAddr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", cfg.SEP2AdminUIAddr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("SEP2AdminUIAddr host = %q, want a loopback address", host)
	}
	if cfg.SEP2AdminUIAllowNonLoopback {
		t.Errorf("SEP2AdminUIAllowNonLoopback: got true, want false (fail-closed default)")
	}
	if cfg.SEP2AdminUIAllowedHosts != nil {
		t.Errorf("SEP2AdminUIAllowedHosts: got %v, want nil", cfg.SEP2AdminUIAllowedHosts)
	}
	if cfg.SEP2AdminUISORLink != "" {
		t.Errorf("SEP2AdminUISORLink: got %q, want empty (unset by default)", cfg.SEP2AdminUISORLink)
	}
}

// TestLoadConfigSEP2AdminUISORLinkReadsThrough is the positive
// value acceptance test at the config layer: an operator supplied
// SEP2_ADMIN_UI_SOR_LINK value passes through loadConfig unchanged, with
// no transformation and no validation error, mirroring
// TestLoadConfigSEP2AdminUIAllowedHostsParsesCommaSeparatedList's shape
// for the sibling admin UI config field.
func TestLoadConfigSEP2AdminUISORLinkReadsThrough(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_SOR_LINK", "https://sor.example/dashboard")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUISORLink != "https://sor.example/dashboard" {
		t.Errorf("SEP2AdminUISORLink: got %q, want %q", cfg.SEP2AdminUISORLink, "https://sor.example/dashboard")
	}
}

// TestLoadConfigSEP2AdminUIKeyEnvOverride verifies an operator opting in
// to the admin UI via the env var is honored unmodified, matching the
// other credential-shaped knobs (-stomp-password's precedence shape).
func TestLoadConfigSEP2AdminUIKeyEnvOverride(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_KEY", "env-admin-token")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUIKey != "env-admin-token" {
		t.Errorf("SEP2AdminUIKey: got %q, want the explicit override unchanged", cfg.SEP2AdminUIKey)
	}
}

// TestLoadConfigSEP2AdminUIKeyFlagShadowsEnv matches the precedence shape
// already covered for -stomp-password / SEP2_STOMP_PASSWORD.
func TestLoadConfigSEP2AdminUIKeyFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_KEY", "env-admin-token")

	cfg, err := loadConfig([]string{"-admin-ui-key=flag-admin-token"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUIKey != "flag-admin-token" {
		t.Errorf("SEP2AdminUIKey: got %q want flag value", cfg.SEP2AdminUIKey)
	}
}

// TestLoadConfigSEP2AdminUIKeyFlagDefaultHidden verifies that even when
// SEP2_ADMIN_UI_KEY is set in the environment, parsing -h does not echo
// the token value, matching TestLoadConfigCredentialFlagDefaultsHidden's
// coverage for -stomp-user / -stomp-password.
func TestLoadConfigSEP2AdminUIKeyFlagDefaultHidden(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_KEY", "topsecret-admin-token")

	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	var key string
	fs.StringVar(&key, "admin-ui-key", "", "admin UI Bearer token; unset disables the admin UI entirely (env: SEP2_ADMIN_UI_KEY)")

	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	usage := buf.String()

	if strings.Contains(usage, "topsecret-admin-token") {
		t.Errorf("usage banner echoed admin UI token value: %q", usage)
	}
	if !strings.Contains(usage, "SEP2_ADMIN_UI_KEY") {
		t.Errorf("usage banner should mention env var SEP2_ADMIN_UI_KEY: %q", usage)
	}
}

// TestLoadConfigSEP2AdminUIAddrEnvOverride verifies a non-loopback bind
// is honored when the operator sets it explicitly, matching
// SEP2ServerAddr's own override shape.
func TestLoadConfigSEP2AdminUIAddrEnvOverride(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_ADDR", "0.0.0.0:8444")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUIAddr != "0.0.0.0:8444" {
		t.Errorf("SEP2AdminUIAddr: got %q, want the explicit override unchanged", cfg.SEP2AdminUIAddr)
	}
}

// TestLoadConfigSEP2AdminUIAddrFlagShadowsEnv matches the precedence
// shape already covered for -sep2-server-addr / SEP2_SERVER_ADDR.
func TestLoadConfigSEP2AdminUIAddrFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_ADDR", "env.example:8444")

	cfg, err := loadConfig([]string{"-admin-ui-addr=flag.example:8444"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUIAddr != "flag.example:8444" {
		t.Errorf("SEP2AdminUIAddr: got %q want flag value", cfg.SEP2AdminUIAddr)
	}
}

// TestLoadConfigSEP2AdminUIAllowNonLoopbackEnvOverride verifies the
// non-loopback opt-in is honored from the env var, matching
// AllowPlaintext's precedence shape.
func TestLoadConfigSEP2AdminUIAllowNonLoopbackEnvOverride(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.SEP2AdminUIAllowNonLoopback {
		t.Errorf("SEP2AdminUIAllowNonLoopback: want true from SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK=true")
	}
}

// TestLoadConfigSEP2AdminUIAllowNonLoopbackFlagShadowsEnv verifies the
// flag wins over the env var, matching AllowPlaintext's precedence
// shape (TestLoadConfigAllowPlaintextFlagShadowsEnv).
func TestLoadConfigSEP2AdminUIAllowNonLoopbackFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK", "false")

	cfg, err := loadConfig([]string{"-admin-ui-allow-non-loopback=true"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.SEP2AdminUIAllowNonLoopback {
		t.Errorf("SEP2AdminUIAllowNonLoopback: want true, flag should shadow the false env value")
	}
}

// TestLoadConfigSEP2AdminUIAllowNonLoopbackBadBool verifies a malformed
// env value is a loadConfig error naming the field, matching
// SEP2_STOMP_ALLOW_PLAINTEXT's existing behavior.
func TestLoadConfigSEP2AdminUIAllowNonLoopbackBadBool(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK", "notabool")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid bool, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK") {
		t.Errorf("error should name the env var: %v", err)
	}
}

// TestLoadConfigSEP2NotificationAllowLoopbackDefaultsFalse verifies the
// switch is off with no env or flag override, so the notifier's default
// destination policy (loopback refused) is what a bare `go run
// ./cmd/bridge` gets (issue 86 acceptance criterion: "with the switch
// unset, behavior is unchanged").
func TestLoadConfigSEP2NotificationAllowLoopbackDefaultsFalse(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2NotificationAllowLoopback {
		t.Errorf("SEP2NotificationAllowLoopback: got true with no override, want false")
	}
}

// TestLoadConfigSEP2NotificationAllowLoopbackEnvOverride verifies the
// loopback opt-in is honored from the env var, matching
// AllowPlaintext's precedence shape.
func TestLoadConfigSEP2NotificationAllowLoopbackEnvOverride(t *testing.T) {
	t.Setenv("SEP2_NOTIFICATION_ALLOW_LOOPBACK", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.SEP2NotificationAllowLoopback {
		t.Errorf("SEP2NotificationAllowLoopback: want true from SEP2_NOTIFICATION_ALLOW_LOOPBACK=true")
	}
}

// TestLoadConfigSEP2NotificationAllowLoopbackFlagShadowsEnv verifies the
// flag wins over the env var, matching AllowPlaintext's precedence shape
// (TestLoadConfigAllowPlaintextFlagShadowsEnv).
func TestLoadConfigSEP2NotificationAllowLoopbackFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_NOTIFICATION_ALLOW_LOOPBACK", "false")

	cfg, err := loadConfig([]string{"-sep2-notification-allow-loopback=true"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.SEP2NotificationAllowLoopback {
		t.Errorf("SEP2NotificationAllowLoopback: want true, flag should shadow the false env value")
	}
}

// TestLoadConfigSEP2NotificationAllowLoopbackFlagDisablesEnv verifies the
// fail-open direction of the flag-shadows-env precedence: an explicit
// -sep2-notification-allow-loopback=false must turn the switch off even
// when the environment enables it. FlagShadowsEnv above only proves the
// flag can turn the switch ON over a false env; an operator disabling the
// permissive state from the command line is the opposite, unpinned
// direction the test coverage review's mutant d walked through.
func TestLoadConfigSEP2NotificationAllowLoopbackFlagDisablesEnv(t *testing.T) {
	t.Setenv("SEP2_NOTIFICATION_ALLOW_LOOPBACK", "true")

	cfg, err := loadConfig([]string{"-sep2-notification-allow-loopback=false"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2NotificationAllowLoopback {
		t.Errorf("SEP2NotificationAllowLoopback: want false, an explicit -sep2-notification-allow-loopback=false must disable it even with the env set true")
	}
}

// TestLoadConfigSEP2NotificationAllowLoopbackBadBool verifies a malformed
// env value is a loadConfig error naming the field, matching
// SEP2_STOMP_ALLOW_PLAINTEXT's existing behavior.
func TestLoadConfigSEP2NotificationAllowLoopbackBadBool(t *testing.T) {
	t.Setenv("SEP2_NOTIFICATION_ALLOW_LOOPBACK", "notabool")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid bool, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_NOTIFICATION_ALLOW_LOOPBACK") {
		t.Errorf("error should name the env var: %v", err)
	}
}

// TestLoadConfigSEP2EnableCCMDefaultsFalse verifies the switch is off
// with no env or flag override, so a bare `go run ./cmd/bridge` keeps
// serving GCM (issue 101 acceptance: default behavior unchanged).
func TestLoadConfigSEP2EnableCCMDefaultsFalse(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2EnableCCM {
		t.Errorf("SEP2EnableCCM: got true with no override, want false")
	}
}

// TestLoadConfigSEP2EnableCCMEnvOverride verifies the CCM opt-in is
// honored from the env var, matching AllowPlaintext's precedence shape.
// SEP2_CCM_ALLOW_NO_OBSERVER must also be set here: validate refuses
// SEP2EnableCCM alone (TestLoadConfigSEP2EnableCCMRequiresObserverAck
// below covers that refusal directly).
func TestLoadConfigSEP2EnableCCMEnvOverride(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "true")
	t.Setenv("SEP2_CCM_ALLOW_NO_OBSERVER", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.SEP2EnableCCM {
		t.Errorf("SEP2EnableCCM: want true from SEP2_ENABLE_CCM=true")
	}
}

// TestLoadConfigSEP2EnableCCMFlagDisablesEnv verifies the fail-open
// direction of the flag-shadows-env precedence: an explicit
// -sep2-enable-ccm=false must turn the switch off even when the
// environment enables it, mirroring
// TestLoadConfigSEP2NotificationAllowLoopbackFlagDisablesEnv.
func TestLoadConfigSEP2EnableCCMFlagDisablesEnv(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "true")

	cfg, err := loadConfig([]string{"-sep2-enable-ccm=false"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2EnableCCM {
		t.Errorf("SEP2EnableCCM: want false, an explicit -sep2-enable-ccm=false must disable it even with the env set true")
	}
}

// TestLoadConfigSEP2EnableCCMBadBool verifies a malformed env value is a
// loadConfig error naming the field, matching
// SEP2_STOMP_ALLOW_PLAINTEXT's existing behavior.
func TestLoadConfigSEP2EnableCCMBadBool(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "notabool")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid bool, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_ENABLE_CCM") {
		t.Errorf("error should name the env var: %v", err)
	}
}

// TestLoadConfigSEP2EnableCCMRequiresObserverAck is the refusal PR 108's
// fix round adds: SEP2EnableCCM set alone must refuse to start (issue 82,
// "Dropping the observer to obtain it is not acceptable: it silently
// deletes the rejected-device record"), and the error must name both
// settings so an operator reading it knows exactly what to set.
func TestLoadConfigSEP2EnableCCMRequiresObserverAck(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "true")
	t.Setenv("SEP2_CCM_ALLOW_NO_OBSERVER", "")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("expected a refusal with SEP2_ENABLE_CCM set and no observer ack, got nil")
	}
	if !strings.Contains(err.Error(), "SEP2_ENABLE_CCM") {
		t.Errorf("error should name SEP2_ENABLE_CCM: %v", err)
	}
	if !strings.Contains(err.Error(), "SEP2_CCM_ALLOW_NO_OBSERVER") {
		t.Errorf("error should name SEP2_CCM_ALLOW_NO_OBSERVER: %v", err)
	}
}

// TestLoadConfigSEP2EnableCCMWithAckStarts is
// TestLoadConfigSEP2EnableCCMRequiresObserverAck's positive sibling: the
// two-flag combination is exactly what the refusal exists to require, and
// it must be a legal, error-free starting configuration.
func TestLoadConfigSEP2EnableCCMWithAckStarts(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "true")
	t.Setenv("SEP2_CCM_ALLOW_NO_OBSERVER", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig with both settings acknowledged: %v", err)
	}
	if !cfg.SEP2EnableCCM || !cfg.SEP2CCMAllowNoObserver {
		t.Errorf("SEP2EnableCCM=%v SEP2CCMAllowNoObserver=%v, want both true", cfg.SEP2EnableCCM, cfg.SEP2CCMAllowNoObserver)
	}
}

// TestLoadConfigSEP2EnableCCMDefaultPathStillStarts is the default-path
// half of the same acceptance criterion: with neither switch touched, the
// refusal above must never fire, matching
// TestLoadConfigSEP2EnableCCMDefaultsFalse but asserting the absence of
// an error explicitly rather than only the field value.
func TestLoadConfigSEP2EnableCCMDefaultPathStillStarts(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "")
	t.Setenv("SEP2_CCM_ALLOW_NO_OBSERVER", "")

	if _, err := loadConfig(nil); err != nil {
		t.Fatalf("default path (neither switch set) must start clean: %v", err)
	}
}

// TestLoadConfigSEP2CCMAllowNoObserverAloneIsNotEnough verifies the ack
// flag alone, with SEP2EnableCCM unset, changes nothing: it is not an
// independent opt-in, only a co-requirement of SEP2EnableCCM.
func TestLoadConfigSEP2CCMAllowNoObserverAloneIsNotEnough(t *testing.T) {
	t.Setenv("SEP2_ENABLE_CCM", "")
	t.Setenv("SEP2_CCM_ALLOW_NO_OBSERVER", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2EnableCCM {
		t.Errorf("SEP2EnableCCM: got true, want false: the ack flag alone must not enable CCM")
	}
}

// TestLoadConfigSEP2AdminUIAllowedHostsParsesCommaSeparatedList verifies
// getenvList's comma-splitting, whitespace-trimming, and empty-entry
// dropping behavior end to end through loadConfig, including a trailing
// comma and repeated commas which must not produce a blank allowlist
// entry.
func TestLoadConfigSEP2AdminUIAllowedHostsParsesCommaSeparatedList(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_ALLOWED_HOSTS", "admin.internal.example, second.example,,third.example,")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"admin.internal.example", "second.example", "third.example"}
	if len(cfg.SEP2AdminUIAllowedHosts) != len(want) {
		t.Fatalf("SEP2AdminUIAllowedHosts: got %v, want %v", cfg.SEP2AdminUIAllowedHosts, want)
	}
	for i, w := range want {
		if cfg.SEP2AdminUIAllowedHosts[i] != w {
			t.Errorf("SEP2AdminUIAllowedHosts[%d]: got %q, want %q", i, cfg.SEP2AdminUIAllowedHosts[i], w)
		}
	}
}

// TestLoadConfigSEP2AdminUIKeyNotRequiredByValidate is the fail-closed
// contract's other half: an unset admin UI key must NOT fail
// config.validate, because empty is the intentional disabled state, not
// a missing-required-field error. This locks in the doc comment on
// config.validate against a future accidental tightening.
func TestLoadConfigSEP2AdminUIKeyNotRequiredByValidate(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_KEY", "")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2AdminUIKey != "" {
		t.Errorf("SEP2AdminUIKey: got %q, want empty", cfg.SEP2AdminUIKey)
	}
}

// TestLoadConfigVersionFlagReturnsBeforeValidate locks in -version as a
// pure query flag: loadConfig must return
// errVersionRequested even when every other required field is left
// unset (SEP2_STOMP_ADDR, SEP2_FEEDER_MRID, etc. all empty, which would
// otherwise fail config.validate). If -version ever regressed to run
// after validate, this test would fail with a different (required
// field missing) error instead of errVersionRequested, catching the
// regression: -version must exit clean regardless of what other config
// is present.
func TestLoadConfigVersionFlagReturnsBeforeValidate(t *testing.T) {
	t.Setenv("SEP2_STOMP_ADDR", "")
	t.Setenv("SEP2_FEEDER_MRID", "")
	t.Setenv("SEP2_SERVER_ADDR", "")
	t.Setenv("SEP2_SERVER_CERT_DIR", "")

	_, err := loadConfig([]string{"-version"})
	if !errors.Is(err, errVersionRequested) {
		t.Fatalf("loadConfig([-version]): got err %v, want errVersionRequested", err)
	}
}

// TestLoadConfigRegistrationPINUnsetByDefault is the no-op
// contract for an operator who never passes either PIN flag: neither
// SEP2RegistrationPIN nor SEP2RegistrationPINs is populated, so
// buildSEP2Policy in main.go leaves sep2config.DefaultPolicy()'s own
// unset state (fail closed at seeding time) exactly as it is today.
func TestLoadConfigRegistrationPINUnsetByDefault(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2RegistrationPIN != nil {
		t.Errorf("SEP2RegistrationPIN: got %v, want nil", cfg.SEP2RegistrationPIN)
	}
	if cfg.SEP2RegistrationPINs != nil {
		t.Errorf("SEP2RegistrationPINs: got %v, want nil", cfg.SEP2RegistrationPINs)
	}
}

// TestLoadConfigRegistrationPINFlagSetsPointer verifies
// -sep2-registration-pin parses into the exact configured value, using
// 123455 (the IEEE 2030.5 section 6.3.5 worked example: PIN 12345,
// digits summing to 15, check digit 5).
func TestLoadConfigRegistrationPINFlagSetsPointer(t *testing.T) {
	cfg, err := loadConfig([]string{"-sep2-registration-pin=123455"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2RegistrationPIN == nil {
		t.Fatal("SEP2RegistrationPIN: got nil, want a populated pointer")
	}
	if *cfg.SEP2RegistrationPIN != 123455 {
		t.Errorf("SEP2RegistrationPIN: got %d, want 123455", *cfg.SEP2RegistrationPIN)
	}
}

// TestLoadConfigRegistrationPINFlagRejectsNonNumeric verifies a
// non-numeric -sep2-registration-pin value is a loadConfig error, and
// that the error deliberately does not echo the raw offending string:
// the flag may carry an operator's mistyped PIN, and a PIN is never
// logged or echoed in an error (see sep2config.SEP2Policy's
// RegistrationPINs doc comment).
func TestLoadConfigRegistrationPINFlagRejectsNonNumeric(t *testing.T) {
	_, err := loadConfig([]string{"-sep2-registration-pin=not-a-number"})
	if err == nil {
		t.Fatal("expected an error for a non-numeric -sep2-registration-pin, got nil")
	}
	if !strings.Contains(err.Error(), "-sep2-registration-pin") {
		t.Errorf("error should name the flag: %v", err)
	}
	if strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error must not echo the raw flag value: %v", err)
	}
}

// TestLoadConfigRegistrationPINFlagRejectsNegative verifies a negative
// -sep2-registration-pin value is rejected at parse time (flag.Uint has
// no negative representation, so this exercises the manual
// strconv.ParseUint path instead of flag's own numeric flag types).
func TestLoadConfigRegistrationPINFlagRejectsNegative(t *testing.T) {
	_, err := loadConfig([]string{"-sep2-registration-pin=-5"})
	if err == nil {
		t.Fatal("expected an error for a negative -sep2-registration-pin, got nil")
	}
	if !strings.Contains(err.Error(), "-sep2-registration-pin") {
		t.Errorf("error should name the flag: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileLoadsExactMap verifies a valid
// -sep2-registration-pin-file populates SEP2RegistrationPINs with
// exactly the entries in the file, keys and values both, and in
// whatever case the file used (ResolveRegistrationPIN normalizes case
// at lookup time, so the loader must not).
func TestLoadConfigRegistrationPINFileLoadsExactMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"f0fa1ac6":123455,"BCD85AA8":234564}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := map[string]uint32{"f0fa1ac6": 123455, "BCD85AA8": 234564}
	if len(cfg.SEP2RegistrationPINs) != len(want) {
		t.Fatalf("SEP2RegistrationPINs: got %v, want %v", cfg.SEP2RegistrationPINs, want)
	}
	for k, v := range want {
		got, ok := cfg.SEP2RegistrationPINs[k]
		if !ok {
			t.Errorf("SEP2RegistrationPINs missing key %q", k)
			continue
		}
		if got != v {
			t.Errorf("SEP2RegistrationPINs[%q]: got %d, want %d", k, got, v)
		}
	}
}

// TestLoadConfigRegistrationPINFileMissing verifies a nonexistent
// -sep2-registration-pin-file path is a distinct, named loadConfig
// error: a typo in the path must fail the boot outright, never fall
// back silently to no per-device PINs.
func TestLoadConfigRegistrationPINFileMissing(t *testing.T) {
	_, err := loadConfig([]string{"-sep2-registration-pin-file=/nonexistent/pins.json"})
	if err == nil {
		t.Fatal("expected an error for a missing PIN file, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should say the file does not exist: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileUnreadable verifies a PIN file that
// exists but cannot be read (permission denied) is a distinct error
// from "does not exist".
func TestLoadConfigRegistrationPINFileUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits; skip under root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"A":123455}`), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for an unreadable PIN file, got nil")
	}
	if !strings.Contains(err.Error(), "is not readable") {
		t.Errorf("error should say the file is not readable: %v", err)
	}
	if strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unreadable and missing must be distinct errors, got: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileMalformedJSON verifies invalid JSON
// syntax is a distinct error from every other PIN-file failure mode.
func TestLoadConfigRegistrationPINFileMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"A":123455,`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "is not valid JSON") {
		t.Errorf("error should say the file is not valid JSON: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileNotAnObject verifies a syntactically
// valid JSON document whose top level is not an object (here, an
// array) is a distinct error from a JSON syntax error.
func TestLoadConfigRegistrationPINFileNotAnObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`[123455,234564]`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for a non-object top-level JSON value, got nil")
	}
	if !strings.Contains(err.Error(), "is not a flat JSON object") {
		t.Errorf("error should say the file is not a flat JSON object: %v", err)
	}
	if strings.Contains(err.Error(), "is not valid JSON") {
		t.Errorf("not-an-object and malformed-JSON must be distinct errors, got: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileNonNumberValue verifies a value that
// is valid JSON but not a JSON number (here, a quoted string) is
// rejected rather than silently accepted: encoding/json's json.Number
// type would otherwise accept "123455" as if it were the bare number
// 123455.
func TestLoadConfigRegistrationPINFileNonNumberValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"A":"123455"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for a non-number PIN value, got nil")
	}
	if !strings.Contains(err.Error(), "is not a JSON number") {
		t.Errorf("error should say the entry is not a JSON number: %v", err)
	}
	if !strings.Contains(err.Error(), `"A"`) {
		t.Errorf("error should name the offending LFDI: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileNonIntegerValue verifies a
// fractional JSON number is rejected as a distinct error from every
// other failure mode, and that the error never echoes the offending
// number.
func TestLoadConfigRegistrationPINFileNonIntegerValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"A":123455.5}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for a non-integer PIN value, got nil")
	}
	if !strings.Contains(err.Error(), "is not an integer") {
		t.Errorf("error should say the entry is not an integer: %v", err)
	}
	if strings.Contains(err.Error(), "123455.5") {
		t.Errorf("error must not echo the raw PIN value: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileOutOfRangeValue verifies a value
// above sep2config.MaxRegistrationPIN (999999) is a distinct error, and
// that the offending value itself never appears in the error message.
func TestLoadConfigRegistrationPINFileOutOfRangeValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"A":1000000}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for an out-of-range PIN value, got nil")
	}
	if !strings.Contains(err.Error(), "out of the IEEE 2030.5 PIN range") {
		t.Errorf("error should say the entry is out of range: %v", err)
	}
	if strings.Contains(err.Error(), "1000000") {
		t.Errorf("error must not echo the raw PIN value: %v", err)
	}
}

// TestLoadConfigRegistrationPINFileEmptyObject verifies an empty JSON
// object is rejected outright rather than silently producing an empty
// map: an empty file must not be indistinguishable from "flag absent".
func TestLoadConfigRegistrationPINFileEmptyObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadConfig([]string{"-sep2-registration-pin-file=" + path})
	if err == nil {
		t.Fatal("expected an error for an empty PIN file, got nil")
	}
	if !strings.Contains(err.Error(), "contains no entries") {
		t.Errorf("error should say the file has no entries: %v", err)
	}
}

// TestLoadConfigRegistrationPINBothFlagsTogether verifies both PIN
// flags can be set at once, each populating its own distinct field with
// no cross-contamination: this is the precedence shape
// SEP2Policy.ResolveRegistrationPIN reads later, per-device wins over
// the fleet default.
func TestLoadConfigRegistrationPINBothFlagsTogether(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(path, []byte(`{"F0FA1AC6":234564}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := loadConfig([]string{
		"-sep2-registration-pin=123455",
		"-sep2-registration-pin-file=" + path,
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2RegistrationPIN == nil || *cfg.SEP2RegistrationPIN != 123455 {
		t.Errorf("SEP2RegistrationPIN: got %v, want 123455", cfg.SEP2RegistrationPIN)
	}
	if len(cfg.SEP2RegistrationPINs) != 1 || cfg.SEP2RegistrationPINs["F0FA1AC6"] != 234564 {
		t.Errorf("SEP2RegistrationPINs: got %v, want {F0FA1AC6: 234564}", cfg.SEP2RegistrationPINs)
	}
}

// writeProgramFile writes a -sep2-program-file document and returns its path.
func writeProgramFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "program.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestLoadConfigDERProgramUnsetByDefault pins the no-op case: with no flag
// and no file, both fields stay nil so buildSEP2Policy leaves the
// compiled-in defaults alone. A non-nil zero here would silently serve
// primacy 0 (the highest priority) to every deployment on upgrade.
func TestLoadConfigDERProgramUnsetByDefault(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ProgramPrimacy != nil {
		t.Errorf("SEP2ProgramPrimacy = %v, want nil", cfg.SEP2ProgramPrimacy)
	}
	if cfg.SEP2ProgramDescription != nil {
		t.Errorf("SEP2ProgramDescription = %v, want nil", cfg.SEP2ProgramDescription)
	}
}

// TestLoadConfigDERProgramFromFile covers the path an admin UI is expected
// to write: a whole-object JSON file the operator restarts the bridge to
// pick up.
func TestLoadConfigDERProgramFromFile(t *testing.T) {
	path := writeProgramFile(t, `{"primacy": 89, "description": "Feeder DER program"}`)

	cfg, err := loadConfig([]string{"-sep2-program-file=" + path})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ProgramPrimacy == nil || *cfg.SEP2ProgramPrimacy != 89 {
		t.Errorf("SEP2ProgramPrimacy = %v, want 89", cfg.SEP2ProgramPrimacy)
	}
	if cfg.SEP2ProgramDescription == nil || *cfg.SEP2ProgramDescription != "Feeder DER program" {
		t.Errorf("SEP2ProgramDescription = %v, want %q", cfg.SEP2ProgramDescription, "Feeder DER program")
	}
}

// TestLoadConfigDERProgramFilePartialLeavesOtherFieldNil is the invariant
// that forces the pointer returns out of loadProgramFile: a file that sets
// only description must not reset primacy, because the zero value it would
// reset to (0) is a legal and higher-priority primacy, not an absent one.
func TestLoadConfigDERProgramFilePartialLeavesOtherFieldNil(t *testing.T) {
	path := writeProgramFile(t, `{"description": "Only a description"}`)

	cfg, err := loadConfig([]string{"-sep2-program-file=" + path})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ProgramPrimacy != nil {
		t.Errorf("SEP2ProgramPrimacy = %v, want nil; a file that omits primacy must leave the compiled-in default alone", cfg.SEP2ProgramPrimacy)
	}
	if cfg.SEP2ProgramDescription == nil || *cfg.SEP2ProgramDescription != "Only a description" {
		t.Errorf("SEP2ProgramDescription = %v, want %q", cfg.SEP2ProgramDescription, "Only a description")
	}
}

// TestLoadConfigDERProgramFlagOverridesFile pins the documented precedence:
// flag beats file, matching the PIN flags.
func TestLoadConfigDERProgramFlagOverridesFile(t *testing.T) {
	path := writeProgramFile(t, `{"primacy": 89, "description": "From file"}`)

	cfg, err := loadConfig([]string{
		"-sep2-program-file=" + path,
		"-sep2-program-primacy=2",
		"-sep2-program-description=From flag",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ProgramPrimacy == nil || *cfg.SEP2ProgramPrimacy != 2 {
		t.Errorf("SEP2ProgramPrimacy = %v, want 2 (flag overrides file)", cfg.SEP2ProgramPrimacy)
	}
	if cfg.SEP2ProgramDescription == nil || *cfg.SEP2ProgramDescription != "From flag" {
		t.Errorf("SEP2ProgramDescription = %v, want %q (flag overrides file)", cfg.SEP2ProgramDescription, "From flag")
	}
}

// TestLoadConfigDERProgramPrimacyZeroIsCarried guards the ambiguity the
// empty-string flag sentinel exists to resolve: 0 is a legal primacy, so an
// explicit -sep2-program-primacy=0 must reach the policy as a configured 0
// rather than being read as "unset".
func TestLoadConfigDERProgramPrimacyZeroIsCarried(t *testing.T) {
	cfg, err := loadConfig([]string{"-sep2-program-primacy=0"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ProgramPrimacy == nil {
		t.Fatal("SEP2ProgramPrimacy = nil for an explicit -sep2-program-primacy=0; 0 is a legal primacy and must be distinguishable from unset")
	}
	if *cfg.SEP2ProgramPrimacy != 0 {
		t.Errorf("SEP2ProgramPrimacy = %d, want 0", *cfg.SEP2ProgramPrimacy)
	}
}

// TestLoadConfigDERProgramFileRejectsBadInput covers the cases where the
// file cannot be trusted. Each must be a load error naming the flag, never
// a silently ignored setting: an operator who sees the bridge come up on
// the default has no way to tell their value was dropped.
func TestLoadConfigDERProgramFileRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON at all", `not json`},
		{"top-level array rather than an object", `[1, 2]`},
		{"misspelled member", `{"primacy_value": 1}`},
		{"primacy as a string", `{"primacy": "1"}`},
		{"primacy not an integer", `{"primacy": 1.5}`},
		{"primacy above the PrimacyType range", `{"primacy": 256}`},
		{"primacy below the PrimacyType range", `{"primacy": -1}`},
		{"description as a number", `{"description": 7}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProgramFile(t, tt.body)
			_, err := loadConfig([]string{"-sep2-program-file=" + path})
			if err == nil {
				t.Fatalf("loadConfig(%s) = nil error, want a load failure", tt.body)
			}
			if !strings.Contains(err.Error(), "-sep2-program-file") {
				t.Errorf("error %q does not name -sep2-program-file", err)
			}
		})
	}
}

// TestLoadConfigDERProgramFileMissingIsAnError: a path the operator named
// but that does not exist is a mistake, not a reason to fall back silently.
func TestLoadConfigDERProgramFileMissingIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := loadConfig([]string{"-sep2-program-file=" + path})
	if err == nil {
		t.Fatal("loadConfig with a nonexistent -sep2-program-file = nil error, want a load failure")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q does not say the file is missing", err)
	}
}
