package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetrypub"
)

// historyLogInterval is the minimum gap between two history log lines of
// the same kind. A faulty publisher sends a frame every few seconds, so
// without it one fault would write a line per frame.
const historyLogInterval = 30 * time.Second

// historySink feeds plottable samples into a history store. It never
// blocks and never returns an error, so it cannot change the path that
// calls it. A nil sink is a no-op.
//
// Reported state is recorded at publish time (observe), from what the
// status publisher is about to send, so the graph does not depend on the
// bus echoing it back. Commanded setpoints are recorded by the control
// subscriber (recordApplied), only for controls it applied.
type historySink struct {
	store *telemetryhistory.Store
	logf  func(format string, args ...any)
}

// withHistory returns cfg with Observe set so every status message the
// publisher builds is charted before it is sent.
func withHistory(cfg telemetrypub.Config, sink *historySink, reg *registry.Registry) telemetrypub.Config {
	cfg.Observe = func(m telemetrypub.Message) { sink.observe(reg, m) }
	return cfg
}

// observe records the reported-state samples of a status message the
// publisher is about to send. A body that does not decode is skipped:
// the publisher built it, so a failure here must not affect the send.
func (h *historySink) observe(reg *registry.Registry, msg telemetrypub.Message) {
	if h == nil || h.store == nil {
		return
	}
	var envelope diff.Message
	if err := json.Unmarshal(msg.Body, &envelope); err != nil {
		h.logf("history: skip status message that does not decode: %v", err)
		return
	}
	h.append(reg, envelope, telemetryhistory.LaneReportedState)
}

// recordApplied charts the commanded setpoint of a delta the control
// path applied. A refused control never became a setpoint, and reported
// state is not read from this path at all: an echoed status frame adds
// nothing.
func (h *historySink) recordApplied(reg *registry.Registry, envelope diff.Message, delta diff.Difference, applied bool) {
	if h == nil || h.store == nil || !applied {
		return
	}
	one := envelope
	one.Input.Message.ForwardDifferences = []diff.Difference{delta}
	h.append(reg, one, telemetryhistory.LaneCommandedSetpoint)
}

// append stores the decoded samples of one lane. Objects absent from reg
// are skipped: any publisher on the bus could otherwise mint series and
// evict the real ones.
func (h *historySink) append(reg *registry.Registry, envelope diff.Message, lane telemetryhistory.Lane) {
	for _, d := range telemetryhistory.DecodeMessage(envelope, h.logf) {
		if d.Lane != lane {
			continue
		}
		if _, ok := reg.LFDI(d.Key.Object); !ok {
			h.logf("history: skip sample for unregistered object %q", d.Key.Object)
			continue
		}
		before := h.store.Evictions()
		if err := h.store.Append(d.Key, d.Sample); err != nil {
			h.logf("history: dropped sample object=%q attribute=%q at=%d: %v",
				d.Key.Object, d.Key.Attribute, d.Sample.At, err)
			continue
		}
		if h.store.Evictions() != before {
			h.logf("history: series cap %d reached, evicted the least recently appended series for object=%q attribute=%q",
				telemetryhistory.MaxSeries, d.Key.Object, d.Key.Attribute)
		}
	}
}

// newRateLimitedLogf wraps out so each distinct format string is emitted
// at most once per interval, with a count of the lines suppressed since.
// Format strings are source constants, so the bucket map stays small.
func newRateLimitedLogf(interval time.Duration, now func() time.Time, out func(string, ...any)) func(string, ...any) {
	type bucket struct {
		last       time.Time
		suppressed int
	}
	var mu sync.Mutex
	buckets := map[string]*bucket{}
	return func(format string, args ...any) {
		mu.Lock()
		b := buckets[format]
		if b == nil {
			b = &bucket{}
			buckets[format] = b
		}
		t := now()
		if !b.last.IsZero() && t.Sub(b.last) < interval {
			b.suppressed++
			mu.Unlock()
			return
		}
		n := b.suppressed
		b.last, b.suppressed = t, 0
		mu.Unlock()
		line := fmt.Sprintf(format, args...)
		if n > 0 {
			line = fmt.Sprintf("%s (%d similar suppressed)", line, n)
		}
		out("%s", line)
	}
}
