package telemetrypub

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// newTestPublisher builds a Publisher over the given source and bus with
// a fixed clock and the production defaults for everything else.
func newTestPublisher(t *testing.T, src StatusSource, bus BusPublisher, mutate func(*Config)) *Publisher {
	t.Helper()

	cfg := Config{
		Source:      src,
		Bus:         bus,
		Destination: "/topic/goss.gridappsd.simulation.input.sim-1",
		Build:       DiffMessageBuilder("sim-1"),
		Interval:    10 * time.Millisecond,
		Now:         func() time.Time { return buildTime },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	t.Parallel()

	full := func() Config {
		return Config{
			Source:      &fakeSource{},
			Bus:         &fakeBus{},
			Destination: "/topic/dest",
			Build:       DiffMessageBuilder("sim-1"),
		}
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no source", func(c *Config) { c.Source = nil }},
		{"no bus", func(c *Config) { c.Bus = nil }},
		{"no destination", func(c *Config) { c.Destination = "" }},
		{"no builder", func(c *Config) { c.Build = nil }},
		{"negative interval", func(c *Config) { c.Interval = -time.Second }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := full()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New with %s: want error, got nil", tc.name)
			}
		})
	}
}

func TestNewDefaultsIntervalToFifteenSeconds(t *testing.T) {
	t.Parallel()

	p, err := New(Config{
		Source:      &fakeSource{},
		Bus:         &fakeBus{},
		Destination: "/topic/dest",
		Build:       DiffMessageBuilder("sim-1"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.interval != DefaultInterval {
		t.Errorf("interval = %s, want %s", p.interval, DefaultInterval)
	}
	if DefaultInterval != 15*time.Second {
		t.Errorf("DefaultInterval = %s, want 15s to match the Python upstream's publish_interval_seconds", DefaultInterval)
	}
}

// TestPublishOnceSendsOneAggregateCoveringEveryDevice is the batching
// invariant: N changed devices produce ONE message on the configured
// destination, carrying each device's own values under its own mRID.
func TestPublishOnceSendsOneAggregateCoveringEveryDevice(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2), snapWithMode("mrid-b", 3))
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce: %v", err)
	}

	sends := bus.snapshot()
	if len(sends) != 1 {
		t.Fatalf("bus Send called %d times, want exactly 1 aggregate", len(sends))
	}
	if sends[0].dest != "/topic/goss.gridappsd.simulation.input.sim-1" {
		t.Errorf("destination = %q, want the configured synthetic simulation-input topic", sends[0].dest)
	}
	if sends[0].contentType != ContentTypeJSON {
		t.Errorf("content type = %q, want %q", sends[0].contentType, ContentTypeJSON)
	}

	view := decodeDiffMessage(t, sends[0].body)
	got := map[string]any{}
	for _, fd := range view.Input.Message.ForwardDifferences {
		got[fd.Object+"|"+fd.Attribute] = fd.Value
	}
	if len(got) != 2 {
		t.Fatalf("forward differences = %+v, want one per device", got)
	}
	if got["mrid-a|DERStatus.operationalModeStatus"] != float64(2) {
		t.Errorf("mrid-a value = %v, want 2", got["mrid-a|DERStatus.operationalModeStatus"])
	}
	if got["mrid-b|DERStatus.operationalModeStatus"] != float64(3) {
		t.Errorf("mrid-b value = %v, want 3", got["mrid-b|DERStatus.operationalModeStatus"])
	}
}

// TestPublishOnceOmitsUnchangedDevices: the second interval carries only
// the device whose mapped value moved.
func TestPublishOnceOmitsUnchangedDevices(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2), snapWithMode("mrid-b", 3))
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (first): %v", err)
	}
	src.set(snapWithMode("mrid-a", 2), snapWithMode("mrid-b", 7))
	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (second): %v", err)
	}

	sends := bus.snapshot()
	if len(sends) != 2 {
		t.Fatalf("bus Send called %d times, want 2", len(sends))
	}
	view := decodeDiffMessage(t, sends[1].body)
	if len(view.Input.Message.ForwardDifferences) != 1 {
		t.Fatalf("second publish carried %+v, want only the changed device", view.Input.Message.ForwardDifferences)
	}
	fd := view.Input.Message.ForwardDifferences[0]
	if fd.Object != "mrid-b" {
		t.Errorf("second publish carried object %q, want mrid-b (the only device that changed)", fd.Object)
	}
	if fd.Value != float64(7) {
		t.Errorf("second publish carried value %v, want 7", fd.Value)
	}
}

