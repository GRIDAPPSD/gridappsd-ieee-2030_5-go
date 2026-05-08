package registry

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

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
	e := Entry{MRID: "mrid-1", Name: "House One", LFDI: "lfdi-1"}
	if err := r.Add(e); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	if got, ok := r.LFDI("mrid-1"); !ok || got != "lfdi-1" {
		t.Errorf("LFDI(mrid-1) = (%q, %t), want (lfdi-1, true)", got, ok)
	}
	if got, ok := r.MRID("lfdi-1"); !ok || got != "mrid-1" {
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
	e := Entry{MRID: "mrid-1", Name: "", LFDI: "lfdi-1"}
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
	if err := r.Add(Entry{MRID: "mrid-1", Name: "Old", LFDI: "lfdi-old"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := r.Add(Entry{MRID: "mrid-1", Name: "New", LFDI: "lfdi-new"}); err != nil {
		t.Fatalf("replacement Add: %v", err)
	}

	if got := r.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 after replacement", got)
	}

	if got, ok := r.LFDI("mrid-1"); !ok || got != "lfdi-new" {
		t.Errorf("LFDI(mrid-1) = (%q, %t), want (lfdi-new, true)", got, ok)
	}
	if got, ok := r.MRID("lfdi-new"); !ok || got != "mrid-1" {
		t.Errorf("MRID(lfdi-new) = (%q, %t), want (mrid-1, true)", got, ok)
	}

	// The old LFDI must no longer reverse-resolve.
	if got, ok := r.MRID("lfdi-old"); ok {
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
		{MRID: "mrid-1", Name: "One", LFDI: "lfdi-1"},
		{MRID: "mrid-2", Name: "Two", LFDI: "lfdi-2"},
		{MRID: "mrid-3", Name: "", LFDI: "lfdi-3"},
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
	pre := Entry{MRID: "mrid-pre", Name: "Pre", LFDI: "lfdi-pre"}
	if err := r.Add(pre); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	bad := []Entry{
		{MRID: "mrid-a", Name: "A", LFDI: "lfdi-a"},
		{MRID: "", Name: "B", LFDI: "lfdi-b"}, // invalid
		{MRID: "mrid-c", Name: "C", LFDI: "lfdi-c"},
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
	for _, l := range []string{"lfdi-a", "lfdi-c"} {
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
	original := Entry{MRID: "mrid-1", Name: "Original", LFDI: "lfdi-original"}
	if err := r.Add(original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	batch := []Entry{
		{MRID: "mrid-1", Name: "Replacement", LFDI: "lfdi-replacement"},
		{MRID: "mrid-2", Name: "Two", LFDI: ""}, // invalid
	}
	err := r.AddBatch(batch)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("AddBatch error = %v, want ErrInvalidEntry", err)
	}

	if got, ok := r.Get("mrid-1"); !ok || got != original {
		t.Errorf("Get(mrid-1) = (%+v, %t), want (%+v, true) (rollback failed)", got, ok, original)
	}
	if got, ok := r.MRID("lfdi-original"); !ok || got != "mrid-1" {
		t.Errorf("MRID(lfdi-original) = (%q, %t), want (mrid-1, true)", got, ok)
	}
	if _, ok := r.MRID("lfdi-replacement"); ok {
		t.Errorf("MRID(lfdi-replacement) succeeded after rollback, want absent")
	}
}

func TestRemoveHappyPath(t *testing.T) {
	t.Parallel()

	r := New()
	e := Entry{MRID: "mrid-1", Name: "House", LFDI: "lfdi-1"}
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
	if _, ok := r.MRID("lfdi-1"); ok {
		t.Errorf("MRID(lfdi-1) found entry after Remove (reverse index leak)")
	}
	if got := r.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestRemoveUnknownIsNoOp(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Add(Entry{MRID: "mrid-1", Name: "X", LFDI: "lfdi-1"}); err != nil {
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
	if err := r.Add(Entry{MRID: "m1", LFDI: "l1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len() after Add = %d, want 1", got)
	}
	if err := r.AddBatch([]Entry{
		{MRID: "m2", LFDI: "l2"},
		{MRID: "m3", LFDI: "l3"},
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
		{MRID: "m1", Name: "One", LFDI: "l1"},
		{MRID: "m2", Name: "Two", LFDI: "l2"},
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
			LFDI: fmt.Sprintf("seed-lfdi-%d", i),
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
					LFDI: fmt.Sprintf("w%d-lfdi-%d", w, i),
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
				_, _ = r.MRID(fmt.Sprintf("seed-lfdi-%d", i%seedCount))
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
			LFDI: fmt.Sprintf("seed-lfdi-%d", i),
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
	real := Entry{MRID: "mrid-real", Name: "Real Device", LFDI: "lfdi-real", Placeholder: false}
	stub := Entry{MRID: "mrid-stub", Name: "Stage 1 Stub", LFDI: "lfdi-stub", Placeholder: true}

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
		{MRID: "m1", Name: "One", LFDI: "l1", Placeholder: true},
		{MRID: "m2", Name: "Two", LFDI: "l2", Placeholder: false},
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
	stage1 := Entry{MRID: "m1", Name: "Device", LFDI: "lfdi-stage1", Placeholder: true}
	if err := r.Add(stage1); err != nil {
		t.Fatalf("Add stage1: %v", err)
	}

	stage2 := Entry{MRID: "m1", Name: "Device", LFDI: "lfdi-stage2", Placeholder: false}
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
	if got.LFDI != "lfdi-stage2" {
		t.Errorf("LFDI = %q, want lfdi-stage2", got.LFDI)
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
			LFDI: fmt.Sprintf("l%d", i),
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
					LFDI: fmt.Sprintf("hot-l%d", i),
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
