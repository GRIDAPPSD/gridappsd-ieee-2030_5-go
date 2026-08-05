package gridappsdclient

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
)

// healthBus models the connection-loss failure at the boundary Supervisor sees.
// The bridge's whole go-stomp connection dies on the 15s read timeout:
// from that instant the router's reader goroutines have exited, so no
// handler is ever invoked again, and every bus operation on the dead
// connection fails. Only a fresh Connect restores it.
//
// Subscribe records every destination it is asked for, in order, so a
// test can assert that a reconnect resubscribed the SAME topics rather
// than merely reconnecting.
type healthBus struct {
	mu          sync.Mutex
	dead        bool
	connectErr  error
	handlers    map[string]fieldbus.Handler
	subscribed  []string
	unsubbed    []string
	connects    int
	disconnects int
	nextTok     fieldbus.Token
}

func newHealthBus() *healthBus {
	return &healthBus{handlers: make(map[string]fieldbus.Handler)}
}

// kill models the go-stomp read timeout tearing the connection down.
func (b *healthBus) kill() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dead = true
	b.handlers = make(map[string]fieldbus.Handler)
}

func (b *healthBus) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connects++
	if b.connectErr != nil {
		return b.connectErr
	}
	b.dead = false
	return nil
}

func (b *healthBus) Disconnect() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disconnects++
	b.handlers = make(map[string]fieldbus.Handler)
	return nil
}

func (b *healthBus) IsConnected() bool { return true }

func (b *healthBus) Subscribe(ctx context.Context, destination string, h fieldbus.Handler) (fieldbus.Token, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dead {
		return 0, errors.New("connection closed unexpectedly")
	}
	b.nextTok++
	b.handlers[destination] = h
	b.subscribed = append(b.subscribed, destination)
	return b.nextTok, nil
}

func (b *healthBus) Unsubscribe(ctx context.Context, destination string, tok fieldbus.Token) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unsubbed = append(b.unsubbed, destination)
	if b.dead {
		return errors.New("connection closed unexpectedly")
	}
	delete(b.handlers, destination)
	return nil
}

func (b *healthBus) Send(ctx context.Context, destination, contentType string, body []byte) error {
	return errors.New("healthBus: Send not used by Supervisor tests")
}

func (b *healthBus) GetResponse(ctx context.Context, destination, contentType string, body []byte) ([]byte, error) {
	return nil, errors.New("healthBus: GetResponse not used by Supervisor tests")
}

// deliver invokes the handler currently registered for destination, as
// the router would on frame arrival. A dead or unsubscribed destination
// has no handler and silently delivers nothing, which is exactly what
// the broken bridge did.
func (b *healthBus) deliver(destination string, body []byte) {
	b.mu.Lock()
	h := b.handlers[destination]
	b.mu.Unlock()
	if h == nil {
		return
	}
	h(map[string]string{"destination": destination}, body)
}

func (b *healthBus) subscribeCount(destination string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, d := range b.subscribed {
		if d == destination {
			n++
		}
	}
	return n
}

func (b *healthBus) connectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

// compile-time assertion: healthBus must satisfy fieldbus.MessageBus, so
// the tests exercise the same method set cmd/bridge hands to Supervisor.
var _ fieldbus.MessageBus = (*healthBus)(nil)

// compile-time assertion: Supervisor must satisfy sim.SubscribeClient.
var _ sim.SubscribeClient = (*Supervisor)(nil)

const (
	testOutputTopic = "/topic/goss.gridappsd.simulation.output.sim-1"
	testInputTopic  = "/topic/goss.gridappsd.simulation.input.sim-1"
	testLogTopic    = "/topic/goss.gridappsd.simulation.log.sim-1"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// captureLog redirects the standard logger for the duration of a test
// and returns a func that reads back what was written.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	buf := &syncBuffer{mu: &mu}
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf.String
}

