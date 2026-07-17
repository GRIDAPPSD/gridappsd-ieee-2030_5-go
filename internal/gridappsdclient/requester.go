package gridappsdclient

import (
	"context"
	"fmt"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
)

// Compile-time assertion: Requester must satisfy internal/cim.Requester.
var _ cim.Requester = (*Requester)(nil)

// requestContentType is the STOMP content-type stamped on every
// GetResponse SEND. internal/cimstomp.Client.Request uses the same
// literal for CIM query bodies (they are JSON), so the wire behavior is
// unchanged by swapping the transport.
const requestContentType = "application/json"

// Requester adapts a fieldbus.MessageBus to internal/cim.Requester:
//
//	Request(ctx context.Context, destination string, body []byte) ([]byte, error)
//
// The wrapped MessageBus must already be connected (the caller calls
// bus.Connect before constructing a Requester); Requester performs no
// dialing, TLS/plaintext selection, or reconnect logic of its own. That
// wiring is GAGO-039's concern.
type Requester struct {
	bus fieldbus.MessageBus
}

// NewRequester returns a Requester backed by bus. bus must already be
// connected; NewRequester does not call bus.Connect.
func NewRequester(bus fieldbus.MessageBus) *Requester {
	return &Requester{bus: bus}
}

// Request issues a correlated request/reply through the underlying
// MessageBus's GetResponse and returns the raw reply body unchanged.
// It satisfies internal/cim.Requester.
func (r *Requester) Request(ctx context.Context, destination string, body []byte) ([]byte, error) {
	reply, err := r.bus.GetResponse(ctx, destination, requestContentType, body)
	if err != nil {
		return nil, fmt.Errorf("gridappsdclient: request %s: %w", destination, err)
	}
	return reply, nil
}
