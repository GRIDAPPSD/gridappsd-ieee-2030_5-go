// Package controlobs is a small, mutex-guarded observation hook for the
// bridge's control-delta down path. It records the last-applied control
// delta, running applied/skipped counters, and the STOMP subscription
// destinations the bridge is using, so a read-only consumer (the
// admin UI controlflow endpoint) can report control-flow state without
// touching the control path's own write logic in any way.
//
// The control path (cmd/bridge's runControlSubscriber, and the pump's
// runPump) is the only writer: it calls Recorded / Skipped / SetTopics.
// Everything else is a reader. This package makes no decision about what
// a delta means and never mutates bridge state; it is pure observation.
package controlobs

import (
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// LastDelta is a plain, read only copy of the most recently applied
// control delta: the same three fields diff.Difference carries (object,
// attribute, value), plus the wall clock time the recording happened.
type LastDelta struct {
	Object    string
	Attribute string
	// Value is a json-decoded wire value (string, float64, bool, nil,
	// or a nested map/slice of those) that the writer never mutates
	// after recording it, so Snapshot's shallow copy of this field is
	// safe. If a future writer ever stores a mutable value here,
	// Snapshot must deep-copy it instead.
	Value     any
	AppliedAt time.Time
}

// Snapshot is a plain, read only copy of the observation hook's current
// state, safe to hand to a caller (for example a JSON handler) with no
// risk of that caller reaching back into the hook's mutable fields.
type Snapshot struct {
	Applied     uint64
	Skipped     uint64
	Last        *LastDelta
	OutputTopic string
	InputTopic  string
}

// Hook is the mutex-guarded observation point. The zero value is ready to
// use: no constructor is required, since there is no invariant to
// establish beyond Go's own zero values (a nil Last, empty topics, zero
// counters).
type Hook struct {
	mu          sync.Mutex
	applied     uint64
	skipped     uint64
	last        *LastDelta
	outputTopic string
	inputTopic  string
}

// SetTopics records the STOMP subscription destinations the control path
// is using: the sim-output topic runPump subscribes to, and the
// control-delta-input topic runControlSubscriber subscribes to. Call once
// at startup, before or after the subscriptions themselves; SetTopics
// only records the strings, it does not subscribe anything itself.
func (h *Hook) SetTopics(outputTopic, inputTopic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outputTopic = outputTopic
	h.inputTopic = inputTopic
}

// Applied records that delta was successfully applied: increments the
// applied counter and replaces Last with delta's own object, attribute,
// and value, timestamped now. Call this from the control path's write
// side immediately after a successful ApplyControlDelta call, never
// speculatively before the call succeeds.
func (h *Hook) Applied(delta diff.Difference) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.applied++
	h.last = &LastDelta{
		Object:    delta.Object,
		Attribute: delta.Attribute,
		Value:     delta.Value,
		AppliedAt: time.Now().UTC(),
	}
}

// Skipped records that a delta was NOT applied (ApplyControlDelta
// returned an error, or the frame failed to decode): increments the
// skipped counter only. Last is left untouched, since a skipped delta
// was never actually applied to bridge state; the hook's Last field is
// specifically "the last delta that took effect," not "the last delta
// seen."
func (h *Hook) Skipped() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.skipped++
}

// Snapshot returns a read only copy of the hook's current state. The
// returned Last, when non-nil, is a fresh copy: mutating it does not
// affect the hook's own state.
func (h *Hook) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	var last *LastDelta
	if h.last != nil {
		copied := *h.last
		last = &copied
	}

	return Snapshot{
		Applied:     h.applied,
		Skipped:     h.skipped,
		Last:        last,
		OutputTopic: h.outputTopic,
		InputTopic:  h.inputTopic,
	}
}
