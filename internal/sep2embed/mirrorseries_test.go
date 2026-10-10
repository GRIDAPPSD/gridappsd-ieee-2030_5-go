package sep2embed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

func u8(v uint8) *uint8  { return &v }
func i8(v int8) *int8    { return &v }
func i64(v int64) *int64 { return &v }

// rtype builds a ReadingType the way the EPRI client posts one.
func rtype(uom, kind, phase uint8, mult int8, flow uint8) *sep2.ReadingType {
	return &sep2.ReadingType{Uom: u8(uom), Kind: u8(kind), Phase: u8(phase), PowerOfTenMultiplier: i8(mult), FlowDirection: u8(flow)}
}

// seedMirror stores a mirror whose deviceLFDI is lfdi.
func seedMirror(t testing.TB, st *assembly.Stores, mup, lfdi string) {
	t.Helper()
	m := sep2.MirrorUsagePoint{MRID: mup, DeviceLFDI: lfdi}
	if err := st.MirrorUsagePoints.Create(context.Background(), mup, m); err != nil {
		t.Fatalf("create mirror %s: %v", mup, err)
	}
}

// postReading stores one reading the way the POST handler stamps it: a
// 20-digit id that is the receipt time in nanoseconds (n breaks ties within
// the second), the href built from it, and the receipt time as seconds.
func postReading(t testing.TB, st *assembly.Stores, mup string, n int64, ts int64, m sep2.MirrorMeterReading) string {
	t.Helper()
	id := fmt.Sprintf("%020d", ts*1_000_000_000+n)
	m.Href = fmt.Sprintf("/mup/%s/mr/%s", mup, id)
	m.LastUpdateTime = ts
	if err := st.MirrorMeterReadings.Create(context.Background(), mup, id, m); err != nil {
		t.Fatalf("create reading %s/%s: %v", mup, id, err)
	}
	return id
}

func powerReading(mrid, desc string, rt *sep2.ReadingType, v int64) sep2.MirrorMeterReading {
	return sep2.MirrorMeterReading{MRID: mrid, Description: desc, ReadingType: rt, Reading: &sep2.Reading{Value: i64(v)}}
}

func remainingIDs(t *testing.T, st *assembly.Stores, mup string) []string {
	t.Helper()
	page, err := st.MirrorMeterReadings.List(context.Background(), mup, store.ListOptions{Unbounded: true})
	if err != nil {
		t.Fatalf("list %s: %v", mup, err)
	}
	var ids []string
	for _, m := range page.Items {
		id, _ := mirrorReadingID(mup, m.Href)
		ids = append(ids, id)
	}
	return ids
}

func TestSweepRemovesTheOldestBeyondThePerSeriesCap(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	var ids []string
	for i := int64(1); i <= 5; i++ {
		ids = append(ids, postReading(t, st, "m1", i, 1000+i, powerReading("w", "Real Power (W)", w, 100*i)))
	}
	// A different type on the same device is its own series and keeps all of
	// its readings.
	v := rtype(29, 0, 0, 0, 0)
	var vIDs []string
	for i := int64(6); i <= 7; i++ {
		vIDs = append(vIDs, postReading(t, st, "m1", i, 1000+i, powerReading("v", "PH_ABC (V)", v, 555)))
	}

	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(1010, 0), time.Hour, 3)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	got := remainingIDs(t, st, "m1")
	want := []string{ids[2], ids[3], ids[4], vIDs[0], vIDs[1]}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("remaining = %v, want %v (the two oldest of the capped series gone)", got, want)
	}
}

func TestSweepRemovesReadingsOlderThanTheAge(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 1, 100, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m1", 2, 399, powerReading("w", "Real Power (W)", w, 2)) // one second past the cutoff
	atCutoff := postReading(t, st, "m1", 3, 400, powerReading("w", "Real Power (W)", w, 3))
	fresh := postReading(t, st, "m1", 4, 4000, powerReading("w", "Real Power (W)", w, 4))

	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(4000, 0), 3600*time.Second, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (stamps 100 and 399 are older than 4000-3600)", removed)
	}
	if got, want := remainingIDs(t, st, "m1"), []string{atCutoff, fresh}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("remaining = %v, want %v (a reading exactly at the cutoff is kept)", got, want)
	}
}

