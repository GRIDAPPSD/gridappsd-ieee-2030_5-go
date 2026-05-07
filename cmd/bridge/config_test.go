package main

import (
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
