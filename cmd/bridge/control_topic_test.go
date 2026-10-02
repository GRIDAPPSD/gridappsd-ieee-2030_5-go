package main

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// The string gridappsd-python v2026.09.0 topics.py application_input_topic
// builds with no simulation id, written out literally so a change to the
// helper cannot move the test with it.
const wantControlTopic = "/topic/goss.gridappsd.IEEE_2030_5.input"

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
	const want = "/topic/goss.gridappsd.envapp.input"
	if got != want {
		t.Errorf("subscribed to %q, want %q", got, want)
	}
}

// With no simulation id the bridge still takes controls from the
// application input topic, and subscribes to nothing else.
func TestRunSimSideWithoutSimulationIDSubscribesOnlyToControlTopic(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)

	ctx, cancel := context.WithCancel(context.Background())
	bus := &fakeControlBus{}
	var hook controlobs.Hook
	done := make(chan error, 1)
	go func() {
		done <- runSimSide(ctx, gridappsdclient.NewSubscriber(bus), h.embed, h.reg, "IEEE_2030_5", "", &hook, &historySink{store: &store, logf: log.Printf})
	}()
	waitFor(2*time.Second, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return bus.handler != nil
	})
	bus.mu.Lock()
	dests := append([]string(nil), bus.dests...)
	bus.mu.Unlock()
	cancel()
	<-done

	if len(dests) != 1 || dests[0] != wantControlTopic {
		t.Errorf("subscribed to %q, want only %q", dests, wantControlTopic)
	}
	snap := hook.Snapshot()
	if snap.InputTopic != wantControlTopic || snap.OutputTopic != "" {
		t.Errorf("hook topics = %q, %q; want %q and empty", snap.InputTopic, snap.OutputTopic, wantControlTopic)
	}
}
