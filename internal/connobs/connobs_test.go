package connobs

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSnapshotOnZeroValueHookIsEmpty(t *testing.T) {
	t.Parallel()

	var h Hook
	snap := h.Snapshot()

	if len(snap.Clients) != 0 {
		t.Errorf("Snapshot().Clients = %+v, want empty", snap.Clients)
	}
	if len(snap.Handshakes) != 0 {
		t.Errorf("Snapshot().Handshakes = %+v, want empty", snap.Handshakes)
	}
}

// TestRecordRequestTracksLastSeenCountAndPaths drives three requests from
// one LFDI through RecordRequest and asserts the recorded field values:
// count increments, last-seen advances, and the path set accumulates
// without duplicates.
func TestRecordRequestTracksLastSeenCountAndPaths(t *testing.T) {
	t.Parallel()

	var h Hook
	const lfdi = "AABBCCDDEEFF00112233445566778899AABBCCDD"

	h.RecordRequest(lfdi, "/edev/0")
	first := h.Snapshot()
	if len(first.Clients) != 1 {
		t.Fatalf("Snapshot().Clients len = %d, want 1", len(first.Clients))
	}
	if first.Clients[0].LFDI != lfdi {
		t.Errorf("Clients[0].LFDI = %q, want %q", first.Clients[0].LFDI, lfdi)
	}
	if first.Clients[0].RequestCount != 1 {
		t.Errorf("Clients[0].RequestCount = %d, want 1", first.Clients[0].RequestCount)
	}
	if first.Clients[0].LastSeen.IsZero() {
		t.Error("Clients[0].LastSeen is zero, want a real timestamp")
	}
	if got := first.Clients[0].Paths; len(got) != 1 || got[0] != "/edev/0" {
		t.Errorf("Clients[0].Paths = %v, want [\"/edev/0\"]", got)
	}

	firstSeen := first.Clients[0].LastSeen
	time.Sleep(2 * time.Millisecond)

	h.RecordRequest(lfdi, "/edev/0") // duplicate path
	h.RecordRequest(lfdi, "/edev/0/der/1")

	second := h.Snapshot()
	if len(second.Clients) != 1 {
		t.Fatalf("Snapshot().Clients len = %d, want 1", len(second.Clients))
	}
	c := second.Clients[0]
	if c.RequestCount != 3 {
		t.Errorf("Clients[0].RequestCount = %d, want 3", c.RequestCount)
	}
	if !c.LastSeen.After(firstSeen) {
		t.Errorf("Clients[0].LastSeen = %v, want after %v", c.LastSeen, firstSeen)
	}
	if len(c.Paths) != 2 {
		t.Fatalf("Clients[0].Paths = %v, want exactly 2 distinct paths", c.Paths)
	}
	if c.Paths[0] != "/edev/0" || c.Paths[1] != "/edev/0/der/1" {
		t.Errorf("Clients[0].Paths = %v, want sorted [\"/edev/0\" \"/edev/0/der/1\"]", c.Paths)
	}
}

// TestRecordRequestKeysByLFDIIndependently confirms two distinct LFDIs
// get two distinct, independently-counted client records.
func TestRecordRequestKeysByLFDIIndependently(t *testing.T) {
	t.Parallel()

	var h Hook
	const lfdiA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const lfdiB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

	h.RecordRequest(lfdiA, "/edev/0")
	h.RecordRequest(lfdiB, "/edev/1")
	h.RecordRequest(lfdiB, "/edev/1")

	snap := h.Snapshot()
	if len(snap.Clients) != 2 {
		t.Fatalf("Snapshot().Clients len = %d, want 2", len(snap.Clients))
	}
	// sorted by LFDI: A before B
	if snap.Clients[0].LFDI != lfdiA || snap.Clients[0].RequestCount != 1 {
		t.Errorf("Clients[0] = %+v, want LFDI %q count 1", snap.Clients[0], lfdiA)
	}
	if snap.Clients[1].LFDI != lfdiB || snap.Clients[1].RequestCount != 2 {
		t.Errorf("Clients[1] = %+v, want LFDI %q count 2", snap.Clients[1], lfdiB)
	}
}

// TestRecordRequestEmptyLFDIIsANoOp confirms the fail-closed contract:
// calling RecordRequest with an empty LFDI (no verified identity) records
// nothing, rather than creating a bogus zero-key entry.
func TestRecordRequestEmptyLFDIIsANoOp(t *testing.T) {
	t.Parallel()

	var h Hook
	h.RecordRequest("", "/edev/0")

	snap := h.Snapshot()
	if len(snap.Clients) != 0 {
		t.Errorf("Snapshot().Clients = %+v, want empty after empty-LFDI RecordRequest", snap.Clients)
	}
}

