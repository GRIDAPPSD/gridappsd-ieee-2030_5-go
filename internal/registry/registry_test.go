package registry

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// testLFDI derives a deterministic, exactly-40-character uppercase hex
// LFDI from an arbitrary distinguishing tag, so fixtures can keep
// readable, distinct seeds ("lfdi-old", "lfdi-new", "seed-lfdi-3") while
// still satisfying validateEntry's canonical LFDI shape.
// SHA-1 is used only as a deterministic 20-byte hash, not for any
// security property; a fixed tag always maps to the same value, and
// distinct tags map to distinct values (collision probability is
// negligible at test-suite scale).
func testLFDI(tag string) string {
	sum := sha1.Sum([]byte(tag))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func TestNewIsEmpty(t *testing.T) {
	t.Parallel()

	r := New()
	if got := r.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
	if snap := r.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot() length = %d, want 0", len(snap))
	}
}

func TestAddHappyPath(t *testing.T) {
	t.Parallel()

	r := New()
	e := Entry{MRID: "mrid-1", Name: "House One", LFDI: testLFDI("lfdi-1")}
	if err := r.Add(e); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	if got, ok := r.LFDI("mrid-1"); !ok || got != testLFDI("lfdi-1") {
		t.Errorf("LFDI(mrid-1) = (%q, %t), want (lfdi-1, true)", got, ok)
	}
	if got, ok := r.MRID(testLFDI("lfdi-1")); !ok || got != "mrid-1" {
		t.Errorf("MRID(lfdi-1) = (%q, %t), want (mrid-1, true)", got, ok)
	}
	if got, ok := r.Get("mrid-1"); !ok || got != e {
		t.Errorf("Get(mrid-1) = (%+v, %t), want (%+v, true)", got, ok, e)
	}
	if got, ok := r.Name("mrid-1"); !ok || got != "House One" {
		t.Errorf("Name(mrid-1) = (%q, %t), want (House One, true)", got, ok)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
}

func TestAddAllowsEmptyName(t *testing.T) {
	t.Parallel()

	r := New()
	e := Entry{MRID: "mrid-1", Name: "", LFDI: testLFDI("lfdi-1")}
	if err := r.Add(e); err != nil {
		t.Fatalf("Add with empty Name returned error: %v", err)
	}
	if got, ok := r.Name("mrid-1"); !ok || got != "" {
		t.Errorf("Name(mrid-1) = (%q, %t), want (\"\", true)", got, ok)
	}
}

func TestAddValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry Entry
	}{
		{"empty mRID", Entry{MRID: "", Name: "X", LFDI: "lfdi-1"}},
		{"empty LFDI", Entry{MRID: "mrid-1", Name: "X", LFDI: ""}},
		{"both empty", Entry{MRID: "", Name: "X", LFDI: ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := New()
			err := r.Add(tt.entry)
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("Add(%+v) error = %v, want ErrInvalidEntry", tt.entry, err)
			}
			if got := r.Len(); got != 0 {
				t.Errorf("Len() after invalid Add = %d, want 0", got)
			}
		})
	}
}

func TestAddReplacement(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Add(Entry{MRID: "mrid-1", Name: "Old", LFDI: testLFDI("lfdi-old")}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := r.Add(Entry{MRID: "mrid-1", Name: "New", LFDI: testLFDI("lfdi-new")}); err != nil {
		t.Fatalf("replacement Add: %v", err)
	}

	if got := r.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 after replacement", got)
	}

	if got, ok := r.LFDI("mrid-1"); !ok || got != testLFDI("lfdi-new") {
		t.Errorf("LFDI(mrid-1) = (%q, %t), want (lfdi-new, true)", got, ok)
	}
	if got, ok := r.MRID(testLFDI("lfdi-new")); !ok || got != "mrid-1" {
		t.Errorf("MRID(lfdi-new) = (%q, %t), want (mrid-1, true)", got, ok)
	}

	// The old LFDI must no longer reverse-resolve.
	if got, ok := r.MRID(testLFDI("lfdi-old")); ok {
		t.Errorf("MRID(lfdi-old) = (%q, true), want (\"\", false) after replacement", got)
	}
	if got, ok := r.Name("mrid-1"); !ok || got != "New" {
		t.Errorf("Name(mrid-1) = (%q, %t), want (New, true)", got, ok)
	}
}

