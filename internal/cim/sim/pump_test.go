package sim

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp/cimstomptest"
)

// fakeSubscribeClient implements SubscribeClient for tests. It records the
// destination Subscribe was called with and feeds caller-supplied frames
// onto an in-memory subscription. Errors set on the fake are surfaced via
// the subscription's Err() once the channel closes.
type fakeSubscribeClient struct {
	mu           sync.Mutex
	subscribeErr error
	dest         string
	frames       [][]byte
	// closeAfterFrames closes the subscription after sending all frames;
	// otherwise the subscription stays open until ctx cancel.
	closeAfterFrames bool
	endErr           error
}

func (f *fakeSubscribeClient) Subscribe(ctx context.Context, destination string) (Subscription, error) {
	f.mu.Lock()
	f.dest = destination
	frames := f.frames
	closeAfter := f.closeAfterFrames
	endErr := f.endErr
	subErr := f.subscribeErr
	f.mu.Unlock()
	if subErr != nil {
		return nil, subErr
	}
	sub, msgsIn := cimstomptest.NewSubscription()
	go func() {
		defer close(msgsIn)
		// Mirror cimstomp.runSubscription's contract: setErr happens
		// before the channel closes. cimstomptest.SetErr writes
		// synchronously so the consumer sees the err the moment it
		// observes close.
		for _, body := range frames {
			select {
			case <-ctx.Done():
				cimstomptest.SetErr(sub, ctx.Err())
				return
			case msgsIn <- cimstomp.Message{Destination: destination, Body: body}:
			}
		}
		if closeAfter {
			if endErr != nil {
				cimstomptest.SetErr(sub, endErr)
			}
			return
		}
		<-ctx.Done()
		cimstomptest.SetErr(sub, ctx.Err())
	}()
	return sub, nil
}

