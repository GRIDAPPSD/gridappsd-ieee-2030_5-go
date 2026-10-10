package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

// meteredReadings wraps the reading store to count the items a List copies
// out and to fail chosen calls.
type meteredReadings struct {
	store.ScopedStore[sep2.MirrorMeterReading]
	copied     atomic.Int64
	failLists  atomic.Int64 // the next N List calls fail
	failDelete func(parent, id string) error
}

func (m *meteredReadings) List(ctx context.Context, parent string, opts store.ListOptions) (store.ListResult[sep2.MirrorMeterReading], error) {
	if m.failLists.Load() > 0 {
		m.failLists.Add(-1)
		return store.ListResult[sep2.MirrorMeterReading]{}, errors.New("list down")
	}
	res, err := m.ScopedStore.List(ctx, parent, opts)
	m.copied.Add(int64(len(res.Items)))
	return res, err
}

func (m *meteredReadings) Delete(ctx context.Context, parent, id string) error {
	if m.failDelete != nil {
		if err := m.failDelete(parent, id); err != nil {
			return err
		}
	}
	return m.ScopedStore.Delete(ctx, parent, id)
}

func metered(st *assembly.Stores) *meteredReadings {
	m := &meteredReadings{ScopedStore: st.MirrorMeterReadings}
	st.MirrorMeterReadings = m
	return m
}

func TestMirrorSeriesCopiesOnlyTheReadingsFromSinceOn(t *testing.T) {
	st := newStores()
	m := metered(st)
	w := rtype(38, 37, 0, 0, 1)
	for _, mup := range []string{"m1", "m2", "m3"} {
		seedMirror(t, st, mup, "LFDI-"+mup)
		for i := int64(0); i < 100; i++ {
			rt := w
			if i > 0 {
				rt = nil
			}
			postReading(t, st, mup, i, 10_000+i*30, powerReading("w", "Real Power (W)", rt, i))
		}
	}
	cache := newMirrorTypeCache()
	since := int64(10_000 + 97*30)
	res, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: since})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if res.TotalSeries != 3 {
		t.Fatalf("series = %d, want 3", res.TotalSeries)
	}
	for _, s := range res.Series {
		if s.Uom != 38 || s.Total != 3 || s.Points[0].Time != since {
			t.Errorf("series %s = uom %d total %d first %d, want uom 38 (typed from before the window), 3 points from %d", s.DeviceLFDI, s.Uom, s.Total, s.Points[0].Time, since)
		}
	}

	// The first poll learned each type by reading the mirror; the second
	// finds it in the cache and copies only the window.
	m.copied.Store(0)
	if _, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: since}); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := m.copied.Load(); got != 9 {
		t.Errorf("second poll copied %d readings out of 300, want the 9 inside the window", got)
	}
}

func TestMirrorSeriesWindowAgreesWithTheWholeStore(t *testing.T) {
	st := newStores()
	w := rtype(38, 37, 0, 0, 1)
	v := rtype(29, 0, 0, 0, 0)
	seedMirror(t, st, "m1", "LFDI-A")
	seedMirror(t, st, "m2", "")
	var n int64
	for i := int64(0); i < 40; i++ {
		n++
		rt := w
		if i > 0 {
			rt = nil
		}
		postReading(t, st, "m1", n, 5000+i*7, powerReading("w", "Real Power (W)", rt, i))
		n++
		postReading(t, st, "m2", n, 5000+i*7, powerReading("v", "PH_ABC (V)", map[bool]*sep2.ReadingType{true: v}[i == 0], i))
	}
	cache := newMirrorTypeCache()
	whole, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, since := range []int64{1, 5000, 5100, 5273, 5274, 6000} {
		got, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: since})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, s := range whole.Series {
			for _, p := range s.Points {
				if p.Time >= since {
					want = append(want, fmt.Sprint(s.DeviceLFDI, s.Mirror, s.Uom, s.Description, p))
				}
			}
		}
		var have []string
		for _, s := range got.Series {
			for _, p := range s.Points {
				have = append(have, fmt.Sprint(s.DeviceLFDI, s.Mirror, s.Uom, s.Description, p))
			}
		}
		if fmt.Sprint(have) != fmt.Sprint(want) {
			t.Errorf("since %d: windowed points differ from the whole store's:\n got %v\nwant %v", since, have, want)
		}
	}
}