func TestAddBatchHappyPath(t *testing.T) {
	t.Parallel()

	r := New()
	batch := []Entry{
		{MRID: "mrid-1", Name: "One", LFDI: testLFDI("lfdi-1")},
		{MRID: "mrid-2", Name: "Two", LFDI: testLFDI("lfdi-2")},
		{MRID: "mrid-3", Name: "", LFDI: testLFDI("lfdi-3")},
	}
	if err := r.AddBatch(batch); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if got := r.Len(); got != len(batch) {
		t.Fatalf("Len() = %d, want %d", got, len(batch))
	}
	for _, e := range batch {
		if got, ok := r.Get(e.MRID); !ok || got != e {
			t.Errorf("Get(%q) = (%+v, %t), want (%+v, true)", e.MRID, got, ok, e)
		}
		if got, ok := r.MRID(e.LFDI); !ok || got != e.MRID {
			t.Errorf("MRID(%q) = (%q, %t), want (%q, true)", e.LFDI, got, ok, e.MRID)
		}
	}
}

func TestAddBatchEmpty(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.AddBatch(nil); err != nil {
		t.Errorf("AddBatch(nil): %v", err)
	}
	if err := r.AddBatch([]Entry{}); err != nil {
		t.Errorf("AddBatch([]Entry{}): %v", err)
	}
	if got := r.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestAddBatchRollsBackOnInvalidEntry(t *testing.T) {
	t.Parallel()

	r := New()
	// Pre-existing entries that must remain untouched on rollback.
	pre := Entry{MRID: "mrid-pre", Name: "Pre", LFDI: testLFDI("lfdi-pre")}
	if err := r.Add(pre); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	bad := []Entry{
		{MRID: "mrid-a", Name: "A", LFDI: testLFDI("lfdi-a")},
		{MRID: "", Name: "B", LFDI: testLFDI("lfdi-b")}, // invalid: empty MRID
		{MRID: "mrid-c", Name: "C", LFDI: testLFDI("lfdi-c")},
	}

	err := r.AddBatch(bad)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("AddBatch error = %v, want ErrInvalidEntry", err)
	}

	// Pre-existing entry must still be present and unchanged.
	if got, ok := r.Get("mrid-pre"); !ok || got != pre {
		t.Errorf("seed entry lost or mutated after rollback: (%+v, %t)", got, ok)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 (only the seed entry)", got)
	}

	// None of the batch entries must be present, including the valid ones
	// that came before the invalid one.
	for _, m := range []string{"mrid-a", "mrid-c"} {
		if _, ok := r.Get(m); ok {
			t.Errorf("Get(%q) succeeded after rollback, want absent", m)
		}
	}
	for _, l := range []string{testLFDI("lfdi-a"), testLFDI("lfdi-c")} {
		if _, ok := r.MRID(l); ok {
			t.Errorf("MRID(%q) succeeded after rollback, want absent", l)
		}
	}
}

func TestAddBatchAtomicReplacementRollsBack(t *testing.T) {
	t.Parallel()

	// If a batch contains a valid replacement of an existing entry but a
	// later entry is invalid, the replacement must also roll back; the
	// original entry must remain intact.
	r := New()
	original := Entry{MRID: "mrid-1", Name: "Original", LFDI: testLFDI("lfdi-original")}
	if err := r.Add(original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	batch := []Entry{
		{MRID: "mrid-1", Name: "Replacement", LFDI: testLFDI("lfdi-replacement")},
		{MRID: "mrid-2", Name: "Two", LFDI: ""}, // invalid
	}
	err := r.AddBatch(batch)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("AddBatch error = %v, want ErrInvalidEntry", err)
	}

	if got, ok := r.Get("mrid-1"); !ok || got != original {
		t.Errorf("Get(mrid-1) = (%+v, %t), want (%+v, true) (rollback failed)", got, ok, original)
	}
	if got, ok := r.MRID(testLFDI("lfdi-original")); !ok || got != "mrid-1" {
		t.Errorf("MRID(lfdi-original) = (%q, %t), want (mrid-1, true)", got, ok)
	}
	if _, ok := r.MRID(testLFDI("lfdi-replacement")); ok {
		t.Errorf("MRID(lfdi-replacement) succeeded after rollback, want absent")
	}
}

