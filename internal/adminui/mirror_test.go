package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// fakeMirror is a MirrorSource that records the query it was given.
type fakeMirror struct {
	mu  sync.Mutex
	res sep2embed.MirrorSeriesResult
	err error
	// during, when set, runs inside the call and may replace the answer, to
	// model readings stored or time passing while a series is built.
	during func(q sep2embed.MirrorQuery) sep2embed.MirrorSeriesResult
	last   struct {
		since             int64
		maxSeries, points int
		device            string
		uom               *uint8
	}
}

func (f *fakeMirror) MirrorSeries(_ context.Context, q sep2embed.MirrorQuery) (sep2embed.MirrorSeriesResult, error) {
	f.mu.Lock()
	f.last.since, f.last.maxSeries, f.last.points, f.last.device, f.last.uom = q.Since, q.MaxSeries, q.MaxPoints, q.Device, q.Uom
	f.mu.Unlock()
	if f.during != nil {
		return f.during(q), f.err
	}
	return f.res, f.err
}

func mirrorServer(t *testing.T, m *fakeMirror) *Server {
	t.Helper()
	src := testSources()
	src.Mirror = m
	src.Registry = &fakeRegistry{entries: []registry.Entry{{MRID: "mrid-1", Name: "pv-1", LFDI: "abcdef"}}}
	s := newServer(t, Config{Key: testKey}, src)
	s.now = func() time.Time { return time.Unix(1_700_000_500, 0) }
	return s
}

func TestMirrorRouteNeedsNoCredentialAndReportsValues(t *testing.T) {
	t.Parallel()
	m := &fakeMirror{res: sep2embed.MirrorSeriesResult{
		TotalSeries: 2,
		Series: []sep2embed.MirrorSeries{
			{DeviceLFDI: "ABCDEF", Uom: 38, Kind: 37, Description: "Real Power (W)", Total: 2,
				Points: []sep2embed.MirrorPoint{{ID: "m1/a.0", Time: 1_700_000_100, Value: 300}, {ID: "m1/b.0", Time: 1_700_000_130, Value: -300.5}}},
			{DeviceLFDI: "NOT-IN-REGISTRY", Uom: 29, Description: "PH_ABC (V)", Total: 1,
				Points: []sep2embed.MirrorPoint{{Time: 1_700_000_100, Value: 555}}},
		},
	}}
	s := mirrorServer(t, m)

	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror?since=1700000000", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET without Authorization = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if m.last.since != 1_700_000_000 {
		t.Errorf("source got since=%d, want 1700000000", m.last.since)
	}
	var got mirrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v; %s", err, rec.Body.String())
	}
	if got.Now != 1_700_000_500 || got.Since != 1_700_000_000 || got.TotalSeries != 2 || got.SeriesTruncated {
		t.Errorf("envelope = now %d since %d total %d truncated %v", got.Now, got.Since, got.TotalSeries, got.SeriesTruncated)
	}
	if len(got.Series) != 2 {
		t.Fatalf("series = %d, want 2", len(got.Series))
	}
	reg, unreg := got.Series[0], got.Series[1]
	if !reg.Registered || reg.MRID != "mrid-1" || reg.Name != "pv-1" || reg.Unit != "W" || reg.DeviceLFDI != "ABCDEF" {
		t.Errorf("registered series = %+v", reg)
	}
	if len(reg.Points) != 2 || reg.Points[0] != (mirrorPointResponse{ID: "m1/a.0", T: 1_700_000_100, V: 300}) || reg.Points[1] != (mirrorPointResponse{ID: "m1/b.0", T: 1_700_000_130, V: -300.5}) {
		t.Errorf("registered points = %+v", reg.Points)
	}
	if unreg.Registered || unreg.MRID != "" || unreg.Name != "" || unreg.DeviceLFDI != "NOT-IN-REGISTRY" || unreg.Unit != "V" {
		t.Errorf("unregistered series = %+v, want it listed by LFDI alone", unreg)
	}
}

func TestMirrorRouteKeepsTheHostAndMethodGates(t *testing.T) {
	t.Parallel()
	s := mirrorServer(t, &fakeMirror{})
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "evil.example"); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Host = %d, want 403", rec.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := doRequest(t, s.Handler(), method, "/apps/soc/api/mirror", "", "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
}

func TestMirrorRouteRefusesABadQueryRatherThanDefaulting(t *testing.T) {
	t.Parallel()
	m := &fakeMirror{}
	s := mirrorServer(t, m)
	for _, q := range []string{"since=abc", "since=-1", "series=x", "points=-5", "since=1.5", "uom=256", "uom=-1", "uom=x"} {
		m.last.maxSeries = -1
		rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror?"+q, "", "localhost")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, rec.Code)
		}
		if m.last.maxSeries != -1 {
			t.Errorf("?%s reached the source", q)
		}
	}
}

func TestMirrorRouteBoundsWhatItAsksFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		query              string
		wantSeries, wantPt int
	}{
		{"", defaultMirrorSeries, defaultMirrorPoints},
		{"?series=7&points=9", 7, 9},
		{"?series=7", 7, defaultMirrorPoints},
		{"?series=0&points=0", defaultMirrorSeries, defaultMirrorPoints},
		{"?series=99999&points=99999", sep2embed.MaxMirrorSeries, maxMirrorResponsePoints / sep2embed.MaxMirrorSeries},
		{"?series=10&points=99999", 10, sep2embed.MaxMirrorPointsSeries},
		{"?series=100&points=2000", 100, maxMirrorResponsePoints / 100},
	} {
		m := &fakeMirror{}
		s := mirrorServer(t, m)
		rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror"+c.query, "", "localhost")
		if rec.Code != http.StatusOK {
			t.Fatalf("%q = %d", c.query, rec.Code)
		}
		if m.last.maxSeries != c.wantSeries || m.last.points != c.wantPt {
			t.Errorf("%q asked the source for %d series x %d points, want %d x %d", c.query, m.last.maxSeries, m.last.points, c.wantSeries, c.wantPt)
		}
	}
}

func TestMirrorRouteSaysWhenItCut(t *testing.T) {
	t.Parallel()
	m := &fakeMirror{res: sep2embed.MirrorSeriesResult{
		TotalSeries: 9, SeriesTruncated: true,
		Series: []sep2embed.MirrorSeries{{DeviceLFDI: "ABCDEF", Uom: 63, Total: 40, Truncated: true, Points: []sep2embed.MirrorPoint{{Time: 5, Value: 1}}}},
	}}
	s := mirrorServer(t, m)
	var got mirrorResponse
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.SeriesTruncated || got.TotalSeries != 9 || !got.Series[0].Truncated || got.Series[0].Total != 40 || got.Series[0].Unit != "var" {
		t.Errorf("response = %+v, want the cuts reported", got)
	}
}

func TestMirrorRouteEmptyIsAnEmptyArrayAndUnknownUomIsNamed(t *testing.T) {
	t.Parallel()
	m := &fakeMirror{}
	s := mirrorServer(t, m)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost")
	if !strings.Contains(rec.Body.String(), `"series":[]`) {
		t.Errorf("empty body = %s, want series as []", rec.Body.String())
	}
	if got := uomUnit(5); got != "uom 5" {
		t.Errorf("uomUnit(5) = %q, want \"uom 5\"", got)
	}
}

func TestMirrorRouteFailureLeaksNothingInternal(t *testing.T) {
	t.Parallel()
	s := mirrorServer(t, &fakeMirror{err: errors.New("store exploded at /secret/path")})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("failure = %d %q, want 500 without the internal error", rec.Code, rec.Body.String())
	}
}

func TestMirrorRouteIsAbsentWithoutASource(t *testing.T) {
	t.Parallel()
	s := newServer(t, Config{Key: testKey}, testSources())
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost")
	if rec.Code == http.StatusOK {
		t.Errorf("unmounted route answered 200: %s", rec.Body.String())
	}
}

// TestPlaneMountsNoMirrorRoute: the exact mirror pattern sits in front of
// the plane, so a plane route on the same path would be silently shadowed.
func TestPlaneMountsNoMirrorRoute(t *testing.T) {
	t.Parallel()
	s := newServer(t, Config{Key: testKey}, testSources())
	for _, p := range s.planePatterns {
		if _, path, _ := strings.Cut(p, " "); path == mirrorRoute {
			t.Errorf("the plane mounts %q, which the mirror route shadows", p)
		}
	}
}

func TestMirrorRouteDefaultsHoldAFullFleetInsideTheBodyBound(t *testing.T) {
	t.Parallel()
	const fleetSeries = 49 * 3
	if defaultMirrorSeries < fleetSeries {
		t.Errorf("defaultMirrorSeries = %d, below the %d series of a 49-device fleet", defaultMirrorSeries, fleetSeries)
	}
	if defaultMirrorSeries*defaultMirrorPoints > maxMirrorResponsePoints {
		t.Errorf("default %d series x %d points exceeds the %d-point body bound", defaultMirrorSeries, defaultMirrorPoints, maxMirrorResponsePoints)
	}
}

func TestMirrorRoutePassesTheDeviceAndUnitFilters(t *testing.T) {
	t.Parallel()
	m := &fakeMirror{}
	s := mirrorServer(t, m)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror?device=ABCDEF&uom=38", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered GET = %d: %s", rec.Code, rec.Body.String())
	}
	if m.last.device != "ABCDEF" || m.last.uom == nil || *m.last.uom != 38 {
		t.Errorf("source got device %q uom %v, want ABCDEF and 38", m.last.device, m.last.uom)
	}
	m.last.uom = nil
	doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost")
	if m.last.device != "" || m.last.uom != nil {
		t.Errorf("unfiltered GET reached the source with device %q uom %v, want neither", m.last.device, m.last.uom)
	}
	doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror?uom=0", "", "localhost")
	if m.last.uom == nil || *m.last.uom != 0 {
		t.Errorf("uom=0 reached the source as %v, want a set zero, not no filter", m.last.uom)
	}
}