func TestMirrorSeriesDropsACachedTypeWhoseReadingIsGone(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	typed := postReading(t, st, "m1", 1, 1000, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 1))
	postReading(t, st, "m1", 2, 2000, powerReading("w", "", nil, 2))
	cache := newMirrorTypeCache()
	res, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: 1500})
	if err != nil || len(res.Series) != 1 || res.Series[0].Uom != 38 {
		t.Fatalf("first poll = %+v, %v; want one uom 38 series", res.Series, err)
	}
	if _, ok := cache.get("m1", "w"); !ok {
		t.Fatal("the type was not cached")
	}

	if err := st.MirrorMeterReadings.Delete(context.Background(), "m1", typed); err != nil {
		t.Fatal(err)
	}
	res, err = mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 || res.Series[0].Uom != 0 {
		t.Errorf("after the typed reading went, series = %+v, want the stale type not applied (uom 0)", res.Series)
	}
	if _, ok := cache.get("m1", "w"); ok {
		t.Error("the cache still holds a type whose reading is gone")
	}
}

func TestMirrorSeriesForgetsTheTypesOfAMirrorThatWentAway(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	typed := postReading(t, st, "m1", 1, 1000, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 1))
	cache := newMirrorTypeCache()
	if _, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.get("m1", "w"); !ok {
		t.Fatal("the type was not cached")
	}
	if err := st.MirrorMeterReadings.Delete(context.Background(), "m1", typed); err != nil {
		t.Fatal(err)
	}
	if _, err := mirrorSeries(context.Background(), st, cache, MirrorQuery{Since: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorSeriesFiltersByDeviceAndUnit(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "lfdi-a")
	seedMirror(t, st, "m2", "")
	w, v := rtype(38, 37, 0, 0, 1), rtype(29, 0, 0, 0, 0)
	postReading(t, st, "m1", 1, 9000, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m1", 2, 9000, powerReading("v", "PH_ABC (V)", v, 2))
	postReading(t, st, "m2", 3, 9000, powerReading("w", "Real Power (W)", w, 3))
	w38 := uint8(38)

	for _, c := range []struct {
		name      string
		q         MirrorQuery
		wantTotal int
		wantFirst string
	}{
		{"device by LFDI, case-insensitive", MirrorQuery{Device: "LFDI-A"}, 2, "lfdi-a"},
		{"device by mirror id when it names none", MirrorQuery{Device: "mirror:m2"}, 1, ""},
		{"unit", MirrorQuery{Uom: &w38}, 2, ""},
		{"device and unit", MirrorQuery{Device: "lfdi-a", Uom: &w38}, 1, "lfdi-a"},
		{"unknown device", MirrorQuery{Device: "nobody"}, 0, ""},
	} {
		res, err := mirrorSeries(context.Background(), st, nil, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.TotalSeries != c.wantTotal || len(res.Series) != c.wantTotal {
			t.Errorf("%s: %d series (total %d), want %d", c.name, len(res.Series), res.TotalSeries, c.wantTotal)
			continue
		}
		if c.wantFirst != "" && res.Series[0].DeviceLFDI != c.wantFirst {
			t.Errorf("%s: first series device %q, want %q", c.name, res.Series[0].DeviceLFDI, c.wantFirst)
		}
	}
}

func TestMirrorSeriesKeepsMirrorsWithoutAnLFDIApart(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "")
	seedMirror(t, st, "m2", "")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 1, 9000, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m2", 2, 9000, powerReading("w", "Real Power (W)", w, 2))

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 2 || res.Series[0].Mirror != "m1" || res.Series[1].Mirror != "m2" || res.Series[0].DeviceLFDI != "" {
		t.Fatalf("series = %+v, want two, named by mirror m1 and m2 with no LFDI", res.Series)
	}

	// The cap counts them apart too.
	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(9010, 0), time.Hour, 1)
	if err != nil || removed != 0 {
		t.Errorf("sweep cap 1 removed %d (%v), want 0: each mirror is its own device", removed, err)
	}
}

func TestMirrorPointIDsAreStableAndDistinct(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	set := sep2.MirrorMeterReading{
		MRID: "w", Description: "Real Power (W)", ReadingType: rtype(38, 37, 0, 0, 1),
		MirrorReadingSet: []sep2.MirrorReadingSet{{Reading: []sep2.Reading{{Value: i64(1)}, {Value: i64(2)}}}},
	}
	id := postReading(t, st, "m1", 1, 9000, set)
	postReading(t, st, "m1", 2, 9000, powerReading("w", "", nil, 3))

	a, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{Since: 9000})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range a.Series[0].Points {
		if seen[p.ID] {
			t.Errorf("point id %q repeats", p.ID)
		}
		seen[p.ID] = true
	}
	if len(seen) != 3 || !seen["m1/"+id+".0"] || !seen["m1/"+id+".1"] {
		t.Errorf("ids = %v, want three distinct including the set's two readings m1/%s.0 and .1", seen, id)
	}
	for _, p := range b.Series[0].Points {
		if !seen[p.ID] {
			t.Errorf("a later poll named point %q that the first did not", p.ID)
		}
	}
}