func TestSweepCapsADeviceAcrossItsMirrors(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	seedMirror(t, st, "m2", "LFDI-A")
	seedMirror(t, st, "m3", "LFDI-B")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 1, 1001, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m2", 2, 1002, powerReading("w", "Real Power (W)", w, 2))
	third := postReading(t, st, "m1", 3, 1003, powerReading("w", "Real Power (W)", w, 3))
	postReading(t, st, "m3", 4, 1001, powerReading("w", "Real Power (W)", w, 4))

	if _, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(1010, 0), time.Hour, 2); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got, want := remainingIDs(t, st, "m1"), []string{third}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("m1 remaining = %v, want %v", got, want)
	}
	if got := remainingIDs(t, st, "m2"); len(got) != 1 {
		t.Errorf("m2 remaining = %v, want its one reading (newest two of LFDI-A are ids 2 and 3)", got)
	}
	if got := remainingIDs(t, st, "m3"); len(got) != 1 {
		t.Errorf("m3 remaining = %v, want its one reading: another device's cap is separate", got)
	}
}

func TestSweepCapDropsTheOldestAcrossMirrorsEvenWhenItIsInALaterMirror(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	seedMirror(t, st, "m2", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	// The oldest reading sits in m2, which sorts after m1.
	postReading(t, st, "m2", 1, 1001, powerReading("w", "Real Power (W)", w, 1))
	newer1 := postReading(t, st, "m1", 2, 1002, powerReading("w", "Real Power (W)", w, 2))
	newer2 := postReading(t, st, "m1", 3, 1003, powerReading("w", "Real Power (W)", w, 3))

	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(1010, 0), time.Hour, 2)
	if err != nil || removed != 1 {
		t.Fatalf("sweep = %d, %v; want 1 removed", removed, err)
	}
	if got := remainingIDs(t, st, "m2"); len(got) != 0 {
		t.Errorf("m2 remaining = %v, want its reading, the oldest of the device, gone", got)
	}
	if got, want := remainingIDs(t, st, "m1"), []string{newer1, newer2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("m1 remaining = %v, want %v", got, want)
	}
}

func TestSweepKeepsATypedReadingWhileUntypedOnesSurvive(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	// Only the first post carries the type; later posts reuse the mRID.
	typed := postReading(t, st, "m1", 1, 1001, powerReading("w", "Real Power (W)", w, 1))
	var untyped []string
	for i := int64(2); i <= 5; i++ {
		untyped = append(untyped, postReading(t, st, "m1", i, 1000+i, powerReading("w", "", nil, i)))
	}

	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(1010, 0), time.Hour, 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Cap 2 of five readings drops the oldest three; the typed one is the
	// oldest, but the survivors need it, so it stays and only two go.
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (the cap ran on the untyped readings)", removed)
	}
	if got, want := remainingIDs(t, st, "m1"), []string{typed, untyped[2], untyped[3]}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remaining = %v, want %v: the typed reading kept so survivors still have a type", got, want)
	}
	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(res.Series) != 1 || res.Series[0].Uom != 38 {
		t.Errorf("series after sweep = %+v, want one series of uom 38", res.Series)
	}
}