// TestMirrorRouteCursorCannotSkipAReadingStoredDuringTheBuild: a reading
// stamped 1001 is stored after the first poll's series were listed, while
// the clock moves on to 1003. The cursor the first poll hands back must still
// find it.
func TestMirrorRouteCursorCannotSkipAReadingStoredDuringTheBuild(t *testing.T) {
	t.Parallel()
	clock := int64(1001)
	var stored []sep2embed.MirrorPoint
	m := &fakeMirror{}
	m.during = func(q sep2embed.MirrorQuery) sep2embed.MirrorSeriesResult {
		var pts []sep2embed.MirrorPoint
		for _, p := range stored {
			if p.Time >= q.Since {
				pts = append(pts, p)
			}
		}
		res := sep2embed.MirrorSeriesResult{TotalSeries: 1, Series: []sep2embed.MirrorSeries{{DeviceLFDI: "ABCDEF", Uom: 38, Total: len(pts), Points: pts}}}
		if len(pts) == 0 {
			res = sep2embed.MirrorSeriesResult{}
		}
		if len(stored) == 0 {
			stored = append(stored, sep2embed.MirrorPoint{ID: "m1/x.0", Time: 1001, Value: 7})
			clock = 1003
		}
		return res
	}
	s := mirrorServer(t, m)
	s.now = func() time.Time { return time.Unix(clock, 0) }

	poll := func(since string) mirrorResponse {
		rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror?since="+since, "", "localhost")
		var got mirrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body: %v; %s", err, rec.Body.String())
		}
		return got
	}
	first := poll("0")
	if len(first.Series) != 0 {
		t.Fatalf("first poll = %+v, want it to miss the reading stored during the build", first)
	}
	if first.Now != 1001 {
		t.Fatalf("first poll now = %d, want 1001, the clock before the series were built", first.Now)
	}
	second := poll("1001")
	if len(second.Series) != 1 || len(second.Series[0].Points) != 1 || second.Series[0].Points[0].ID != "m1/x.0" || second.Series[0].Points[0].V != 7 {
		t.Errorf("second poll from the first poll's now = %+v, want the reading stamped 1001", second)
	}
}

func TestMirrorRouteRefusesWhenTooManyBuildsAreRunning(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{}, maxMirrorBuilds)
	m := &fakeMirror{}
	m.during = func(sep2embed.MirrorQuery) sep2embed.MirrorSeriesResult {
		entered <- struct{}{}
		<-release
		return sep2embed.MirrorSeriesResult{}
	}
	s := mirrorServer(t, m)

	codes := make(chan int, maxMirrorBuilds)
	for range maxMirrorBuilds {
		go func() {
			codes <- doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost").Code
		}()
	}
	for range maxMirrorBuilds {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the allowed builds did not all start")
		}
	}
	extra := make(chan *httptest.ResponseRecorder, 1)
	go func() { extra <- doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost") }()
	select {
	case rec := <-extra:
		if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != mirrorBusy || rec.Header().Get("Retry-After") == "" {
			t.Errorf("extra build = %d %q Retry-After %q, want 503 %q and a Retry-After", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), mirrorBusy)
		}
	case <-time.After(3 * time.Second):
		t.Error("an extra build waited instead of being refused")
	}
	close(release)
	for range maxMirrorBuilds {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("a build admitted before the limit = %d, want 200", code)
		}
	}
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror", "", "localhost"); rec.Code != http.StatusOK {
		t.Errorf("after the builds finished = %d, want 200: slots must be released", rec.Code)
	}
}

// Both the mirror series route and the watts control routes are mounted when
// both sources are set, and the plane keeps every other path.
func TestMirrorAndControlRoutesAreMountedTogether(t *testing.T) {
	t.Parallel()
	m := &fakeMirror{}
	ctl := newFakeControl()
	src := testSources()
	src.Mirror = m
	src.Control = ctl
	s := newServer(t, Config{Key: testKey}, src)
	s.now = func() time.Time { return time.Unix(1_700_000_500, 0) }

	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/mirror?since=1700000000", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("mirror GET = %d, want 200; %s", rec.Code, rec.Body)
	}
	if m.last.since != 1_700_000_000 {
		t.Errorf("mirror source got since=%d, want 1700000000", m.last.since)
	}
	rec = postControl(t, s.Handler(), "application/json", `{"mrid":"_pv-1","watts":800}`, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("control send = %d, want 200; %s", rec.Code, rec.Body)
	}
	if want := []controlCall{{"_pv-1", 800, 300}}; !slices.Equal(ctl.calls, want) {
		t.Errorf("control calls = %+v, want %+v", ctl.calls, want)
	}
}