func TestRemoveHappyPath(t *testing.T) {
	t.Parallel()

	r := New()
	e := Entry{MRID: "mrid-1", Name: "House", LFDI: testLFDI("lfdi-1")}
	if err := r.Add(e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := r.Remove("mrid-1")
	if !ok || got != e {
		t.Errorf("Remove(mrid-1) = (%+v, %t), want (%+v, true)", got, ok, e)
	}
	if _, ok := r.Get("mrid-1"); ok {
		t.Errorf("Get(mrid-1) found entry after Remove")
	}
	if _, ok := r.MRID(testLFDI("lfdi-1")); ok {
		t.Errorf("MRID(lfdi-1) found entry after Remove (reverse index leak)")
	}
	if got := r.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestRemoveUnknownIsNoOp(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Add(Entry{MRID: "mrid-1", Name: "X", LFDI: testLFDI("lfdi-1")}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := r.Remove("does-not-exist")
	if ok {
		t.Errorf("Remove(unknown) ok = true, want false")
	}
	if got != (Entry{}) {
		t.Errorf("Remove(unknown) entry = %+v, want zero Entry", got)
	}
	if n := r.Len(); n != 1 {
		t.Errorf("Len() = %d, want 1", n)
	}
}

func TestLenTracksAcrossOperations(t *testing.T) {
	t.Parallel()

	r := New()
	if got := r.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
	if err := r.Add(Entry{MRID: "m1", LFDI: testLFDI("l1")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len() after Add = %d, want 1", got)
	}
	if err := r.AddBatch([]Entry{
		{MRID: "m2", LFDI: testLFDI("l2")},
		{MRID: "m3", LFDI: testLFDI("l3")},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if got := r.Len(); got != 3 {
		t.Errorf("Len() after AddBatch = %d, want 3", got)
	}
	if _, ok := r.Remove("m2"); !ok {
		t.Fatalf("Remove(m2) returned ok=false")
	}
	if got := r.Len(); got != 2 {
		t.Errorf("Len() after Remove = %d, want 2", got)
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	t.Parallel()

	r := New()
	entries := []Entry{
		{MRID: "m1", Name: "One", LFDI: testLFDI("l1")},
		{MRID: "m2", Name: "Two", LFDI: testLFDI("l2")},
	}
	if err := r.AddBatch(entries); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	snap := r.Snapshot()
	if got, want := len(snap), r.Len(); got != want {
		t.Fatalf("Snapshot length = %d, Len() = %d", got, want)
	}

	// Mutating the snapshot must not affect the registry.
	for i := range snap {
		snap[i].Name = "MUTATED"
	}

	if got, _ := r.Get("m1"); got.Name != "One" {
		t.Errorf("Registry mutated through snapshot: m1.Name = %q, want One", got.Name)
	}
	if got, _ := r.Get("m2"); got.Name != "Two" {
		t.Errorf("Registry mutated through snapshot: m2.Name = %q, want Two", got.Name)
	}

	// Truncating the snapshot must not affect the registry either.
	snap = snap[:0]
	if got := r.Len(); got != len(entries) {
		t.Errorf("Len() after snapshot truncation = %d, want %d", got, len(entries))
	}
}

func TestLookupMissReturnsZeroAndFalse(t *testing.T) {
	t.Parallel()

	r := New()
	if got, ok := r.LFDI("nope"); ok || got != "" {
		t.Errorf("LFDI(nope) = (%q, %t), want (\"\", false)", got, ok)
	}
	if got, ok := r.MRID("nope"); ok || got != "" {
		t.Errorf("MRID(nope) = (%q, %t), want (\"\", false)", got, ok)
	}
	if got, ok := r.Get("nope"); ok || got != (Entry{}) {
		t.Errorf("Get(nope) = (%+v, %t), want (zero, false)", got, ok)
	}
	if got, ok := r.Name("nope"); ok || got != "" {
		t.Errorf("Name(nope) = (%q, %t), want (\"\", false)", got, ok)
	}
}

// TestConcurrentAccess hammers the registry with multiple goroutines
// performing reads and writes. Designed to be run under -race; the goal
// is to flush any unprotected map access into a race-detector flag.
//
// Errors from goroutines flow through a buffered channel; the test waits
// on a WaitGroup so no goroutine outlives the test.
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	const (
		writers = 4
		readers = 8
		opsEach = 200
	)

	r := New()

	// Seed so readers have something to find.
	const seedCount = 64
	for i := 0; i < seedCount; i++ {
		e := Entry{
			MRID: fmt.Sprintf("seed-mrid-%d", i),
			Name: fmt.Sprintf("seed-%d", i),
			LFDI: testLFDI(fmt.Sprintf("seed-lfdi-%d", i)),
		}
		if err := r.Add(e); err != nil {
			t.Fatalf("seed Add: %v", err)
		}
	}

	errs := make(chan error, writers+readers)
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				e := Entry{
					MRID: fmt.Sprintf("w%d-mrid-%d", w, i),
					Name: fmt.Sprintf("w%d-%d", w, i),
					LFDI: testLFDI(fmt.Sprintf("w%d-lfdi-%d", w, i)),
				}
				if err := r.Add(e); err != nil {
					errs <- fmt.Errorf("writer %d Add: %w", w, err)
					return
				}
				if i%4 == 0 {
					_, _ = r.Remove(e.MRID)
				}
			}
		}()
	}

	for ri := 0; ri < readers; ri++ {
		ri := ri
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				_, _ = r.LFDI(fmt.Sprintf("seed-mrid-%d", i%seedCount))
				_, _ = r.MRID(testLFDI(fmt.Sprintf("seed-lfdi-%d", i%seedCount)))
				_, _ = r.Get(fmt.Sprintf("seed-mrid-%d", i%seedCount))
				_, _ = r.Name(fmt.Sprintf("seed-mrid-%d", i%seedCount))
				_ = r.Len()
				if i%32 == 0 {
					_ = r.Snapshot()
				}
				_ = ri
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent op error: %v", err)
	}

	// After the storm, the seed entries must still be intact.
	for i := 0; i < seedCount; i++ {
		want := Entry{
			MRID: fmt.Sprintf("seed-mrid-%d", i),
			Name: fmt.Sprintf("seed-%d", i),
			LFDI: testLFDI(fmt.Sprintf("seed-lfdi-%d", i)),
		}
		got, ok := r.Get(want.MRID)
		if !ok || got != want {
			t.Errorf("seed entry %d: Get = (%+v, %t), want (%+v, true)", i, got, ok, want)
		}
	}
}

// TestEntryPlaceholderRoundTrip verifies that Entry.Placeholder is
// preserved through Add, AddBatch, Get, and Snapshot. The flag is
// informational and does not affect lookup behavior; this test pins
// the round-trip so a future change cannot quietly drop it.
func TestEntryPlaceholderRoundTrip(t *testing.T) {
	t.Parallel()

	r := New()
	real := Entry{MRID: "mrid-real", Name: "Real Device", LFDI: testLFDI("lfdi-real"), Placeholder: false}
	stub := Entry{MRID: "mrid-stub", Name: "Stage 1 Stub", LFDI: testLFDI("lfdi-stub"), Placeholder: true}

	if err := r.Add(real); err != nil {
		t.Fatalf("Add(real): %v", err)
	}
	if err := r.Add(stub); err != nil {
		t.Fatalf("Add(stub): %v", err)
	}

	if got, ok := r.Get("mrid-real"); !ok || got != real {
		t.Errorf("Get(mrid-real) = (%+v, %t), want (%+v, true)", got, ok, real)
	}
	if got, ok := r.Get("mrid-stub"); !ok || got != stub {
		t.Errorf("Get(mrid-stub) = (%+v, %t), want (%+v, true)", got, ok, stub)
	}

	// Snapshot must preserve Placeholder on every entry.
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot length = %d, want 2", len(snap))
	}
	for _, e := range snap {
		switch e.MRID {
		case "mrid-real":
			if e.Placeholder {
				t.Errorf("snapshot entry mrid-real Placeholder = true, want false")
			}
		case "mrid-stub":
			if !e.Placeholder {
				t.Errorf("snapshot entry mrid-stub Placeholder = false, want true")
			}
		default:
			t.Errorf("unexpected snapshot entry: %+v", e)
		}
	}
}

// TestEntryPlaceholderAddBatchRoundTrip verifies the same flag
// preservation through the AddBatch path.
func TestEntryPlaceholderAddBatchRoundTrip(t *testing.T) {
	t.Parallel()

	r := New()
	batch := []Entry{
		{MRID: "m1", Name: "One", LFDI: testLFDI("l1"), Placeholder: true},
		{MRID: "m2", Name: "Two", LFDI: testLFDI("l2"), Placeholder: false},
	}
	if err := r.AddBatch(batch); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	for _, want := range batch {
		got, ok := r.Get(want.MRID)
		if !ok || got != want {
			t.Errorf("Get(%q) = (%+v, %t), want (%+v, true)", want.MRID, got, ok, want)
		}
	}
}

// TestEntryPlaceholderReplacementUpdatesFlag verifies that a Stage 2
// re-add with Placeholder=false overwrites a Stage 1 placeholder entry.
func TestEntryPlaceholderReplacementUpdatesFlag(t *testing.T) {
	t.Parallel()

	r := New()
	stage1 := Entry{MRID: "m1", Name: "Device", LFDI: testLFDI("lfdi-stage1"), Placeholder: true}
	if err := r.Add(stage1); err != nil {
		t.Fatalf("Add stage1: %v", err)
	}

	stage2 := Entry{MRID: "m1", Name: "Device", LFDI: testLFDI("lfdi-stage2"), Placeholder: false}
	if err := r.Add(stage2); err != nil {
		t.Fatalf("Add stage2: %v", err)
	}

	got, ok := r.Get("m1")
	if !ok {
		t.Fatal("Get(m1) returned ok=false after replacement")
	}
	if got.Placeholder {
		t.Errorf("Placeholder = true after replacement, want false")
	}
	if got.LFDI != testLFDI("lfdi-stage2") {
		t.Errorf("LFDI = %q, want lfdi-stage2", got.LFDI)
	}
}

// TestMRIDResolvesCanonicalLFDI proves the canonical-LFDI reverse index
// (MRID) resolves an entry's LFDI back to its mRID, and reports not found
// for an unregistered LFDI.
func TestMRIDResolvesCanonicalLFDI(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Add(Entry{MRID: "m1", LFDI: testLFDI("CANON")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got, ok := r.MRID(testLFDI("CANON")); !ok || got != "m1" {
		t.Errorf("MRID(CANON) = (%q, %t), want (m1, true)", got, ok)
	}
	if _, ok := r.MRID("unknown"); ok {
		t.Error("MRID(unknown) ok = true, want false")
	}
}

// TestLFDIIndexMutationIntegrity proves the canonical LFDI reverse index
// stays consistent through Add-replacement and Remove: a re-add that
// changes a device's LFDI leaves no stale entry resolving to the mRID,
// and a Remove clears the entry too.
func TestLFDIIndexMutationIntegrity(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Add(Entry{MRID: "m1", LFDI: testLFDI("CANON-OLD")}); err != nil {
		t.Fatalf("Add old: %v", err)
	}
	if _, ok := r.MRID(testLFDI("CANON-OLD")); !ok {
		t.Fatal("MRID(CANON-OLD) not found after initial Add")
	}

	// Re-add with a changed LFDI: the old LFDI must no longer resolve.
	if err := r.Add(Entry{MRID: "m1", LFDI: testLFDI("CANON-NEW")}); err != nil {
		t.Fatalf("Add new: %v", err)
	}
	if _, ok := r.MRID(testLFDI("CANON-OLD")); ok {
		t.Error("MRID(CANON-OLD) still resolves after LFDI change; stale index entry")
	}
	got, ok := r.MRID(testLFDI("CANON-NEW"))
	if !ok || got != "m1" {
		t.Errorf("MRID(CANON-NEW) = (%q, %t), want (m1, true)", got, ok)
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1 after LFDI-changing replacement", r.Len())
	}

	// Remove must clear the LFDI entry.
	if _, ok := r.Remove("m1"); !ok {
		t.Fatal("Remove(m1) ok = false")
	}
	if _, ok := r.MRID(testLFDI("CANON-NEW")); ok {
		t.Error("MRID(CANON-NEW) still resolves after Remove; index leak")
	}
}

// TestSnapshotConcurrentSafe verifies that Snapshot, taken while writes
// are in flight, returns a consistent independent copy. Race detector is
// the primary signal here; we also do light correctness checks.
func TestSnapshotConcurrentSafe(t *testing.T) {
	t.Parallel()

	r := New()
	for i := 0; i < 32; i++ {
		if err := r.Add(Entry{
			MRID: fmt.Sprintf("m%d", i),
			LFDI: testLFDI(fmt.Sprintf("l%d", i)),
		}); err != nil {
			t.Fatalf("seed Add: %v", err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 1000
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.Add(Entry{
					MRID: fmt.Sprintf("hot-m%d", i),
					LFDI: testLFDI(fmt.Sprintf("hot-l%d", i)),
				})
				_, _ = r.Remove(fmt.Sprintf("hot-m%d", i-1))
				i++
			}
		}
	}()

	for i := 0; i < 50; i++ {
		snap := r.Snapshot()
		// Iterate the snapshot; if it is a view rather than a copy,
		// the concurrent Add/Remove will race on the slice and the
		// detector will fire.
		for _, e := range snap {
			if e.MRID == "" {
				t.Errorf("snapshot contained zero MRID entry: %+v", e)
			}
		}
	}

	close(stop)
	wg.Wait()
}

// TestValidateEntryRejectsMalformedMRID pins the MRID rejection
// paths: leading/trailing whitespace and embedded control characters are
// both rejected via ErrInvalidEntry, distinct from the pre-existing
// empty-MRID case already covered by TestAddValidation.
func TestValidateEntryRejectsMalformedMRID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mrid string
	}{
		{"leading whitespace", " mrid-1"},
		{"trailing whitespace", "mrid-1 "},
		{"leading and trailing whitespace", " mrid-1 "},
		{"embedded newline", "mrid-1\n"},
		{"embedded tab", "mrid\t-1"},
		{"embedded null byte", "mrid-1\x00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := New()
			err := r.Add(Entry{MRID: tc.mrid, LFDI: testLFDI("lfdi-1")})
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("Add(MRID=%q) error = %v, want ErrInvalidEntry", tc.mrid, err)
			}
			if got := r.Len(); got != 0 {
				t.Errorf("Len() after invalid Add = %d, want 0", got)
			}
		})
	}
}

