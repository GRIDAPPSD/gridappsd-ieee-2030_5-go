package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/gridappsdclient"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetrypub"
)

const testAppID = "IEEE_2030_5"

const (
	socAttr     = "DERStatus.stateOfChargeStatus"
	controlAttr = "DERControl.DERControlBase.opModTargetW"
	frameEpoch  = int64(1700000000)
)

// historyHarness runs runControlSubscriber against an unstarted embed
// and an empty registry, so every control delta is skipped and counted
// by hook.Skipped, which tests use as a "frame processed" signal. It is
// for control-path behavior only: reported state reaches history
// through the publisher (sinkHarness, publishToHistory).
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
	return startHistoryHarnessApp(t, history, testAppID)
}

func startHistoryHarnessApp(t *testing.T, history *telemetryhistory.Store, appID string) *historyHarness {
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
		h.done <- runControlSubscriber(ctx, gridappsdclient.NewSubscriber(h.bus), embed, reg, appID, "sim-1", h.hook, sink)
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

// statusFrame builds the body the status publisher sends for one device
// reporting stateOfChargeStatus hundredths: no control delta.
func statusFrame(t *testing.T, mrid string, hundredths uint16, epoch int64) []byte {
	t.Helper()
	diffs, err := telemetrypub.MapDERStatusToDifferences(mrid, sep2.DERStatus{
		StateOfChargeStatus: &sep2.StateOfChargeStatusType{Value: hundredths},
	})
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	return rawFrame(t, epoch, diffs...)
}

// sinkHarness drives historySink.observe directly, the call the
// publisher makes before each send, with no bus and no goroutine.
type sinkHarness struct {
	reg   *registry.Registry
	store *telemetryhistory.Store
	sink  *historySink
}

func newSinkHarness(t *testing.T, store *telemetryhistory.Store, mrids ...string) *sinkHarness {
	t.Helper()
	h := &sinkHarness{
		reg:   registry.New(),
		store: store,
		sink:  &historySink{store: store, logf: newRateLimitedLogf(historyLogInterval, time.Now, log.Printf)},
	}
	h.register(t, mrids...)
	return h
}

func (h *sinkHarness) register(t *testing.T, mrids ...string) {
	t.Helper()
	for _, m := range mrids {
		if err := h.reg.Add(registry.Entry{MRID: m, LFDI: strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(m))))}); err != nil {
			t.Fatalf("registry.Add(%s): %v", m, err)
		}
	}
}

func (h *sinkHarness) observe(body []byte) {
	h.sink.observe(h.reg, telemetrypub.Message{ContentType: telemetrypub.ContentTypeJSON, Body: body})
}

// switchBus is a bus that fails every send while down is set. tried
// counts every send attempt, accepted counts the ones that succeeded.
type switchBus struct {
	down     atomic.Bool
	tried    atomic.Int64
	accepted atomic.Int64

	destMu sync.Mutex
	dests  []string
}

func (b *switchBus) Send(_ context.Context, dest, _ string, _ []byte) error {
	b.destMu.Lock()
	b.dests = append(b.dests, dest)
	b.destMu.Unlock()
	b.tried.Add(1)
	if b.down.Load() {
		return errors.New("bus down")
	}
	b.accepted.Add(1)
	return nil
}

// mutableStatusSource is a StatusSource whose snapshot a test replaces.
type mutableStatusSource struct {
	mu    sync.Mutex
	snaps []sep2embed.DERStatusSnapshot
}

func (s *mutableStatusSource) set(snaps ...sep2embed.DERStatusSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = snaps
}

func (s *mutableStatusSource) DERStatusSnapshots(context.Context) ([]sep2embed.DERStatusSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sep2embed.DERStatusSnapshot(nil), s.snaps...), nil
}

func socSnapshot(mrid string, hundredths uint16) sep2embed.DERStatusSnapshot {
	return sep2embed.DERStatusSnapshot{
		MRID:   mrid,
		EDevID: "edev-" + mrid,
		DERID:  "der-" + mrid,
		Status: sep2.DERStatus{StateOfChargeStatus: &sep2.StateOfChargeStatusType{Value: hundredths}},
	}
}

