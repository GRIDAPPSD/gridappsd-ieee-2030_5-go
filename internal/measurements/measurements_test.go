package measurements

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNewIsEmpty(t *testing.T) {
	t.Parallel()

	tbl := New()
	if got := tbl.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
	if _, ok := tbl.Lookup("anything"); ok {
		t.Errorf("Lookup on empty table returned ok=true")
	}
}

func TestAddHappyPath(t *testing.T) {
	t.Parallel()

	tbl := New()
	if err := tbl.Add("meas-1", "dev-1"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := tbl.Lookup("meas-1")
	if !ok || got != "dev-1" {
		t.Errorf("Lookup(meas-1) = (%q, %t), want (dev-1, true)", got, ok)
	}
	if got := tbl.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
}

func TestAddValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		measMRID    string
		deviceMRID  string
	}{
		{"empty measurement", "", "dev-1"},
		{"empty device", "meas-1", ""},
		{"both empty", "", ""},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tbl := New()
			err := tbl.Add(tt.measMRID, tt.deviceMRID)
			if !errors.Is(err, ErrInvalidMapping) {
				t.Fatalf("Add(%q, %q) error = %v, want ErrInvalidMapping",
					tt.measMRID, tt.deviceMRID, err)
			}
			if got := tbl.Len(); got != 0 {
				t.Errorf("Len() after invalid Add = %d, want 0", got)
			}
		})
	}
}

func TestAddReplacement(t *testing.T) {
	t.Parallel()

	tbl := New()
	if err := tbl.Add("meas-1", "dev-old"); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := tbl.Add("meas-1", "dev-new"); err != nil {
		t.Fatalf("replacement Add: %v", err)
	}
	got, ok := tbl.Lookup("meas-1")
	if !ok || got != "dev-new" {
		t.Errorf("Lookup(meas-1) = (%q, %t), want (dev-new, true)", got, ok)
	}
	if got := tbl.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 after replacement", got)
	}
}

func TestAddBatchHappyPath(t *testing.T) {
	t.Parallel()

	tbl := New()
	batch := []Mapping{
		{MeasurementMRID: "m1", DeviceMRID: "d1"},
		{MeasurementMRID: "m2", DeviceMRID: "d2"},
		{MeasurementMRID: "m3", DeviceMRID: "d2"}, // multiple measurements per device is normal
	}
	if err := tbl.AddBatch(batch); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if got := tbl.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	for _, m := range batch {
		got, ok := tbl.Lookup(m.MeasurementMRID)
		if !ok || got != m.DeviceMRID {
			t.Errorf("Lookup(%q) = (%q, %t), want (%q, true)",
				m.MeasurementMRID, got, ok, m.DeviceMRID)
		}
	}
}

func TestAddBatchEmptyIsNoOp(t *testing.T) {
	t.Parallel()

	tbl := New()
	if err := tbl.AddBatch(nil); err != nil {
		t.Errorf("AddBatch(nil): %v", err)
	}
	if err := tbl.AddBatch([]Mapping{}); err != nil {
		t.Errorf("AddBatch([]): %v", err)
	}
	if got := tbl.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestAddBatchRollsBackOnInvalidEntry(t *testing.T) {
	t.Parallel()

	tbl := New()
	if err := tbl.Add("seed-meas", "seed-dev"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	bad := []Mapping{
		{MeasurementMRID: "m-a", DeviceMRID: "d-a"},
		{MeasurementMRID: "", DeviceMRID: "d-b"}, // invalid
		{MeasurementMRID: "m-c", DeviceMRID: "d-c"},
	}
	err := tbl.AddBatch(bad)
	if !errors.Is(err, ErrInvalidMapping) {
		t.Fatalf("AddBatch error = %v, want ErrInvalidMapping", err)
	}
	if got, ok := tbl.Lookup("seed-meas"); !ok || got != "seed-dev" {
		t.Errorf("seed mapping lost or mutated after rollback: (%q, %t)", got, ok)
	}
	if got := tbl.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 (only the seed)", got)
	}
	for _, m := range []string{"m-a", "m-c"} {
		if _, ok := tbl.Lookup(m); ok {
			t.Errorf("Lookup(%q) succeeded after rollback, want absent", m)
		}
	}
}

func TestLookupMissReturnsZeroAndFalse(t *testing.T) {
	t.Parallel()

	tbl := New()
	if got, ok := tbl.Lookup("nope"); ok || got != "" {
		t.Errorf("Lookup(nope) = (%q, %t), want (\"\", false)", got, ok)
	}
}

func TestStatsCountsHitsAndMisses(t *testing.T) {
	t.Parallel()

	tbl := New()
	if err := tbl.Add("meas-1", "dev-1"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Two hits, three misses.
	tbl.Lookup("meas-1")
	tbl.Lookup("meas-1")
	tbl.Lookup("meas-x")
	tbl.Lookup("meas-y")
	tbl.Lookup("meas-z")

	hits, misses := tbl.Stats()
	if hits != 2 {
		t.Errorf("hits = %d, want 2", hits)
	}
	if misses != 3 {
		t.Errorf("misses = %d, want 3", misses)
	}
}

// TestConcurrentAccess hammers the table with multiple goroutines under
// -race; the goal is to surface any unprotected map access.
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	const (
		writers = 4
		readers = 8
		opsEach = 200
	)

	tbl := New()

	const seedCount = 64
	for i := 0; i < seedCount; i++ {
		if err := tbl.Add(
			fmt.Sprintf("seed-meas-%d", i),
			fmt.Sprintf("seed-dev-%d", i%4),
		); err != nil {
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
				if err := tbl.Add(
					fmt.Sprintf("w%d-meas-%d", w, i),
					fmt.Sprintf("w%d-dev-%d", w, i%4),
				); err != nil {
					errs <- fmt.Errorf("writer %d Add: %w", w, err)
					return
				}
			}
		}()
	}
	for ri := 0; ri < readers; ri++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				_, _ = tbl.Lookup(fmt.Sprintf("seed-meas-%d", i%seedCount))
				_ = tbl.Len()
				_, _ = tbl.Stats()
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent op error: %v", err)
	}

	// Seeds must still be intact.
	for i := 0; i < seedCount; i++ {
		want := fmt.Sprintf("seed-dev-%d", i%4)
		got, ok := tbl.Lookup(fmt.Sprintf("seed-meas-%d", i))
		if !ok || got != want {
			t.Errorf("seed %d: Lookup = (%q, %t), want (%q, true)", i, got, ok, want)
		}
	}
}
