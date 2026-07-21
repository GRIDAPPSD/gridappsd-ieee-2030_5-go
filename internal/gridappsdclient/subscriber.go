package gridappsdclient

import (
	"context"
	"fmt"
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
// unresponsive broker must not stall teardown past SIGINT: see GAGO-041.
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
// dialing of its own. That wiring is GAGO-039's concern.
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
// unsubscribes, records ctx.Err() on sub (once), and closes msgs.
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
// sub.Err() therefore only ever reports ctx.Err(), never a broker-side
// drop, unlike cimstomp.Subscription which can also report a wrapped
// ErrConnectionLost. Revisit this comment and Err's doc once GAG-009
// exposes an Errors() channel upstream.
func (s *Subscriber) relay(ctx context.Context, dest string, tok fieldbus.Token, raw <-chan cimstomp.Message, sub *subscription) {
	defer close(sub.msgs)

	shutdown := func() {
		unsubCtx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
		defer cancel()
		if err := s.bus.Unsubscribe(unsubCtx, dest, tok); err != nil {
			// An unresponsive or erroring broker must not stall
			// teardown past SIGINT (GAGO-041). Record the failure so
			// it is observable via sub.Err() rather than swallowed;
			// setErr only keeps the first error, so this wins over
			// ctx.Err() below when the bus call is what actually
			// failed.
			sub.setErr(fmt.Errorf("gridappsdclient.Subscriber: unsubscribe %s: %w", dest, err))
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
