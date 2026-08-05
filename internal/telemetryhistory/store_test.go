package telemetryhistory

import (
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

// TestSampleIsSixteenBytes pins Sample's size so the documented memory
// ceiling (MaxSeries * SamplesPerSeries * 16 bytes, see this package's
// doc comment) stays true if Sample is ever edited. A field added here
// without updating the ceiling documentation would otherwise silently
// double the real ceiling without any test noticing.
func TestSampleIsSixteenBytes(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(Sample{}); got != 16 {
		t.Fatalf("unsafe.Sizeof(Sample{}) = %d, want 16", got)
	}
}

// TestStore_TwoAttributesOnOneDeviceAreIndependentSeries asserts the
// series key is (object, attribute), not object alone: two attributes on
// the same device mRID must not overwrite each other's ring.
func TestStore_TwoAttributesOnOneDeviceAreIndependentSeries(t *testing.T) {
	t.Parallel()
	s := &Store{}
	const object = "50B15A48-9611-40DF-983E-93679DA72871"
	keyA := SeriesKey{Object: object, Attribute: "DERStatus.stateOfChargeStatus"}
	keyB := SeriesKey{Object: object, Attribute: "DERControl.DERControlBase.opModTargetW"}

	s.Append(keyA, Sample{At: 1, Value: 65.0})
	s.Append(keyB, Sample{At: 1, Value: 5000.0})

	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot() returned %d series, want 2 (got %+v)", len(snap), snap)
	}

	byKey := make(map[SeriesKey][]Sample, len(snap))
	for _, series := range snap {
		byKey[series.Key] = series.Samples
	}

	a, ok := byKey[keyA]
	if !ok || len(a) != 1 || a[0].Value != 65.0 {
		t.Fatalf("series %+v = %+v, want one sample with Value 65.0", keyA, a)
	}
	b, ok := byKey[keyB]
	if !ok || len(b) != 1 || b[0].Value != 5000.0 {
		t.Fatalf("series %+v = %+v, want one sample with Value 5000.0", keyB, b)
	}
}

// TestStore_RingEvictsOldestSampleWithoutReallocating appends well past
// SamplesPerSeries and asserts: the ring never grows past its
// preallocated capacity (no reallocation), and the sample that falls out
// is the OLDEST one, never the newest.
func TestStore_RingEvictsOldestSampleWithoutReallocating(t *testing.T) {
	t.Parallel()
	s := &Store{}
	key := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}

	const overflow = 5
	total := SamplesPerSeries + overflow
	for i := 0; i < total; i++ {
		s.Append(key, Sample{At: int64(i), Value: float64(i)})
	}

	entry := s.series[key]
	if entry == nil {
		t.Fatalf("series %+v not present after %d appends", key, total)
	}
	if got := len(entry.ring.buf); got != SamplesPerSeries {
		t.Fatalf("ring.buf len = %d, want %d (must not reallocate)", got, SamplesPerSeries)
	}
	if got := cap(entry.ring.buf); got != SamplesPerSeries {
		t.Fatalf("ring.buf cap = %d, want %d (must not reallocate)", got, SamplesPerSeries)
	}

	samples := entry.ring.snapshot()
	if len(samples) != SamplesPerSeries {
		t.Fatalf("ring.snapshot() returned %d samples, want %d", len(samples), SamplesPerSeries)
	}
	// The first `overflow` samples (At 0..overflow-1) were pushed out;
	// the oldest surviving sample must be At == overflow, and the newest
	// must be At == total-1. If the ring dropped the NEWEST sample
	// instead of the oldest, samples[0].At would be 0 and
	// samples[len-1].At would be total-1-overflow: this assertion fails
	// under that bug.
	if got := samples[0].At; got != int64(overflow) {
		t.Fatalf("oldest surviving sample At = %d, want %d (oldest %d samples should have been dropped)", got, overflow, overflow)
	}
	if got := samples[len(samples)-1].At; got != int64(total-1) {
		t.Fatalf("newest sample At = %d, want %d", got, total-1)
	}
}