// TestPublishOnceWithNoChangesSendsNothingAndLogs is the requirement
// that cost three end-to-end runs a cycle each: an interval with nothing
// to say must not put an empty envelope on the bus, and must not be
// silent about it either.
func TestPublishOnceWithNoChangesSendsNothingAndLogs(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2))
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (first): %v", err)
	}
	logBuf.Reset()
	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (second): %v", err)
	}

	if sends := bus.snapshot(); len(sends) != 1 {
		t.Fatalf("bus Send called %d times, want 1: the unchanged interval must send nothing", len(sends))
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "no device") {
		t.Errorf("log output = %q, want it to state that no device changed", logged)
	}
	if !strings.Contains(logged, "nothing published") {
		t.Errorf("log output = %q, want it to state that nothing was published", logged)
	}
}

// TestPublishOnceWithNoStoredStatusSendsNothingAndLogs is the empty-fleet
// case: no device has ever PUT a DERStatus, so there is nothing to
// aggregate. Same rule: no empty envelope, and not silent.
func TestPublishOnceWithNoStoredStatusSendsNothingAndLogs(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	src := &fakeSource{}
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce: %v", err)
	}
	if sends := bus.snapshot(); len(sends) != 0 {
		t.Fatalf("bus Send called %d times, want 0", len(sends))
	}
	if !strings.Contains(logBuf.String(), "no device") {
		t.Errorf("log output = %q, want it to state that no device reported", logBuf.String())
	}
}

// TestPublishOnceSendFailureKeepsTheUpdatePending: a bus failure must
// not consume the change. The next interval republishes it.
func TestPublishOnceSendFailureKeepsTheUpdatePending(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2))
	bus := &fakeBus{}
	sendErr := errors.New("broker down")
	bus.setErr(sendErr)
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); !errors.Is(err, sendErr) {
		t.Fatalf("publishOnce error = %v, want it to wrap %v", err, sendErr)
	}
	if sends := bus.snapshot(); len(sends) != 0 {
		t.Fatalf("bus recorded %d sends, want 0 (the send failed)", len(sends))
	}

	bus.setErr(nil)
	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce after recovery: %v", err)
	}
	sends := bus.snapshot()
	if len(sends) != 1 {
		t.Fatalf("bus recorded %d sends after recovery, want 1: the failed update must be republished", len(sends))
	}
	view := decodeDiffMessage(t, sends[0].body)
	if len(view.Input.Message.ForwardDifferences) != 1 ||
		view.Input.Message.ForwardDifferences[0].Object != "mrid-a" {
		t.Errorf("republished payload = %+v, want mrid-a's pending update", view.Input.Message.ForwardDifferences)
	}
}

// TestPublishOncePublishUnchangedRepublishesEverything pins the OFF
// position of the suppression switch: full-snapshot semantics, every
// device every interval even when nothing moved.
func TestPublishOncePublishUnchangedRepublishesEverything(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2), snapWithMode("mrid-b", 3))
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, func(c *Config) { c.PublishUnchanged = true })

	for range 3 {
		if err := p.publishOnce(context.Background()); err != nil {
			t.Fatalf("publishOnce: %v", err)
		}
	}

	sends := bus.snapshot()
	if len(sends) != 3 {
		t.Fatalf("bus Send called %d times, want 3: suppression is off", len(sends))
	}
	for i, s := range sends {
		view := decodeDiffMessage(t, s.body)
		if len(view.Input.Message.ForwardDifferences) != 2 {
			t.Errorf("send %d carried %d differences, want 2 (both devices, every interval)",
				i, len(view.Input.Message.ForwardDifferences))
		}
	}
}

