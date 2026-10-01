package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
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
	reg     *registry.Registry
	bus     *fakeControlBus
	hook    *controlobs.Hook
	history *telemetryhistory.Store
	embed   *sep2embed.Embed
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
	h := &historyHarness{reg: reg, bus: &fakeControlBus{}, hook: &controlobs.Hook{}, history: history, embed: embed, done: make(chan error, 1)}
	go func() {
		sink := &historySink{store: history, logf: newRateLimitedLogf(historyLogInterval, time.Now, log.Printf)}
		h.done <- runControlSubscriber(ctx, gridappsdclient.NewSubscriber(h.bus), embed, reg, "sim-1", h.hook, sink)
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

// register adds mrids to the registry the subscriber consults. The
// embed was built before this call, so it holds no device for them and
// refuses every control delta addressed to them.
func (h *historyHarness) register(t *testing.T, mrids ...string) {
	t.Helper()
	for _, m := range mrids {
		if err := h.reg.Add(registry.Entry{MRID: m, LFDI: strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(m))))}); err != nil {
			t.Fatalf("registry.Add(%s): %v", m, err)
		}
	}
}

func (h *historyHarness) waitFrames(t *testing.T, frames int) {
	t.Helper()
	h.waitDeltas(t, frames*skipsPerFrame)
}

// waitDeltas waits until the control path has handled n deltas.
func (h *historyHarness) waitDeltas(t *testing.T, n int) {
	t.Helper()
	total := func() uint64 { s := h.hook.Snapshot(); return s.Applied + s.Skipped }
	waitFor(3*time.Second, func() bool { return total() >= uint64(n) })
	if got := total(); got != uint64(n) {
		t.Fatalf("hook Applied+Skipped = %d, want %d (frames not all processed)", got, n)
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
	h.register(t, "M")

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
		h.register(t, fmt.Sprintf("dev-%03d", i))
		h.bus.deliver(socFrame(t, fmt.Sprintf("dev-%03d", i), 5000, frameEpoch+int64(i)))
		frames++
	}
	const repeats = telemetryhistory.SamplesPerSeries + 60
	h.register(t, "hot")
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
	h.register(t, "M")

	h.bus.deliver([]byte(`{"input": not json`))
	// A good frame after the bad one proves the loop survived and gives
	// a processed-frame signal.
	h.bus.deliver(socFrame(t, "M", 4000, frameEpoch))
	h.waitFrames(t, 1)

	// Only the good frame's state-of-charge series exists: the malformed
	// body added none, and the refused control delta is not charted.
	snap := store.Snapshot()
	soc, ok := findSeries(snap, "M", socAttr)
	if len(snap) != 1 || !ok || len(soc.Samples) != 1 || soc.Samples[0].Value != 40.0 {
		t.Errorf("history after malformed+good frame = %+v, want exactly M/%s at 40.0", snap, socAttr)
	}
	if s := h.hook.Snapshot(); s.Applied != 0 || s.Skipped != skipsPerFrame {
		t.Errorf("hook Applied/Skipped = %d/%d, want 0/%d", s.Applied, s.Skipped, skipsPerFrame)
	}
	if !strings.Contains(logs.String(), "skip malformed frame") {
		t.Errorf("malformed frame was not logged; log = %q", logs.String())
	}
}

// rawFrame builds an input frame from arbitrary differences.
func rawFrame(t *testing.T, epoch int64, diffs ...diff.Difference) []byte {
	t.Helper()
	b := diff.NewBuilder("sim-1")
	for _, d := range diffs {
		if err := b.AddDifference(d.Object, d.Attribute, d.Value, d.Value); err != nil {
			t.Fatalf("AddDifference: %v", err)
		}
	}
	body, err := b.Bytes(epoch)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	return body
}