// TestRecordHandshakeRecordsAcceptAndRejectWithReason asserts the exact
// field values of both an accepted and a rejected handshake attempt, per
// data-invariants: reject reason must be the actual reason, not a
// synthesized string, and Known must reflect the caller-supplied value
// exactly.
func TestRecordHandshakeRecordsAcceptAndRejectWithReason(t *testing.T) {
	t.Parallel()

	var h Hook
	const acceptedLFDI = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"

	h.RecordHandshake(HandshakeAttempt{
		LFDI:       acceptedLFDI,
		RemoteAddr: "10.0.0.5:54321",
		Accepted:   true,
		Known:      true,
	})
	h.RecordHandshake(HandshakeAttempt{
		LFDI:       "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD",
		RemoteAddr: "10.0.0.9:12345",
		Accepted:   false,
		Reason:     "x509: certificate signed by unknown authority",
		Known:      false,
	})

	snap := h.Snapshot()
	if len(snap.Handshakes) != 2 {
		t.Fatalf("Snapshot().Handshakes len = %d, want 2", len(snap.Handshakes))
	}

	accept := snap.Handshakes[0]
	if !accept.Accepted {
		t.Error("Handshakes[0].Accepted = false, want true")
	}
	if accept.LFDI != acceptedLFDI {
		t.Errorf("Handshakes[0].LFDI = %q, want %q", accept.LFDI, acceptedLFDI)
	}
	if accept.Reason != "" {
		t.Errorf("Handshakes[0].Reason = %q, want empty on an accepted attempt", accept.Reason)
	}
	if !accept.Known {
		t.Error("Handshakes[0].Known = false, want true")
	}
	if accept.RemoteAddr != "10.0.0.5:54321" {
		t.Errorf("Handshakes[0].RemoteAddr = %q, want %q", accept.RemoteAddr, "10.0.0.5:54321")
	}
	if accept.At.IsZero() {
		t.Error("Handshakes[0].At is zero, want a real timestamp")
	}

	reject := snap.Handshakes[1]
	if reject.Accepted {
		t.Error("Handshakes[1].Accepted = true, want false")
	}
	if reject.Reason != "x509: certificate signed by unknown authority" {
		t.Errorf("Handshakes[1].Reason = %q, want the exact verifier error", reject.Reason)
	}
	if reject.Known {
		t.Error("Handshakes[1].Known = true, want false (unrecognized cert)")
	}
}

// TestRecordHandshakeOverwritesCallerSuppliedAt confirms At is always
// stamped by the Hook itself (now), never trusted from the caller: a
// caller passing a stale or zero At must not see that value survive into
// the recorded snapshot.
func TestRecordHandshakeOverwritesCallerSuppliedAt(t *testing.T) {
	t.Parallel()

	var h Hook
	stale := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	h.RecordHandshake(HandshakeAttempt{LFDI: "EE", Accepted: true, At: stale})

	snap := h.Snapshot()
	if len(snap.Handshakes) != 1 {
		t.Fatalf("Snapshot().Handshakes len = %d, want 1", len(snap.Handshakes))
	}
	if snap.Handshakes[0].At.Equal(stale) {
		t.Error("Handshakes[0].At equals the caller-supplied stale value, want the Hook's own recording time")
	}
	if snap.Handshakes[0].At.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("Handshakes[0].At = %v, want close to now", snap.Handshakes[0].At)
	}
}

// TestRecordHandshakeCapsLogAtMaxHandshakeLog confirms the ring buffer
// drops the oldest entries once it reaches maxHandshakeLog, rather than
// growing without bound.
func TestRecordHandshakeCapsLogAtMaxHandshakeLog(t *testing.T) {
	t.Parallel()

	var h Hook
	for i := 0; i < maxHandshakeLog+10; i++ {
		h.RecordHandshake(HandshakeAttempt{
			LFDI:     fmt.Sprintf("LFDI-%d", i),
			Accepted: true,
		})
	}

	snap := h.Snapshot()
	if len(snap.Handshakes) != maxHandshakeLog {
		t.Fatalf("Snapshot().Handshakes len = %d, want %d", len(snap.Handshakes), maxHandshakeLog)
	}
	// The oldest 10 entries (LFDI-0 through LFDI-9) must have been
	// dropped; the buffer must start at LFDI-10.
	if want := "LFDI-10"; snap.Handshakes[0].LFDI != want {
		t.Errorf("Handshakes[0].LFDI = %q, want %q (oldest entries dropped)", snap.Handshakes[0].LFDI, want)
	}
	if want := fmt.Sprintf("LFDI-%d", maxHandshakeLog+9); snap.Handshakes[len(snap.Handshakes)-1].LFDI != want {
		t.Errorf("Handshakes[last].LFDI = %q, want %q (newest entry kept)", snap.Handshakes[len(snap.Handshakes)-1].LFDI, want)
	}
}

// TestRecordRequestCapsPathsPerClient confirms a single LFDI's recorded
// path set stops growing once it reaches maxPathsPerClient: RequestCount
// keeps incrementing (the request itself still happened), but distinct
// paths beyond the cap are not added.
func TestRecordRequestCapsPathsPerClient(t *testing.T) {
	t.Parallel()

	var h Hook
	const lfdi = "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"

	for i := 0; i < maxPathsPerClient+10; i++ {
		h.RecordRequest(lfdi, fmt.Sprintf("/edev/%d", i))
	}

	snap := h.Snapshot()
	if len(snap.Clients) != 1 {
		t.Fatalf("Snapshot().Clients len = %d, want 1", len(snap.Clients))
	}
	c := snap.Clients[0]
	if len(c.Paths) != maxPathsPerClient {
		t.Fatalf("Clients[0].Paths len = %d, want %d (capped)", len(c.Paths), maxPathsPerClient)
	}
	if want := uint64(maxPathsPerClient + 10); c.RequestCount != want {
		t.Errorf("Clients[0].RequestCount = %d, want %d (every request still counted, even once paths cap is hit)", c.RequestCount, want)
	}
}

