package main

import (
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// The topic string is the one gridappsd-python v2026.09.0 topics.py
// application_input_topic builds for these arguments, written out
// literally so a change to the helper cannot move the test with it.
const wantControlTopic = "/topic/goss.gridappsd.simulation.IEEE_2030_5.sim-1.input"

func TestControlSubscriberListensOnApplicationInputTopic(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)

	h.bus.mu.Lock()
	got := h.bus.dest
	h.bus.mu.Unlock()
	if got != wantControlTopic {
		t.Errorf("subscribed to %q, want %q", got, wantControlTopic)
	}
}

func TestControlOnApplicationInputTopicIsApplied(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	if !h.bus.deliverTo(wantControlTopic, socFrame(t, "M", 6500, frameEpoch)) {
		t.Fatal("frame on the application input topic was not delivered")
	}
	h.waitFrames(t, 1)
}

func TestControlOnSimulationInputTopicIsNotApplied(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	const simInput = "/topic/goss.gridappsd.simulation.input.sim-1"
	if h.bus.deliverTo(simInput, socFrame(t, "M", 6500, frameEpoch)) {
		t.Fatal("frame on the simulation input topic reached the control subscriber")
	}
	snap := h.hook.Snapshot()
	if snap.Applied != 0 || snap.Skipped != 0 {
		t.Errorf("hook Applied=%d Skipped=%d, want 0 and 0", snap.Applied, snap.Skipped)
	}
}

func TestApplicationIDSettingChangesControlTopic(t *testing.T) {
	t.Setenv("SEP2_APPLICATION_ID", "envapp")
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	var store telemetryhistory.Store
	h := startHistoryHarnessApp(t, &store, cfg.ApplicationID)

	h.bus.mu.Lock()
	got := h.bus.dest
	h.bus.mu.Unlock()
	const want = "/topic/goss.gridappsd.simulation.envapp.sim-1.input"
	if got != want {
		t.Errorf("subscribed to %q, want %q", got, want)
	}
}