func captureHistoryLog(t *testing.T) *lockedBuf {
	t.Helper()
	var logs lockedBuf
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

// Not parallel: it swaps the process-wide log writer.
func TestHistoryKeepsSeriesSortedFiniteAndFreeOfDuplicates(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	// The sequence from review: a normal pair, a zero time, a time far
	// in the future, then a redelivery of the first time with a new value.
	h.bus.deliver(socFrame(t, "M", 1000, 1700000000))
	h.bus.deliver(socFrame(t, "M", 2000, 1700000015))
	h.bus.deliver(socFrame(t, "M", 3000, 0))
	h.bus.deliver(socFrame(t, "M", 4000, 4102444800))
	h.bus.deliver(socFrame(t, "M", 1100, 1700000000))
	h.waitFrames(t, 5)

	s, ok := findSeries(store.Snapshot(), "M", socAttr)
	if !ok {
		t.Fatal("series missing")
	}
	want := []telemetryhistory.Sample{{At: 1700000000, Value: 11.0}, {At: 1700000015, Value: 20.0}}
	if len(s.Samples) != len(want) {
		t.Fatalf("samples = %+v, want %+v", s.Samples, want)
	}
	for i := range want {
		if s.Samples[i] != want[i] {
			t.Errorf("sample[%d] = %+v, want %+v", i, s.Samples[i], want[i])
		}
	}
}

// Out-of-order arrival must still leave the series sorted by time.
func TestHistoryInsertsOutOfOrderSampleInTimeOrder(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	h.bus.deliver(socFrame(t, "M", 1000, 1700000030))
	h.bus.deliver(socFrame(t, "M", 2000, 1700000000))
	h.bus.deliver(socFrame(t, "M", 3000, 1700000015))
	h.waitFrames(t, 3)

	s, _ := findSeries(store.Snapshot(), "M", socAttr)
	var got []int64
	for _, x := range s.Samples {
		got = append(got, x.At)
	}
	if fmt.Sprint(got) != fmt.Sprint([]int64{1700000000, 1700000015, 1700000030}) {
		t.Errorf("times = %v, want ascending", got)
	}
}

// Not parallel: it swaps the process-wide log writer.
func TestHistoryRefusesNonFiniteValuesAndLogsThem(t *testing.T) {
	logs := captureHistoryLog(t)
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	// 1 * 10^400 overflows float64 to +Inf.
	h.bus.deliver(rawFrame(t, frameEpoch, diff.Difference{Object: "M", Attribute: socAttr,
		Value: map[string]any{"multiplier": 400.0, "value": 1.0}}))
	h.waitDeltas(t, 1)

	snap := store.Snapshot()
	if len(snap) != 0 {
		t.Errorf("history = %+v, want empty (non-finite refused)", snap)
	}
	if _, err := json.Marshal(snap); err != nil {
		t.Errorf("json.Marshal(snapshot): %v", err)
	}
	if !strings.Contains(logs.String(), "non-finite") {
		t.Errorf("non-finite value not logged; log = %q", logs.String())
	}
}

// Not parallel: it swaps the process-wide log writer.
func TestHistoryLogsWrongShapeRateLimitedAndKeepsRoutineFieldsQuiet(t *testing.T) {
	logs := captureHistoryLog(t)
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	const n = 20
	for i := 0; i < n; i++ {
		h.bus.deliver(rawFrame(t, frameEpoch+int64(i), diff.Difference{Object: "M", Attribute: socAttr, Value: "not-a-number"}))
	}
	// A routine, non-plottable field: must add no history log line.
	h.bus.deliver(rawFrame(t, frameEpoch, diff.Difference{Object: "M", Attribute: "DERStatus.readingTime", Value: 123.0}))
	h.waitDeltas(t, n+1)

	out := logs.String()
	if c := strings.Count(out, "unrecognized value shape"); c < 1 || c > 2 {
		t.Errorf("wrong-shape faults logged %d times for %d frames, want 1 or 2 (rate limited); log = %q", c, n, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "history") && strings.Contains(line, "readingTime") {
			t.Errorf("routine non-plottable field was logged by history: %q", line)
		}
	}
	if len(store.Snapshot()) != 0 {
		t.Errorf("history = %+v, want empty", store.Snapshot())
	}
}

func TestHistorySkipsUnregisteredObjects(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "known")

	h.bus.deliver(socFrame(t, "stranger", 5000, frameEpoch))
	h.bus.deliver(socFrame(t, "known", 6000, frameEpoch))
	h.waitFrames(t, 2)

	snap := store.Snapshot()
	if len(snap) != 1 || snap[0].Key.Object != "known" {
		t.Errorf("history = %+v, want only the registered object", snap)
	}
}

// Not parallel: it swaps the process-wide log writer.
func TestHistoryLogsSeriesEviction(t *testing.T) {
	logs := captureHistoryLog(t)
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)

	frames := telemetryhistory.MaxSeries + 1
	for i := 0; i < frames; i++ {
		id := fmt.Sprintf("dev-%03d", i)
		h.register(t, id)
		h.bus.deliver(socFrame(t, id, 5000, frameEpoch+int64(i)))
	}
	h.waitFrames(t, frames)

	if len(store.Snapshot()) != telemetryhistory.MaxSeries {
		t.Fatalf("series = %d, want %d", len(store.Snapshot()), telemetryhistory.MaxSeries)
	}
	if !strings.Contains(logs.String(), "evicted") {
		t.Errorf("eviction not logged; log = %q", logs.String())
	}
}

// A control the control path refused is not a commanded setpoint, so it
// must not be charted as one.
func TestHistoryDoesNotChartRefusedControl(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	// The embed holds no device for M, so the control path refuses it.
	h.bus.deliver(socFrame(t, "M", 6500, frameEpoch))
	h.waitFrames(t, 1)

	if h.hook.Snapshot().Applied != 0 {
		t.Fatalf("control unexpectedly applied; test premise broken")
	}
	if _, ok := findSeries(store.Snapshot(), "M", controlAttr); ok {
		t.Errorf("refused control was charted as %s", controlAttr)
	}
	if _, ok := findSeries(store.Snapshot(), "M", socAttr); !ok {
		t.Errorf("state of charge missing; the refusal must not suppress the reported state")
	}
}

func TestRateLimitedLogfEmitsOncePerIntervalPerFormat(t *testing.T) {
	var lines []string
	now := time.Unix(1000, 0)
	logf := newRateLimitedLogf(30*time.Second, func() time.Time { return now },
		func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })

	logf("fault A %d", 1)
	logf("fault A %d", 2)
	logf("fault B %d", 3) // a different format has its own bucket
	now = now.Add(29 * time.Second)
	logf("fault A %d", 4)
	now = now.Add(2 * time.Second)
	logf("fault A %d", 5)

	want := []string{"fault A 1", "fault B 3", "fault A 5 (2 similar suppressed)"}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}
