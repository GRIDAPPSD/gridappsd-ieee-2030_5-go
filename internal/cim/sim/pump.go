package sim

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// SubscribeClient is the small interface Pump consumes. The cimstomp
// *Client satisfies it; tests pass a fake. Defining the interface here
// keeps cimstomp-as-test-dependency from leaking into Pump's tests.
type SubscribeClient interface {
	Subscribe(ctx context.Context, destination string) (*cimstomp.Subscription, error)
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
}

// NewPump constructs a Pump bound to the given client and simulation ID.
// No I/O happens until Run is called.
func NewPump(client SubscribeClient, simID string) *Pump {
	return &Pump{client: client, simID: simID}
}

// Run subscribes to the simulation output topic for the bound simID and
// blocks, dispatching each decoded MeasurementFrame to handler. Returns
// when ctx is canceled, the subscription closes (broker teardown), or a
// fatal transport error occurs.
//
// Frame decoding errors are logged and skipped; the loop continues with
// the next frame. Handler errors are logged and the loop continues; the
// handler is expected to be idempotent or to manage its own retry. If a
// handler error should stop the pump, the handler must cancel ctx
// itself.
//
// Returns:
//   - nil if the subscription closed without a recorded error.
//   - context.Canceled or context.DeadlineExceeded on ctx-driven exit.
//   - a wrapped error for Subscribe failures or broker-side teardown.
func (p *Pump) Run(ctx context.Context, handler func(MeasurementFrame) error) error {
	dest := OutputTopic(p.simID)
	sub, err := p.client.Subscribe(ctx, dest)
	if err != nil {
		return fmt.Errorf("sim.Pump: subscribe %s: %w", dest, err)
	}

	for msg := range sub.Messages() {
		var frame MeasurementFrame
		if derr := decodeFrame(msg.Body, &frame); derr != nil {
			log.Printf("sim.Pump: skip malformed frame on %s: %v", dest, derr)
			continue
		}
		if herr := handler(frame); herr != nil {
			log.Printf("sim.Pump: handler error on %s timestamp=%d: %v",
				dest, frame.Message.Timestamp, herr)
			continue
		}
	}

	// Channel closed. Surface whichever cause the Subscription recorded.
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