// TestStore_SeriesCapEvictsLeastRecentlyAppended fills the store past
// MaxSeries and asserts: the store never exceeds MaxSeries series, and
// the series evicted is the one that has gone longest without a new
// Append (least-recently-appended), not an arbitrary one.
func TestStore_SeriesCapEvictsLeastRecentlyAppended(t *testing.T) {
	t.Parallel()
	s := &Store{}

	keyFor := func(i int) SeriesKey {
		return SeriesKey{Object: fmt.Sprintf("dev-%d", i), Attribute: "DERStatus.stateOfChargeStatus"}
	}

	// Fill to exactly MaxSeries. keyFor(0) is the least-recently-appended
	// series at this point: it was appended first and never touched
	// again.
	for i := 0; i < MaxSeries; i++ {
		s.Append(keyFor(i), Sample{At: int64(i), Value: float64(i)})
	}
	if got := len(s.series); got != MaxSeries {
		t.Fatalf("len(series) after filling to cap = %d, want %d", got, MaxSeries)
	}

	// Touch every series except keyFor(0) again, so keyFor(0) is now
	// unambiguously the least-recently-appended series.
	for i := 1; i < MaxSeries; i++ {
		s.Append(keyFor(i), Sample{At: int64(1000 + i), Value: float64(i)})
	}

	// One more, brand new series forces an eviction.
	newKey := SeriesKey{Object: "dev-new", Attribute: "DERStatus.stateOfChargeStatus"}
	s.Append(newKey, Sample{At: 9999, Value: 1.0})

	if got := len(s.series); got != MaxSeries {
		t.Fatalf("len(series) after overflow append = %d, want %d (store must never exceed the cap)", got, MaxSeries)
	}
	if _, stillPresent := s.series[keyFor(0)]; stillPresent {
		t.Fatalf("series %+v (least-recently-appended) was not evicted", keyFor(0))
	}
	if _, present := s.series[newKey]; !present {
		t.Fatalf("newly appended series %+v was not retained", newKey)
	}
	// Every other original series must have survived: only the LRA one
	// should have been evicted.
	for i := 1; i < MaxSeries; i++ {
		if _, present := s.series[keyFor(i)]; !present {
			t.Fatalf("series %+v was evicted, want only keyFor(0) evicted", keyFor(i))
		}
	}
}

// TestStore_SnapshotIsADefensiveCopy asserts that mutating a slice
// returned by Snapshot never affects the store's own retained data.
func TestStore_SnapshotIsADefensiveCopy(t *testing.T) {
	t.Parallel()
	s := &Store{}
	key := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}
	s.Append(key, Sample{At: 1, Value: 65.0})

	snap := s.Snapshot()
	snap[0].Samples[0].Value = 999999.0
	snap[0].Key.Object = "tampered"

	snap2 := s.Snapshot()
	if len(snap2) != 1 || snap2[0].Samples[0].Value != 65.0 {
		t.Fatalf("store state changed after caller mutated a Snapshot result: %+v", snap2)
	}
	if snap2[0].Key.Object != "dev-1" {
		t.Fatalf("store's series key changed after caller mutated a Snapshot result: %+v", snap2[0].Key)
	}
}

// TestStore_ConcurrentAppendAndSnapshot exercises the store under
// concurrent writers and readers; run with -race.
func TestStore_ConcurrentAppendAndSnapshot(t *testing.T) {
	s := &Store{}
	var wg sync.WaitGroup

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := SeriesKey{Object: fmt.Sprintf("dev-%d", g), Attribute: "DERStatus.stateOfChargeStatus"}
			for i := 0; i < 200; i++ {
				s.Append(key, Sample{At: int64(i), Value: float64(i)})
			}
		}(g)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()
}