func TestSweepRemovesUntypedReadingsPastTheAge(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	typed := postReading(t, st, "m1", 1, 1000, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m1", 2, 1001, powerReading("w", "", nil, 2)) // untyped and old
	fresh1 := postReading(t, st, "m1", 3, 5000, powerReading("w", "", nil, 3))
	fresh2 := postReading(t, st, "m1", 4, 5001, powerReading("w", "", nil, 4))

	removed, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(5010, 0), time.Hour, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1: the old untyped reading goes, the old typed one stays for the survivors", removed)
	}
	if got, want := remainingIDs(t, st, "m1"), []string{typed, fresh1, fresh2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

func TestSweepRemovesAnOldTypedReadingWhenNothingUntypedSurvives(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 1, 1000, powerReading("w", "Real Power (W)", w, 1))
	fresh := postReading(t, st, "m1", 2, 5000, powerReading("w", "Real Power (W)", w, 2))

	if _, err := sweepMirrorReadings(context.Background(), st, nil, time.Unix(5010, 0), time.Hour, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got, want := remainingIDs(t, st, "m1"), []string{fresh}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

func TestSweepRefusesANonPositiveBound(t *testing.T) {
	st := newStores()
	for _, c := range []struct {
		age time.Duration
		n   int
	}{{0, 10}, {time.Hour, 0}, {-time.Hour, 10}} {
		if _, err := sweepMirrorReadings(context.Background(), st, nil, time.Now(), c.age, c.n); err == nil {
			t.Errorf("sweep(age=%v, n=%d) accepted, want a refusal", c.age, c.n)
		}
	}
}

func TestMirrorSeriesScalesAndStampsWithTheReceiptTime(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	postReading(t, st, "m1", 1, 5000, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 3, 0), 12))
	postReading(t, st, "m1", 2, 5030, powerReading("e", "Energy", rtype(72, 12, 0, -3, 0), 12345))
	postReading(t, st, "m1", 3, 5060, powerReading("p", "Plain", rtype(29, 0, 0, 0, 0), 555))

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	byUom := map[uint8]MirrorSeries{}
	for _, s := range res.Series {
		byUom[s.Uom] = s
	}
	if len(res.Series) != 3 || res.TotalSeries != 3 || res.SeriesTruncated {
		t.Fatalf("series = %d total=%d truncated=%v, want 3, 3, false", len(res.Series), res.TotalSeries, res.SeriesTruncated)
	}
	for _, c := range []struct {
		uom  uint8
		at   int64
		want float64
	}{{38, 5000, 12000}, {72, 5030, 12.345}, {29, 5060, 555}} {
		s := byUom[c.uom]
		if len(s.Points) != 1 || s.Points[0].Time != c.at || s.Points[0].Value != c.want {
			t.Errorf("uom %d points = %+v, want one point {%d %v}", c.uom, s.Points, c.at, c.want)
		}
		if s.DeviceLFDI != "LFDI-A" || s.Mirror != "" {
			t.Errorf("uom %d device = %q mirror = %q, want LFDI-A and no mirror id", c.uom, s.DeviceLFDI, s.Mirror)
		}
	}
	if byUom[38].Kind != 37 || byUom[38].Description != "Real Power (W)" {
		t.Errorf("uom 38 kind/description = %d/%q, want 37 and Real Power (W)", byUom[38].Kind, byUom[38].Description)
	}
}

func TestMirrorSeriesIsNotSplitBySignChanges(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	// The client sets flowDirection from the sign: 1 forward, 19 reverse.
	postReading(t, st, "m1", 1, 6000, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 300))
	postReading(t, st, "m1", 2, 6030, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 19), -300))
	postReading(t, st, "m1", 3, 6060, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 0, 1), 200))

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("series = %d, want 1: a flowDirection change must not split the line", len(res.Series))
	}
	got := res.Series[0].Points
	want := []MirrorPoint{{Time: 6000, Value: 300}, {Time: 6030, Value: -300}, {Time: 6060, Value: 200}}
	if fmt.Sprint(bare(got)) != fmt.Sprint(want) {
		t.Errorf("points = %v, want %v", got, want)
	}
}

func TestMirrorSeriesSinceIsInclusiveAndReportsNothingOlder(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	for i := int64(1); i <= 4; i++ {
		postReading(t, st, "m1", i, 7000+10*i, powerReading("w", "Real Power (W)", w, i))
	}
	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{Since: 7030})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	got := res.Series[0].Points
	if want := []MirrorPoint{{Time: 7030, Value: 3}, {Time: 7040, Value: 4}}; fmt.Sprint(bare(got)) != fmt.Sprint(want) {
		t.Errorf("points since 7030 = %v, want %v", got, want)
	}
	res, err = mirrorSeries(context.Background(), st, nil, MirrorQuery{Since: 7041})
	if err != nil || len(res.Series) != 0 || res.TotalSeries != 0 {
		t.Errorf("since past every point: %+v, %v; want no series", res, err)
	}
	if res.Series == nil {
		t.Error("Series is nil, want an empty slice so it encodes as []")
	}
}