// TestValidateEntryRejectsMalformedLFDI pins the LFDI allowlist:
// only exactly-40-character uppercase hex is accepted. Too short, too
// long, lowercase, and non-hex characters are each rejected via
// ErrInvalidEntry, distinct from the pre-existing empty-LFDI case already
// covered by TestAddValidation.
func TestValidateEntryRejectsMalformedLFDI(t *testing.T) {
	t.Parallel()

	valid := testLFDI("lfdi-1") // exactly 40 uppercase hex characters

	cases := []struct {
		name string
		lfdi string
	}{
		{"too short", valid[:39]},
		{"too long", valid + "A"},
		{"lowercase", strings.ToLower(valid)},
		{"non-hex character", "G" + valid[1:]},
		{"hex-looking but wrong length (39)", valid[:20] + valid[21:]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := New()
			err := r.Add(Entry{MRID: "mrid-1", LFDI: tc.lfdi})
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("Add(LFDI=%q) error = %v, want ErrInvalidEntry", tc.lfdi, err)
			}
			if got := r.Len(); got != 0 {
				t.Errorf("Len() after invalid Add = %d, want 0", got)
			}
		})
	}
}

// TestWithMaxEntriesDefaultIsUnlimited proves a Registry constructed with
// no options, or explicitly with WithMaxEntries(0) or a negative value,
// never returns ErrRegistryFull no matter how many distinct entries are
// added: this is the backward-compatibility contract for every
// caller of New() from before this option existed.
func TestWithMaxEntriesDefaultIsUnlimited(t *testing.T) {
	t.Parallel()

	ctors := []struct {
		name string
		reg  *Registry
	}{
		{"no options", New()},
		{"WithMaxEntries(0)", New(WithMaxEntries(0))},
		{"WithMaxEntries(-1)", New(WithMaxEntries(-1))},
	}

	for _, tc := range ctors {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 500; i++ {
				e := Entry{
					MRID: fmt.Sprintf("mrid-%d", i),
					LFDI: testLFDI(fmt.Sprintf("unlimited-lfdi-%d", i)),
				}
				if err := tc.reg.Add(e); err != nil {
					t.Fatalf("Add entry %d: %v, want no error (unlimited registry)", i, err)
				}
			}
			if got := tc.reg.Len(); got != 500 {
				t.Errorf("Len() = %d, want 500", got)
			}
		})
	}
}

