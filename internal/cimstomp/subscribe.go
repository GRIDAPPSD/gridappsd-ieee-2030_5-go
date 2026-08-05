package cimstomp

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/go-stomp/stomp/v3"
)

// defaultMaxFrameBodyBytes is the default cap on an incoming subscription
// frame's body, applied in runSubscription (Leon M1). It exists
// to bound memory from a broker bug, a misbehaving publisher, or a
// misrouted destination that floods the subscription with unexpectedly
// large payloads; ~16MiB comfortably covers the largest legitimate
// simulation-output frame observed in practice while still being a hard
// stop against an unbounded one. Override per-Subscribe via
// WithMaxFrameBodyBytes.
const defaultMaxFrameBodyBytes = 16 * 1024 * 1024

// oversizedFrameLogEvery bounds the oversized-frame drop log rate,
// mirroring the malformed-frame rate limit in internal/cim/sim.Pump.
// Without a limit, a steady stream of oversized
// frames from a misbehaving publisher turns the memory-DoS defense
// isOversizedFrame provides into a log-DoS: one unbounded log line per
// dropped frame. The first oversized frame on a given subscription
// always logs (so a one-off oversized frame is never silent); after
// that, only every oversizedFrameLogEvery-th oversized frame logs,
// carrying the running total so an operator can still see the rate
// without a log line per frame.
const oversizedFrameLogEvery = 100

// SubscribeOption configures optional Subscribe behavior.
type SubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	maxFrameBodyBytes int
}

// WithMaxFrameBodyBytes overrides the default oversized-frame cap
// (defaultMaxFrameBodyBytes) for this Subscribe call. A frame whose body
// exceeds the cap is dropped (logged, not forwarded) and the subscription
// continues; it is not torn down and Subscription.Err is not set for it,
// since an oversized frame is not itself proof the connection or broker
// session is unhealthy.
func WithMaxFrameBodyBytes(n int) SubscribeOption {
	return func(o *subscribeOptions) { o.maxFrameBodyBytes = n }
}

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
func (c *Client) Subscribe(ctx context.Context, destination string, opts ...SubscribeOption) (*Subscription, error) {
	options := subscribeOptions{maxFrameBodyBytes: defaultMaxFrameBodyBytes}
	for _, opt := range opts {
		opt(&options)
	}

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
		return nil, wrapTransportErr(fmt.Sprintf("cimstomp.Client: subscribe %s", destination), err)
	}

	// subscriptionBufferSize (16) is a hand-picked cushion, not a tuned
	// value: it absorbs a short burst from the broker (e.g. simulation
	// output ticks arriving faster than a slow handler drains them)
	// without the sender blocking on every frame. It is not exposed as a
	// per-call option (Dutch M2) because no caller in this
	// codebase has needed a different value; sendOrCancel already keeps
	// ctx-cancel responsive even when the buffer is full, so a caller
	// with a genuinely slower consumer is not at risk of a stuck
	// goroutine, only of a bounded backlog. Revisit if a future
	// subscriber's burst profile needs a larger cushion.
	const subscriptionBufferSize = 16
	out := &Subscription{msgs: make(chan Message, subscriptionBufferSize)}

	go runSubscription(ctx, stompSub, out, options.maxFrameBodyBytes)
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
// A frame whose Body exceeds maxFrameBodyBytes is dropped instead of
// forwarded: logged, and the loop continues with the next frame (Leon
// M1). This is a deliberate drop-and-continue, not a subscription
// teardown: an oversized frame does not by itself mean the connection or
// broker session is unhealthy, so the cap protects memory without
// escalating to Err/close on what may be one bad publisher.
//
// The single defer guarantees out.msgs is closed exactly once and the
// underlying stomp.Subscription is unsubscribed. Unsubscribe errors are
// not propagated; the listener is in teardown.
func runSubscription(ctx context.Context, stompSub *stomp.Subscription, out *Subscription, maxFrameBodyBytes int) {
	defer func() {
		_ = stompSub.Unsubscribe()
		close(out.msgs)
	}()

	// oversizedFrameCount tracks oversized frames dropped on this
	// subscription so isOversizedFrame can rate-limit its log line per
	// oversizedFrameLogEvery. Plain (not atomic): runSubscription is the
	// sole goroutine that reads or writes it.
	var oversizedFrameCount uint64

	for {
		select {
		case <-ctx.Done():
			out.setErr(ctx.Err())
			return

		case msg, ok := <-stompSub.C:
			if !ok {
				// Broker closed the subscription channel. Surface as a
				// transport-level loss so callers can
				// errors.Is(err, ErrConnectionLost) on Subscription.Err
				// and drive a Reconnect/resubscribe (Dutch H1, mirrors
				// the wrap pattern used by Request).
				out.setErr(wrapTransportErr("cimstomp: subscription channel closed by broker", stomp.ErrClosedUnexpectedly))
				return
			}
			if msg == nil {
				out.setErr(wrapTransportErr("cimstomp: nil message from broker", stomp.ErrClosedUnexpectedly))
				return
			}
			if msg.Err != nil {
				out.setErr(wrapTransportErr("cimstomp: subscription error", msg.Err))
				return
			}
			if isOversizedFrame(msg.Destination, len(msg.Body), maxFrameBodyBytes, &oversizedFrameCount) {
				continue
			}

			frame := Message{
				Destination: msg.Destination,
				Headers:     selectHeaders(msg.Header),
				Body:        append([]byte(nil), msg.Body...),
			}

			// Deliver the frame, but do not block if the consumer is
			// slow AND the ctx is canceled in the meantime. sendOrCancel
			// keeps ctx-cancel responsive even when out.msgs is full.
			if !sendOrCancel(ctx, out, frame) {
				return
			}
		}
	}
}

