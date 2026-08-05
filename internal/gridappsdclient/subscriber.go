package gridappsdclient

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// subscriptionMsgBuf matches cimstomp.Subscription's channel buffer size
// (internal/cimstomp/subscribe.go: make(chan Message, 16)), so the two
// transports present the same backpressure behavior to sim.Pump.
const subscriptionMsgBuf = 16

// unsubscribeTimeout bounds relay's shutdown call to bus.Unsubscribe. An
// unresponsive broker must not stall teardown past SIGINT.
const unsubscribeTimeout = 5 * time.Second

// subscription adapts gridappsd-go's callback-based
// fieldbus.MessageBus.Subscribe onto sim.Subscription. It is constructed
// only by Subscriber.Subscribe.
type subscription struct {
	msgs chan cimstomp.Message

	errMu sync.Mutex
	err   error
}

// Messages returns the channel of received frames, closed when the
// subscription ends (ctx cancel is the only end trigger; see
// Subscriber.relay's doc comment for why there is no broker-teardown
// signal yet).
func (s *subscription) Messages() <-chan cimstomp.Message { return s.msgs }

// Err returns the cause that ended the subscription. Stable to call
// after Messages is drained.
func (s *subscription) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *subscription) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// compile-time assertion: subscription must satisfy sim.Subscription.
var _ sim.Subscription = (*subscription)(nil)

// Subscriber adapts a fieldbus.MessageBus to sim.SubscribeClient. The
// wrapped MessageBus must already be connected (the caller calls
// bus.Connect before constructing a Subscriber); Subscriber performs no
// dialing of its own. That wiring is out of scope for this package.
type Subscriber struct {
	bus fieldbus.MessageBus
}

// NewSubscriber returns a Subscriber backed by bus. bus must already be
// connected; NewSubscriber does not call bus.Connect.
func NewSubscriber(bus fieldbus.MessageBus) *Subscriber {
	return &Subscriber{bus: bus}
}

// compile-time assertion: Subscriber must satisfy sim.SubscribeClient.
var _ sim.SubscribeClient = (*Subscriber)(nil)

// Subscribe registers a fieldbus.Handler on destination and returns a
// sim.Subscription whose Messages channel receives each delivered frame
// until ctx is canceled. It satisfies sim.SubscribeClient.
//
// The handler copies each message body (append([]byte(nil), body...)) so
// the returned cimstomp.Message does not alias a buffer the router may
// reuse, then hands the message to a single relay goroutine over an
// unbuffered raw channel, guarded by ctx so a handler invoked after ctx
// is done does not block forever. See relay's doc comment for the full
// shutdown and backpressure contract.
func (s *Subscriber) Subscribe(ctx context.Context, destination string) (sim.Subscription, error) {
	raw := make(chan cimstomp.Message) // unbuffered handoff from handler to relay
	sub := &subscription{msgs: make(chan cimstomp.Message, subscriptionMsgBuf)}

	handler := func(headers map[string]string, body []byte) {
		msg := cimstomp.Message{
			Destination: destination,
			Headers:     headers,
			Body:        append([]byte(nil), body...),
		}
		select {
		case raw <- msg:
		case <-ctx.Done():
			// A late/in-flight delivery after shutdown: drop it rather
			// than block forever. No relay goroutine remains to drain
			// raw once ctx is done.
		}
	}

	tok, err := s.bus.Subscribe(ctx, destination, handler)
	if err != nil {
		return nil, fmt.Errorf("gridappsdclient.Subscriber: subscribe %s: %w", destination, err)
	}

	go s.relay(ctx, destination, tok, raw, sub)

	return sub, nil
}

