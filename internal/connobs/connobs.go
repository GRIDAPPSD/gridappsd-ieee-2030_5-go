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
//     verification hook, once for each connection attempt that reaches
//     certificate verification, i.e. one where a client presented a
//     certificate and chain-building ran, whether that chain was
//     ultimately accepted or rejected. This is NOT every connection
//     attempt: crypto/tls only calls VerifyPeerCertificate after a
//     client certificate has been presented, so a client that presents
//     no certificate at all, or a connection that fails TLS negotiation
//     before any certificate is exchanged (protocol version mismatch, no
//     shared cipher suite, and so on), aborts before this hook ever runs
//     and is therefore not recorded here. A rejected handshake that DOES
//     reach this hook never reaches RecordRequest (the connection is
//     closed before any HTTP request is read), so for that subset of
//     rejections, this is the only way they become observable at all.
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

const (
	// maxHandshakeLog bounds the handshake attempt ring buffer so a
	// long-running bridge under repeated failed-handshake probing (or a
	// misconfigured client retrying rapidly) cannot grow this log without
	// bound. Oldest entries are dropped first once the log is full.
	maxHandshakeLog = 200

	// maxTrackedClients bounds the number of distinct LFDIs RecordRequest
	// will track at once. connObserveMiddleware runs outside sep2embed's
	// ACL (see auth.go's doc comment on why: it answers "did this LFDI
	// reach the listener", not "was this request authorized"), so a
	// valid-cert device that presents a distinct certificate on every
	// connection would otherwise grow this map without bound. Once the
	// cap is reached, RecordRequest evicts the least-recently-seen
	// tracked client to make room for a new one.
	maxTrackedClients = 500

	// maxPathsPerClient bounds the number of distinct resource paths
	// recorded per tracked client. The same unauthenticated-probing
	// concern that motivates maxTrackedClients applies per path: a single
	// LFDI probing many distinct paths must not grow its own paths set
	// without bound. Paths beyond the cap are simply not added (the
	// already-recorded set and the request count are unaffected).
	maxPathsPerClient = 50
)

// ClientSnapshot is a read only copy of one connected client's recorded
// state.
type ClientSnapshot struct {
	LFDI string

	// LastSeen is the wall-clock time (UTC) of the most recently recorded
	// request from this LFDI.
	LastSeen time.Time

	// Age is how long before this Snapshot the last request was recorded,
	// by the Hook's clock. It is never negative.
	Age time.Duration

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
// seen is a monotonically increasing per-Hook sequence number stamped at
// every RecordRequest for this client, used only to pick the
// least-recently-seen client on eviction: it breaks ties that lastSeen's
// wall-clock resolution could otherwise leave ambiguous between two
// requests recorded in rapid succession.
type clientState struct {
	lastSeen     time.Time
	requestCount uint64
	paths        map[string]struct{}
	seen         uint64
}

// Hook is the mutex-guarded observation point. The zero value is ready to
// use: no constructor is required.
type Hook struct {
	// Now is the clock for recorded times and ages; nil means time.Now.
	// Set it before the Hook is shared, as tests do to control time.
	Now func() time.Time

	mu         sync.Mutex
	clients    map[string]*clientState
	handshakes []HandshakeAttempt
	seenSeq    uint64
}

func (h *Hook) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// RecordRequest records one request from lfdi against path: increments
// that LFDI's cumulative request count, updates its last-seen time to
// now, and adds path to its recorded path set (a no-op if path was
// already recorded for this LFDI, or if that LFDI's path set is already
// at maxPathsPerClient). lfdi must be non-empty; callers with no
// verified identity (see sep2embed's identityMiddleware fail-closed
// path) must not call this, since there is nothing to key the record
// by.
//
// Tracking a never-seen-before lfdi when the client map is already at
// maxTrackedClients evicts the least-recently-seen tracked client first,
// per this package's doc comment on maxTrackedClients.
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
		if len(h.clients) >= maxTrackedClients {
			h.evictOldestLocked()
		}
		c = &clientState{paths: make(map[string]struct{})}
		h.clients[lfdi] = c
	}
	c.requestCount++
	c.lastSeen = h.now()
	h.seenSeq++
	c.seen = h.seenSeq
	if _, seen := c.paths[path]; !seen && len(c.paths) < maxPathsPerClient {
		c.paths[path] = struct{}{}
	}
}

// evictOldestLocked removes the tracked client with the smallest seen
// sequence number (i.e. the least-recently-seen client) from h.clients,
// making room for one more tracked client under maxTrackedClients.
// Callers must hold h.mu. A no-op on an empty h.clients (defensive only:
// RecordRequest never calls this unless len(h.clients) >=
// maxTrackedClients, which is never true when h.clients is empty).
func (h *Hook) evictOldestLocked() {
	var oldestLFDI string
	var oldestSeen uint64
	found := false
	for lfdi, c := range h.clients {
		if !found || c.seen < oldestSeen {
			oldestLFDI, oldestSeen = lfdi, c.seen
			found = true
		}
	}
	if found {
		delete(h.clients, oldestLFDI)
	}
}

// RecordHandshake appends attempt to the handshake log, stamping At with
// the current time (any caller-supplied At is overwritten, so the
// recorded time always reflects when this hook observed the attempt, not
// when the caller happened to construct the value). Once the log reaches
// maxHandshakeLog entries, the oldest entry is dropped to make room for
// the new one.
func (h *Hook) RecordHandshake(attempt HandshakeAttempt) {
	attempt.At = h.now()
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

	now := h.now()
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
			Age:          max(now.Sub(c.lastSeen), 0),
			RequestCount: c.requestCount,
			Paths:        paths,
		})
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].LFDI < clients[j].LFDI })

	handshakes := make([]HandshakeAttempt, len(h.handshakes))
	copy(handshakes, h.handshakes)

	return Snapshot{Clients: clients, Handshakes: handshakes}
}