// TestPump_RunDispatchesFrames verifies the Pump decodes each measurement
// frame body and invokes the handler with the typed struct. The Pump must
// stop on ctx cancel without leaking the subscription goroutine.
func TestPump_RunDispatchesFrames(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"simulation_id":"42","message":{"timestamp":1,"measurements":{"_a":{"measurement_mrid":"_a","magnitude":1.5}}}}`),
		[]byte(`{"simulation_id":"42","message":{"timestamp":2,"measurements":{"_b":{"measurement_mrid":"_b","value":7}}}}`),
	}
	fake := &fakeSubscribeClient{frames: frames, closeAfterFrames: true}

	pump := NewPump(fake, "42")

	var got []MeasurementFrame
	var mu sync.Mutex
	handler := func(m MeasurementFrame) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pump.Run(ctx, handler); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fake.dest != OutputTopic("42") {
		t.Errorf("Subscribe destination = %q, want %q", fake.dest, OutputTopic("42"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("handler invocations = %d, want 2", len(got))
	}
	if got[0].Message.Timestamp != 1 || got[1].Message.Timestamp != 2 {
		t.Errorf("frames decoded out of order or wrong: %+v", got)
	}
}

// TestPump_RunHandlerErrorContinuesLoop verifies that a handler error logs
// but does not break the loop; the Pump keeps dispatching frames after a
// handler returns an error.
func TestPump_RunHandlerErrorContinuesLoop(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"simulation_id":"x","message":{"timestamp":1,"measurements":{}}}`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":2,"measurements":{}}}`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":3,"measurements":{}}}`),
	}
	fake := &fakeSubscribeClient{frames: frames, closeAfterFrames: true}
	pump := NewPump(fake, "x")

	var seen atomic.Int32
	handler := func(m MeasurementFrame) error {
		seen.Add(1)
		if m.Message.Timestamp == 2 {
			return errors.New("handler said no")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pump.Run(ctx, handler); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := seen.Load(); got != 3 {
		t.Errorf("handler invocations = %d, want 3 (handler errors must not break the loop)", got)
	}
}

// TestPump_RunCtxCancel verifies that ctx cancel terminates Run cleanly
// and returns ctx.Err.
func TestPump_RunCtxCancel(t *testing.T) {
	fake := &fakeSubscribeClient{frames: nil, closeAfterFrames: false}
	pump := NewPump(fake, "x")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- pump.Run(ctx, func(m MeasurementFrame) error { return nil })
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestPump_RunSubscribeError verifies that a Subscribe failure surfaces
// from Run as a wrapped error, without invoking the handler.
func TestPump_RunSubscribeError(t *testing.T) {
	wantErr := errors.New("broker said no")
	fake := &fakeSubscribeClient{subscribeErr: wantErr}
	pump := NewPump(fake, "x")

	called := atomic.Int32{}
	handler := func(m MeasurementFrame) error {
		called.Add(1)
		return nil
	}
	err := pump.Run(context.Background(), handler)
	if err == nil {
		t.Fatalf("Run: expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Run error = %v, want wrapping %v", err, wantErr)
	}
	if got := called.Load(); got != 0 {
		t.Errorf("handler called %d times, want 0 on Subscribe failure", got)
	}
}

// TestPump_RunOnHandlerErrorStopsWhenPolicyReturnsFalse verifies that
// WithOnHandlerError lets a caller stop Run on a handler error, in
// contrast to the default log-and-continue behavior covered by
// TestPump_RunHandlerErrorContinuesLoop (Dutch M4).
func TestPump_RunOnHandlerErrorStopsWhenPolicyReturnsFalse(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"simulation_id":"x","message":{"timestamp":1,"measurements":{}}}`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":2,"measurements":{}}}`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":3,"measurements":{}}}`),
	}
	fake := &fakeSubscribeClient{frames: frames, closeAfterFrames: true}

	wantErr := errors.New("handler said stop")
	pump := NewPump(fake, "x", WithOnHandlerError(func(err error) bool {
		return false // stop on the first handler error
	}))

	var seen atomic.Int32
	handler := func(m MeasurementFrame) error {
		seen.Add(1)
		if m.Message.Timestamp == 2 {
			return wantErr
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := pump.Run(ctx, handler)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want wrapping %v", err, wantErr)
	}
	if got := seen.Load(); got != 2 {
		t.Errorf("handler invocations = %d, want 2 (Run must stop after the OnHandlerError policy returns false, before dispatching frame 3)", got)
	}
}

// TestPump_RunOnHandlerErrorContinuesWhenPolicyReturnsTrue verifies the
// other half of WithOnHandlerError: a policy that returns true keeps Run
// going past a handler error, same as the nil-policy default.
func TestPump_RunOnHandlerErrorContinuesWhenPolicyReturnsTrue(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"simulation_id":"x","message":{"timestamp":1,"measurements":{}}}`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":2,"measurements":{}}}`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":3,"measurements":{}}}`),
	}
	fake := &fakeSubscribeClient{frames: frames, closeAfterFrames: true}

	var policyCalls atomic.Int32
	pump := NewPump(fake, "x", WithOnHandlerError(func(err error) bool {
		policyCalls.Add(1)
		return true
	}))

	var seen atomic.Int32
	handler := func(m MeasurementFrame) error {
		seen.Add(1)
		if m.Message.Timestamp == 2 {
			return errors.New("handler said no")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pump.Run(ctx, handler); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := seen.Load(); got != 3 {
		t.Errorf("handler invocations = %d, want 3", got)
	}
	if got := policyCalls.Load(); got != 1 {
		t.Errorf("OnHandlerError calls = %d, want 1", got)
	}
}

// TestPump_RunCtxCancelDuringDispatchStopsPromptly is a regression test
// for the Run-level ctx.Done() select arm (Dutch M3).
// The fake never closes its subscription on its own (closeAfterFrames:
// false with an empty frame list means it just blocks on ctx.Done()), so
// this proves Run's own ctx-aware select, not the subscription's closing,
// is what unblocks Run.
func TestPump_RunCtxCancelDuringDispatchStopsPromptly(t *testing.T) {
	fake := &fakeSubscribeClient{frames: nil, closeAfterFrames: false}
	pump := NewPump(fake, "x")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- pump.Run(ctx, func(m MeasurementFrame) error { return nil })
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancel")
	}
}

// TestPump_RunRateLimitsMalformedFrameLogging pins the Leon L1
// rate limit: with malformedFrameLogEvery+5 consecutive malformed frames,
// only the first frame and the malformedFrameLogEvery-th frame log a
// line, not all of them. This proves the counter-with-periodic-log
// mechanism, not just that it compiles.
func TestPump_RunRateLimitsMalformedFrameLogging(t *testing.T) {
	frames := make([][]byte, malformedFrameLogEvery+5)
	for i := range frames {
		frames[i] = []byte(`not-json`)
	}
	fake := &fakeSubscribeClient{frames: frames, closeAfterFrames: true}
	pump := NewPump(fake, "x")

	var buf bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pump.Run(ctx, func(m MeasurementFrame) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	got := 0
	for _, l := range lines {
		if strings.Contains(l, "skip malformed frame") {
			got++
		}
	}
	// Expect exactly 2 log lines: count=1 (first frame) and
	// count=malformedFrameLogEvery. The remaining malformedFrameLogEvery+5-2
	// frames must NOT each produce their own log line.
	if got != 2 {
		t.Errorf("malformed-frame log lines = %d, want 2 (count=1 and count=%d), got lines:\n%s", got, malformedFrameLogEvery, buf.String())
	}
	if !strings.Contains(buf.String(), "count=1") {
		t.Errorf("expected a log line for count=1, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "count=100") {
		t.Errorf("expected a log line for count=%d, got:\n%s", malformedFrameLogEvery, buf.String())
	}
}

// TestPump_RunSkipsMalformedFrame verifies that an undecodable frame is
// dropped (logged) but the loop continues with subsequent frames.
func TestPump_RunSkipsMalformedFrame(t *testing.T) {
	frames := [][]byte{
		[]byte(`not-json`),
		[]byte(`{"simulation_id":"x","message":{"timestamp":99,"measurements":{}}}`),
	}
	fake := &fakeSubscribeClient{frames: frames, closeAfterFrames: true}
	pump := NewPump(fake, "x")

	var got []MeasurementFrame
	var mu sync.Mutex
	handler := func(m MeasurementFrame) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pump.Run(ctx, handler); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Message.Timestamp != 99 {
		t.Errorf("expected one decoded frame with timestamp 99, got %+v", got)
	}
}
