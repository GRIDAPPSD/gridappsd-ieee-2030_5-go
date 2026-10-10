package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// fakeMirror is a MirrorSource that records the query it was given.
type fakeMirror struct {
	res  sep2embed.MirrorSeriesResult
	err  error
	last struct {
		since             int64
		maxSeries, points int
	}
}

func (f *fakeMirror) MirrorSeries(_ context.Context, since int64, maxSeries, maxPoints int) (sep2embed.MirrorSeriesResult, error) {
	f.last.since, f.last.maxSeries, f.last.points = since, maxSeries, maxPoints
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
				Points: []sep2embed.MirrorPoint{{Time: 1_700_000_100, Value: 300}, {Time: 1_700_000_130, Value: -300.5}}},
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
	if len(reg.Points) != 2 || reg.Points[0] != (mirrorPointResponse{T: 1_700_000_100, V: 300}) || reg.Points[1] != (mirrorPointResponse{T: 1_700_000_130, V: -300.5}) {
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
	for _, q := range []string{"since=abc", "since=-1", "series=x", "points=-5", "since=1.5"} {
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
