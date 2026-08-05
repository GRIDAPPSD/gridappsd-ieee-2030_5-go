// Package cim provides typed wrappers for the CIM-related GridAPPS-D
// requests the bridge issues over STOMP: SPARQL queries, model info,
// object dictionaries, the CIM Dictionary feeder summary, and platform
// status.
//
// The package is consumer-side: it depends on a small Requester
// interface (Request(ctx, destination, body) ([]byte, error)). Any
// concrete type that satisfies it can be plugged in. In practice the
// bridge passes a *cimstomp.Client. Tests pass a mock. This keeps the
// cimstomp package focused on the STOMP request/reply primitive while
// CIM-shaped concerns evolve here independently.
//
// Wire format matches the GridAPPS-D platform's own STOMP request/reply
// envelope and response shapes.
package cim
