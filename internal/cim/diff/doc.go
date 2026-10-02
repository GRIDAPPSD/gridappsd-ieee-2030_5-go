// Package diff is the Go equivalent of the Python
// gridappsd.difference_builder.DifferenceBuilder. It accumulates forward
// and reverse CIM-attribute changes and emits the JSON message that
// GridAPPS-D expects on the simulation input topic.
//
// Pure construction: no broker IO, no goroutines, no auth. The bridge's
// pub/sub layer consumes Builder.Bytes and sends the payload
// to the topic sim.InputTopic(<sim_id>) names.
//
// The wire shape mirrors the Python upstream byte-for-byte:
//
//	{"command":"update","input":{"message":{"timestamp":<epoch>,
//	  "difference_mrid":"<uuidv4>","reverse_differences":[...],
//	  "forward_differences":[...]}, "simulation_id":"<id>"}}
//
// simulation_id is omitted when not set. difference_mrid is generated
// fresh on every Message or Bytes call (the bridge correlates by
// message identity, not builder identity), matching the Python upstream.
//
// Message, MessageNow, Bytes, and BytesNow all return an error
// (wrapping ErrRandFailure on the entropy-read path) instead of
// panicking on a crypto/rand failure. A failed publish is
// the caller's normal not-crash-the-bridge failure mode; see
// newUUIDv4's doc comment in diff.go for the full rationale.
//
// Out of scope: STOMP publishing, CIM attribute schema validation, and
// persistence.
//
// # Security model
//
// This package builds wire-format JSON only. It does not sign, encrypt,
// or otherwise authenticate messages. The difference_mrid is a fresh
// UUIDv4 from crypto/rand; it identifies a message for correlation,
// not for authentication. The timestamp is caller-supplied and not
// validated. Replay protection, signing, and broker-side authorization
// are the responsibility of the layer that publishes these messages
// (the GridAPPS-D broker plus its STOMP transport). Treat the bytes
// produced here as a control-plane payload that requires a trusted
// transport.
package diff
