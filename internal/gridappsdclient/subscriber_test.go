package gridappsdclient

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// unsubscribeCall records one Unsubscribe invocation on fakeSubscribeBus.
type unsubscribeCall struct {
	ctx         context.Context
	destination string
	tok         fieldbus.Token
}

// fakeSubscribeBus implements fieldbus.MessageBus for subscriber tests.
// Subscribe records the destination and stashes the handler so the test
// can invoke it directly to simulate the router delivering messages.
// Unsubscribe calls are recorded so tests can assert exactly-once /
// idempotent-under-double-cancel behavior.
type fakeSubscribeBus struct {
	mu               sync.Mutex
	subscribeErr     error
	tokenToReturn    fieldbus.Token
	gotDestination   string
	handler          fieldbus.Handler
	unsubscribeCalls []unsubscribeCall
}

func (f *fakeSubscribeBus) Connect(ctx context.Context) error { return nil }
func (f *fakeSubscribeBus) Disconnect() error                 { return nil }
func (f *fakeSubscribeBus) IsConnected() bool                 { return true }

func (f *fakeSubscribeBus) Subscribe(ctx context.Context, destination string, h fieldbus.Handler) (fieldbus.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribeErr != nil {
		return 0, f.subscribeErr
	}
	f.gotDestination = destination
	f.handler = h
	return f.tokenToReturn, nil
}

func (f *fakeSubscribeBus) Unsubscribe(ctx context.Context, destination string, tok fieldbus.Token) error {
	f.mu.Lock()
	f.unsubscribeCalls = append(f.unsubscribeCalls, unsubscribeCall{ctx: ctx, destination: destination, tok: tok})
	f.mu.Unlock()
	return nil
}

func (f *fakeSubscribeBus) Send(ctx context.Context, destination, contentType string, body []byte) error {
	return errors.New("fakeSubscribeBus: Send not used by Subscriber tests")
}

func (f *fakeSubscribeBus) GetResponse(ctx context.Context, destination, contentType string, body []byte) ([]byte, error) {
	return nil, errors.New("fakeSubscribeBus: GetResponse not used by Subscriber tests")
}

// deliver invokes the handler captured by Subscribe as the router would
// on message arrival. Panics if called before Subscribe.
func (f *fakeSubscribeBus) deliver(headers map[string]string, body []byte) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	h(headers, body)
}

func (f *fakeSubscribeBus) unsubscribeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.unsubscribeCalls)
}

func (f *fakeSubscribeBus) lastUnsubscribeCall() unsubscribeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsubscribeCalls[len(f.unsubscribeCalls)-1]
}

// compile-time assertion: fakeSubscribeBus must satisfy fieldbus.MessageBus.
var _ fieldbus.MessageBus = (*fakeSubscribeBus)(nil)

// compile-time assertion: Subscriber must satisfy sim.SubscribeClient.
var _ sim.SubscribeClient = (*Subscriber)(nil)

func TestSubscriber_SubscribeUsesDestination(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{tokenToReturn: 7}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const dest = "/topic/goss.gridappsd.simulation.output.42"
	if _, err := s.Subscribe(ctx, dest); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if bus.gotDestination != dest {
		t.Errorf("bus.Subscribe destination = %q, want %q", bus.gotDestination, dest)
	}
}

func TestSubscriber_SubscribeErrorWrapped(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("broker said no")
	bus := &fakeSubscribeBus{subscribeErr: wantErr}
	s := NewSubscriber(bus)

	sub, err := s.Subscribe(context.Background(), "dest")
	if sub != nil {
		t.Errorf("Subscribe returned non-nil Subscription on error: %v", sub)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Subscribe error = %v, want wrapping %v", err, wantErr)
	}
}

