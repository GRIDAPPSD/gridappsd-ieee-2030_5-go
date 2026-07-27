// Package connobs is a small, mutex-guarded observation hook for the
// embedded IEEE 2030.5 listener's connection surface. It answers the
// question "served != connected": the registry and the served EndDevice
// store both show the SEEDED roster, but neither one records which LFDIs
// have actually completed an mTLS handshake and issued requests. This
// package is that missing record, read only for every consumer except its
// two writers.
//
// There are two independent recordings, fed from two different seams:
//
//   - RecordRequest is called from sep2embed's per-request identity
//     middleware, once the caller's LFDI has been extracted from the
//     verified TLS peer certificate. It tracks, per LFDI: the last-seen
//     wall-clock time, a cumulative request count, and the set of
//     resource paths that LFDI has touched.
//   - RecordHandshake is called from the mTLS listener's certificate
//     verification hook, once per connection attempt, whether the chain
//     was accepted or rejected. A rejected handshake never reaches
//     RecordRequest (the connection is closed before any HTTP request is
//     read), so this is the only way a rejected connection attempt
//     becomes observable at all.
//
// Both writers are additive observation only: this package makes no
// accept/reject decision and never mutates bridge state; it mirrors
// internal/controlobs's "pure observation" posture, per-LFDI instead of
// per-control-delta.
package connobs

import (
	"sort"
	"sync"
	"time"
)

// maxHandshakeLog bounds the handshake attempt ring buffer so a
// long-running bridge under repeated failed-handshake probing (or a
// misconfigured client retrying rapidly) cannot grow this log without
// bound. Oldest entries are dropped first once the log is full.
const maxHandshakeLog = 200

// ClientSnapshot is a read only copy of one connected client's recorded
// state.
type ClientSnapshot struct {
	LFDI string

	// LastSeen is the wall-clock time (UTC) of the most recently recorded
	// request from this LFDI.
	LastSeen time.Time

	// RequestCount is the cumulative number of requests recorded from
	// this LFDI since the Hook was created.
	RequestCount uint64

	// Paths is the sorted, de-duplicated set of resource paths this LFDI
	// has requested. Sorted so Snapshot's output is deterministic and
	// test-friendly; the underlying recording order is not preserved.
	Paths []string
}

// HandshakeAttempt is a read only copy of one recorded mTLS connection
// attempt.
type HandshakeAttempt struct {
	// LFDI is the spec section 6.3.4 LFDI derived from the presented leaf
	// certificate (via core's sepTLS.LFDI), best effort: empty if the
	// leaf certificate could not be parsed at all, in which case Reason
	// explains why.
	LFDI string

	// RemoteAddr is the TCP peer address ("host:port") of the connection
	// attempt.
	RemoteAddr string

	// Accepted reports whether the existing CSIP-aware chain
	// verification (sepTLS.VerifyPeerCertWithHardwareModuleSAN) accepted
	// this certificate. This field, and every other field on this type,
	// is recorded strictly AFTER that verifier has already run and
	// produced its real verdict: this hook never influences the
	// verdict, it only observes it.
	Accepted bool

	// Reason is empty when Accepted is true. When Accepted is false, it
	// is the verifier's own error message: the actual rejection reason
	// (bad chain, expired cert, untrusted root, unparseable leaf, and so
	// on), never a synthesized or generic string.
	Reason string

	// Known reports whether LFDI matches an entry already present in the
	// bridge's mRID-to-LFDI registry, i.e. whether this connecting
	// device is part of the seeded/served roster this bridge already
	// recognizes, as opposed to a certificate this bridge has never seen
	// associated with a device before. Always false when LFDI is empty
	// (an unparseable leaf can never be "known").
	Known bool

	// At is the wall-clock time (UTC) this attempt was recorded.
	At time.Time
}

// Snapshot is a read only copy of the Hook's current state.
type Snapshot struct {
	// Clients is sorted by LFDI for deterministic output.
	Clients []ClientSnapshot

	// Handshakes is the recorded connection-attempt ring buffer, oldest
	// first, capped at maxHandshakeLog entries.
	Handshakes []HandshakeAttempt
}

// clientState is the mutable, internal-only per-LFDI record. The paths
// set is a map for O(1) recording; Snapshot renders it to a sorted slice.
type clientState struct {
	lastSeen     time.Time
	requestCount uint64
	paths        map[string]struct{}
}

// Hook is the mutex-guarded observation point. The zero value is ready to
// use: no constructor is required.
type Hook struct {
	mu         sync.Mutex
	clients    map[string]*clientState
	handshakes []HandshakeAttempt
}

// RecordRequest records one request from lfdi against path: increments
// that LFDI's cumulative request count, updates its last-seen time to
// now, and adds path to its recorded path set (a no-op if path was
// already recorded for this LFDI). lfdi must be non-empty; callers with
// no verified identity (see sep2embed's identityMiddleware fail-closed
// path) must not call this, since there is nothing to key the record by.
func (h *Hook) RecordRequest(lfdi, path string) {
	if lfdi == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients == nil {
		h.clients = make(map[string]*clientState)
	}
	c, ok := h.clients[lfdi]
	if !ok {
		c = &clientState{paths: make(map[string]struct{})}
		h.clients[lfdi] = c
	}
	c.requestCount++
	c.lastSeen = time.Now().UTC()
	c.paths[path] = struct{}{}
}

// RecordHandshake appends attempt to the handshake log, stamping At with
// the current time (any caller-supplied At is overwritten, so the
// recorded time always reflects when this hook observed the attempt, not
// when the caller happened to construct the value). Once the log reaches
// maxHandshakeLog entries, the oldest entry is dropped to make room for
// the new one.
func (h *Hook) RecordHandshake(attempt HandshakeAttempt) {
	attempt.At = time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handshakes = append(h.handshakes, attempt)
	if len(h.handshakes) > maxHandshakeLog {
		h.handshakes = h.handshakes[len(h.handshakes)-maxHandshakeLog:]
	}
}

// Snapshot returns a read only copy of the Hook's current state. Every
// returned slice, map-derived or otherwise, is a fresh copy: mutating the
// result does not affect the Hook's own state.
func (h *Hook) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients := make([]ClientSnapshot, 0, len(h.clients))
	for lfdi, c := range h.clients {
		paths := make([]string, 0, len(c.paths))
		for p := range c.paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		clients = append(clients, ClientSnapshot{
			LFDI:         lfdi,
			LastSeen:     c.lastSeen,
			RequestCount: c.requestCount,
			Paths:        paths,
		})
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].LFDI < clients[j].LFDI })

	handshakes := make([]HandshakeAttempt, len(h.handshakes))
	copy(handshakes, h.handshakes)

	return Snapshot{Clients: clients, Handshakes: handshakes}
}