func TestMirrorSeriesSinceIncludesAReadingAtTheExactSecondBoundary(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 0, 7030, powerReading("w", "Real Power (W)", w, 3))
	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{Since: 7030})
	if err != nil || len(res.Series) != 1 {
		t.Fatalf("series = %+v, %v; want one", res.Series, err)
	}
	if want := []MirrorPoint{{Time: 7030, Value: 3}}; fmt.Sprint(bare(res.Series[0].Points)) != fmt.Sprint(want) {
		t.Errorf("points = %v, want %v: the id 7030*1e9 is the first one in range", res.Series[0].Points, want)
	}
}

func TestMirrorSeriesShowsAnUnregisteredDeviceByItsLFDI(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "UNREGISTERED-LFDI")
	seedMirror(t, st, "m2", "")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 1, 8000, powerReading("w", "Real Power (W)", w, 5))
	postReading(t, st, "m2", 2, 8000, powerReading("w", "Real Power (W)", w, 6))

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(res.Series) != 2 {
		t.Fatalf("series = %d, want 2", len(res.Series))
	}
	// The empty LFDI sorts first.
	if s := res.Series[0]; s.DeviceLFDI != "" || s.Mirror != "m2" || s.Points[0].Value != 6 {
		t.Errorf("mirror with no LFDI = %+v, want it named by mirror id m2", s)
	}
	if s := res.Series[1]; s.DeviceLFDI != "UNREGISTERED-LFDI" || s.Mirror != "" || s.Points[0].Value != 5 {
		t.Errorf("mirror with an LFDI = %+v, want it named by that LFDI", s)
	}
}

func TestMirrorSeriesCapsPointsAndSeriesAndSaysSo(t *testing.T) {
	st := newStores()
	for _, m := range []string{"m1", "m2", "m3"} {
		seedMirror(t, st, m, "LFDI-"+m)
	}
	w := rtype(38, 37, 0, 0, 1)
	for i := int64(1); i <= 5; i++ {
		postReading(t, st, "m1", i, 9000+i, powerReading("w", "Real Power (W)", w, i))
	}
	postReading(t, st, "m2", 6, 9000, powerReading("w", "Real Power (W)", w, 1))
	postReading(t, st, "m3", 7, 9000, powerReading("w", "Real Power (W)", w, 1))

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{MaxSeries: 2, MaxPoints: 3})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(res.Series) != 2 || res.TotalSeries != 3 || !res.SeriesTruncated {
		t.Fatalf("series = %d total=%d truncated=%v, want 2, 3, true", len(res.Series), res.TotalSeries, res.SeriesTruncated)
	}
	s := res.Series[0]
	if !s.Truncated || s.Total != 5 {
		t.Errorf("m1 truncated=%v total=%d, want true and 5", s.Truncated, s.Total)
	}
	if want := []MirrorPoint{{Time: 9003, Value: 3}, {Time: 9004, Value: 4}, {Time: 9005, Value: 5}}; fmt.Sprint(bare(s.Points)) != fmt.Sprint(want) {
		t.Errorf("m1 points = %v, want the newest three %v", s.Points, want)
	}
	if o := res.Series[1]; o.Truncated || o.Total != 1 {
		t.Errorf("m2 truncated=%v total=%d, want false and 1", o.Truncated, o.Total)
	}
}

func TestMirrorSeriesReadsASetAndSkipsAReadingWithNoValue(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, -1, 1)
	set := sep2.MirrorMeterReading{
		MRID: "w", Description: "Real Power (W)", ReadingType: w,
		MirrorReadingSet: []sep2.MirrorReadingSet{{Reading: []sep2.Reading{{Value: i64(15)}, {}, {Value: i64(25)}}}},
	}
	postReading(t, st, "m1", 1, 9500, set)
	postReading(t, st, "m1", 2, 9510, sep2.MirrorMeterReading{MRID: "w", ReadingType: w, Reading: &sep2.Reading{}})

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(res.Series))
	}
	if want := []MirrorPoint{{Time: 9500, Value: 1.5}, {Time: 9500, Value: 2.5}}; fmt.Sprint(bare(res.Series[0].Points)) != fmt.Sprint(want) {
		t.Errorf("points = %v, want %v (the valueless readings are skipped, not zero)", res.Series[0].Points, want)
	}
}