// TestPublishOnceSourceFailureSendsNothing: a read failure is surfaced,
// and nothing is published from a partial or absent snapshot.
func TestPublishOnceSourceFailureSendsNothing(t *testing.T) {
	t.Parallel()

	srcErr := errors.New("store read failed")
	src := &fakeSource{err: srcErr}
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); !errors.Is(err, srcErr) {
		t.Fatalf("publishOnce error = %v, want it to wrap %v", err, srcErr)
	}
	if sends := bus.snapshot(); len(sends) != 0 {
		t.Fatalf("bus Send called %d times, want 0", len(sends))
	}
}

// TestRunPublishesOnItsOwnTimerAndExitsOnCancel covers the lifecycle:
// the loop publishes without any request touching it, and returns
// promptly on context cancellation with its goroutine finished.
func TestRunPublishesOnItsOwnTimerAndExitsOnCancel(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2))
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for len(bus.snapshot()) == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("no publish within 3s: the timer loop never fired")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel (goroutine leak)")
	}
}

// TestRunSurvivesAFailingBus: a bus outage must not take the publisher
// down, because a protocol server that keeps serving devices while the
// platform side is broken is the whole point of splitting the two.
func TestRunSurvivesAFailingBus(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2))
	bus := &fakeBus{}
	bus.setErr(errors.New("broker down"))
	p := newTestPublisher(t, src, bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for src.callCount() < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("publisher stopped polling after a bus failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	bus.setErr(nil)
	for len(bus.snapshot()) == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("publisher never recovered after the bus came back")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel")
	}
}

// TestMarkIsSafeWhilePublishing exercises the marking seam under the
// race detector: Mark is called concurrently (as the core-side arrival
// observer will call it, from the 2030.5 request goroutine) while the
// publisher's own loop selects, sends and clears.
func TestMarkIsSafeWhilePublishing(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2), snapWithMode("mrid-b", 3))
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, func(c *Config) { c.Interval = time.Millisecond })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 2000 {
			p.Mark("mrid-a")
			src.set(snapWithMode("mrid-a", uint8(i%7)), snapWithMode("mrid-b", 3))
		}
	}()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	wg.Wait()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel")
	}

	// Every published difference must still belong to the device it was
	// read from: concurrency must not cross device identities.
	for _, s := range bus.snapshot() {
		view := decodeDiffMessage(t, s.body)
		for _, fd := range view.Input.Message.ForwardDifferences {
			if fd.Object != "mrid-a" && fd.Object != "mrid-b" {
				t.Fatalf("published difference for unknown object %q", fd.Object)
			}
		}
	}
}

// TestPublisherNeverPublishesADeviceUnderAnotherIdentity is the identity
// invariant end to end: the published object for each device is exactly
// the mRID its snapshot carried, never its EDevID and never a sibling's
// mRID.
func TestPublisherNeverPublishesADeviceUnderAnotherIdentity(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	a := snapWithMode("mrid-a", 2)
	b := snapWithMode("mrid-b", 3)
	src.set(a, b)
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce: %v", err)
	}

	view := decodeDiffMessage(t, bus.snapshot()[0].body)
	wantValues := map[string]float64{"mrid-a": 2, "mrid-b": 3}
	seen := map[string]bool{}
	for _, fd := range view.Input.Message.ForwardDifferences {
		want, ok := wantValues[fd.Object]
		if !ok {
			t.Fatalf("published object %q is not a known device mRID", fd.Object)
		}
		if fd.Value != want {
			t.Errorf("device %q published value %v, want %v (values crossed between devices)", fd.Object, fd.Value, want)
		}
		seen[fd.Object] = true
	}
	for mrid := range wantValues {
		if !seen[mrid] {
			t.Errorf("device %q was not published at all", mrid)
		}
	}
	if strings.Contains(string(bus.snapshot()[0].body), a.EDevID) || strings.Contains(string(bus.snapshot()[0].body), b.EDevID) {
		t.Errorf("published payload leaks the server-assigned URL index instead of the CIM mRID.\nbody = %s", bus.snapshot()[0].body)
	}
}

