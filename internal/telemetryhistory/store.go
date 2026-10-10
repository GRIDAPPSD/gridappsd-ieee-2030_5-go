// Package telemetryhistory is a bounded, in-memory time-series store for
// the admin UI's telemetry history view. It retains no more than
// MaxSeries distinct (object, attribute) series, each capped at
// SamplesPerSeries samples, in preallocated ring buffers that never
// reallocate. Nothing here is written to disk: on process restart the
// store is empty and refills from the bus, deliberately (see the
// admin-UI telemetry graphing plan, section 4). This package imports
// neither ieee-2030_5-core-go nor internal/telemetrypub, and contains no
// HTTP code: it is a pure, mutex-guarded data structure, following the
// house pattern in internal/connobs and internal/controlobs (named caps,
// mutex, Snapshot returning defensive copies).
//
// The memory ceiling is fixed in bytes, not in wall-clock duration:
// MaxSeries * SamplesPerSeries * unsafe.Sizeof(Sample{}) = 800 * 1440 *
// 16 bytes = 17.6 MiB, hard, regardless of how fast samples arrive. A
// faster publish rate buys resolution within the fixed window, never
// more memory. TestSampleIsSixteenBytes pins Sample's size so an edit to
// that struct cannot silently change this ceiling without a test
// noticing.
package telemetryhistory

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

// Errors returned by Store.Append for a sample the store refuses. A
// refused sample is never stored and never alters the series.
var (
	// ErrBadTime is a zero or negative timestamp: the chart encoder
	// refuses such points, and a zero usually means an unstamped frame.
	ErrBadTime = errors.New("telemetryhistory: non-positive sample time")

	// ErrFutureTime is a timestamp later than receipt plus MaxFutureSkew.
	ErrFutureTime = errors.New("telemetryhistory: sample time too far in the future")

	// ErrNonFinite is a NaN or infinite value, which json.Marshal of a
	// snapshot cannot encode.
	ErrNonFinite = errors.New("telemetryhistory: non-finite sample value")
)

// MaxFutureSkew is how far ahead of the local clock a sample time may
// be and still be kept. Five minutes tolerates ordinary publisher clock
// drift; a later time would otherwise sit as the series' newest sample
// and push every real sample behind it.
const MaxFutureSkew = 5 * time.Minute

const (
	// MaxSeries bounds the number of distinct (object, attribute) series
	// the Store retains at once. Once the cap is reached, appending a
	// sample under a series key the Store has not seen before evicts the
	// least-recently-appended existing series to make room, so a
	// larger-than-expected feeder degrades by dropping its stalest
	// series rather than by growing without bound.
	//
	// 800 is twice the 392 series of a 49-device fleet (7 reported
	// attributes and one setpoint each).
	MaxSeries = 800

	// SamplesPerSeries bounds the number of samples retained per series.
	// Each series' ring is preallocated at this capacity on first use
	// and never grows: once full, appending a sample overwrites the
	// OLDEST sample in that series, never the newest. 1440 is 6 hours at
	// the telemetry publisher's 15s cadence
	// (internal/telemetrypub/publisher.go's DefaultInterval).
	SamplesPerSeries = 1440
)

// Sample is one retained point: a Unix-second timestamp (At) taken from
// the difference message envelope's own publisher-stamped time, not from
// local receipt (see DecodeMessage), and the decoded, already-scaled
// value. Exactly 16 bytes (int64 +
// float64, no pointers, no padding); TestSampleIsSixteenBytes in this
// package asserts that with unsafe.Sizeof so the documented memory
// ceiling above stays true if this struct is ever edited.
type Sample struct {
	At    int64
	Value float64
}

// SeriesKey identifies one retained series. Object is the CIM equipment
// mRID the sample belongs to; Attribute is the dotted attribute path
// within it (for example "DERStatus.stateOfChargeStatus"). This is
// deliberately a wider key than one-series-per-device: two attributes on
// the same device are two independent series and must not overwrite
// each other.
type SeriesKey struct {
	Object    string
	Attribute string
}

// SeriesSnapshot is a read-only copy of one series' current state,
// oldest sample first.
type SeriesSnapshot struct {
	Key     SeriesKey
	Samples []Sample
}

// ring is a preallocated, fixed-capacity circular buffer of Samples. buf
// is allocated at SamplesPerSeries length on first use (newRing) and
// never resized after that: len(buf) and cap(buf) are SamplesPerSeries
// for the ring's entire lifetime. filled tracks how many of those slots
// currently hold a valid sample (0..SamplesPerSeries); next is the index
// the next Append writes to. Once filled reaches SamplesPerSeries, next
// is also the index of the OLDEST sample, since Append always writes to
// the slot one past the most recently written one, wrapping around, and
// that slot is necessarily the one holding the oldest surviving value.
type ring struct {
	buf    []Sample
	filled int
	next   int
}

func newRing() ring {
	return ring{buf: make([]Sample, SamplesPerSeries)}
}

// append writes s into the ring, overwriting the oldest sample once the
// ring is full. Never reallocates buf.
func (r *ring) append(s Sample) {
	r.buf[r.next] = s
	r.next = (r.next + 1) % SamplesPerSeries
	if r.filled < SamplesPerSeries {
		r.filled++
	}
}

