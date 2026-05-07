package sim

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/cimstomp"
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

func (f *fakeSubscribeClient) Subscribe(ctx context.Context, destination string) (*cimstomp.Subscription, error) {
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
	sub, msgsIn := cimstomp.NewSubscriptionForTest()
	go func() {
		defer close(msgsIn)
		// Mirror cimstomp.runSubscription's contract: setErr happens
		// before the channel closes. SetErrForTest writes synchronously
		// so the consumer sees the err the moment it observes close.
		for _, body := range frames {
			select {
			case <-ctx.Done():
				sub.SetErrForTest(ctx.Err())
				return
			case msgsIn <- cimstomp.Message{Destination: destination, Body: body}:
			}
		}
		if closeAfter {
			if endErr != nil {
				sub.SetErrForTest(endErr)
			}
			return
		}
		<-ctx.Done()
		sub.SetErrForTest(ctx.Err())
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
