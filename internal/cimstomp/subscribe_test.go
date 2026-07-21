package cimstomp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"
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

// TestIsOversizedFrame_DropsAboveCapKeepsAtOrBelow pins the GAGO-023
// Leon M1 frame-size cap: a body at or under the cap is kept (not
// oversized), a body over the cap is reported oversized (and dropped by
// the caller, runSubscription).
func TestIsOversizedFrame_DropsAboveCapKeepsAtOrBelow(t *testing.T) {
	cases := []struct {
		name    string
		bodyLen int
		cap     int
		want    bool
	}{
		{"under cap", 10, 100, false},
		{"exactly at cap", 100, 100, false},
		{"one byte over cap", 101, 100, true},
		{"far over cap", defaultMaxFrameBodyBytes * 2, defaultMaxFrameBodyBytes, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isOversizedFrame("/topic/x", tc.bodyLen, tc.cap, nil)
			if got != tc.want {
				t.Errorf("isOversizedFrame(bodyLen=%d, cap=%d) = %v, want %v", tc.bodyLen, tc.cap, got, tc.want)
			}
		})
	}
}

// TestIsOversizedFrame_RateLimitsDropLogging pins the GAGO-023 follow-up
// oversized-frame log rate limit: with oversizedFrameLogEvery+5
// consecutive oversized frames sharing one counter, only the first call
// and the oversizedFrameLogEvery-th call log a line, not all of them.
// This proves the counter-with-periodic-log mechanism (mirroring
// internal/cim/sim.Pump's malformed-frame rate limit), not just that it
// compiles.
func TestIsOversizedFrame_RateLimitsDropLogging(t *testing.T) {
	var buf bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	}()

	var count uint64
	for i := 0; i < oversizedFrameLogEvery+5; i++ {
		if !isOversizedFrame("/topic/x", 200, 100, &count) {
			t.Fatalf("isOversizedFrame call %d: want true (oversized)", i)
		}
	}

	if count != oversizedFrameLogEvery+5 {
		t.Fatalf("count = %d, want %d", count, oversizedFrameLogEvery+5)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	got := 0
	for _, l := range lines {
		if strings.Contains(l, "dropping oversized subscription frame") {
			got++
		}
	}
	// Expect exactly 2 log lines: count=1 (first frame) and
	// count=oversizedFrameLogEvery. The remaining oversizedFrameLogEvery+5-2
	// frames must NOT each produce their own log line.
	if got != 2 {
		t.Errorf("oversized-frame log lines = %d, want 2 (count=1 and count=%d), got lines:\n%s", got, oversizedFrameLogEvery, buf.String())
	}
	if !strings.Contains(buf.String(), "count=1)") {
		t.Errorf("expected a log line for count=1, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), fmt.Sprintf("count=%d)", oversizedFrameLogEvery)) {
		t.Errorf("expected a log line for count=%d, got:\n%s", oversizedFrameLogEvery, buf.String())
	}
}

// TestWithMaxFrameBodyBytes_OverridesDefault verifies the SubscribeOption
// actually changes the cap subscribeOptions carries, so a caller who
// wants a smaller (or larger) cap than defaultMaxFrameBodyBytes gets it.
func TestWithMaxFrameBodyBytes_OverridesDefault(t *testing.T) {
	opts := subscribeOptions{maxFrameBodyBytes: defaultMaxFrameBodyBytes}
	WithMaxFrameBodyBytes(1024)(&opts)
	if opts.maxFrameBodyBytes != 1024 {
		t.Errorf("maxFrameBodyBytes after WithMaxFrameBodyBytes(1024) = %d, want 1024", opts.maxFrameBodyBytes)
	}
}

// TestSendOrCancel_CtxDoneWinsOverFullChannel pins the claim behind
// runSubscription's send arm (GAGO-023 Dutch M1): when the consumer is
// slow and out.msgs is already full, a cancelled ctx must win rather than
// sendOrCancel blocking forever. The 1-buffered channel is pre-filled so
// the send case can never proceed; if sendOrCancel blocked instead of
// selecting on ctx.Done, this test would hang and the suite's own timeout
// would fail it rather than deadlocking silently.
func TestSendOrCancel_CtxDoneWinsOverFullChannel(t *testing.T) {
	out := &Subscription{msgs: make(chan Message, 1)}
	out.msgs <- Message{Body: []byte("already queued")} // fill the buffer

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() {
		done <- sendOrCancel(ctx, out, Message{Body: []byte("second frame")})
	}()

	select {
	case ok := <-done:
		if ok {
			t.Errorf("sendOrCancel returned true (delivered) with a full channel and cancelled ctx, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendOrCancel blocked instead of observing ctx.Done on a full channel")
	}

	if err := out.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("out.Err() = %v, want context.Canceled", err)
	}
}

// TestSendOrCancel_DeliversWhenRoom is the companion happy path: with room
// in the channel and a live ctx, sendOrCancel delivers rather than taking
// the ctx.Done branch.
func TestSendOrCancel_DeliversWhenRoom(t *testing.T) {
	out := &Subscription{msgs: make(chan Message, 1)}

	ok := sendOrCancel(context.Background(), out, Message{Body: []byte("frame")})
	if !ok {
		t.Fatalf("sendOrCancel returned false with room in the channel and a live ctx")
	}

	got := <-out.msgs
	if string(got.Body) != "frame" {
		t.Errorf("delivered Body = %q, want %q", got.Body, "frame")
	}
	if err := out.Err(); err != nil {
		t.Errorf("out.Err() = %v, want nil on successful delivery", err)
	}
}
