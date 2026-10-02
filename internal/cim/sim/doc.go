// Package sim carries simulation-specific topic helpers and message
// types for GridAPPS-D simulation pub/sub.
//
// When a simulation id is set, the bridge subscribes to the
// per-simulation output topic (OutputTopic) for measurement frames. It
// always subscribes to the application
// input topic (ApplicationInputTopic) for control deltas, and publishes
// device status to the application output topic (ApplicationOutputTopic),
// both called with no simulation id.
// It neither publishes to nor subscribes to the per-simulation input
// topic (InputTopic). Topic strings are constructed by the helpers in
// this package and match the wire form gridappsd-python's topics.py
// emits.
//
// Subscribe is provided by cimstomp.Client (see internal/cimstomp).
// Publishing difference messages is a direct call to cimstomp.Publisher
// with the bytes from cim/diff.Builder; no wrapper is provided in this
// package because the call site is one line.
//
// The Pump type in this package is the optional glue that subscribes to
// an output topic, decodes each frame, and dispatches to a handler. Use
// it when the only thing you want to do with measurements is
// frame-by-frame handling. For lower-level control (e.g., batching,
// fan-out, replay), build directly on cimstomp.Client.Subscribe.
package sim