// put inserts s keeping the ring ordered by At: a time newer than every
// stored sample is appended, an equal time replaces the stored sample,
// and an older time is inserted in place (dropping the oldest sample if
// that overflows the ring). Never reallocates buf.
func (r *ring) put(s Sample) {
	if r.filled == 0 || s.At > r.buf[(r.next-1+SamplesPerSeries)%SamplesPerSeries].At {
		r.append(s)
		return
	}
	cur := r.snapshot()
	i := sort.Search(len(cur), func(i int) bool { return cur[i].At >= s.At })
	if i < len(cur) && cur[i].At == s.At {
		cur[i] = s
	} else {
		cur = append(cur, Sample{})
		copy(cur[i+1:], cur[i:])
		cur[i] = s
		if len(cur) > SamplesPerSeries {
			cur = cur[1:]
		}
	}
	copy(r.buf, cur)
	r.filled = len(cur)
	r.next = len(cur) % SamplesPerSeries
}

// snapshot returns a freshly allocated copy of the ring's current
// contents, oldest sample first. Mutating the result never affects the
// ring.
func (r *ring) snapshot() []Sample {
	out := make([]Sample, r.filled)
	if r.filled < SamplesPerSeries {
		copy(out, r.buf[:r.filled])
		return out
	}
	// Full ring: the oldest sample lives at index `next` (see the type
	// doc comment), so unwrap starting there.
	n := copy(out, r.buf[r.next:])
	copy(out[n:], r.buf[:r.next])
	return out
}

// seriesEntry is the Store's internal, mutable per-series record.
// lastAppendSeq is a monotonically increasing per-Store sequence number
// stamped on every Append to this series, used only to identify the
// least-recently-appended series on eviction: it breaks ties that
// wall-clock timestamps (which come from the publisher, not local
// receipt order) cannot reliably resolve.
type seriesEntry struct {
	ring          ring
	lastAppendSeq uint64
}

// Store is the bounded, mutex-guarded time-series store. The zero value
// is ready to use: no constructor is required, matching internal/connobs
// and internal/controlobs's Hook types.
type Store struct {
	mu        sync.Mutex
	series    map[SeriesKey]*seriesEntry
	seenSeq   uint64
	evictions uint64

	// now is the clock for the MaxFutureSkew check; nil means time.Now.
	now func() time.Time
}

// Evictions returns how many series the store has evicted to stay under
// MaxSeries since it was created.
func (s *Store) Evictions() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictions
}

// Append records one sample under key, creating the series if this is
// the first sample seen for it. A sample with a non-positive time, a time
// beyond MaxFutureSkew ahead of the local clock, or a NaN or infinite
// value is refused with ErrBadTime, ErrFutureTime or ErrNonFinite and
// changes nothing. Each series is kept ordered by time: a sample whose
// time equals a stored one replaces it, and an older time is inserted in
// order, so the newest sample is always the latest time seen. If key is new and the Store already
// holds MaxSeries series, the least-recently-appended existing series is
// evicted first. Within a series, once SamplesPerSeries samples have
// been recorded, the oldest sample is overwritten; the ring never
// reallocates past that capacity.
func (s *Store) Append(key SeriesKey, sample Sample) error {
	if sample.At <= 0 {
		return ErrBadTime
	}
	if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
		return ErrNonFinite
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if sample.At > now().Add(MaxFutureSkew).Unix() {
		return ErrFutureTime
	}

	if s.series == nil {
		s.series = make(map[SeriesKey]*seriesEntry)
	}
	e, ok := s.series[key]
	if !ok {
		if len(s.series) >= MaxSeries {
			s.evictLeastRecentlyAppendedLocked()
		}
		e = &seriesEntry{ring: newRing()}
		s.series[key] = e
	}
	e.ring.put(sample)
	s.seenSeq++
	e.lastAppendSeq = s.seenSeq
	return nil
}

// evictLeastRecentlyAppendedLocked removes the series whose
// lastAppendSeq is smallest (i.e. the series that has gone longest
// without a new Append), making room for one more series under
// MaxSeries. Callers must hold s.mu. A no-op on an empty s.series
// (defensive only: Append never calls this unless len(s.series) >=
// MaxSeries, which cannot be true when s.series is empty since
// MaxSeries > 0).
func (s *Store) evictLeastRecentlyAppendedLocked() {
	var oldestKey SeriesKey
	var oldestSeq uint64
	found := false
	for key, e := range s.series {
		if !found || e.lastAppendSeq < oldestSeq {
			oldestKey, oldestSeq = key, e.lastAppendSeq
			found = true
		}
	}
	if found {
		delete(s.series, oldestKey)
		s.evictions++
	}
}

// Snapshot returns a read-only copy of every retained series, sorted by
// (Object, Attribute) for deterministic output. Every returned slice is
// a fresh copy: mutating the result, including the per-series Samples
// slices, never affects the Store's own state.
func (s *Store) Snapshot() []SeriesSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]SeriesSnapshot, 0, len(s.series))
	for key, e := range s.series {
		out = append(out, SeriesSnapshot{Key: key, Samples: e.ring.snapshot()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Object != out[j].Key.Object {
			return out[i].Key.Object < out[j].Key.Object
		}
		return out[i].Key.Attribute < out[j].Key.Attribute
	})
	return out
}
