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

// fakeStompHeader implements the small stompHeader interface for tests
// of selectHeaders without requiring a real *frame.Header.
type fakeStompHeader struct {
	values map[string]string
}

func (f *fakeStompHeader) Get(key string) string { return f.values[key] }

func TestSelectHeaders_PicksOnlyKnownHeaders(t *testing.T) {
	h := &fakeStompHeader{values: map[string]string{
		"destination":          "/topic/x",
		"content-type":         "application/json",
		"reply-to":             "/temp-queue/r.1",
		"correlation-id":       "abc",
		"message-id":           "ID:0",
		"subscription":         "1",
		gossHasSubjectHeader:   "True",
		gossSubjectHeader:      "tok",
		"some-unrelated-thing": "drop me",
	}}
	got := selectHeaders(h)
	if got["destination"] != "/topic/x" {
		t.Errorf("destination = %q", got["destination"])
	}
	if got["content-type"] != "application/json" {
		t.Errorf("content-type = %q", got["content-type"])
	}
	if got[gossHasSubjectHeader] != "True" {
		t.Errorf("GOSS_HAS_SUBJECT = %q", got[gossHasSubjectHeader])
	}
	// GOSS_SUBJECT carries the auth token on outbound SENDs and must
	// NEVER be surfaced on inbound frames, even when the broker echoes
	// it back. This guards against handler code logging the token.
	if v, present := got[gossSubjectHeader]; present {
		t.Errorf("GOSS_SUBJECT was not filtered out of inbound headers: got %q", v)
	}
	if _, present := got["some-unrelated-thing"]; present {
		t.Errorf("unrelated header was not filtered out")
	}
}

func TestSelectHeaders_NilReturnsNil(t *testing.T) {
	if got := selectHeaders(nil); got != nil {
		t.Errorf("selectHeaders(nil) = %v, want nil", got)
	}
}

func TestSelectHeaders_EmptyReturnsNil(t *testing.T) {
	if got := selectHeaders(&fakeStompHeader{values: map[string]string{}}); got != nil {
		t.Errorf("selectHeaders on empty header returned %v, want nil", got)
	}
}

// TestNewTestSubscription_HelperDeliversFramesAndErr exercises the
// test-only Subscription seam so other packages (cim/sim) can build
// fake subscriptions in their own unit tests via cimstomptest.
func TestNewTestSubscription_HelperDeliversFramesAndErr(t *testing.T) {
	sub, msgsIn := NewTestSubscription()

	go func() {
		msgsIn <- Message{Destination: "/topic/x", Body: []byte("one")}
		SetTestErr(sub, errors.New("end of stream"))
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