func TestSubscriber_MessagesRoundTripByteIdentical(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const dest = "/topic/goss.gridappsd.simulation.output.42"
	sub, err := s.Subscribe(ctx, dest)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	wantHeaders := map[string]string{"destination": dest}
	wantBody := []byte(`{"simulation_id":"42","message":{"timestamp":1}}`)
	bus.deliver(wantHeaders, wantBody)

	select {
	case got := <-sub.Messages():
		if got.Destination != dest {
			t.Errorf("Destination = %q, want %q", got.Destination, dest)
		}
		if got.Headers["destination"] != dest {
			t.Errorf("Headers[destination] = %q, want %q", got.Headers["destination"], dest)
		}
		if string(got.Body) != string(wantBody) {
			t.Errorf("Body = %q, want %q", got.Body, wantBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivered message")
	}
}

func TestSubscriber_BodyCopyIsolation(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := s.Subscribe(ctx, "dest")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	body := []byte(`{"a":1}`)
	bus.deliver(nil, body)

	select {
	case got := <-sub.Messages():
		// Mutate the handler's original slice after delivery. The
		// Subscriber must have copied it internally
		// (append([]byte(nil), body...)); the received message must be
		// unaffected by this later mutation.
		body[0] = 'X'
		if string(got.Body) == string(body) {
			t.Errorf("received Body aliases the handler's original slice; want an independent copy")
		}
		if string(got.Body) != `{"a":1}` {
			t.Errorf("received Body = %q, want unmutated %q", got.Body, `{"a":1}`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivered message")
	}
}

// TestSubscriber_SlowConsumerBlocksNoDropsNoReorder verifies backpressure:
// a producer delivering messages faster than the consumer reads them
// blocks (does not drop), and the consumer eventually sees every message
// in the order it was delivered.
func TestSubscriber_SlowConsumerBlocksNoDropsNoReorder(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := s.Subscribe(ctx, "dest")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const n = 30 // well past the 16-slot msgs buffer plus 1 in-flight relay slot
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := 0; i < n; i++ {
			bus.deliver(nil, []byte{byte(i)})
		}
	}()

	// Give the producer a head start to fill the buffer and block, without
	// any consumer reading yet.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-producerDone:
		t.Fatal("producer finished delivering all messages before the consumer read any; backpressure did not block it")
	default:
		// expected: producer is blocked
	}

	var got []byte
	for i := 0; i < n; i++ {
		select {
		case msg := <-sub.Messages():
			got = append(got, msg.Body[0])
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d; got %d so far", i, len(got))
		}
	}

	select {
	case <-producerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not finish after consumer drained all messages")
	}

	if len(got) != n {
		t.Fatalf("received %d messages, want %d (no drops)", len(got), n)
	}
	for i, b := range got {
		if int(b) != i {
			t.Fatalf("message %d = %d, want %d (no reordering)", i, b, i)
		}
	}
}

func TestSubscriber_CtxCancelClosesMessagesAndSetsErr(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	sub, err := s.Subscribe(ctx, "dest")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case _, ok := <-sub.Messages():
		if ok {
			t.Fatal("Messages() delivered a value on ctx cancel with no prior deliveries; want a closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Messages() to close after ctx cancel")
	}

	if !errors.Is(sub.Err(), context.Canceled) {
		t.Errorf("Err() = %v, want context.Canceled", sub.Err())
	}
}

func TestSubscriber_CtxCancelCallsUnsubscribeExactlyOnceUnderDoubleCancel(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{tokenToReturn: 99}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	const dest = "dest"
	sub, err := s.Subscribe(ctx, dest)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()
	cancel() // context.CancelFunc is idempotent; calling it again must not double-Unsubscribe

	// Wait for the relay goroutine to observe ctx.Done and finish its
	// shutdown sequence (Unsubscribe, setErr, close) before asserting.
	select {
	case <-sub.Messages():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	// Give any (incorrect) second Unsubscribe call a moment to land before
	// we count; the shutdown path itself is synchronous ahead of the
	// channel close, so this is a safety margin, not the mechanism.
	time.Sleep(50 * time.Millisecond)

	if got := bus.unsubscribeCallCount(); got != 1 {
		t.Fatalf("Unsubscribe called %d times, want exactly 1", got)
	}
	call := bus.lastUnsubscribeCall()
	if call.destination != dest {
		t.Errorf("Unsubscribe destination = %q, want %q", call.destination, dest)
	}
	if call.tok != fieldbus.Token(99) {
		t.Errorf("Unsubscribe token = %v, want %v", call.tok, fieldbus.Token(99))
	}
}

// TestSubscriber_HandlerDeliveryAfterCtxCancelDoesNotPanicOrBlock covers
// the in-flight-delivery race: the router may still invoke the handler
// after ctx is done (readLoop exits asynchronously). The handler's send
// to raw is ctx-guarded, so a late delivery must return promptly without
// panicking (raw is never closed, so a panic here would mean a send on a
// closed channel elsewhere, which must not happen) and without blocking
// forever (no relay goroutine is left running to drain raw).
func TestSubscriber_HandlerDeliveryAfterCtxCancelDoesNotPanicOrBlock(t *testing.T) {
	t.Parallel()

	bus := &fakeSubscribeBus{}
	s := NewSubscriber(bus)

	ctx, cancel := context.WithCancel(context.Background())
	sub, err := s.Subscribe(ctx, "dest")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()
	// Drain the close so we know the relay goroutine has exited.
	<-sub.Messages()

	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.deliver(nil, []byte("late"))
	}()

	select {
	case <-done:
		// expected: the handler's ctx-guarded select returns promptly.
	case <-time.After(2 * time.Second):
		t.Fatal("late handler delivery blocked forever after ctx cancel and relay exit")
	}
}

// compile-time sanity: cimstomp.Message is the wire DTO Subscription
// delivers; referenced here so a signature drift on either type fails
// this test file to build, not silently.
var _ = cimstomp.Message{}
