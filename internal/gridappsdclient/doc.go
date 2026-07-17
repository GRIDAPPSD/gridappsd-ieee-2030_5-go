// Package gridappsdclient adapts the standard GridAPPS-D Go client
// (github.com/GRIDAPPSD/gridappsd-go, specifically fieldbus.MessageBus)
// onto the bridge's own consumer-side interfaces, so bridge code can run
// over gridappsd-go instead of this repo's own STOMP implementation in
// internal/cimstomp.
//
// Wiring scope (GAGO-038 vs GAGO-039): this package holds ONLY the
// adapter types. It does not dial a broker, does not select TLS vs
// plaintext (see gridappsd.Config.AllowPlaintext in gridappsd-go), and
// is not wired into cmd/bridge. Every constructor here takes an
// already-Connect'ed fieldbus.MessageBus; connecting it, and swapping
// cmd/bridge over to use it, is GAGO-039's scope.
//
// # Request/reply: cim.Requester
//
// Requester adapts fieldbus.MessageBus.GetResponse to
// internal/cim.Requester (Request(ctx, destination, body) ([]byte,
// error)). This mapping is direct: gridappsd-go's GetResponse is a
// correlated request/reply primitive with the same (ctx, destination,
// body) -> ([]byte, error) shape cim.Requester expects. See requester.go.
//
// # Subscribe: sim.SubscribeClient is NOT implemented here
//
// internal/cim/sim.SubscribeClient declares:
//
//	Subscribe(ctx context.Context, destination string) (*cimstomp.Subscription, error)
//
// The return type is a concrete *cimstomp.Subscription, a struct whose
// fields (msgs, err) are unexported. Only code inside the cimstomp
// package can construct one. cimstomp does expose
// NewTestSubscription/SetTestErr (internal/cimstomp/testseam.go) and the
// internal/cimstomp/cimstomptest wrapper around them, but both are
// explicitly documented as test-only ("Do not call these from
// production code" / "Do not import cimstomptest from production
// code"). Using either here to synthesize a Subscription from
// gridappsd-go's callback-based fieldbus.MessageBus.Subscribe would
// violate that documented contract, not satisfy it.
//
// gridappsd-go's Subscribe is also shaped differently in a way that
// cannot be bridged by construction alone: it is callback-based
// (fieldbus.Handler = func(headers map[string]string, body []byte), no
// per-message error) and returns an opaque fieldbus.Token, not a
// channel-plus-Err() handle. Converting callback delivery into a
// *cimstomp.Subscription's channel-plus-Err shape is mechanically
// possible in principle (spawn a goroutine, forward each callback
// invocation onto a channel), but doing so still requires constructing
// the concrete *cimstomp.Subscription result type, which hits the same
// unexported-field wall above.
//
// This is a genuine interface-shape gap, not an implementation gap: no
// type outside package cimstomp can satisfy sim.SubscribeClient as
// currently declared. Per GAGO-038's scope, internal/cim/sim is not
// touched by this package; resolving the gap (most likely: change
// SubscribeClient to return an interface backed by Messages()/Err()
// methods rather than the concrete cimstomp type, or add a sanctioned
// production constructor to cimstomp) is left to a follow-up card. See
// the GAGO-038 PR description for the full writeup.
package gridappsdclient