// TestRecordRequestEvictsLeastRecentlySeenClientAtCap confirms tracked
// clients are capped at maxTrackedClients: once the cap is reached, the
// least-recently-seen client is evicted to make room for a new one,
// rather than the map growing without bound.
func TestRecordRequestEvictsLeastRecentlySeenClientAtCap(t *testing.T) {
	t.Parallel()

	var h Hook

	// Fill to exactly the cap, each with a distinct, strictly increasing
	// LastSeen (sequential RecordRequest calls under the Hook's own
	// mutex already guarantee strictly increasing wall-clock stamps
	// across distinct calls; no synthetic Sleep is needed for ordering).
	for i := 0; i < maxTrackedClients; i++ {
		h.RecordRequest(fmt.Sprintf("LFDI-%04d", i), "/edev/0")
	}
	if got := len(h.Snapshot().Clients); got != maxTrackedClients {
		t.Fatalf("after filling to cap: Snapshot().Clients len = %d, want %d", got, maxTrackedClients)
	}

	// LFDI-0000 is the least-recently-seen tracked client at this point
	// (it was recorded first and never touched again). One more distinct
	// LFDI must evict it, not grow the map past the cap.
	h.RecordRequest("LFDI-NEW", "/edev/0")

	snap := h.Snapshot()
	if len(snap.Clients) != maxTrackedClients {
		t.Fatalf("Snapshot().Clients len = %d, want %d (capped, not grown)", len(snap.Clients), maxTrackedClients)
	}
	for _, c := range snap.Clients {
		if c.LFDI == "LFDI-0000" {
			t.Fatalf("LFDI-0000 (least-recently-seen) is still tracked; want it evicted to make room for LFDI-NEW")
		}
	}
	found := false
	for _, c := range snap.Clients {
		if c.LFDI == "LFDI-NEW" {
			found = true
		}
	}
	if !found {
		t.Fatal("LFDI-NEW is not tracked; want it recorded after evicting the least-recently-seen client")
	}
}

// TestSnapshotReturnsIndependentCopies confirms mutating the returned
// Snapshot's slices does not affect the Hook's own state, matching
// controlobs.Snapshot's independent-copy contract.
func TestSnapshotReturnsIndependentCopies(t *testing.T) {
	t.Parallel()

	var h Hook
	h.RecordRequest("AAAA", "/edev/0")
	h.RecordHandshake(HandshakeAttempt{LFDI: "AAAA", Accepted: true})

	snap := h.Snapshot()
	snap.Clients[0].Paths[0] = "mutated"
	snap.Handshakes[0].LFDI = "mutated"

	again := h.Snapshot()
	if again.Clients[0].Paths[0] != "/edev/0" {
		t.Errorf("Hook state mutated via caller's Clients slice: Paths[0] = %q, want %q", again.Clients[0].Paths[0], "/edev/0")
	}
	if again.Handshakes[0].LFDI != "AAAA" {
		t.Errorf("Hook state mutated via caller's Handshakes slice: LFDI = %q, want %q", again.Handshakes[0].LFDI, "AAAA")
	}
}

// TestConcurrentRecordRequestAndHandshakeIsRaceFree drives many goroutines
// against RecordRequest and RecordHandshake concurrently (run with
// -race). It asserts the final aggregate counts are exactly what was
// recorded: correctness under concurrency, not just absence of a crash.
func TestConcurrentRecordRequestAndHandshakeIsRaceFree(t *testing.T) {
	t.Parallel()

	var h Hook
	const goroutines = 50
	const perGoroutine = 40

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			lfdi := fmt.Sprintf("LFDI-%02d", g%5) // 5 distinct LFDIs, contended
			for i := 0; i < perGoroutine; i++ {
				h.RecordRequest(lfdi, fmt.Sprintf("/edev/%d", i%3))
				h.RecordHandshake(HandshakeAttempt{LFDI: lfdi, Accepted: true, Known: true})
			}
		}(g)
	}
	wg.Wait()

	snap := h.Snapshot()
	if len(snap.Clients) != 5 {
		t.Fatalf("Snapshot().Clients len = %d, want 5 distinct LFDIs", len(snap.Clients))
	}
	var totalRequests uint64
	for _, c := range snap.Clients {
		totalRequests += c.RequestCount
	}
	wantRequests := uint64(goroutines * perGoroutine)
	if totalRequests != wantRequests {
		t.Errorf("total RequestCount across clients = %d, want %d", totalRequests, wantRequests)
	}
	if len(snap.Handshakes) != maxHandshakeLog {
		t.Errorf("Snapshot().Handshakes len = %d, want %d (capped)", len(snap.Handshakes), maxHandshakeLog)
	}
}
