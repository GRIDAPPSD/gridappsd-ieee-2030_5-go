package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// historyLogInterval is the minimum gap between two history log lines of
// the same kind. A faulty publisher sends a frame every few seconds, so
// without it one fault would write a line per frame.
const historyLogInterval = 30 * time.Second

// historySink feeds decoded input-topic samples into a history store. It
// never blocks and never returns an error, so it cannot change the
// control path that calls it. A nil sink is a no-op.
type historySink struct {
	store *telemetryhistory.Store
	logf  func(format string, args ...any)
}

// record charts what delta contributes to history. A reported-state
// sample is charted as received; a commanded setpoint only when the
// control path applied it (applied), because a refused control never
// became a setpoint. Objects absent from reg are skipped: any publisher
// on the bus could otherwise mint series and evict the real ones.
func (h *historySink) record(reg *registry.Registry, envelope diff.Message, delta diff.Difference, applied bool) {
	if h == nil || h.store == nil {
		return
	}
	one := envelope
	one.Input.Message.ForwardDifferences = []diff.Difference{delta}
	for _, d := range telemetryhistory.DecodeMessage(one, h.logf) {
		if d.Lane == telemetryhistory.LaneCommandedSetpoint && !applied {
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
