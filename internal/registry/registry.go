package registry

import (
	"errors"
	"fmt"
	"sync"
)

// Entry is a single mapping between IEEE 2030.5 identity and CIM identity.
// It mirrors the Python upstream HouseLookup(mRID, name, lfdi) shape from
// gridappsd_adapter.py.
//
// Placeholder marks an entry whose LFDI was generated as a Stage 1
// stand-in (e.g., a deterministic hash of the mRID) rather than derived
// from a real device certificate. Defaults to false. Operators can grep
// the populate-time logs for "(placeholder)" to confirm whether the
// real LFDI mapping has landed yet. The flag is informational; lookup
// behavior is identical for placeholder and real entries.
type Entry struct {
	MRID        string // CIM master resource ID
	Name        string // human friendly name (optional, may be empty)
	LFDI        string // IEEE 2030.5 long form device ID, hex string
	Placeholder bool   // true when LFDI is a Stage 1 stand-in
}

// ErrInvalidEntry is returned by Add and AddBatch when an Entry has an
// empty MRID or empty LFDI. An empty Name is allowed.
var ErrInvalidEntry = errors.New("registry: invalid entry")

// Registry maintains a bidirectional mRID to LFDI mapping plus the
// optional Name attribute. It is safe for concurrent use; reads vastly
// outnumber writes in normal bridge operation, so an RWMutex guards the
// internal state. Reads take RLock, mutations take Lock.
type Registry struct {
	mu        sync.RWMutex
	mridIndex map[string]Entry  // mRID -> Entry
	lfdiIndex map[string]string // LFDI -> mRID
}

// New returns an empty Registry ready for use.
func New() *Registry {
	return &Registry{
		mridIndex: make(map[string]Entry),
		lfdiIndex: make(map[string]string),
	}
}

// Add inserts or replaces the mapping for e.MRID. If an existing entry
// for the same MRID has a different LFDI, the old reverse index entry is
// removed so it no longer resolves. Returns ErrInvalidEntry wrapped with
// context if e.MRID or e.LFDI is empty.
func (r *Registry) Add(e Entry) error {
	if err := validate(e); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unsafeAdd(e)
	return nil
}

// AddBatch inserts the supplied entries atomically. All entries are
// validated before any mutation occurs; if any entry is invalid, the
// registry is left untouched and the first validation error is returned.
// A nil or empty slice is a no op.
func (r *Registry) AddBatch(es []Entry) error {
	for i, e := range es {
		if err := validate(e); err != nil {
			return fmt.Errorf("registry: AddBatch entry %d: %w", i, err)
		}
	}
	if len(es) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range es {
		r.unsafeAdd(e)
	}
	return nil
}

// LFDI returns the LFDI mapped to mrid. The bool reports presence.
func (r *Registry) LFDI(mrid string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.mridIndex[mrid]
	if !ok {
		return "", false
	}
	return e.LFDI, true
}

// MRID returns the mRID mapped to lfdi. The bool reports presence.
func (r *Registry) MRID(lfdi string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mrid, ok := r.lfdiIndex[lfdi]
	return mrid, ok
}

// Get returns the full Entry for mrid. The bool reports presence.
func (r *Registry) Get(mrid string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.mridIndex[mrid]
	return e, ok
}

// Name returns the human friendly name for mrid. The bool reports
// presence; the returned string may be empty even when ok is true.
func (r *Registry) Name(mrid string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.mridIndex[mrid]
	if !ok {
		return "", false
	}
	return e.Name, true
}

// Remove deletes the mapping for mrid from both indexes. It returns the
// removed Entry and true on success, or the zero Entry and false if no
// such mapping exists. Calling Remove on an unknown mRID is a no op.
func (r *Registry) Remove(mrid string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.mridIndex[mrid]
	if !ok {
		return Entry{}, false
	}
	delete(r.mridIndex, mrid)
	delete(r.lfdiIndex, e.LFDI)
	return e, true
}

// Len returns the current entry count.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.mridIndex)
}

// Snapshot returns a copy of all entries. Iteration order is unspecified.
// Callers may mutate the returned slice without affecting the registry.
// Intended for diagnostics and tests; do not use in hot paths.
func (r *Registry) Snapshot() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.mridIndex))
	for _, e := range r.mridIndex {
		out = append(out, e)
	}
	return out
}

// validate enforces the public contract: empty MRID or empty LFDI is
// rejected; empty Name is allowed.
func validate(e Entry) error {
	if e.MRID == "" {
		return fmt.Errorf("%w: empty MRID", ErrInvalidEntry)
	}
	if e.LFDI == "" {
		return fmt.Errorf("%w: empty LFDI for MRID %q", ErrInvalidEntry, e.MRID)
	}
	return nil
}

// unsafeAdd inserts e and keeps the reverse index consistent. Caller
// must hold the write lock. If an existing entry for e.MRID maps to a
// different LFDI, the stale reverse index entry is removed first.
func (r *Registry) unsafeAdd(e Entry) {
	if prev, ok := r.mridIndex[e.MRID]; ok && prev.LFDI != e.LFDI {
		delete(r.lfdiIndex, prev.LFDI)
	}
	r.mridIndex[e.MRID] = e
	r.lfdiIndex[e.LFDI] = e.MRID
}
