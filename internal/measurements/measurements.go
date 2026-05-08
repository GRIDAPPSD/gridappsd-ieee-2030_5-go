package measurements

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Mapping is a single (measurement-mRID, device-mRID) pair. The
// device-mRID is the parent ConductingEquipment that owns the
// measurement; for the Stage 1 schema this is the
// PowerElectronicsConnection.
type Mapping struct {
	MeasurementMRID string
	DeviceMRID      string
}

// ErrInvalidMapping is returned by Add and AddBatch when either field
// of a Mapping is empty.
var ErrInvalidMapping = errors.New("measurements: invalid mapping")

// Table maps measurement-mRIDs to their parent device-mRIDs and tracks
// hit/miss counters for diagnostic logging. Safe for concurrent use.
//
// Hits and misses are atomic counters rather than mutex-guarded ints so
// the Lookup hot path stays inside the RLock and never widens to a
// write lock just to bump a counter.
type Table struct {
	mu     sync.RWMutex
	index  map[string]string // measurement-mRID -> device-mRID
	hits   atomic.Uint64
	misses atomic.Uint64
}

// New returns an empty Table ready for use.
func New() *Table {
	return &Table{index: make(map[string]string)}
}

// Add inserts or replaces the mapping for measMRID. Returns
// ErrInvalidMapping wrapped with context if either argument is empty.
func (t *Table) Add(measMRID, deviceMRID string) error {
	if err := validate(measMRID, deviceMRID); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index[measMRID] = deviceMRID
	return nil
}

// AddBatch inserts the supplied mappings atomically. All mappings are
// validated before any mutation; on the first invalid entry the table
// is left untouched and the validation error is returned with the
// offending index. A nil or empty slice is a no op.
func (t *Table) AddBatch(ms []Mapping) error {
	for i, m := range ms {
		if err := validate(m.MeasurementMRID, m.DeviceMRID); err != nil {
			return fmt.Errorf("measurements: AddBatch entry %d: %w", i, err)
		}
	}
	if len(ms) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range ms {
		t.index[m.MeasurementMRID] = m.DeviceMRID
	}
	return nil
}

// Lookup returns the device-mRID mapped to measMRID. The bool reports
// presence. Each call increments either the hits or misses counter.
func (t *Table) Lookup(measMRID string) (string, bool) {
	t.mu.RLock()
	dev, ok := t.index[measMRID]
	t.mu.RUnlock()
	if ok {
		t.hits.Add(1)
	} else {
		t.misses.Add(1)
	}
	return dev, ok
}

// Len returns the current mapping count.
func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.index)
}

// Stats returns the cumulative hit and miss counters. The counters are
// monotonic since New; callers that want a delta should sample at
// intervals and subtract.
func (t *Table) Stats() (hits, misses uint64) {
	return t.hits.Load(), t.misses.Load()
}

// validate enforces the public contract: empty measurement or empty
// device mRID is rejected.
func validate(measMRID, deviceMRID string) error {
	if measMRID == "" {
		return fmt.Errorf("%w: empty measurement mRID", ErrInvalidMapping)
	}
	if deviceMRID == "" {
		return fmt.Errorf("%w: empty device mRID for measurement %q", ErrInvalidMapping, measMRID)
	}
	return nil
}