// isOversizedFrame reports whether bodyLen exceeds maxFrameBodyBytes, and
// if so logs the drop. Split out from runSubscription's size check
// (Leon M1) so the cap decision itself is directly unit-testable
// without needing a real *stomp.Subscription to drive a frame through
// the goroutine.
//
// count is the caller's running oversized-frame counter for this
// subscription; isOversizedFrame increments it on every drop and only
// logs on the first drop and every oversizedFrameLogEvery-th drop after
// that, mirroring the malformed-frame rate limit in
// internal/cim/sim.Pump.dispatch. count is nil-safe: passing nil (as
// existing unit tests do) skips the counting and always logs, which
// keeps TestIsOversizedFrame_DropsAboveCapKeepsAtOrBelow's per-case
// assertions on the boolean return value unaffected by log-rate state.
func isOversizedFrame(destination string, bodyLen, maxFrameBodyBytes int, count *uint64) bool {
	if bodyLen <= maxFrameBodyBytes {
		return false
	}
	if count == nil {
		log.Printf("cimstomp: dropping oversized subscription frame on %s: body=%d bytes exceeds cap=%d bytes",
			destination, bodyLen, maxFrameBodyBytes)
		return true
	}
	*count++
	if *count == 1 || *count%oversizedFrameLogEvery == 0 {
		log.Printf("cimstomp: dropping oversized subscription frame on %s: body=%d bytes exceeds cap=%d bytes (count=%d)",
			destination, bodyLen, maxFrameBodyBytes, *count)
	}
	return true
}

// sendOrCancel delivers frame on out.msgs, but does not block forever if
// the consumer is slow and ctx is canceled in the meantime. It returns
// true if frame was delivered, false if ctx was done first (in which case
// out.setErr(ctx.Err()) has already been called and the caller should stop
// forwarding).
//
// Split out from runSubscription's send arm (Dutch M1) so the
// ctx-vs-full-channel race can be unit-tested directly without a real
// *stomp.Subscription: subscribe_test.go fills a 1-buffered Subscription,
// cancels ctx, and asserts this returns false without blocking.
func sendOrCancel(ctx context.Context, out *Subscription, frame Message) bool {
	select {
	case out.msgs <- frame:
		return true
	case <-ctx.Done():
		out.setErr(ctx.Err())
		return false
	}
}

// headersOfInterest is the set of STOMP/GridAPPS-D headers the bridge
// surfaces on Subscription messages. Anything else is dropped to keep
// the public Message type small. If a future ticket needs an additional
// header, add it here; growing the surface intentionally is preferable
// to exposing the full *frame.Header.
//
// gossSubjectHeader (GOSS_SUBJECT) carries the auth token on outbound
// Request SENDs and is intentionally NOT in this allowlist. A healthy
// broker does not echo it on inbound subscription frames, but if any
// path ever does (broker bug, misconfigured route, test fixture) we
// must not surface the token to subscription handlers, since handlers
// frequently log their messages. gossHasSubjectHeader (the boolean
// signal) stays; it is not a secret.
var headersOfInterest = []string{
	"destination",
	"content-type",
	"reply-to",
	"correlation-id",
	"message-id",
	"subscription",
	gossHasSubjectHeader,
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
