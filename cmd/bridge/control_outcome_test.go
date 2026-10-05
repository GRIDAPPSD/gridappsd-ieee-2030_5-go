package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// startSeededControlHarness runs the subscriber against an embed seeded
// with mrid, so a valid delta for it is really issued.
func startSeededControlHarness(t *testing.T, mrid string, history *telemetryhistory.Store) (*fakeControlBus, *controlobs.Hook) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	reg := registry.New()
	lfdi := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(mrid))))
	if err := reg.Add(registry.Entry{MRID: mrid, LFDI: lfdi}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	embed, err := newSEP2Embed(ctx, config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: t.TempDir(),
	}, reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	bus := &fakeControlBus{}
	hook := &controlobs.Hook{}
	sink := &historySink{store: history, logf: newRateLimitedLogf(historyLogInterval, time.Now, log.Printf)}
	go func() {
		_ = runControlSubscriber(ctx, gridappsdclient.NewSubscriber(bus), embed, reg, testAppID, hook, sink)
	}()
	waitFor(2*time.Second, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return bus.handler != nil
	})
	bus.mu.Lock()
	ok := bus.handler != nil
	bus.mu.Unlock()
	if !ok {
		t.Fatal("control subscriber never called Subscribe")
	}
	return bus, hook
}

func controlMessage(t *testing.T, diffMRID string, epoch int64, deltas ...diff.Difference) []byte {
	t.Helper()
	var m diff.Message
	m.Command = "update"
	m.Input.Message.DifferenceMRID = diffMRID
	m.Input.Message.Timestamp = epoch
	m.Input.Message.ForwardDifferences = deltas
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func targetW(mrid string, value float64) diff.Difference {
	return diff.Difference{Object: mrid, Attribute: controlAttr,
		Value: map[string]any{"multiplier": 0.0, "value": value}}
}

func waitProcessed(t *testing.T, hook *controlobs.Hook, n uint64) {
	t.Helper()
	total := func() uint64 {
		s := hook.Snapshot()
		return s.Applied + s.Restated + s.Skipped + s.EmptyFrames
	}
	waitFor(3*time.Second, func() bool { return total() >= n })
	if got := total(); got != n {
		t.Fatalf("processed %d items, want %d", got, n)
	}
}

// A restated setpoint is counted Restated, not Applied; it is not charted
// again, Last stays the delta that took effect, and each message keeps its
// own outcome under its difference_mrid.
func TestSubscriberCountsRestatedSeparatelyFromApplied(t *testing.T) {
	const mrid = "mrid-outcome-1"
	var history telemetryhistory.Store
	bus, hook := startSeededControlHarness(t, mrid, &history)

	bus.deliver(controlMessage(t, "msg-1", frameEpoch, targetW(mrid, 5000)))
	waitProcessed(t, hook, 1)
	bus.deliver(controlMessage(t, "msg-2", frameEpoch+60, targetW(mrid, 5000)))
	waitProcessed(t, hook, 2)
	bus.deliver(controlMessage(t, "msg-3", frameEpoch+120, targetW(mrid, 5000), diff.Difference{Object: "unknown", Attribute: controlAttr, Value: map[string]any{"multiplier": 0.0, "value": 1.0}}))
	waitProcessed(t, hook, 4)

	snap := hook.Snapshot()
	if snap.Applied != 1 || snap.Restated != 2 || snap.Skipped != 1 {
		t.Errorf("applied=%d restated=%d skipped=%d, want 1 2 1", snap.Applied, snap.Restated, snap.Skipped)
	}
	if s, ok := findSeries(history.Snapshot(), mrid, controlAttr); !ok || len(s.Samples) != 1 {
		t.Errorf("commanded series = %+v (found=%v), want exactly one sample: restatements are not new setpoints", s, ok)
	}
	if snap.Last == nil || snap.Last.Object != mrid {
		t.Errorf("Last = %+v, want the applied delta", snap.Last)
	}

	m1, _ := hook.Outcome("msg-1")
	m2, _ := hook.Outcome("msg-2")
	m3, ok3 := hook.Outcome("msg-3")
	if len(m1.Deltas) != 1 || m1.Deltas[0].Result != controlobs.ResultIssued {
		t.Errorf("msg-1 outcome = %+v, want issued", m1)
	}
	if len(m2.Deltas) != 1 || m2.Deltas[0].Result != controlobs.ResultRestated {
		t.Errorf("msg-2 outcome = %+v, want restated", m2)
	}
	if !ok3 || len(m3.Deltas) != 2 || m3.Deltas[0].Result != controlobs.ResultRestated ||
		m3.Deltas[1].Result != controlobs.ResultRefused || !strings.Contains(m3.Deltas[1].Reason, "unregistered") {
		t.Errorf("msg-3 outcome = %+v, want restated then refused with the unregistered-device reason", m3)
	}
}

// Valid JSON with no forward differences is logged and counted, once per
// interval in the log, and every one is counted.
// Not parallel: it swaps the process-wide log writer.
func TestSubscriberLogsAndCountsFrameWithNoDifferences(t *testing.T) {
	logs := captureHistoryLog(t)
	var history telemetryhistory.Store
	bus, hook := startSeededControlHarness(t, "mrid-outcome-2", &history)

	bus.deliver([]byte(`{}`))
	bus.deliver([]byte(`{"command":"update","input":{"message":{"difference_mrid":"empty-1","forward_differences":[]}}}`))
	waitProcessed(t, hook, 2)

	snap := hook.Snapshot()
	if snap.EmptyFrames != 2 || snap.Skipped != 0 || snap.Applied != 0 {
		t.Errorf("emptyFrames=%d skipped=%d applied=%d, want 2 0 0", snap.EmptyFrames, snap.Skipped, snap.Applied)
	}
	if n := strings.Count(logs.String(), "carries no forward_differences"); n != 1 {
		t.Errorf("empty-frame log lines = %d, want 1 (rate-limited); log = %q", n, logs.String())
	}
	if m, ok := hook.Outcome("empty-1"); !ok || len(m.Deltas) != 0 {
		t.Errorf("Outcome(empty-1) = %+v found=%v, want a stored message with no deltas", m, ok)
	}
}