func TestMirrorSeriesSinceBeyondAnyIDIsEmptyNotAPanic(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	postReading(t, st, "m1", 1, 9000, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 1))
	for _, since := range []int64{math.MaxInt64, math.MaxInt64 / 1_000_000_000, math.MaxInt64/1_000_000_000 + 1} {
		res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{Since: since})
		if err != nil || len(res.Series) != 0 {
			t.Errorf("since %d = %+v, %v; want empty", since, res.Series, err)
		}
	}
}

func TestSweepLedgerSkipsTheCopyWhenNothingIsDue(t *testing.T) {
	st := newStores()
	m := metered(st)
	w := rtype(38, 37, 0, 0, 1)
	seedMirror(t, st, "m1", "LFDI-A")
	seedMirror(t, st, "m2", "LFDI-B")
	for i := int64(0); i < 20; i++ {
		postReading(t, st, "m1", i, 9000+i, powerReading("w", "Real Power (W)", w, i))
		postReading(t, st, "m2", i, 9000+i, powerReading("w", "Real Power (W)", w, i))
	}
	led := &sweepLedger{}
	now := time.Unix(9100, 0)
	ctx := context.Background()
	sweep := func() int {
		n, err := sweepMirrorReadings(ctx, st, led, now, time.Hour, 25)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	m.copied.Store(0)
	if sweep() != 0 || m.copied.Load() < 40 {
		t.Fatalf("first sweep copied %d, want a full pass of 40", m.copied.Load())
	}
	m.copied.Store(0)
	if sweep() != 0 {
		t.Fatal("second sweep removed something")
	}
	if got := m.copied.Load(); got > 2 {
		t.Errorf("idle sweep copied %d readings, want at most the oldest of each of 2 mirrors", got)
	}

	// Five more readings keep device A at 25, which is not over the cap of
	// 25, and the bound only grows by what was added: still no full pass.
	for i := int64(20); i < 25; i++ {
		postReading(t, st, "m1", i, 9000+i, powerReading("w", "Real Power (W)", w, i))
	}
	m.copied.Store(0)
	if sweep() != 0 || m.copied.Load() > 2 {
		t.Errorf("sweep at the cap copied %d, want a skip", m.copied.Load())
	}

	// One more takes the bound past the cap: a full pass runs and trims it.
	postReading(t, st, "m1", 25, 9025, powerReading("w", "Real Power (W)", w, 25))
	m.copied.Store(0)
	if n := sweep(); n != 1 {
		t.Errorf("sweep past the cap removed %d, want 1", n)
	}
	if m.copied.Load() < 40 {
		t.Errorf("sweep past the cap copied %d, want a full pass", m.copied.Load())
	}
	if got := len(remainingIDs(t, st, "m1")); got != 25 {
		t.Errorf("m1 holds %d readings after the trim, want 25", got)
	}
}

func TestSweepLedgerSeesAnAgedReadingAndAShrunkenStore(t *testing.T) {
	st := newStores()
	w := rtype(38, 37, 0, 0, 1)
	seedMirror(t, st, "m1", "LFDI-A")
	for i := int64(0); i < 3; i++ {
		postReading(t, st, "m1", i, 9000+i, powerReading("w", "Real Power (W)", w, i))
	}
	led := &sweepLedger{}
	ctx := context.Background()
	at := func(sec int64) int {
		n, err := sweepMirrorReadings(ctx, st, led, time.Unix(sec, 0), 100*time.Second, 100)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if at(9050) != 0 || at(9050) != 0 {
		t.Fatal("nothing is old yet")
	}
	// Time passes with no new post: the oldest reading is now past the age.
	if n := at(9101); n != 1 {
		t.Errorf("sweep after the oldest aged removed %d, want 1", n)
	}

}

func TestSweepLedgerTreatsAShrunkenStoreAsUnknown(t *testing.T) {
	st := newStores()
	m := metered(st)
	w := rtype(38, 37, 0, 0, 1)
	seedMirror(t, st, "m1", "LFDI-A")
	for i := int64(0); i < 4; i++ {
		postReading(t, st, "m1", i, 9000+i, powerReading("w", "Real Power (W)", w, i))
	}
	led := &sweepLedger{}
	ctx := context.Background()
	for range 2 {
		if _, err := sweepMirrorReadings(ctx, st, led, time.Unix(9010, 0), time.Hour, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MirrorMeterReadings.Delete(ctx, "m1", remainingIDs(t, st, "m1")[0]); err != nil {
		t.Fatal(err)
	}
	m.copied.Store(0)
	if _, err := sweepMirrorReadings(ctx, st, led, time.Unix(9010, 0), time.Hour, 100); err != nil {
		t.Fatal(err)
	}
	if got := m.copied.Load(); got < 3 {
		t.Errorf("sweep after an outside delete copied %d readings, want a full pass of 3", got)
	}
}

func TestSweepReportsAFailedListingAndRemovesNothing(t *testing.T) {
	st := newStores()
	m := metered(st)
	seedMirror(t, st, "m1", "LFDI-A")
	postReading(t, st, "m1", 1, 100, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 1))
	m.failLists.Store(1)

	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(100000, 0), time.Hour, 10)
	if err == nil || removed != 0 {
		t.Fatalf("sweep with the listing down = %d, %v; want 0 and an error", removed, err)
	}
	if got := len(remainingIDs(t, st, "m1")); got != 1 {
		t.Errorf("%d readings remain, want the old one untouched when the listing failed", got)
	}
}

func TestSweepLeavesAReadingItCannotDeleteAndRemovesTheRest(t *testing.T) {
	st := newStores()
	m := metered(st)
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	stuck := postReading(t, st, "m1", 1, 100, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m1", 2, 101, powerReading("w", "Real Power (W)", w, 2))
	postReading(t, st, "m1", 3, 102, powerReading("w", "Real Power (W)", w, 3))
	m.failDelete = func(_, id string) error {
		if id == stuck {
			return errors.New("delete down")
		}
		return nil
	}
	led := &sweepLedger{}

	removed, err := sweepMirrorReadings(context.Background(), st, led, time.Unix(100000, 0), time.Hour, 10)
	if err != nil || removed != 2 {
		t.Fatalf("sweep = %d, %v; want 2 removed and no error", removed, err)
	}
	if got := remainingIDs(t, st, "m1"); fmt.Sprint(got) != fmt.Sprint([]string{stuck}) {
		t.Errorf("remaining = %v, want only the reading that could not be deleted", got)
	}

	// The failure leaves the ledger unset so the next sweep is a full pass
	// and retries it.
	m.failDelete = nil
	if removed, err := sweepMirrorReadings(context.Background(), st, led, time.Unix(100000, 0), time.Hour, 10); err != nil || removed != 1 {
		t.Errorf("retry = %d, %v; want the stuck reading removed", removed, err)
	}
}

func TestRetentionLoopSurvivesAFailedSweep(t *testing.T) {
	st := newStores()
	plain := *st // reads the store without the injected failures
	m := metered(st)
	seedMirror(t, st, "m1", "LFDI-A")
	postReading(t, st, "m1", 1, 100, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 1))
	m.failLists.Store(2)
	e := &Embed{stores: st, mirrorMaxAge: time.Hour, mirrorInterval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.runMirrorRetention(ctx) }()

	deadline := time.After(2 * time.Second)
	for len(remainingIDs(t, &plain, "m1")) != 0 {
		select {
		case <-deadline:
			cancel()
			wg.Wait()
			t.Fatal("the loop did not sweep again after its first two sweeps failed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
	if m.failLists.Load() != 0 {
		t.Errorf("failLists = %d, want both failures consumed before the reading went", m.failLists.Load())
	}
}

func TestMirrorBoundsAndTypeCacheReachEmbed(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"ccm listener", Config{EnableCCM: true}},
		{"server-go listener", Config{}},
	} {
		c.cfg.MirrorReadingRetention = 54 * time.Hour
		c.cfg.MirrorReadingMaxPerSeries = 77
		c.cfg.MirrorRetentionInterval = 3 * time.Second
		e := newTuningEmbed(t, c.cfg)
		age, n, iv := e.mirrorRetentionSettings()
		if age != 54*time.Hour || n != 77 || iv != 3*time.Second {
			t.Errorf("%s: retention = %v/%d/%v, want 54h/77/3s", c.name, age, n, iv)
		}
		if e.mirrorTypes == nil {
			t.Errorf("%s: no mirror type cache", c.name)
		}
	}
}