// publishToHistory runs the publisher, wired through withHistory with a
// fixed clock, against a bus that accepts every send, until one send has
// been accepted. History holds the status by then.
func publishToHistory(t *testing.T, sink *historySink, reg *registry.Registry, snaps ...sep2embed.DERStatusSnapshot) {
	t.Helper()
	src := &mutableStatusSource{}
	src.set(snaps...)
	bus := &switchBus{}
	pub, err := telemetrypub.New(withHistory(telemetrypub.Config{
		Source:      src,
		Bus:         bus,
		Destination: "dest",
		Build:       telemetrypub.DiffMessageBuilder("sim-1"),
		Interval:    5 * time.Millisecond,
		Now:         func() time.Time { return time.Unix(frameEpoch, 0) },
	}, sink, reg))
	if err != nil {
		t.Fatalf("telemetrypub.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx) }()
	waitFor(3*time.Second, func() bool { return bus.accepted.Load() >= 1 })
	cancel()
	<-done
	if bus.accepted.Load() < 1 {
		t.Fatal("bus never accepted a send")
	}
}

func findSeries(snap []telemetryhistory.SeriesSnapshot, object, attr string) (telemetryhistory.SeriesSnapshot, bool) {
	for _, s := range snap {
		if s.Key.Object == object && s.Key.Attribute == attr {
			return s, true
		}
	}
	return telemetryhistory.SeriesSnapshot{}, false
}

// Built through newStatusPublisher, the function main uses, so a
// publisher wired without the history hook fails here.
func TestBridgePublisherChartsStatusOnlyAfterTheBusAccepts(t *testing.T) {
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store, "bat-1", "bat-2")
	src := &mutableStatusSource{}
	src.set(socSnapshot("bat-1", 6500), socSnapshot("bat-2", 3000))
	bus := &switchBus{}
	bus.down.Store(true)
	pub, err := newStatusPublisher(config{SimulationID: "sim-1", ApplicationID: defaultApplicationID, SEP2TelemetryInterval: 5 * time.Millisecond},
		src, bus, h.sink, h.reg)
	if err != nil {
		t.Fatalf("newStatusPublisher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)

	// Bus down for many cycles: nothing was accepted, so nothing is charted.
	const cycles = 6
	waitFor(3*time.Second, func() bool { return bus.tried.Load() >= cycles })
	if bus.tried.Load() < cycles {
		t.Fatalf("test premise broken: only %d send attempts", bus.tried.Load())
	}
	if snap := store.Snapshot(); len(snap) != 0 {
		t.Fatalf("history after %d failed sends = %+v, want empty", bus.tried.Load(), snap)
	}

	// Bus up: each changed value is charted once, however many cycles run.
	bus.down.Store(false)
	waitFor(3*time.Second, func() bool { return bus.accepted.Load() >= 1 })
	// Samples are stamped in whole seconds and a repeat of one second
	// replaces the earlier sample, so let the clock move on.
	time.Sleep(1100 * time.Millisecond)
	src.set(socSnapshot("bat-1", 6400), socSnapshot("bat-2", 3000))
	waitFor(3*time.Second, func() bool { return bus.accepted.Load() >= 2 })
	time.Sleep(60 * time.Millisecond)
	stop()

	snap := store.Snapshot()
	b1, ok1 := findSeries(snap, "bat-1", socAttr)
	b2, ok2 := findSeries(snap, "bat-2", socAttr)
	if !ok1 || !ok2 {
		t.Fatalf("history = %+v, want both series", snap)
	}
	if len(b1.Samples) != 2 || b1.Samples[0].Value != 65.0 || b1.Samples[1].Value != 64.0 {
		t.Errorf("bat-1 samples = %+v, want 65 then 64", b1.Samples)
	}
	if len(b2.Samples) != 1 || b2.Samples[0].Value != 30.0 {
		t.Errorf("bat-2 samples = %+v, want one sample of 30", b2.Samples)
	}
}

// An echoed status frame on the control path adds nothing: reported
// state is recorded from the publisher only.
func TestEchoedStatusFrameAddsNothingToHistory(t *testing.T) {
	var store telemetryhistory.Store
	h := startHistoryHarness(t, &store)
	h.register(t, "M")

	h.bus.deliver(socFrame(t, "M", 6500, frameEpoch))
	h.waitFrames(t, 1)

	if snap := store.Snapshot(); len(snap) != 0 {
		t.Errorf("history after an echoed status frame = %+v, want empty", snap)
	}
}

func TestHistoryStaysBoundedUnderLongRun(t *testing.T) {
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store)

	const distinct = telemetryhistory.MaxSeries + 44
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("dev-%03d", i)
		h.register(t, id)
		h.observe(statusFrame(t, id, 5000, frameEpoch+int64(i)))
	}
	const repeats = telemetryhistory.SamplesPerSeries + 60
	h.register(t, "hot")
	for i := 0; i < repeats; i++ {
		h.observe(statusFrame(t, "hot", uint16(i%10000), frameEpoch+int64(i)))
	}

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

	// Neither the malformed body nor the good frame's echoed status adds
	// a series, and the refused control delta is not charted.
	if snap := store.Snapshot(); len(snap) != 0 {
		t.Errorf("history after malformed+good frame = %+v, want empty", snap)
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
	h := newSinkHarness(t, &store, "M")

	// The sequence from review: a normal pair, a zero time, a time far
	// in the future, then a redelivery of the first time with a new value.
	h.observe(statusFrame(t, "M", 1000, 1700000000))
	h.observe(statusFrame(t, "M", 2000, 1700000015))
	h.observe(statusFrame(t, "M", 3000, 0))
	h.observe(statusFrame(t, "M", 4000, 4102444800))
	h.observe(statusFrame(t, "M", 1100, 1700000000))

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
	h := newSinkHarness(t, &store, "M")

	h.observe(statusFrame(t, "M", 1000, 1700000030))
	h.observe(statusFrame(t, "M", 2000, 1700000000))
	h.observe(statusFrame(t, "M", 3000, 1700000015))

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
	h := newSinkHarness(t, &store, "M")

	// 1 * 10^400 overflows float64 to +Inf.
	h.observe(rawFrame(t, frameEpoch, diff.Difference{Object: "M", Attribute: socAttr,
		Value: map[string]any{"multiplier": 400.0, "value": 1.0}}))

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
	h := newSinkHarness(t, &store, "M")

	const n = 20
	for i := 0; i < n; i++ {
		h.observe(rawFrame(t, frameEpoch+int64(i), diff.Difference{Object: "M", Attribute: socAttr, Value: "not-a-number"}))
	}
	// A routine, non-plottable field: must add no history log line.
	h.observe(rawFrame(t, frameEpoch, diff.Difference{Object: "M", Attribute: "DERStatus.readingTime", Value: 123.0}))

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
	h := newSinkHarness(t, &store, "known")

	h.observe(statusFrame(t, "stranger", 5000, frameEpoch))
	h.observe(statusFrame(t, "known", 6000, frameEpoch))

	snap := store.Snapshot()
	if len(snap) != 1 || snap[0].Key.Object != "known" {
		t.Errorf("history = %+v, want only the registered object", snap)
	}
}

// Not parallel: it swaps the process-wide log writer.
func TestHistoryLogsSeriesEviction(t *testing.T) {
	logs := captureHistoryLog(t)
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store)

	frames := telemetryhistory.MaxSeries + 1
	for i := 0; i < frames; i++ {
		id := fmt.Sprintf("dev-%03d", i)
		h.register(t, id)
		h.observe(statusFrame(t, id, 5000, frameEpoch+int64(i)))
	}

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
}

// An applied control records its commanded setpoint, scaled as before.
func TestAppliedControlRecordsCommandedSetpoint(t *testing.T) {
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store, "M")
	envelope := diff.Message{}
	envelope.Input.Message.Timestamp = frameEpoch
	delta := diff.Difference{Object: "M", Attribute: controlAttr,
		Value: map[string]any{"multiplier": 1.0, "value": 5.0}}

	h.sink.recordApplied(h.reg, envelope, delta, true)

	s, ok := findSeries(store.Snapshot(), "M", controlAttr)
	if !ok || len(s.Samples) != 1 || s.Samples[0].Value != 50.0 || s.Samples[0].At != frameEpoch {
		t.Errorf("commanded series = %+v (found=%v), want one sample of 50 at %d", s, ok, frameEpoch)
	}

	refusedEnvelope := diff.Message{}
	refusedEnvelope.Input.Message.Timestamp = frameEpoch + 30
	refused := diff.Difference{Object: "M", Attribute: controlAttr,
		Value: map[string]any{"multiplier": 1.0, "value": 9.0}}
	h.sink.recordApplied(h.reg, refusedEnvelope, refused, false)
	if s, _ := findSeries(store.Snapshot(), "M", controlAttr); len(s.Samples) != 1 {
		t.Errorf("refused control added a sample: %+v", s.Samples)
	}
}

