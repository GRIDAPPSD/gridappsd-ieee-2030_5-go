package main

import (
	"strings"
	"testing"
)

// TestTelemetryHistoryTopicsDefaultIsEmpty pins the off-by-default
// posture: with neither the flag nor the env set, the list is nil, which
// is what the wiring in run() reads to decide "start no subscriber
// goroutine at all".
func TestTelemetryHistoryTopicsDefaultIsEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.SEP2TelemetryHistoryTopics) != 0 {
		t.Errorf("SEP2TelemetryHistoryTopics = %v, want empty by default", cfg.SEP2TelemetryHistoryTopics)
	}
}

// TestTelemetryHistoryTopicsFromFlag asserts the flag parses a
// comma-separated list, trims whitespace, and drops empty entries.
func TestTelemetryHistoryTopicsFromFlag(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{
		"-sep2-telemetry-history-topics=/topic/goss.gridappsd.simulation.input.sim-1, /topic/goss.gridappsd.simulation.output.sim-1,,",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{
		"/topic/goss.gridappsd.simulation.input.sim-1",
		"/topic/goss.gridappsd.simulation.output.sim-1",
	}
	if len(cfg.SEP2TelemetryHistoryTopics) != len(want) {
		t.Fatalf("SEP2TelemetryHistoryTopics = %v, want %v", cfg.SEP2TelemetryHistoryTopics, want)
	}
	for i, w := range want {
		if cfg.SEP2TelemetryHistoryTopics[i] != w {
			t.Errorf("SEP2TelemetryHistoryTopics[%d] = %q, want %q", i, cfg.SEP2TelemetryHistoryTopics[i], w)
		}
	}
}

// TestTelemetryHistoryTopicsFromEnv mirrors the flag test via the env
// var, and TestTelemetryHistoryTopicsFlagShadowsEnv confirms flag
// precedence, matching every other knob in this file's family.
func TestTelemetryHistoryTopicsFromEnv(t *testing.T) {
	t.Setenv("SEP2_TELEMETRY_HISTORY_TOPICS", "/topic/a,/topic/b")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"/topic/a", "/topic/b"}
	if len(cfg.SEP2TelemetryHistoryTopics) != len(want) {
		t.Fatalf("SEP2TelemetryHistoryTopics = %v, want %v", cfg.SEP2TelemetryHistoryTopics, want)
	}
	for i, w := range want {
		if cfg.SEP2TelemetryHistoryTopics[i] != w {
			t.Errorf("SEP2TelemetryHistoryTopics[%d] = %q, want %q", i, cfg.SEP2TelemetryHistoryTopics[i], w)
		}
	}
}

func TestTelemetryHistoryTopicsFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_TELEMETRY_HISTORY_TOPICS", "/topic/from-env")

	cfg, err := loadConfig([]string{"-sep2-telemetry-history-topics=/topic/from-flag"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.SEP2TelemetryHistoryTopics) != 1 || cfg.SEP2TelemetryHistoryTopics[0] != "/topic/from-flag" {
		t.Errorf("SEP2TelemetryHistoryTopics = %v, want the flag value to shadow the env", cfg.SEP2TelemetryHistoryTopics)
	}
}

// TestTelemetryHistoryTopicsRejectsWildcards asserts both wildcard
// characters are refused, in both leading and trailing position, with an
// error naming the flag. A wildcard on a shared broker would subscribe
// to every simulation on it, so this must fail closed at config parse.
func TestTelemetryHistoryTopicsRejectsWildcards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		topic string
	}{
		{"trailing >", "/topic/goss.gridappsd.simulation.input.>"},
		{"leading >", ">.gridappsd.simulation.input.sim-1"},
		{"trailing *", "/topic/goss.gridappsd.*"},
		{"leading *", "*.gridappsd.simulation.input.sim-1"},
		{"embedded >", "/topic/goss.>.simulation"},
		{"embedded *", "/topic/goss.*.simulation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadConfig([]string{"-sep2-telemetry-history-topics=" + tc.topic})
			if err == nil {
				t.Fatalf("loadConfig(%q): want error, got nil", tc.topic)
			}
			if !strings.Contains(err.Error(), "sep2-telemetry-history-topics") {
				t.Errorf("error = %v, want it to name -sep2-telemetry-history-topics", err)
			}
			if !strings.Contains(err.Error(), "wildcard") {
				t.Errorf("error = %v, want it to explain the wildcard refusal", err)
			}
		})
	}
}

// TestTelemetryHistoryTopicsEnforcesCap asserts exceeding
// maxTelemetryHistoryTopics is a startup error naming the flag, not a
// silent truncation.
func TestTelemetryHistoryTopicsEnforcesCap(t *testing.T) {
	t.Parallel()

	topics := make([]string, maxTelemetryHistoryTopics+1)
	for i := range topics {
		topics[i] = "/topic/dest-" + string(rune('a'+i))
	}
	_, err := loadConfig([]string{"-sep2-telemetry-history-topics=" + strings.Join(topics, ",")})
	if err == nil {
		t.Fatal("loadConfig: want error for over-cap topic list, got nil")
	}
	if !strings.Contains(err.Error(), "sep2-telemetry-history-topics") {
		t.Errorf("error = %v, want it to name -sep2-telemetry-history-topics", err)
	}
}

// TestTelemetryHistoryTopicsAtCapIsAccepted is the boundary case for the
// cap test above: exactly maxTelemetryHistoryTopics destinations must be
// accepted, not rejected off-by-one.
func TestTelemetryHistoryTopicsAtCapIsAccepted(t *testing.T) {
	t.Parallel()

	topics := make([]string, maxTelemetryHistoryTopics)
	for i := range topics {
		topics[i] = "/topic/dest-" + string(rune('a'+i))
	}
	cfg, err := loadConfig([]string{"-sep2-telemetry-history-topics=" + strings.Join(topics, ",")})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.SEP2TelemetryHistoryTopics) != maxTelemetryHistoryTopics {
		t.Errorf("SEP2TelemetryHistoryTopics = %v, want %d entries", cfg.SEP2TelemetryHistoryTopics, maxTelemetryHistoryTopics)
	}
}
