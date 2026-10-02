package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetrypub"
)

// stubStatusSource satisfies telemetrypub.StatusSource without standing
// up a real embedded server.
type stubStatusSource struct{}

func (*stubStatusSource) DERStatusSnapshots(context.Context) ([]sep2embed.DERStatusSnapshot, error) {
	return nil, nil
}

// TestTelemetryFlagsDefaults pins both defaults: a 15 second publish
// interval, matching the Python upstream's publish_interval_seconds, and
// suppression of unchanged devices ON.
func TestTelemetryFlagsDefaults(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2TelemetryInterval != 15*time.Second {
		t.Errorf("SEP2TelemetryInterval = %s, want 15s", cfg.SEP2TelemetryInterval)
	}
	if cfg.SEP2TelemetryPublishUnchanged {
		t.Error("SEP2TelemetryPublishUnchanged = true by default, want false (unchanged devices suppressed)")
	}
}

func TestTelemetryFlagsFromFlags(t *testing.T) {
	cfg, err := loadConfig([]string{"-sep2-telemetry-interval=30s", "-sep2-telemetry-publish-unchanged=true"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2TelemetryInterval != 30*time.Second {
		t.Errorf("SEP2TelemetryInterval = %s, want 30s", cfg.SEP2TelemetryInterval)
	}
	if !cfg.SEP2TelemetryPublishUnchanged {
		t.Error("SEP2TelemetryPublishUnchanged = false, want true from the flag")
	}
}

func TestTelemetryFlagsFromEnv(t *testing.T) {
	t.Setenv("SEP2_TELEMETRY_INTERVAL", "45s")
	t.Setenv("SEP2_TELEMETRY_PUBLISH_UNCHANGED", "true")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2TelemetryInterval != 45*time.Second {
		t.Errorf("SEP2TelemetryInterval = %s, want 45s from the env", cfg.SEP2TelemetryInterval)
	}
	if !cfg.SEP2TelemetryPublishUnchanged {
		t.Error("SEP2TelemetryPublishUnchanged = false, want true from the env")
	}
}

func TestTelemetryFlagShadowsEnv(t *testing.T) {
	t.Setenv("SEP2_TELEMETRY_INTERVAL", "45s")

	cfg, err := loadConfig([]string{"-sep2-telemetry-interval=5s"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2TelemetryInterval != 5*time.Second {
		t.Errorf("SEP2TelemetryInterval = %s, want the flag value 5s to shadow the env", cfg.SEP2TelemetryInterval)
	}
}

// TestTelemetryIntervalRejectsUnusableValues: a zero or negative
// interval would either spin or panic in time.NewTicker, so it must stop
// the bridge at config load rather than at first publish.
func TestTelemetryIntervalRejectsUnusableValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
	}{
		{"zero", []string{"-sep2-telemetry-interval=0s"}, ""},
		{"negative", []string{"-sep2-telemetry-interval=-5s"}, ""},
		{"not a duration via env", nil, "abc"},
		{"bare number via env", nil, "15"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("SEP2_TELEMETRY_INTERVAL", tc.env)
			}
			_, err := loadConfig(tc.args)
			if err == nil {
				t.Fatal("loadConfig: want error, got nil")
			}
			if !strings.Contains(err.Error(), "telemetry-interval") && !strings.Contains(err.Error(), "TELEMETRY_INTERVAL") {
				t.Errorf("error = %v, want it to name the offending knob", err)
			}
		})
	}
}

// TestTelemetryPublisherConfigMapping asserts the projection onto
// telemetrypub.Config, including the two deferred seams: the
// destination is the application output topic and nothing else,
// and the builder is supplied by this layer rather than assumed inside
// the publisher.
func TestTelemetryPublisherConfigMapping(t *testing.T) {
	t.Parallel()

	cfg := config{
		SimulationID:                  "sim-123",
		ApplicationID:                 "IEEE_2030_5",
		SEP2TelemetryInterval:         30 * time.Second,
		SEP2TelemetryPublishUnchanged: true,
	}
	src := &stubStatusSource{}
	bus := &fakeBusPublisherForTest{}

	got := telemetryPublisherConfig(cfg, src, bus)

	// Quoted from gridappsd-python v2026.09.0 application_output_topic.
	const wantDest = "/topic/goss.gridappsd.IEEE_2030_5.output"
	if got.Destination != wantDest {
		t.Errorf("Destination = %q, want %q", got.Destination, wantDest)
	}
	if got.Destination == sim.InputTopic("sim-123") {
		t.Errorf("Destination = %q must not be the simulation input topic", got.Destination)
	}
	if strings.Contains(got.Destination, "goss.gridappsd.process") {
		t.Errorf("Destination = %q must never be a platform process queue", got.Destination)
	}
	if got.Interval != 30*time.Second {
		t.Errorf("Interval = %s, want 30s", got.Interval)
	}
	if !got.PublishUnchanged {
		t.Error("PublishUnchanged = false, want it carried through from config")
	}
	if got.Build == nil {
		t.Error("Build = nil, want the diff message builder supplied by this layer")
	}
	if got.Source != telemetrypub.StatusSource(src) {
		t.Error("Source is not the passed-in status source")
	}
	if got.Bus != telemetrypub.BusPublisher(bus) {
		t.Error("Bus is not the passed-in bus")
	}

	// The projected config must actually construct a publisher.
	if _, err := telemetrypub.New(got); err != nil {
		t.Fatalf("telemetrypub.New on the projected config: %v", err)
	}
}
