// Package gridappsdclient adapts the standard GridAPPS-D Go client
// (github.com/GRIDAPPSD/gridappsd-go, specifically fieldbus.MessageBus)
// onto the bridge's own consumer-side interfaces, so bridge code can run
// over gridappsd-go instead of this repo's own STOMP implementation in
// internal/cimstomp.
//
// Wiring scope: this package holds ONLY the
// adapter types. It does not dial a broker, does not select TLS vs
// plaintext (see gridappsd.Config.AllowPlaintext in gridappsd-go), and
// is not wired into cmd/bridge. Every constructor here takes an
// already-Connect'ed fieldbus.MessageBus; connecting it, and swapping
// cmd/bridge over to use it, is out of scope for this package.
//
// # Request/reply: cim.Requester
//
// Requester adapts fieldbus.MessageBus.GetResponse to
// internal/cim.Requester (Request(ctx, destination, body) ([]byte,
// error)). This mapping is direct: gridappsd-go's GetResponse is a
// correlated request/reply primitive with the same (ctx, destination,
// body) -> ([]byte, error) shape cim.Requester expects. See requester.go.
//
// # Subscribe: sim.SubscribeClient, resolved via an interface reshape
//
// internal/cim/sim.SubscribeClient originally declared:
//
//	Subscribe(ctx context.Context, destination string) (*cimstomp.Subscription, error)
//
// The return type was a concrete *cimstomp.Subscription, a struct whose
// fields (msgs, err) are unexported, so only code inside the cimstomp
// package could construct one. cimstomp's NewTestSubscription/SetTestErr
// (internal/cimstomp/testseam.go) and the internal/cimstomp/cimstomptest
// wrapper around them could technically produce one from outside the
// package, but both are explicitly documented as test-only ("Do not
// call these from production code" / "Do not import cimstomptest from
// production code"), so they were not an available path either. This
// was a genuine interface-shape gap, not an implementation gap: no type
// outside package cimstomp could satisfy sim.SubscribeClient as
// originally declared.
//
// Design decision (Noor): reshape the interface
// rather than route around it. internal/cim/sim now declares a small
// Subscription interface (Messages() <-chan cimstomp.Message, Err()
// error) alongside SubscribeClient, and *cimstomp.Subscription
// satisfies it unchanged, no cimstomp-side change needed.
// cimstomp.Message stays the shared wire DTO; only the handle around
// the channel is abstracted. *cimstomp.Client.Subscribe still returns
// the concrete *cimstomp.Subscription and so no longer satisfies
// SubscribeClient directly; cmd/bridge carries a small wrapper
// (cimstompSubscribeClient) at its one call site to narrow the return
// type. See the sim-side reshape commit and cmd/bridge/main.go for the
// wrapper.
//
// Subscriber (subscriber.go) is the gridappsd-go-backed implementation
// of the reshaped interfaces: a callback-to-channel relay adapting
// fieldbus.MessageBus's callback-based Subscribe (fieldbus.Handler =
// func(headers map[string]string, body []byte), opaque fieldbus.Token)
// onto sim.Subscription's channel-plus-Err() shape. See subscriber.go
// for the full backpressure and shutdown contract, including the
// documented gap left open upstream (gridappsd-go: no
// broker-teardown signal reaches MessageBus callers yet, so Err() only
// ever reports ctx.Err()).
//
// Swapping cmd/bridge's connectClient over to dial a gridappsd-go
// fieldbus.MessageBus instead of cimstomp.Client remains out of scope
// for this package.
package gridappsdclient
