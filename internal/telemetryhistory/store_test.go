package telemetryhistory

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
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
		s.Append(key, Sample{At: int64(i + 1), Value: float64(i)})
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
	// The first `overflow` samples (At 1..overflow) were pushed out;
	// the oldest surviving sample must be At == overflow+1, and the newest
	// must be At == total. If the ring dropped the NEWEST sample
	// instead of the oldest, samples[0].At would be 1 and
	// samples[len-1].At would be total-overflow: this assertion fails
	// under that bug.
	if got := samples[0].At; got != int64(overflow+1) {
		t.Fatalf("oldest surviving sample At = %d, want %d (oldest %d samples should have been dropped)", got, overflow+1, overflow)
	}
	if got := samples[len(samples)-1].At; got != int64(total) {
		t.Fatalf("newest sample At = %d, want %d", got, total)
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
		s.Append(keyFor(i), Sample{At: int64(i + 1), Value: float64(i)})
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

// TestStore_SeriesReadsOneSeriesAsACopy asserts Series returns only the
// keyed series, oldest first, as a copy, and reports a missing key.
func TestStore_SeriesReadsOneSeriesAsACopy(t *testing.T) {
	t.Parallel()
	s := &Store{}
	key := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}
	for _, sm := range []Sample{{At: 2, Value: 64}, {At: 1, Value: 65}} {
		if err := s.Append(key, sm); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Append(SeriesKey{Object: "dev-2", Attribute: key.Attribute}, Sample{At: 1, Value: 10}); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Series(key)
	if !ok || len(got) != 2 || got[0] != (Sample{At: 1, Value: 65}) || got[1] != (Sample{At: 2, Value: 64}) {
		t.Fatalf("Series = %+v, %v; want dev-1's two samples oldest first", got, ok)
	}
	got[0].Value = 999
	if again, _ := s.Series(key); again[0].Value != 65 {
		t.Errorf("store changed after the caller mutated a Series result: %+v", again)
	}
	if got, ok := s.Series(SeriesKey{Object: "dev-3", Attribute: key.Attribute}); ok || got != nil {
		t.Errorf("missing series = %+v, %v; want nil and false", got, ok)
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
				s.Append(key, Sample{At: int64(i + 1), Value: float64(i)})
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

func atTimes(samples []Sample) []int64 {
	out := make([]int64, len(samples))
	for i, s := range samples {
		out[i] = s.At
	}
	return out
}

func TestStore_RefusesZeroNonFiniteAndFutureSamples(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000100, 0)
	s := &Store{now: func() time.Time { return now }}
	key := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}

	if err := s.Append(key, Sample{At: 1700000000, Value: 1}); err != nil {
		t.Fatalf("valid append: %v", err)
	}
	cases := []struct {
		name   string
		sample Sample
		want   error
	}{
		{"zero time", Sample{At: 0, Value: 2}, ErrBadTime},
		{"negative time", Sample{At: -5, Value: 2}, ErrBadTime},
		{"NaN", Sample{At: 1700000010, Value: math.NaN()}, ErrNonFinite},
		{"+Inf", Sample{At: 1700000010, Value: math.Inf(1)}, ErrNonFinite},
		{"just past the skew bound", Sample{At: now.Add(MaxFutureSkew).Unix() + 1, Value: 2}, ErrFutureTime},
	}
	for _, c := range cases {
		if err := s.Append(key, c.sample); !errors.Is(err, c.want) {
			t.Errorf("%s: Append err = %v, want %v", c.name, err, c.want)
		}
	}
	// Exactly at the bound is kept.
	if err := s.Append(key, Sample{At: now.Add(MaxFutureSkew).Unix(), Value: 3}); err != nil {
		t.Errorf("sample exactly at the skew bound refused: %v", err)
	}
	snap := s.Snapshot()
	if got := atTimes(snap[0].Samples); len(got) != 2 {
		t.Errorf("times = %v, want only the valid sample and the one at the bound", got)
	}
}

func TestStore_KeepsSeriesSortedAndReplacesDuplicateTime(t *testing.T) {
	t.Parallel()
	s := &Store{}
	key := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}
	for _, x := range []Sample{{30, 3}, {10, 1}, {20, 2}, {20, 2.5}, {40, 4}, {10, 1.5}} {
		if err := s.Append(key, x); err != nil {
			t.Fatalf("Append(%+v): %v", x, err)
		}
	}
	got := s.Snapshot()[0].Samples
	want := []Sample{{10, 1.5}, {20, 2.5}, {30, 3}, {40, 4}}
	if len(got) != len(want) {
		t.Fatalf("samples = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A full ring must stay at capacity when an older sample is inserted, and
// the oldest sample is the one lost.
func TestStore_OutOfOrderInsertIntoFullRingDropsOldest(t *testing.T) {
	t.Parallel()
	s := &Store{}
	key := SeriesKey{Object: "dev-1", Attribute: "DERStatus.stateOfChargeStatus"}
	for i := 0; i < SamplesPerSeries; i++ {
		// Even times only, leaving odd gaps to insert into.
		s.Append(key, Sample{At: int64(2 * (i + 1)), Value: float64(i)})
	}
	if err := s.Append(key, Sample{At: 3, Value: -1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := s.Snapshot()[0].Samples
	if len(got) != SamplesPerSeries {
		t.Fatalf("len = %d, want %d", len(got), SamplesPerSeries)
	}
	if got[0].At != 3 || got[1].At != 4 {
		t.Errorf("head = %v, want [3 4 ...] (At 2 dropped, 3 inserted in order)", atTimes(got[:3]))
	}
	if got[len(got)-1].At != int64(2*SamplesPerSeries) {
		t.Errorf("newest At = %d, want %d", got[len(got)-1].At, 2*SamplesPerSeries)
	}
	for i := 1; i < len(got); i++ {
		if got[i].At <= got[i-1].At {
			t.Fatalf("not strictly ascending at %d: %v", i, atTimes(got[i-1:i+1]))
		}
	}
}

func TestStore_CountsEvictions(t *testing.T) {
	t.Parallel()
	s := &Store{}
	for i := 0; i < MaxSeries+3; i++ {
		s.Append(SeriesKey{Object: fmt.Sprintf("d%d", i), Attribute: "a"}, Sample{At: 1700000000, Value: 1})
	}
	if got := s.Evictions(); got != 3 {
		t.Errorf("Evictions = %d, want 3", got)
	}
}

// TestStore_HoldsA49DeviceFleetWithoutEvicting appends the shape of a
// 49-device fleet (7 reported attributes and one setpoint per device) for
// 20 rounds and asserts every series is kept with every sample.
func TestStore_HoldsA49DeviceFleetWithoutEvicting(t *testing.T) {
	t.Parallel()
	const devices, rounds = 49, 20
	attrs := []string{
		"DERStatus.a0", "DERStatus.a1", "DERStatus.a2", "DERStatus.a3",
		"DERStatus.a4", "DERStatus.a5", "DERStatus.a6", "DERControl.DERControlBase.opModTargetW",
	}
	s := &Store{}
	for r := 0; r < rounds; r++ {
		for d := 0; d < devices; d++ {
			for _, a := range attrs {
				if err := s.Append(SeriesKey{Object: fmt.Sprintf("m-%02d", d), Attribute: a}, Sample{At: 1700000000 + int64(r), Value: float64(r)}); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
		}
	}
	snap := s.Snapshot()
	if len(snap) != devices*len(attrs) {
		t.Fatalf("held %d series, want %d", len(snap), devices*len(attrs))
	}
	for _, ss := range snap {
		if len(ss.Samples) != rounds {
			t.Fatalf("%v holds %d samples, want %d", ss.Key, len(ss.Samples), rounds)
		}
	}
	if got := s.Evictions(); got != 0 {
		t.Errorf("Evictions = %d, want 0", got)
	}
	if devices*len(attrs)*2 > MaxSeries {
		t.Errorf("MaxSeries %d leaves under 2x margin over %d series", MaxSeries, devices*len(attrs))
	}
}
