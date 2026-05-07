package cimstomp

import (
	"context"
	"errors"
	"testing"
)

// Unit tests for Client.Subscribe and the Subscription type that do not
// require a live broker. Broker round-trips are exercised in
// subscribe_integration_test.go.

func TestSubscribe_NotConnected(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1", User: "u", Password: "p"})
	_, err := c.Subscribe(context.Background(), "/topic/foo")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Subscribe before Connect: got err = %v, want ErrNotConnected", err)
	}
}

func TestSubscribe_ContextAlreadyCancelled(t *testing.T) {
	// A cancelled context must short-circuit Subscribe without touching the
	// wire, surfacing ctx.Err().
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1"})
	c.markConnectedForTest()
	defer c.unmarkConnectedForTest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Subscribe(ctx, "/topic/foo")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe with cancelled ctx: got err = %v, want context.Canceled", err)
	}
}

func TestSubscribe_NilConnReturnsErrNotConnected(t *testing.T) {
	// markConnectedForTest flips the flag without standing up a real conn;
	// the in-Subscribe nil-conn guard must short-circuit. Mirrors the
	// equivalent guard in Request (Dutch H2 / item C).
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1"})
	c.markConnectedForTest()
	defer c.unmarkConnectedForTest()

	_, err := c.Subscribe(context.Background(), "/topic/foo")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Subscribe with nil c.conn: got err = %v, want ErrNotConnected", err)
	}
}

// TestNewSubscriptionForTest_HelperDeliversFramesAndErr exercises the
// test-only Subscription constructor so other packages (cim/sim) can
// build fake subscriptions in their own unit tests.
func TestNewSubscriptionForTest_HelperDeliversFramesAndErr(t *testing.T) {
	sub, msgsIn, errIn := NewSubscriptionForTest()

	go func() {
		msgsIn <- Message{Destination: "/topic/x", Body: []byte("one")}
		errIn <- errors.New("end of stream")
		close(msgsIn)
	}()

	got := <-sub.Messages()
	if string(got.Body) != "one" {
		t.Errorf("Body = %q, want %q", got.Body, "one")
	}
	// Drain the rest until the channel closes.
	for range sub.Messages() {
	}
	if err := sub.Err(); err == nil || err.Error() != "end of stream" {
		t.Errorf("Err() = %v, want end of stream", err)
	}
}
