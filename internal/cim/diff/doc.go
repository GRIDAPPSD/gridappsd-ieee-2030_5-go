// Package diff is the Go equivalent of the Python
// gridappsd.difference_builder.DifferenceBuilder. It accumulates forward
// and reverse CIM-attribute changes and emits the JSON message that
// GridAPPS-D expects on the simulation input topic.
//
// Pure construction: no broker IO, no goroutines, no auth. The bridge's
// pub/sub layer (GAGO-011) consumes Builder.Bytes and sends the payload
// to /topic/goss.gridappsd.simulation.input.<sim_id>. See research-stomp-
// cim-catalog.md section 1 in the project knowledge for destination
// semantics.
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
// Out of scope: STOMP publishing, CIM attribute schema validation, and
// persistence.
package diff
