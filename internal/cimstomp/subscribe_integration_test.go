//go:build integration

// Integration tests for Client.Subscribe. Exercises the full STOMP
// subscribe path against a live ActiveMQ broker. Round-trip uses a
// Publisher to send into the same topic the Client is subscribed to.
//
// Run with:
//
//	docker compose up -d
//	go test -tags=integration -race ./internal/cimstomp/

package cimstomp

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"
)

// sendRaw publishes a raw body to dest from a fresh STOMP connection.
// Callable from goroutines: errors are returned, not delivered through
// (*testing.T).Fatalf, so go vet is satisfied. The body is sent
// byte-exact (we do not use Publisher.Publish because that one builds a
// structured JSON envelope; this round-trip test wants exact wire
// content).
func sendRaw(dest string, body []byte) error {
	conn, err := stomp.Dial("tcp", testBrokerAddr,
		stomp.ConnOpt.Login(testUser, testPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Disconnect()
	if err := conn.Send(dest, "application/json", body); err != nil {
		return fmt.Errorf("send to %s: %w", dest, err)
	}
	return nil
}

func TestIntegration_SubscribeReceivesFrames(t *testing.T) {
	requireBroker(t)

	// Stand up the token responder so Subscribe-caller's Client.Connect
	// completes successfully.
	fs := startFakeServer(t, "tok-sub", "/queue/never-replied", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	dest := fmt.Sprintf("/topic/test.cimstomp.subscribe.%d", time.Now().UnixNano())
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := c.Subscribe(subCtx, dest)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Give the subscription a moment to register on the broker before we
	// start publishing; without it the first frame can be dropped on a
	// non-durable topic.
	time.Sleep(200 * time.Millisecond)

	bodies := []string{
		`{"simulation_id":"X","message":{"timestamp":1,"measurements":{}}}`,
		`{"simulation_id":"X","message":{"timestamp":2,"measurements":{}}}`,
		`{"simulation_id":"X","message":{"timestamp":3,"measurements":{}}}`,
	}
	go func() {
		for _, b := range bodies {
			if err := sendRaw(dest, []byte(b)); err != nil {
				t.Logf("sendRaw: %v", err)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	got := make([]string, 0, len(bodies))
	timeout := time.After(5 * time.Second)
	for len(got) < len(bodies) {
		select {
		case msg, ok := <-sub.Messages():
			if !ok {
				t.Fatalf("subscription closed before all frames received; got %d/%d", len(got), len(bodies))
			}
			got = append(got, string(msg.Body))
		case <-timeout:
			t.Fatalf("timeout waiting for frames; got %d/%d", len(got), len(bodies))
		}
	}

	for i, b := range bodies {
		if got[i] != b {
			t.Errorf("frame[%d] = %q, want %q", i, got[i], b)
		}
	}
}

func TestIntegration_SubscribeCtxCancelTearsDown(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-cancel", "/queue/never-replied", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	dest := fmt.Sprintf("/topic/test.cimstomp.cancel.%d", time.Now().UnixNano())

	subCtx, subCancel := context.WithCancel(context.Background())
	sub, err := c.Subscribe(subCtx, dest)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	subCancel()

	// Messages channel must close shortly after ctx cancel; Err must
	// reflect ctx.Canceled (not a broker error).
	select {
	case _, ok := <-sub.Messages():
		if ok {
			t.Fatalf("Messages channel delivered a frame after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Messages channel did not close within 2s of ctx cancel")
	}
	// After channel close, drain remaining (none) and check Err.
	for range sub.Messages() {
	}
	if err := sub.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("sub.Err() = %v, want context.Canceled", err)
	}
}

func TestIntegration_SubscribeMultipleConcurrent(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-multi", "/queue/never-replied", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	const n = 3
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()

	subs := make([]*Subscription, n)
	dests := make([]string, n)
	for i := 0; i < n; i++ {
		dests[i] = fmt.Sprintf("/topic/test.cimstomp.multi.%d.%d", time.Now().UnixNano(), i)
		s, err := c.Subscribe(subCtx, dests[i])
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		subs[i] = s
	}

	// Allow registrations to settle.
	time.Sleep(200 * time.Millisecond)

	// Publish to each topic.
	for i, d := range dests {
		i, d := i, d
		body := fmt.Sprintf(`{"id":%d}`, i)
		go func() {
			if err := sendRaw(d, []byte(body)); err != nil {
				t.Logf("sendRaw[%d]: %v", i, err)
			}
		}()
	}

	// Each subscription must receive its own frame.
	var wg sync.WaitGroup
	wg.Add(n)
	got := make([]string, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			select {
			case msg := <-subs[i].Messages():
				got[i] = string(msg.Body)
			case <-time.After(5 * time.Second):
				t.Errorf("sub[%d]: timeout waiting for frame", i)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		want := fmt.Sprintf(`{"id":%d}`, i)
		if got[i] != want {
			t.Errorf("sub[%d] body = %q, want %q", i, got[i], want)
		}
	}
}

// TestIntegration_SubscribeNoGoroutineLeak asserts that after Subscribe ->
// ctx cancel -> drain the goroutine count returns to baseline. A leak
// here would mean the listener goroutine missed the ctx-cancel teardown
// path. Run with -race to also catch concurrent access on the channels.
func TestIntegration_SubscribeNoGoroutineLeak(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-leak", "/queue/never-replied", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	// Settle goroutine count after Connect.
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()

	const iters = 8
	for i := 0; i < iters; i++ {
		dest := fmt.Sprintf("/topic/test.cimstomp.leak.%d.%d", time.Now().UnixNano(), i)
		subCtx, subCancel := context.WithCancel(context.Background())
		sub, err := c.Subscribe(subCtx, dest)
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		subCancel()
		// Drain so the listener goroutine returns.
		for range sub.Messages() {
		}
	}

	// Allow scheduler a moment to retire goroutines.
	time.Sleep(200 * time.Millisecond)
	end := runtime.NumGoroutine()
	// Some slack for runtime housekeeping; a real leak would scale with
	// iters and we run 8 iterations.
	if end > base+2 {
		t.Errorf("goroutine count grew from %d to %d after %d Subscribe/cancel cycles", base, end, iters)
	}
}

// TestIntegration_SubscribeDropsOversizedFrame proves the GAGO-023
// Leon M1 cap end to end over a live broker: a frame over the configured
// cap is dropped (never delivered on Messages), while a frame under the
// cap on the same subscription is delivered normally afterward. This
// closes the gap the unit test TestIsOversizedFrame_DropsAboveCapKeepsAtOrBelow
// leaves open: that the decision function is wired into the real
// runSubscription loop rather than only unit-tested in isolation.
func TestIntegration_SubscribeDropsOversizedFrame(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-oversize", "/queue/never-replied", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	dest := fmt.Sprintf("/topic/test.cimstomp.oversize.%d", time.Now().UnixNano())
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := c.Subscribe(subCtx, dest, WithMaxFrameBodyBytes(16))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	oversized := []byte(`{"this body is well over sixteen bytes"}`)
	small := []byte(`ok`)

	if err := sendRaw(dest, oversized); err != nil {
		t.Fatalf("sendRaw oversized: %v", err)
	}
	if err := sendRaw(dest, small); err != nil {
		t.Fatalf("sendRaw small: %v", err)
	}

	select {
	case msg, ok := <-sub.Messages():
		if !ok {
			t.Fatalf("subscription closed before the small frame arrived")
		}
		if string(msg.Body) != string(small) {
			t.Errorf("delivered frame body = %q, want %q (oversized frame should have been dropped, not delivered)", msg.Body, small)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the small frame; oversized frame may have wedged the subscription")
	}
}

// TestIntegration_SubscribeAfterCloseFailsCleanly ensures Subscribe on a
// closed Client surfaces ErrNotConnected (not a panic).
func TestIntegration_SubscribeAfterCloseFailsCleanly(t *testing.T) {
	requireBroker(t)
	fs := startFakeServer(t, "tok-after", "/queue/never-replied", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := c.Subscribe(context.Background(), "/topic/test.x")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Subscribe after Close: got err = %v, want ErrNotConnected", err)
	}
}
