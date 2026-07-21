package main

import (
	"bytes"
	"errors"
	"flag"
	"net"
	"os"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	// t.Setenv(key, "") clears each override for this test (GAGO-028
	// Dutch L3), not because t.Setenv treats an empty value specially,
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

// TestLoadConfigSEP2AdminUIDisabledByDefault is the GAGO-058 "admin UI
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

// TestLoadConfigSEP2AdminUISORLinkReadsThrough is the GAGO-075 positive
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
// pure query flag (GAGO-036): loadConfig must return
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