// relay is the sole owner and closer of sub.msgs. It forwards raw to
// msgs with a blocking, ctx-guarded send until ctx is done, then
// unsubscribes (bounded by unsubscribeTimeout so an unresponsive
// broker cannot stall teardown past SIGINT), records an
// error on sub (once: the Unsubscribe failure if there was one,
// otherwise ctx.Err()), and closes msgs.
//
// Backpressure: the send to sub.msgs blocks (guarded only by
// ctx.Done()), so a slow sim.Pump consumer blocks relay, which blocks
// the handler's send to raw, which blocks the router's per-destination
// readLoop (see gridappsd-go internal/router.readLoop). No message is
// ever dropped by relay; the only drop path is the handler's own
// ctx-guarded select after shutdown has already begun (see Subscribe's
// doc comment).
//
// raw is never closed. The handler goroutine invoked by the router may
// still be in flight when ctx fires (router.readLoop's exit is
// asynchronous with respect to ctx), so closing raw here would race a
// concurrent send in the handler and panic; the handler's own
// ctx-guarded select is what stops it from blocking forever instead.
//
// There is currently no broker-teardown signal surfaced by
// gridappsd-go's router.Router to fieldbus.MessageBus callers (readLoop
// only reports to an internal errSink; see gridappsd-go GAG-009).
// sub.Err() therefore only ever reports ctx.Err() or a bounded
// Unsubscribe failure at shutdown, never a broker-side drop mid-stream,
// unlike cimstomp.Subscription which can also report a wrapped
// ErrConnectionLost. Revisit this comment and Err's doc once GAG-009
// exposes an Errors() channel upstream.
func (s *Subscriber) relay(ctx context.Context, dest string, tok fieldbus.Token, raw <-chan cimstomp.Message, sub *subscription) {
	defer close(sub.msgs)

	shutdown := func() {
		// unsubCtx is derived from context.Background(), not ctx: ctx is
		// already Done at this point (that is why shutdown is running),
		// so deriving the timeout from it would produce an
		// already-expired deadline instead of a fresh unsubscribeTimeout
		// window.
		unsubCtx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
		defer cancel()

		// The production fieldbus.MessageBus (gridappsd-go's
		// Router.Unsubscribe) discards its ctx argument entirely and
		// delegates to go-stomp's Subscription.Unsubscribe, which takes
		// no ctx at all and blocks on go-stomp's own
		// UnsubscribeReceiptTimeout (30s default; cimstomp's dial never
		// overrides it). Passing unsubCtx to s.bus.Unsubscribe therefore
		// does NOT bound the real call: an unresponsive broker can still
		// stall a synchronous call for up to 30s regardless of
		// unsubscribeTimeout. To bound shutdown regardless of whether
		// the callee ever looks at ctx, run the call in its own
		// goroutine and race it against unsubCtx.Done() instead of
		// waiting on the call directly. resultCh is buffered so the
		// goroutine's send never blocks even after we've stopped
		// waiting on it (CRITICAL 2).
		resultCh := make(chan error, 1)
		go func() {
			resultCh <- s.bus.Unsubscribe(unsubCtx, dest, tok)
		}()

		select {
		case err := <-resultCh:
			if err != nil {
				// An erroring broker must not be swallowed. Record it so
				// it is observable via sub.Err(); setErr only keeps the
				// first error, so this wins over ctx.Err() below when
				// the bus call is what actually failed.
				sub.setErr(fmt.Errorf("gridappsdclient.Subscriber: unsubscribe %s: %w", dest, err))
			}
		case <-unsubCtx.Done():
			// The Unsubscribe call has not returned within
			// unsubscribeTimeout, either because it is legitimately slow
			// or because (the real-world case) it ignores unsubCtx
			// entirely. Abandon the wait, not the goroutine: it keeps
			// running until go-stomp's own internal timeout eventually
			// unblocks it, and we log whatever it returns then rather
			// than dropping it silently. The process is already
			// shutting down (ctx fired before shutdown was even called),
			// so a single lingering goroutine bounded by go-stomp's own
			// timeout is an acceptable cost for a bounded teardown.
			sub.setErr(fmt.Errorf("gridappsdclient.Subscriber: unsubscribe %s: %w", dest, unsubCtx.Err()))
			go func() {
				if err := <-resultCh; err != nil {
					log.Printf("gridappsdclient.Subscriber: abandoned unsubscribe %s completed after the shutdown bound: %v", dest, err)
				}
			}()
		}

		sub.setErr(ctx.Err())
	}

	for {
		select {
		case <-ctx.Done():
			shutdown()
			return
		case msg := <-raw:
			select {
			case sub.msgs <- msg:
			case <-ctx.Done():
				shutdown()
				return
			}
		}
	}
}
