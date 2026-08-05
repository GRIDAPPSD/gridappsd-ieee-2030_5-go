package sim

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// malformedFrameLogEvery bounds the malformed-frame log rate (Leon
// L1): a misbehaving publisher sending a steady stream of
// undecodable frames would otherwise flood the log at broker frame rate.
// The first malformed frame on a given Pump always logs (so a one-off
// bad frame is never silent); after that, only every malformedFrameLogEvery-th
// malformed frame logs, carrying the running total so an operator can
// still see the rate without a log line per frame.
const malformedFrameLogEvery = 100

// Subscription is the small interface Pump consumes from a live
// subscribe call. *cimstomp.Subscription satisfies it unchanged: the
// method set below is exactly the subset of *cimstomp.Subscription that
// Pump uses. Declaring it here (rather than requiring the concrete
// cimstomp type) lets any transport, not just cimstomp, back a
// SubscribeClient: see internal/gridappsdclient.Subscriber for the
// gridappsd-go-backed implementation.
//
// cimstomp.Message stays the shared value DTO on the wire between
// SubscribeClient implementations and Pump; only the handle around the
// channel is now abstracted.
type Subscription interface {
	Messages() <-chan cimstomp.Message
	Err() error
}

// SubscribeClient is the small interface Pump consumes. The cimstomp
// *Client satisfies it (via *cimstomp.Subscription implementing
// Subscription above); tests pass a fake. Defining the interface here
// keeps cimstomp-as-test-dependency from leaking into Pump's tests.
type SubscribeClient interface {
	Subscribe(ctx context.Context, destination string) (Subscription, error)
}

// PumpOption configures optional Pump behavior. Pass to NewPump.
type PumpOption func(*Pump)

// WithOnHandlerError sets a callback invoked after handler returns an
// error, in addition to the existing log line. The callback receives the
// handler's error and returns true to keep Run running (the default
// behavior when no callback is set, matching the pre-existing
// log-and-continue contract) or false to stop Run, which then returns a
// wrapped version of that error to the caller.
//
// This gives a caller a caller-controlled handler-error policy (Dutch
// M4) instead of only "cancel ctx yourself from inside handler",
// without changing the default fire-and-continue behavior for existing
// callers that do not set it.
func WithOnHandlerError(f func(error) bool) PumpOption {
	return func(p *Pump) { p.onHandlerError = f }
}

// Pump subscribes to a simulation output topic and dispatches each
// decoded MeasurementFrame to a caller-supplied handler. Pump is the
// thin glue layer over cimstomp.Subscribe + JSON decode + handler call;
// for richer flows (batching, fan-out, custom decode), build directly on
// cimstomp.Client.Subscribe.
//
// Pump is single-use: one Run per Pump. To process a different
// simulation, construct a new Pump.
type Pump struct {
	client SubscribeClient
	simID  string

	// onHandlerError is the caller-supplied handler-error policy set via
	// WithOnHandlerError. nil means the default: log and continue.
	onHandlerError func(error) bool

	// malformedFrameCount tracks malformed frames seen so dispatch can
	// rate-limit its log line per malformedFrameLogEvery (Leon
	// L1). atomic because Pump is documented single-use/single-Run,
	// but atomic costs nothing here and removes any future temptation to
	// call dispatch from more than one goroutine.
	malformedFrameCount atomic.Uint64
}

// NewPump constructs a Pump bound to the given client and simulation ID.
// No I/O happens until Run is called. Options configure optional
// behavior; existing two-argument call sites are unaffected.
func NewPump(client SubscribeClient, simID string, opts ...PumpOption) *Pump {
	p := &Pump{client: client, simID: simID}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Run subscribes to the simulation output topic for the bound simID and
// blocks, dispatching each decoded MeasurementFrame to handler. Returns
// when ctx is canceled, the subscription closes (broker teardown), or a
// fatal transport error occurs.
//
// Frame decoding errors are logged and skipped; the loop continues with
// the next frame. Handler errors are logged and the loop continues by
// default; a caller can install a stop-on-error policy via
// WithOnHandlerError. If a handler error should stop the pump without an
// OnHandlerError policy, the handler must cancel ctx itself.
//
// Returns:
//   - nil if the subscription closed without a recorded error.
//   - context.Canceled or context.DeadlineExceeded on ctx-driven exit.
//   - a wrapped error for Subscribe failures, broker-side teardown, or a
//     handler error that the OnHandlerError policy chose to stop on.
func (p *Pump) Run(ctx context.Context, handler func(MeasurementFrame) error) error {
	dest := OutputTopic(p.simID)
	sub, err := p.client.Subscribe(ctx, dest)
	if err != nil {
		return fmt.Errorf("sim.Pump: subscribe %s: %w", dest, err)
	}

	msgs := sub.Messages()
	for {
		select {
		case <-ctx.Done():
			// Symmetric with cimstomp's own ctx-aware send arm
			// (sendOrCancel in internal/cimstomp/subscribe.go): Run
			// gets its own direct ctx.Done() exit rather than relying
			// solely on the Subscription closing (Dutch M3).
			// This is a second line of defense for a
			// SubscribeClient/Subscription implementation that does
			// not itself close msgs promptly on ctx cancel; cimstomp's
			// own implementation already does, so for the production
			// path this arm and the msgs-closed arm race normally and
			// either may fire first.
			return ctx.Err()

		case msg, ok := <-msgs:
			if !ok {
				// Channel closed. Surface whichever cause the
				// Subscription recorded.
				endErr := sub.Err()
				if endErr == nil {
					return nil
				}
				// ctx errors pass through verbatim so callers can errors.Is them.
				if errors.Is(endErr, context.Canceled) || errors.Is(endErr, context.DeadlineExceeded) {
					return endErr
				}
				return fmt.Errorf("sim.Pump: subscription ended: %w", endErr)
			}

			if derr := p.dispatch(dest, msg, handler); derr != nil {
				return derr
			}
		}
	}
}

// dispatch decodes a single frame and invokes handler, applying the log-
// and-continue default or the caller's OnHandlerError policy. A non-nil
// return means Run should stop and return that error.
func (p *Pump) dispatch(dest string, msg cimstomp.Message, handler func(MeasurementFrame) error) error {
	var frame MeasurementFrame
	if derr := decodeFrame(msg.Body, &frame); derr != nil {
		n := p.malformedFrameCount.Add(1)
		if n == 1 || n%malformedFrameLogEvery == 0 {
			log.Printf("sim.Pump: skip malformed frame on %s (count=%d): %v", dest, n, derr)
		}
		return nil
	}
	if herr := handler(frame); herr != nil {
		log.Printf("sim.Pump: handler error on %s timestamp=%d: %v",
			dest, frame.Message.Timestamp, herr)
		if p.onHandlerError != nil && !p.onHandlerError(herr) {
			return fmt.Errorf("sim.Pump: handler error, stopping per OnHandlerError policy: %w", herr)
		}
	}
	return nil
}