type syncBuffer struct {
	mu  *sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestSupervisor_IdleLongerThanTheDeadlineKeepsSubscriptionAlive is the
// core regression test. A quiet simulation is entirely ordinary, so
// an idle interval that spans many liveness deadlines must not end,
// error, or degrade the subscription; and a frame that arrives after all
// that silence must still be delivered, with its body intact.
//
// The real deadline is go-stomp's 15s read timeout. It is expressed here
// as many probe intervals of complete inbound silence rather than as a
// wall-clock 15s wait, so the assertion is on the behavior (no
// inactivity-driven teardown, no spurious reconnect) rather than on the
// clock.
func TestSupervisor_IdleLongerThanTheDeadlineKeepsSubscriptionAlive(t *testing.T) {
	bus := newHealthBus()
	s := NewSupervisor(bus,
		WithProbeDestination(testLogTopic),
		WithProbeInterval(time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := s.Subscribe(ctx, testOutputTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Complete inbound silence across many liveness deadlines.
	const idleProbes = 50
	waitFor(t, "the watchdog to run many idle probes", func() bool {
		return bus.subscribeCount(testLogTopic) >= idleProbes
	})

	// The subscription must be untouched: not ended, not errored, and
	// not "recovered" from a healthy state.
	select {
	case _, ok := <-sub.Messages():
		if !ok {
			t.Fatalf("subscription ended during an idle period: Err = %v", sub.Err())
		}
		t.Fatalf("received a message that was never delivered")
	default:
	}
	if err := sub.Err(); err != nil {
		t.Fatalf("Err after idle = %v, want nil", err)
	}
	if got := bus.connectCount(); got != 0 {
		t.Errorf("Connect calls after a healthy idle period = %d, want 0 (no spurious reconnect)", got)
	}
	if got := bus.subscribeCount(testOutputTopic); got != 1 {
		t.Errorf("Subscribe calls for %s = %d, want 1 (the idle period must not resubscribe)", testOutputTopic, got)
	}

	// The property that actually matters: a frame published after the
	// idle period is still received, with the exact body.
	body := []byte(`{"simulation_id":"sim-1","message":{"timestamp":42}}`)
	bus.deliver(testOutputTopic, body)

	select {
	case msg, ok := <-sub.Messages():
		if !ok {
			t.Fatalf("Messages closed instead of delivering the post-idle frame: Err = %v", sub.Err())
		}
		if string(msg.Body) != string(body) {
			t.Errorf("Body = %q, want %q", msg.Body, body)
		}
		if msg.Destination != testOutputTopic {
			t.Errorf("Destination = %q, want %q", msg.Destination, testOutputTopic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a frame delivered after the idle period was never received: the subscription went deaf")
	}
}

// TestSupervisor_BrokerDeathReconnectsAndResubscribesEveryTopic asserts
// the other half: a genuine connection loss must produce a reconnect AND
// a resubscribe to the SAME destinations, and frames must flow again on
// every one of them afterwards. Reconnecting without resubscribing
// leaves a live socket receiving nothing, which looks healthier than the
// dead connection while being just as deaf.
func TestSupervisor_BrokerDeathReconnectsAndResubscribesEveryTopic(t *testing.T) {
	bus := newHealthBus()
	s := NewSupervisor(bus,
		WithProbeDestination(testLogTopic),
		WithProbeInterval(time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	outSub, err := s.Subscribe(ctx, testOutputTopic)
	if err != nil {
		t.Fatalf("Subscribe %s: %v", testOutputTopic, err)
	}
	inSub, err := s.Subscribe(ctx, testInputTopic)
	if err != nil {
		t.Fatalf("Subscribe %s: %v", testInputTopic, err)
	}

	// go-stomp's read timeout tears the connection down.
	bus.kill()

	waitFor(t, "the bus to be reconnected and both topics resubscribed", func() bool {
		return bus.connectCount() >= 1 &&
			bus.subscribeCount(testOutputTopic) >= 2 &&
			bus.subscribeCount(testInputTopic) >= 2
	})

	if got := bus.subscribeCount(testOutputTopic); got < 2 {
		t.Errorf("Subscribe calls for %s = %d, want at least 2 (resubscribe after reconnect)", testOutputTopic, got)
	}
	if got := bus.subscribeCount(testInputTopic); got < 2 {
		t.Errorf("Subscribe calls for %s = %d, want at least 2 (resubscribe after reconnect)", testInputTopic, got)
	}

	// Both subscriptions must be live again, on the same handles the
	// consumer already holds.
	for _, tc := range []struct {
		dest string
		sub  sim.Subscription
		body []byte
	}{
		{testOutputTopic, outSub, []byte(`{"simulation_id":"sim-1","topic":"output"}`)},
		{testInputTopic, inSub, []byte(`{"simulation_id":"sim-1","topic":"input"}`)},
	} {
		bus.deliver(tc.dest, tc.body)
		select {
		case msg, ok := <-tc.sub.Messages():
			if !ok {
				t.Fatalf("%s: Messages closed after recovery: Err = %v", tc.dest, tc.sub.Err())
			}
			if string(msg.Body) != string(tc.body) {
				t.Errorf("%s: Body = %q, want %q", tc.dest, msg.Body, tc.body)
			}
			if msg.Destination != tc.dest {
				t.Errorf("%s: Destination = %q, want %q", tc.dest, msg.Destination, tc.dest)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no frame received after reconnect: resubscribe did not restore delivery", tc.dest)
		}
	}
}

// TestSupervisor_ProbeFailureAndReconnectFailureAreLogged asserts the
// failure path is loud. A bridge that is running but deaf is worse than
// one that exits, because an operator sees a healthy process and
// concludes the simulation is sending nothing.
func TestSupervisor_ProbeFailureAndReconnectFailureAreLogged(t *testing.T) {
	logged := captureLog(t)

	bus := newHealthBus()
	bus.connectErr = errors.New("dial tcp 127.0.0.1:61613: connection refused")

	s := NewSupervisor(bus,
		WithProbeDestination(testLogTopic),
		WithProbeInterval(time.Millisecond),
		WithMaxRecoverAttempts(3),
	)
	s.sleep = func(context.Context, time.Duration) bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := s.Subscribe(ctx, testOutputTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	bus.kill()

	// Exhausting the attempts must end the subscription, not leave it
	// open and silently deaf.
	select {
	case _, ok := <-sub.Messages():
		if ok {
			t.Fatal("received a message that was never delivered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscription stayed open after the bus was declared unrecoverable: still deaf")
	}

	endErr := sub.Err()
	if endErr == nil {
		t.Fatal("Err after an unrecoverable bus = nil, want a non-nil cause")
	}
	if !errors.Is(endErr, ErrBusUnrecovered) {
		t.Errorf("Err = %v, want it to wrap ErrBusUnrecovered", endErr)
	}
	if !strings.Contains(endErr.Error(), "connection refused") {
		t.Errorf("Err = %v, want it to carry the underlying dial failure", endErr)
	}

	out := logged()
	if !strings.Contains(out, "ERROR bus liveness probe") {
		t.Errorf("log did not record the probe failure at ERROR:\n%s", out)
	}
	if got := strings.Count(out, "ERROR bus reconnect attempt"); got != 3 {
		t.Errorf("ERROR lines for failed reconnect attempts = %d, want 3 (one per attempt):\n%s", got, out)
	}
	if !strings.Contains(out, "ERROR bus recovery abandoned") {
		t.Errorf("log did not record the give-up at ERROR:\n%s", out)
	}
}

// TestSupervisor_NoProbeDestinationIsLoudlyUnsupervised asserts the
// degenerate configuration announces itself instead of quietly
// pretending to supervise.
func TestSupervisor_NoProbeDestinationIsLoudlyUnsupervised(t *testing.T) {
	logged := captureLog(t)

	bus := newHealthBus()
	s := NewSupervisor(bus, WithProbeInterval(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.Subscribe(ctx, testOutputTopic); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	waitFor(t, "the unsupervised warning", func() bool {
		return strings.Contains(logged(), "ERROR no probe destination configured")
	})
	if got := bus.subscribeCount(testLogTopic); got != 0 {
		t.Errorf("probe subscribes with no probe destination = %d, want 0", got)
	}
}

// TestSupervisor_CtxCancelClosesAndUnsubscribes keeps the shutdown
// contract Subscriber already had: cancel ends the subscription, records
// ctx.Err, closes Messages, and drops the bus subscription.
func TestSupervisor_CtxCancelClosesAndUnsubscribes(t *testing.T) {
	bus := newHealthBus()
	s := NewSupervisor(bus,
		WithProbeDestination(testLogTopic),
		WithProbeInterval(time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	sub, err := s.Subscribe(ctx, testOutputTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case _, ok := <-sub.Messages():
		if ok {
			t.Fatal("received a message that was never delivered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Messages was not closed within 3s of ctx cancel")
	}

	if err := sub.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", err)
	}

	bus.mu.Lock()
	unsubbed := append([]string(nil), bus.unsubbed...)
	bus.mu.Unlock()
	found := false
	for _, d := range unsubbed {
		if d == testOutputTopic {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Unsubscribe destinations = %v, want %s present", unsubbed, testOutputTopic)
	}
}

// TestBackoffFor pins the reconnect backoff schedule: doubling from
// recoverBackoffBase, capped at recoverBackoffMax, never zero (a zero
// backoff would turn a dead broker into a hot reconnect loop).
func TestBackoffFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 500 * time.Millisecond},
		{2, time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},
		{6, recoverBackoffMax},
		{20, recoverBackoffMax},
	} {
		if got := backoffFor(tc.attempt); got != tc.want {
			t.Errorf("backoffFor(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}
