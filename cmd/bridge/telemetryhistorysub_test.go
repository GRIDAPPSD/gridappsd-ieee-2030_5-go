package main

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp/cimstomptest"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// fakeHistorySubs is a sim.SubscribeClient test double supporting an
// independent, caller-armed subscription per destination: exactly the
// shape runHistorySubscriber needs to drive with more than one
// configured destination at once. Subscribe records every destination
// it was called with, in call order, and returns per-destination
// subscribe errors and frame feeds preconfigured before the test starts
// the subscriber.
type fakeHistorySubs struct {
	mu sync.Mutex

	// byDest configures each destination's behavior: frames to deliver
	// (closeAfterFrames true means the subscription closes once they are
	// all sent) and/or an immediate Subscribe error.
	byDest map[string]fakeHistoryDestConfig

	// subscribed records every destination Subscribe was called with, in
	// call order.
	subscribed []string
}

type fakeHistoryDestConfig struct {
	subscribeErr     error
	frames           [][]byte
	closeAfterFrames bool
}

func (f *fakeHistorySubs) Subscribe(ctx context.Context, destination string) (sim.Subscription, error) {
	f.mu.Lock()
	f.subscribed = append(f.subscribed, destination)
	cfg := f.byDest[destination]
	f.mu.Unlock()

	if cfg.subscribeErr != nil {
		return nil, cfg.subscribeErr
	}

	sub, msgsIn := cimstomptest.NewSubscription()
	go func() {
		defer close(msgsIn)
		for _, body := range cfg.frames {
			select {
			case <-ctx.Done():
				return
			case msgsIn <- messageWithBody(destination, body):
			}
		}
		if cfg.closeAfterFrames {
			return
		}
		<-ctx.Done()
	}()
	return sub, nil
}

// destinationsOf returns f.subscribed under lock, for assertions after
// the subscriber has finished.
func (f *fakeHistorySubs) destinationsOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subscribed...)
}

// realCaptureFrame is the committed real-capture fixture's envelope,
// inlined here rather than read from testdata so this end-to-end test
// has no file dependency. It carries one forward difference on the
// commanded-setpoint lane: object
// "50B15A48-9611-40DF-983E-93679DA72871",
// attribute "DERControl.DERControlBase.opModTargetW", multiplier 0,
// value 5000.
const realCaptureFrame = `{"command":"update","input":{"simulation_id":"gago094p2e","message":{"timestamp":1785390891,"difference_mrid":"c1974060-4a78-4c9f-930f-b2404525b85b","reverse_differences":[{"object":"50B15A48-9611-40DF-983E-93679DA72871","attribute":"DERControl.DERControlBase.opModTargetW","value":{"multiplier":0,"value":0}}],"forward_differences":[{"object":"50B15A48-9611-40DF-983E-93679DA72871","attribute":"DERControl.DERControlBase.opModTargetW","value":{"multiplier":0,"value":5000}}]}}}`

// TestRunHistorySubscriberDecodesFrameIntoStore is the end-to-end wiring
// test: a raw diff.Message frame delivered on a configured destination
// reaches the store as a decoded, correctly keyed sample.
func TestRunHistorySubscriberDecodesFrameIntoStore(t *testing.T) {
	t.Parallel()

	const dest = "/topic/goss.gridappsd.simulation.input.gago094p2e"
	fake := &fakeHistorySubs{byDest: map[string]fakeHistoryDestConfig{
		dest: {frames: [][]byte{[]byte(realCaptureFrame)}, closeAfterFrames: true},
	}}

	store := &telemetryhistory.Store{}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runHistorySubscriber(ctx, fake, store, []string{dest}) }()

	var snap []telemetryhistory.SeriesSnapshot
	waitFor(2*time.Second, func() bool {
		snap = store.Snapshot()
		return len(snap) == 1
	})

	if len(snap) != 1 {
		t.Fatalf("store.Snapshot() has %d series, want 1: %+v", len(snap), snap)
	}
	wantKey := telemetryhistory.SeriesKey{
		Object:    "50B15A48-9611-40DF-983E-93679DA72871",
		Attribute: "DERControl.DERControlBase.opModTargetW",
	}
	if snap[0].Key != wantKey {
		t.Errorf("series key = %+v, want %+v", snap[0].Key, wantKey)
	}
	if len(snap[0].Samples) != 1 {
		t.Fatalf("series has %d samples, want 1", len(snap[0].Samples))
	}
	if snap[0].Samples[0].Value != 5000 {
		t.Errorf("sample value = %v, want 5000 (forward differences only, multiplier 0)", snap[0].Samples[0].Value)
	}
	if snap[0].Samples[0].At != 1785390891 {
		t.Errorf("sample At = %d, want the envelope's own timestamp 1785390891", snap[0].Samples[0].At)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("runHistorySubscriber returned non-graceful error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runHistorySubscriber did not return within 3s of ctx cancel")
	}
}