func TestMirrorSeriesTypesAReadingThatReusesAnMRID(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	postReading(t, st, "m1", 1, 9600, powerReading("w", "Real Power (W)", rtype(38, 37, 0, 1, 1), 5))
	postReading(t, st, "m1", 2, 9630, powerReading("w", "", nil, 6))

	res, err := mirrorSeries(context.Background(), st, nil, MirrorQuery{})
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(res.Series) != 1 || res.Series[0].Uom != 38 || res.Series[0].Description != "Real Power (W)" {
		t.Fatalf("series = %+v, want one uom 38 series named Real Power (W)", res.Series)
	}
	if want := []MirrorPoint{{Time: 9600, Value: 50}, {Time: 9630, Value: 60}}; fmt.Sprint(bare(res.Series[0].Points)) != fmt.Sprint(want) {
		t.Errorf("points = %v, want %v (the inherited multiplier 1 applies)", res.Series[0].Points, want)
	}
}

func TestMirrorSeriesWithNoMirrorStoreIsEmpty(t *testing.T) {
	res, err := mirrorSeries(context.Background(), &assembly.Stores{}, nil, MirrorQuery{})
	if err != nil || len(res.Series) != 0 || res.Series == nil {
		t.Errorf("empty stores: %+v, %v; want an empty non-nil series list and no error", res, err)
	}
}

func TestScaleReading(t *testing.T) {
	for _, c := range []struct {
		v    int64
		mult int8
		want float64
	}{{7, 0, 7}, {7, 3, 7000}, {12345, -3, 12.345}, {-300, 0, -300}, {5, -1, 0.5}, {3, -1, 0.3}, {1, -9, 1e-9}} {
		if got := scaleReading(c.v, c.mult); got != c.want {
			t.Errorf("scaleReading(%d, %d) = %v, want %v", c.v, c.mult, got, c.want)
		}
	}
}

func TestRunSweepsMirrorReadingsAndStopsWithTheContext(t *testing.T) {
	st := newStores()
	seedMirror(t, st, "m1", "LFDI-A")
	w := rtype(38, 37, 0, 0, 1)
	postReading(t, st, "m1", 1, 100, powerReading("w", "Real Power (W)", w, 1)) // decades old
	e := &Embed{
		srv:            &blockingServer{},
		notifier:       coresub.NewManager(st.Subscriptions, 1, 1),
		stores:         st,
		mirrorMaxAge:   time.Hour,
		mirrorInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for len(remainingIDs(t, st, "m1")) != 0 {
		select {
		case <-deadline:
			t.Fatal("the old reading was not swept within 2s of Run starting")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled; the sweep goroutine is not joined")
	}
}

func TestMirrorRetentionSettingsDefaultsAndOverrides(t *testing.T) {
	age, n, iv := (&Embed{}).mirrorRetentionSettings()
	if age != DefaultMirrorReadingRetention || n != DefaultMirrorReadingMaxPerSeries || iv != DefaultMirrorRetentionInterval {
		t.Errorf("defaults = %v/%d/%v, want %v/%d/%v", age, n, iv, DefaultMirrorReadingRetention, DefaultMirrorReadingMaxPerSeries, DefaultMirrorRetentionInterval)
	}
	age, n, iv = (&Embed{mirrorMaxAge: 2 * time.Hour, mirrorMaxPerSeries: 77, mirrorInterval: 3 * time.Second}).mirrorRetentionSettings()
	if age != 2*time.Hour || n != 77 || iv != 3*time.Second {
		t.Errorf("overrides = %v/%d/%v, want 2h/77/3s", age, n, iv)
	}
}

// blockingServer is a protocolServer that serves until its ctx ends.
type blockingServer struct{}

func (*blockingServer) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (*blockingServer) Addr() string                  { return "127.0.0.1:0" }

// bare returns pts with the ids cleared, for comparing times and values.
func bare(pts []MirrorPoint) []MirrorPoint {
	out := make([]MirrorPoint, len(pts))
	for i, p := range pts {
		out[i] = MirrorPoint{Time: p.Time, Value: p.Value}
	}
	return out
}
