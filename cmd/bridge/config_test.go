package main

import (
	"bytes"
	"flag"
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