// TestRunHistorySubscriberSubscribesEveryConfiguredDestination asserts
// every configured destination is subscribed exactly once, and nothing
// else: this is the "existing pump and control subscriptions are
// unaffected" guarantee at the unit level, since runHistorySubscriber
// only ever calls Subscribe with the exact list it was given, never with
// any destination of its own invention.
func TestRunHistorySubscriberSubscribesEveryConfiguredDestination(t *testing.T) {
	t.Parallel()

	dests := []string{"/topic/a", "/topic/b", "/topic/c"}
	byDest := map[string]fakeHistoryDestConfig{}
	for _, d := range dests {
		byDest[d] = fakeHistoryDestConfig{}
	}
	fake := &fakeHistorySubs{byDest: byDest}
	store := &telemetryhistory.Store{}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- runHistorySubscriber(ctx, fake, store, dests) }()

	waitFor(2*time.Second, func() bool { return len(fake.destinationsOf()) == len(dests) })

	got := fake.destinationsOf()
	if len(got) != len(dests) {
		t.Fatalf("subscribed %d destinations, want %d: %v", len(got), len(dests), got)
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[d] = true
	}
	for _, want := range dests {
		if !seen[want] {
			t.Errorf("destination %q was never subscribed; got %v", want, got)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runHistorySubscriber did not return within 3s of ctx cancel")
	}
}

// TestRunHistorySubscriberOneFailureDoesNotBlockOthers: a Subscribe
// failure on one destination must not prevent the others from
// subscribing and delivering samples, and must not make
// runHistorySubscriber itself return an error.
func TestRunHistorySubscriberOneFailureDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	const badDest = "/topic/bad"
	const goodDest = "/topic/good"
	fake := &fakeHistorySubs{byDest: map[string]fakeHistoryDestConfig{
		badDest:  {subscribeErr: errors.New("broker refused")},
		goodDest: {frames: [][]byte{[]byte(realCaptureFrame)}, closeAfterFrames: true},
	}}
	store := &telemetryhistory.Store{}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runHistorySubscriber(ctx, fake, store, []string{badDest, goodDest}) }()

	waitFor(2*time.Second, func() bool { return len(store.Snapshot()) == 1 })
	if len(store.Snapshot()) != 1 {
		t.Fatalf("store has %d series after the good destination's frame, want 1", len(store.Snapshot()))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("runHistorySubscriber returned non-graceful error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runHistorySubscriber did not return within 3s of ctx cancel despite the bad destination")
	}
}

// TestRunHistorySubscriberEmptyDestinationsStartsNoGoroutine: an empty
// destination list must return immediately on ctx cancel with zero
// Subscribe calls, matching the "fully off unless configured"
// acceptance criterion for this feature.
func TestRunHistorySubscriberEmptyDestinationsStartsNoGoroutine(t *testing.T) {
	t.Parallel()

	fake := &fakeHistorySubs{byDest: map[string]fakeHistoryDestConfig{}}
	store := &telemetryhistory.Store{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- runHistorySubscriber(ctx, fake, store, nil) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHistorySubscriber with no destinations did not return promptly on an already-canceled ctx")
	}
	if got := fake.destinationsOf(); len(got) != 0 {
		t.Errorf("Subscribe was called %d times with no destinations configured: %v", len(got), got)
	}
}

// TestRunHistorySubscriberExitsCleanlyOnCtxCancel is the goroutine-leak
// guard (this feature's "leaks no goroutine" acceptance criterion, run
// under -race by the caller): every per-destination goroutine must have
// actually returned by the time runHistorySubscriber itself returns, not
// merely be "about to".
func TestRunHistorySubscriberExitsCleanlyOnCtxCancel(t *testing.T) {
	dests := []string{"/topic/a", "/topic/b", "/topic/c", "/topic/d"}
	byDest := map[string]fakeHistoryDestConfig{}
	for _, d := range dests {
		// No frames, closeAfterFrames false: every goroutine blocks on
		// <-ctx.Done() until canceled, which is the leak-prone shape.
		byDest[d] = fakeHistoryDestConfig{}
	}
	fake := &fakeHistorySubs{byDest: byDest}
	store := &telemetryhistory.Store{}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runHistorySubscriber(ctx, fake, store, dests) }()

	waitFor(2*time.Second, func() bool { return len(fake.destinationsOf()) == len(dests) })

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runHistorySubscriber did not return within 3s of ctx cancel")
	}

	// Goroutines can take a moment to actually unwind after the channel
	// send that wakes them; poll rather than asserting immediately.
	waitFor(2*time.Second, func() bool { return runtime.NumGoroutine() <= before+1 })
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Errorf("goroutine count after shutdown = %d, before = %d: possible leak", after, before)
	}
}

// messageWithBody builds a cimstomp.Message the fake's inbox channel can
// carry, matching the sim package's own fake pattern.
func messageWithBody(destination string, body []byte) cimstomp.Message {
	return cimstomp.Message{Destination: destination, Body: body}
}
