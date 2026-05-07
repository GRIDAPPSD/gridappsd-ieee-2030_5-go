package cimstomp

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-stomp/stomp/v3"
)

// Message is a single frame received on a Subscription. It is a thin
// snapshot of the underlying go-stomp message with only the fields
// callers in this codebase need: Destination, a small selected-headers
// map, and the Body bytes. The body is copied; the underlying frame
// buffer can be reused by the broker.
//
// Headers carries the subset of STOMP headers the bridge cares about:
// reply-to, correlation-id, content-type, and any GOSS_* headers. We do
// not expose the raw *frame.Header to keep the Subscription API a
// stable boundary across go-stomp upgrades.
type Message struct {
	Destination string
	Headers     map[string]string
	Body        []byte
}

// Subscription is the handle for an active Subscribe. The caller drives
// it via the Messages channel (closed when the subscription ends) and
// reads the cause via Err once the channel is closed. Unsubscribe is
// triggered by canceling the context passed to Subscribe; on broker-side
// teardown the channel also closes and Err records the broker reason.
type Subscription struct {
	msgs chan Message

	errMu sync.Mutex
	err   error
}

// Messages returns the channel of received frames. The channel is closed
// when the subscription ends (ctx cancel, broker close, or Client close).
// After the channel is closed, call Err to retrieve the cause.
func (s *Subscription) Messages() <-chan Message { return s.msgs }

// Err returns the cause that ended the subscription. Stable to call
// after Messages is drained. Returns nil if the subscription ended
// without a recorded error (rare; usually ctx cancel sets context.Canceled
// and broker close sets a wrapped broker error).
func (s *Subscription) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// setErr records the cause that ended the subscription. Safe to call
// from the listener goroutine; later callers of Err see the value.
func (s *Subscription) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// NewSubscriptionForTest is a test-only constructor that returns a live
// *Subscription and the caller-driven inbox channel. Other packages in
// this repo (notably internal/cim/sim) use it to build fake
// subscriptions without dialing a broker.
//
// The returned msgs channel is what the Subscription.Messages caller
// will read. The test goroutine should send frames on msgs, optionally
// call (*Subscription).SetErrForTest to record an end-cause, then close
// msgs to end the subscription.
//
// Ordering contract: SetErrForTest writes synchronously to the
// Subscription's Err state. A test that calls SetErrForTest BEFORE
// closing msgs guarantees the consumer sees the err the moment it
// observes the channel close. Tests that close msgs without calling
// SetErrForTest leave Err() returning nil.
//
// Despite the name, this is not behind a build tag: it is exported for
// use by other packages' _test.go files, which means it must be in the
// regular build. The cost is one extra exported symbol pair; the
// alternative (duplicating the channel plumbing in every test) was
// worse.
func NewSubscriptionForTest() (*Subscription, chan<- Message) {
	msgs := make(chan Message, 8)
	s := &Subscription{msgs: msgs}
	return s, msgs
}

// SetErrForTest is the test-only side of the NewSubscriptionForTest
// pair. Tests use it to record the cause that Subscription.Err will
// return after the messages channel is closed.
func (s *Subscription) SetErrForTest(err error) { s.setErr(err) }

// Subscribe begins a STOMP subscription on destination and returns a
// Subscription whose Messages channel receives frames until ctx is
// canceled, the broker tears down the subscription, or the Client is
// Closed. Concurrent subscriptions are supported; Subscribe does not
// serialize against Request, so a long-lived subscription does not block
// request/reply.
//
// The destination must be the full STOMP form (typically "/topic/...";
// for simulation streams use sim.OutputTopic / sim.InputTopic /
// sim.LogTopic). Subscribe does NOT prepend "/queue/"; that path is
// reserved for Request which targets bare GridAPPS-D request queues.
//
// Errors:
//   - ErrNotConnected if called before Connect or after Close.
//   - context.Canceled if ctx is already done.
//   - wrapped broker errors otherwise.
//
// Once Subscribe returns successfully the listener goroutine owns the
// underlying *stomp.Subscription. Cancelling ctx unsubscribes from the
// broker, drains in-flight frames, closes Messages, and sets Err. The
// goroutine exits whether Subscribe is canceled, the broker closes the
// subscription channel, or Client.Close races against either.
func (c *Client) Subscribe(ctx context.Context, destination string) (*Subscription, error) {
	if !c.connected.Load() {
		return nil, ErrNotConnected
	}
	if err := ctx.Err(); err != nil {
		return nil, mapCtxErr(err)
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil, ErrNotConnected
	}

	stompSub, err := conn.Subscribe(destination, stomp.AckAuto)
	if err != nil {
		return nil, fmt.Errorf("cimstomp.Client: subscribe %s: %w", destination, err)
	}

	out := &Subscription{msgs: make(chan Message, 16)}

	go runSubscription(ctx, stompSub, out)
	return out, nil
}

// runSubscription is the subscription's listener goroutine. It forwards
// frames from the underlying stomp.Subscription onto out.msgs and
// shuts down on three triggers:
//
//  1. ctx.Done: caller-driven Unsubscribe. Sets Err to ctx.Err().
//  2. stomp.Subscription.C closes: broker tore down the subscription.
//     Sets Err to a wrapped broker error.
//  3. A frame with msg.Err != nil: the broker delivered an ERROR. Sets
//     Err and exits; subsequent frames (if any) are not forwarded.
//
// The single defer guarantees out.msgs is closed exactly once and the
// underlying stomp.Subscription is unsubscribed. Unsubscribe errors are
// not propagated; the listener is in teardown.
func runSubscription(ctx context.Context, stompSub *stomp.Subscription, out *Subscription) {
	defer func() {
		_ = stompSub.Unsubscribe()
		close(out.msgs)
	}()

	for {
		select {
		case <-ctx.Done():
			out.setErr(ctx.Err())
			return

		case msg, ok := <-stompSub.C:
			if !ok {
				out.setErr(fmt.Errorf("cimstomp: subscription channel closed by broker"))
				return
			}
			if msg == nil {
				out.setErr(fmt.Errorf("cimstomp: nil message from broker"))
				return
			}
			if msg.Err != nil {
				out.setErr(fmt.Errorf("cimstomp: subscription error: %w", msg.Err))
				return
			}

			frame := Message{
				Destination: msg.Destination,
				Headers:     selectHeaders(msg.Header),
				Body:        append([]byte(nil), msg.Body...),
			}

			// Deliver the frame, but do not block if the consumer is
			// slow AND the ctx is canceled in the meantime. The select
			// keeps ctx-cancel responsive even when out.msgs is full.
			select {
			case out.msgs <- frame:
			case <-ctx.Done():
				out.setErr(ctx.Err())
				return
			}
		}
	}
}

// headersOfInterest is the set of STOMP/GridAPPS-D headers the bridge
// surfaces on Subscription messages. Anything else is dropped to keep
// the public Message type small. If a future ticket needs an additional
// header, add it here; growing the surface intentionally is preferable
// to exposing the full *frame.Header.
var headersOfInterest = []string{
	"destination",
	"content-type",
	"reply-to",
	"correlation-id",
	"message-id",
	"subscription",
	gossHasSubjectHeader,
	gossSubjectHeader,
}

// stompHeader is the subset of *frame.Header methods used by
// selectHeaders. Defined as an interface so a future stub can be passed
// in unit tests; in production the only implementation is
// *github.com/go-stomp/stomp/v3/frame.Header.
type stompHeader interface {
	Get(key string) string
}

func selectHeaders(h stompHeader) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(headersOfInterest))
	for _, k := range headersOfInterest {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