// A frame carrying both a status delta and an applied control charts
// the control only: the reported state in it is an echo.
func TestAppliedControlAlongsideEchoedStatusAddsNoReportedSample(t *testing.T) {
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store, "M")
	var envelope diff.Message
	if err := json.Unmarshal(socFrame(t, "M", 6500, frameEpoch), &envelope); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	var control diff.Difference
	for _, d := range envelope.Input.Message.ForwardDifferences {
		if d.Attribute == controlAttr {
			control = d
		}
	}

	h.sink.recordApplied(h.reg, envelope, control, true)

	if _, ok := findSeries(store.Snapshot(), "M", socAttr); ok {
		t.Errorf("echoed status charted alongside an applied control")
	}
	if _, ok := findSeries(store.Snapshot(), "M", controlAttr); !ok {
		t.Errorf("applied control not charted")
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

// The destination the bridge's real publisher sends to, read at the bus:
// the application output topic,
// and never the application input topic the control subscriber listens on.
func TestBridgePublisherSendsToTheApplicationOutputTopic(t *testing.T) {
	var store telemetryhistory.Store
	h := newSinkHarness(t, &store, "bat-1")
	src := &mutableStatusSource{}
	src.set(socSnapshot("bat-1", 6500))
	bus := &switchBus{}
	pub, err := newStatusPublisher(config{SimulationID: "X", ApplicationID: "IEEE_2030_5", SEP2TelemetryInterval: 5 * time.Millisecond},
		src, bus, h.sink, h.reg)
	if err != nil {
		t.Fatalf("newStatusPublisher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx) }()
	waitFor(3*time.Second, func() bool { return bus.accepted.Load() >= 1 })
	cancel()
	<-done

	bus.destMu.Lock()
	defer bus.destMu.Unlock()
	if len(bus.dests) == 0 {
		t.Fatal("bus saw no send")
	}
	const want = "/topic/goss.gridappsd.application.IEEE_2030_5.X.output"
	for _, d := range bus.dests {
		if d != want {
			t.Errorf("destination = %q, want %q", d, want)
		}
		if d == sim.ApplicationInputTopic("IEEE_2030_5", "X") {
			t.Errorf("destination %q is the application input topic the control subscriber reads, so status would echo back", d)
		}
	}
}
