package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetrypub"
)

const (
	socAttr     = "DERStatus.stateOfChargeStatus"
	controlAttr = "DERControl.DERControlBase.opModTargetW"
	frameEpoch  = int64(1700000000)
)

// historyHarness runs runControlSubscriber against an unstarted embed
// and an empty registry, so every control delta is skipped and counted
// by hook.Skipped, which tests use as a "frame processed" signal.
type historyHarness struct {
	bus     *fakeControlBus
	hook    *controlobs.Hook
	history *telemetryhistory.Store
	done    chan error
}

func startHistoryHarness(t *testing.T, history *telemetryhistory.Store) *historyHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	reg := registry.New()
	embed, err := newSEP2Embed(ctx, config{
		SEP2ServerAddr:    "127.0.0.1:0",
		SEP2ServerCertDir: t.TempDir(),
	}, reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	h := &historyHarness{bus: &fakeControlBus{}, hook: &controlobs.Hook{}, history: history, done: make(chan error, 1)}
	go func() {
		h.done <- runControlSubscriber(ctx, gridappsdclient.NewSubscriber(h.bus), embed, reg, "sim-1", h.hook, history)
	}()
	waitFor(2*time.Second, func() bool {
		h.bus.mu.Lock()
		defer h.bus.mu.Unlock()
		return h.bus.handler != nil
	})
	h.bus.mu.Lock()
	ok := h.bus.handler != nil
	h.bus.mu.Unlock()
	if !ok {
		t.Fatal("control subscriber never called Subscribe")
	}
	return h
}

// skipsPerFrame is how many deltas the empty-registry control path
// skips per socFrame: the state-of-charge delta and the control delta.
const skipsPerFrame = 2

func (h *historyHarness) waitFrames(t *testing.T, frames int) {
	t.Helper()
	n := frames * skipsPerFrame
	waitFor(3*time.Second, func() bool { return h.hook.Snapshot().Skipped >= uint64(n) })
	if got := h.hook.Snapshot().Skipped; got != uint64(n) {
		t.Fatalf("hook Skipped = %d, want %d (frames not all processed)", got, n)
	}
}

// socFrame builds an input frame as the telemetry publisher would for a
// device reporting stateOfChargeStatus hundredths, plus one control
// delta for mrid, which this harness's empty registry skips.
func socFrame(t *testing.T, mrid string, hundredths uint16, epoch int64) []byte {
	t.Helper()
	diffs, err := telemetrypub.MapDERStatusToDifferences(mrid, sep2.DERStatus{
		StateOfChargeStatus: &sep2.StateOfChargeStatusType{Value: hundredths},
	})
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	b := diff.NewBuilder("sim-1")
	for _, d := range diffs {
		if err := b.AddDifference(d.Object, d.Attribute, d.Value, d.Value); err != nil {
			t.Fatalf("AddDifference: %v", err)
		}
	}
	if err := b.AddDifference(mrid, controlAttr,
		map[string]any{"multiplier": 0.0, "value": 100.0},
		map[string]any{"multiplier": 0.0, "value": 0.0}); err != nil {
		t.Fatalf("AddDifference: %v", err)
	}
	body, err := b.Bytes(epoch)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	return body
}

func findSeries(snap []telemetryhistory.SeriesSnapshot, object, attr string) (telemetryhistory.SeriesSnapshot, bool) {
	for _, s := range snap {
		if s.Key.Object == object && s.Key.Attribute == attr {
			return s, true
		}
	}
	return telemetryhistory.SeriesSnapshot{}, false
}

func TestInputFrameFeedsHistoryWithScaledStateOfCharge(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)

	h.bus.deliver(socFrame(t, "M", 6500, frameEpoch))
	h.waitFrames(t, 1)

	s, ok := findSeries(store.Snapshot(), "M", socAttr)
	if !ok || len(s.Samples) != 1 {
		t.Fatalf("series for M/%s = %+v (found=%v), want one sample", socAttr, s, ok)
	}
	got := s.Samples[0]
	if got.Value != 65.0 {
		t.Errorf("state of charge = %v, want 65.0", got.Value)
	}
	// The envelope's own time, not local receipt time.
	if got.At != frameEpoch {
		t.Errorf("sample At = %d, want envelope time %d", got.At, frameEpoch)
	}
}

func TestHistoryStaysBoundedUnderLongRun(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)

	const distinct = telemetryhistory.MaxSeries + 44
	frames := 0
	for i := 0; i < distinct; i++ {
		h.bus.deliver(socFrame(t, fmt.Sprintf("dev-%03d", i), 5000, frameEpoch+int64(i)))
		frames++
	}
	const repeats = telemetryhistory.SamplesPerSeries + 60
	for i := 0; i < repeats; i++ {
		h.bus.deliver(socFrame(t, "hot", uint16(i%10000), frameEpoch+int64(i)))
		frames++
	}
	h.waitFrames(t, frames)

	snap := store.Snapshot()
	if len(snap) > telemetryhistory.MaxSeries {
		t.Errorf("series = %d, want <= %d", len(snap), telemetryhistory.MaxSeries)
	}
	hot, ok := findSeries(snap, "hot", socAttr)
	if !ok {
		t.Fatal("hot series evicted although it was appended most recently")
	}
	if len(hot.Samples) != telemetryhistory.SamplesPerSeries {
		t.Errorf("hot samples = %d, want %d", len(hot.Samples), telemetryhistory.SamplesPerSeries)
	}
	// Newest sample survives the wrap.
	if last := hot.Samples[len(hot.Samples)-1]; last.At != frameEpoch+int64(repeats-1) {
		t.Errorf("newest sample At = %d, want %d", last.At, frameEpoch+int64(repeats-1))
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Not parallel: it swaps the process-wide log writer.
func TestMalformedFrameLeavesHistoryAndControlPathUntouched(t *testing.T) {
	var logs lockedBuf
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)

	h.bus.deliver([]byte(`{"input": not json`))
	// A good frame after the bad one proves the loop survived and gives
	// a processed-frame signal.
	h.bus.deliver(socFrame(t, "M", 4000, frameEpoch))
	h.waitFrames(t, 1)

	// Only the good frame's two series exist: the malformed body added none.
	snap := store.Snapshot()
	soc, ok := findSeries(snap, "M", socAttr)
	if len(snap) != 2 || !ok || len(soc.Samples) != 1 || soc.Samples[0].Value != 40.0 {
		t.Errorf("history after malformed+good frame = %+v, want exactly M/%s at 40.0 and M/%s", snap, socAttr, controlAttr)
	}
	if s := h.hook.Snapshot(); s.Applied != 0 || s.Skipped != skipsPerFrame {
		t.Errorf("hook Applied/Skipped = %d/%d, want 0/%d", s.Applied, s.Skipped, skipsPerFrame)
	}
	if !strings.Contains(logs.String(), "skip malformed frame") {
		t.Errorf("malformed frame was not logged; log = %q", logs.String())
	}
}