// TestAddReturnsErrRegistryFullAtCapacity proves Add rejects a new mRID
// once the registry is at its WithMaxEntries bound, and leaves the
// registry unmutated (the entry is not partially inserted).
func TestAddReturnsErrRegistryFullAtCapacity(t *testing.T) {
	t.Parallel()

	r := New(WithMaxEntries(2))
	if err := r.Add(Entry{MRID: "m1", LFDI: testLFDI("cap-l1")}); err != nil {
		t.Fatalf("Add m1: %v", err)
	}
	if err := r.Add(Entry{MRID: "m2", LFDI: testLFDI("cap-l2")}); err != nil {
		t.Fatalf("Add m2: %v", err)
	}

	err := r.Add(Entry{MRID: "m3", LFDI: testLFDI("cap-l3")})
	if !errors.Is(err, ErrRegistryFull) {
		t.Fatalf("Add at capacity: error = %v, want ErrRegistryFull", err)
	}
	if got := r.Len(); got != 2 {
		t.Errorf("Len() after rejected Add = %d, want 2 (unmutated)", got)
	}
	if _, ok := r.Get("m3"); ok {
		t.Error("Get(m3) succeeded after ErrRegistryFull, want absent")
	}
}

// TestAddReplacementAtCapacitySucceeds proves that replacing an existing
// mRID does not count against the capacity bound: a registry at capacity
// can still Add an entry for an mRID it already holds, because that Add
// does not grow the entry count.
func TestAddReplacementAtCapacitySucceeds(t *testing.T) {
	t.Parallel()

	r := New(WithMaxEntries(1))
	if err := r.Add(Entry{MRID: "m1", Name: "Old", LFDI: testLFDI("cap-old")}); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	if err := r.Add(Entry{MRID: "m1", Name: "New", LFDI: testLFDI("cap-new")}); err != nil {
		t.Fatalf("replacement Add at capacity: %v, want no error", err)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
	if got, ok := r.Name("m1"); !ok || got != "New" {
		t.Errorf("Name(m1) = (%q, %t), want (New, true)", got, ok)
	}
}

// TestAddBatchReturnsErrRegistryFullAtCapacity proves AddBatch rejects a
// batch that would push the registry past its WithMaxEntries bound, and
// that the rejection is atomic: none of the batch's entries land, even
// the ones that would have fit individually.
func TestAddBatchReturnsErrRegistryFullAtCapacity(t *testing.T) {
	t.Parallel()

	r := New(WithMaxEntries(2))
	if err := r.Add(Entry{MRID: "pre", LFDI: testLFDI("batch-cap-pre")}); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	batch := []Entry{
		{MRID: "m1", LFDI: testLFDI("batch-cap-1")},
		{MRID: "m2", LFDI: testLFDI("batch-cap-2")},
	}
	err := r.AddBatch(batch)
	if !errors.Is(err, ErrRegistryFull) {
		t.Fatalf("AddBatch error = %v, want ErrRegistryFull", err)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len() after rejected AddBatch = %d, want 1 (unmutated except seed)", got)
	}
	for _, m := range []string{"m1", "m2"} {
		if _, ok := r.Get(m); ok {
			t.Errorf("Get(%q) succeeded after ErrRegistryFull, want absent", m)
		}
	}
}

// TestAddBatchReplacementsDoNotCountAgainstCapacity proves a batch
// consisting solely of replacements for mRIDs already present can
// succeed even when the registry is already at capacity, since no new
// mRID is being introduced.
func TestAddBatchReplacementsDoNotCountAgainstCapacity(t *testing.T) {
	t.Parallel()

	r := New(WithMaxEntries(2))
	if err := r.AddBatch([]Entry{
		{MRID: "m1", LFDI: testLFDI("batch-repl-old-1")},
		{MRID: "m2", LFDI: testLFDI("batch-repl-old-2")},
	}); err != nil {
		t.Fatalf("seed AddBatch: %v", err)
	}

	err := r.AddBatch([]Entry{
		{MRID: "m1", LFDI: testLFDI("batch-repl-new-1")},
		{MRID: "m2", LFDI: testLFDI("batch-repl-new-2")},
	})
	if err != nil {
		t.Fatalf("all-replacement AddBatch at capacity: %v, want no error", err)
	}
	if got := r.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if got, ok := r.LFDI("m1"); !ok || got != testLFDI("batch-repl-new-1") {
		t.Errorf("LFDI(m1) = (%q, %t), want (%q, true)", got, ok, testLFDI("batch-repl-new-1"))
	}
}