// TestPublishOnceDeviceWithNoMappedFieldSendsNothingAndSettles covers
// the ErrNoContent branch: a device that stored a DERStatus carrying no
// field the mapping reads is a change (it went from nothing to
// something) but has nothing publishable. No envelope goes out, the
// reason is logged, and the device settles rather than re-logging every
// interval forever.
func TestPublishOnceDeviceWithNoMappedFieldSendsNothingAndSettles(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	src := &fakeSource{}
	src.set(sep2embed.DERStatusSnapshot{MRID: "mrid-a", EDevID: "8", DERID: "1"})
	bus := &fakeBus{}
	p := newTestPublisher(t, src, bus, nil)

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (first): %v", err)
	}
	if sends := bus.snapshot(); len(sends) != 0 {
		t.Fatalf("bus Send called %d times, want 0", len(sends))
	}
	if !strings.Contains(logBuf.String(), "no mapped field") {
		t.Errorf("log output = %q, want it to state that the device carried no mapped field", logBuf.String())
	}

	logBuf.Reset()
	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (second): %v", err)
	}
	if strings.Contains(logBuf.String(), "no mapped field") {
		t.Errorf("the empty device was re-selected on an unchanged interval: %q", logBuf.String())
	}

	// A later DERStatus carrying a real field must still publish.
	src.set(snapWithMode("mrid-a", 4))
	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce (third): %v", err)
	}
	sends := bus.snapshot()
	if len(sends) != 1 {
		t.Fatalf("bus Send called %d times after the device reported a real field, want 1", len(sends))
	}
	view := decodeDiffMessage(t, sends[0].body)
	if len(view.Input.Message.ForwardDifferences) != 1 ||
		view.Input.Message.ForwardDifferences[0].Object != "mrid-a" ||
		view.Input.Message.ForwardDifferences[0].Value != float64(4) {
		t.Errorf("published %+v, want mrid-a operationalModeStatus 4", view.Input.Message.ForwardDifferences)
	}
}

// Observe runs only after the bus accepts the send, with the exact
// bytes sent: a dead bus charts nothing, each changed value is charted
// once, and an unchanged interval records nothing.
func TestPublishOnceObservesOnlyAfterTheBusAccepts(t *testing.T) {
	t.Parallel()

	src := &fakeSource{}
	src.set(snapWithMode("mrid-a", 2))
	bus := &fakeBus{}
	bus.setErr(errors.New("broker down"))
	var observed []Message
	p := newTestPublisher(t, src, bus, func(c *Config) {
		c.Observe = func(m Message) { observed = append(observed, m) }
	})

	for i := 0; i < 5; i++ {
		if err := p.publishOnce(context.Background()); err == nil {
			t.Fatal("publishOnce error = nil, want the send failure")
		}
	}
	if len(observed) != 0 {
		t.Fatalf("Observe called %d times over 5 failed sends, want 0", len(observed))
	}

	bus.setErr(nil)
	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce after recovery: %v", err)
	}
	sends := bus.snapshot()
	if len(sends) != 1 || len(observed) != 1 || !bytes.Equal(observed[0].Body, sends[0].body) {
		t.Fatalf("observed %d, sent %d; want one each with equal bytes", len(observed), len(sends))
	}
	view := decodeDiffMessage(t, observed[0].Body)
	if len(view.Input.Message.ForwardDifferences) != 1 || view.Input.Message.ForwardDifferences[0].Object != "mrid-a" {
		t.Errorf("observed payload = %+v, want mrid-a's update", view.Input.Message.ForwardDifferences)
	}

	if err := p.publishOnce(context.Background()); err != nil {
		t.Fatalf("publishOnce unchanged: %v", err)
	}
	if len(observed) != 1 {
		t.Errorf("Observe called %d times, want 1: an unchanged interval records nothing", len(observed))
	}
}
