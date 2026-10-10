package adminui

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// outputHistory is a HistorySource over fixed series.
type outputHistory struct {
	series []telemetryhistory.SeriesSnapshot
	during func()
}

func (f *outputHistory) Snapshot() []telemetryhistory.SeriesSnapshot {
	if f.during != nil {
		f.during()
	}
	return f.series
}

func histSeries(obj, attr string, samples ...telemetryhistory.Sample) telemetryhistory.SeriesSnapshot {
	return telemetryhistory.SeriesSnapshot{Key: telemetryhistory.SeriesKey{Object: obj, Attribute: attr}, Samples: samples}
}

const (
	attrSoC      = "DERStatus.stateOfChargeStatus"
	attrSetpoint = "DERControl.DERControlBase.opModTargetW"
)

func pageServer(t *testing.T, h *outputHistory) *Server {
	t.Helper()
	src := testSources()
	src.Mirror = &fakeMirror{}
	src.History = h
	src.Registry = &fakeRegistry{entries: []registry.Entry{
		{MRID: "m-b", Name: "pv-b", LFDI: "BBBB", SFDI: "222222222", Placeholder: true},
		{MRID: "m-a", Name: "bat-a", LFDI: "AAAA", SFDI: "111111111"},
	}}
	s := newServer(t, Config{Key: testKey}, src)
	s.now = func() time.Time { return time.Unix(1_700_000_500, 0) }
	return s
}

func TestDevicesListsNameAndMridSortedAndNothingElse(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/devices", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `[{"mrid":"m-a","name":"bat-a"},{"mrid":"m-b","name":"pv-b"}]`+"\n" {
		t.Errorf("body = %s", got)
	}
	for _, banned := range []string{"lfdi", "AAAA", "BBBB", "sfdi", "111111111", "222222222", "placeholder"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("body carries %q: %s", banned, rec.Body.String())
		}
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	assertSocHeaders(t, rec.Header())
}

func TestDevicesSortByNameThenMrid(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	s.registry = &fakeRegistry{entries: []registry.Entry{
		{MRID: "m-1", Name: "zeta"},
		{MRID: "m-3", Name: "alpha"},
		{MRID: "m-2", Name: "alpha"},
		{MRID: "m-4", Name: "mid"},
	}}
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/devices", "", "localhost")
	want := `[{"mrid":"m-2","name":"alpha"},{"mrid":"m-3","name":"alpha"},{"mrid":"m-4","name":"mid"},{"mrid":"m-1","name":"zeta"}]` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %s, want %s", rec.Body.String(), want)
	}
}

func TestDevicesEmptyRegistryIsAnEmptyArray(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	s.registry = &fakeRegistry{}
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/devices", "", "localhost")
	if rec.Body.String() != "[]\n" {
		t.Errorf("body = %q, want []", rec.Body.String())
	}
}

func TestOutputKeepsReportedStateSinceAndNamesDevices(t *testing.T) {
	t.Parallel()
	h := &outputHistory{series: []telemetryhistory.SeriesSnapshot{
		histSeries("m-a", attrSetpoint, telemetryhistory.Sample{At: 1_700_000_300, Value: 5000}),
		histSeries("m-a", attrSoC, telemetryhistory.Sample{At: 1_700_000_100, Value: 40}, telemetryhistory.Sample{At: 1_700_000_200, Value: 41.5}, telemetryhistory.Sample{At: 1_700_000_300, Value: 42}),
		histSeries("m-b", "Other.thing", telemetryhistory.Sample{At: 1_700_000_300, Value: 1}),
		histSeries("m-z", attrSoC, telemetryhistory.Sample{At: 1_700_000_050, Value: 9}),
	}}
	s := pageServer(t, h)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output?since=1700000200", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got outputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got.Now != 1_700_000_500 || got.Since != 1_700_000_200 || got.TotalSeries != 1 || got.SeriesTruncated || len(got.Series) != 1 {
		t.Fatalf("envelope = %+v", got)
	}
	sr := got.Series[0]
	if sr.MRID != "m-a" || sr.Name != "bat-a" || sr.Attribute != attrSoC || sr.Total != 2 || sr.Truncated {
		t.Errorf("series = %+v", sr)
	}
	if len(sr.Points) != 2 || sr.Points[0] != (outputPointResponse{T: 1_700_000_200, V: 41.5}) || sr.Points[1] != (outputPointResponse{T: 1_700_000_300, V: 42}) {
		t.Errorf("points = %+v, want the two at or after since", sr.Points)
	}
	for _, banned := range []string{"DERControl", "opModTargetW", "Other.thing", "m-z"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("body carries %q: %s", banned, rec.Body.String())
		}
	}
	assertSocHeaders(t, rec.Header())
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestOutputDeviceFilterAndNowReadBeforeSnapshot(t *testing.T) {
	t.Parallel()
	h := &outputHistory{series: []telemetryhistory.SeriesSnapshot{
		histSeries("m-a", attrSoC, telemetryhistory.Sample{At: 1_700_000_100, Value: 40}),
		histSeries("m-b", attrSoC, telemetryhistory.Sample{At: 1_700_000_100, Value: 50}),
	}}
	s := pageServer(t, h)
	clock := int64(1_700_000_500)
	s.now = func() time.Time { return time.Unix(clock, 0) }
	h.during = func() { clock += 100 }
	var got outputResponse
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output?device=m-b", "", "localhost")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Series) != 1 || got.Series[0].MRID != "m-b" || got.Series[0].Points[0].V != 50 {
		t.Errorf("series = %+v, want only m-b", got.Series)
	}
	if got.Now != 1_700_000_500 {
		t.Errorf("now = %d, want the clock read before the snapshot", got.Now)
	}
}

func TestOutputCapsSayWhatTheyCut(t *testing.T) {
	t.Parallel()
	var samples []telemetryhistory.Sample
	for i := int64(0); i < 10; i++ {
		samples = append(samples, telemetryhistory.Sample{At: 1_700_000_000 + i, Value: float64(i)})
	}
	h := &outputHistory{series: []telemetryhistory.SeriesSnapshot{
		histSeries("m-a", attrSoC, samples...),
		histSeries("m-b", attrSoC, samples...),
		histSeries("m-c", attrSoC, samples...),
	}}
	s := pageServer(t, h)
	var got outputResponse
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output?series=2&points=3", "", "localhost")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TotalSeries != 3 || !got.SeriesTruncated || len(got.Series) != 2 {
		t.Errorf("envelope = total %d truncated %v kept %d", got.TotalSeries, got.SeriesTruncated, len(got.Series))
	}
	sr := got.Series[0]
	if sr.Total != 10 || !sr.Truncated || len(sr.Points) != 3 || sr.Points[0].V != 7 || sr.Points[2].V != 9 {
		t.Errorf("series = %+v, want the newest 3 of 10", sr)
	}
}

func TestOutputRejectsBadNumbers(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	for _, q := range []string{"since=x", "since=-1", "series=1.5", "points=-2"} {
		if rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output?"+q, "", "localhost"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rec.Code)
		}
	}
}

func TestOutputBusyWhenAllBuildSlotsAreTaken(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	for range maxOutputBuilds {
		s.outputBuilds <- struct{}{}
	}
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output", "", "localhost")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), outputBusy) {
		t.Errorf("full gate = %d retry %q body %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	<-s.outputBuilds
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output", "", "localhost"); rec.Code != http.StatusOK {
		t.Errorf("after a slot freed = %d, want 200", rec.Code)
	}
}

func assertSocHeaders(t *testing.T, h http.Header) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestPageServedWithHeadersAndContentTypes(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	cases := []struct{ path, ctype, contains string }{
		{"/apps/soc/", "text/html; charset=utf-8", "loading"},
		{"/apps/soc/app.js", "text/javascript; charset=utf-8", "use strict"},
		{"/apps/soc/style.css", "text/css; charset=utf-8", "font-family"},
		{"/apps/soc/uplot.min.js", "text/javascript; charset=utf-8", "uPlot"},
		{"/apps/soc/uplot.min.css", "text/css; charset=utf-8", ".uplot"},
	}
	for _, c := range cases {
		rec := doRequest(t, s.Handler(), http.MethodGet, c.path, "", "localhost")
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", c.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s Content-Type = %q, want %q", c.path, got, c.ctype)
		}
		if !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s body lacks %q: %s", c.path, c.contains, rec.Body.String())
		}
		assertSocHeaders(t, rec.Header())
	}
}

func TestPageRedirectsAndKeepsGates(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc", "", "localhost")
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/apps/soc/" {
		t.Errorf("/apps/soc = %d Location %q, want 307 to /apps/soc/", rec.Code, rec.Header().Get("Location"))
	}
	for _, p := range []string{"/apps/soc", "/apps/soc/", "/apps/soc/app.js", "/apps/soc/style.css", "/apps/soc/uplot.min.js", "/apps/soc/uplot.min.css", "/apps/soc/api/devices", "/apps/soc/api/output", "/apps/soc/api/mirror"} {
		if rec := doRequest(t, s.Handler(), http.MethodGet, p, "", "evil.example"); rec.Code != http.StatusForbidden {
			t.Errorf("%s foreign Host = %d, want 403", p, rec.Code)
		}
		if rec := doRequest(t, s.Handler(), http.MethodPost, p, "", "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s POST = %d, want 405", p, rec.Code)
		}
	}
}

func TestMirrorAndSoCRoutesCarryTheSecurityHeaders(t *testing.T) {
	t.Parallel()
	src := testSources()
	src.SoC = newFakeSoC()
	src.Mirror = &fakeMirror{}
	s := newServer(t, Config{Key: testKey}, src)
	for _, p := range []string{"/apps/soc/api/mirror", "/apps/soc/api/soc/x"} {
		rec := doRequest(t, s.Handler(), http.MethodGet, p, "", "localhost")
		assertSocHeaders(t, rec.Header())
	}
	rec := postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","percent":80}`, "localhost")
	assertSocHeaders(t, rec.Header())
}

func TestOtherAppsPathsStillReachThePlane(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	for _, p := range []string{"/apps/other", "/apps/soc/unknown", "/apps/soc/api/nope"} {
		rec := doRequest(t, s.Handler(), http.MethodGet, p, "", "localhost")
		// The plane demands the key; none of our handlers answers these.
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
			t.Errorf("%s = %d, want the plane's login response", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "loading") || rec.Header().Get("Content-Security-Policy") == socPageCSP {
			t.Errorf("%s was answered by the page: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestPageAbsentWithoutMirrorOrSoC(t *testing.T) {
	t.Parallel()
	src := testSources()
	s := newServer(t, Config{Key: testKey}, src)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/", "", "localhost")
	if rec.Header().Get("Content-Security-Policy") == socPageCSP || strings.Contains(rec.Body.String(), "loading") {
		t.Errorf("page served with no mirror or SoC source: %d", rec.Code)
	}
}

// Every file in socpage is embedded and served at /apps/soc/<name> (index.html
// at the root), and the page's own references name only mounted paths.
func TestEveryEmbeddedPageFileIsServedAndIndexUsesOnlyMountedPaths(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	entries, err := fs.ReadDir(socpage.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() == "socpage.go" {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) != 5 {
		t.Fatalf("embedded page files = %v, want index.html, app.js, style.css, uplot.min.js, uplot.min.css", names)
	}
	for _, n := range names {
		want, err := fs.ReadFile(socpage.Files, n)
		if err != nil || len(want) == 0 {
			t.Errorf("embedded %s: len %d, err %v", n, len(want), err)
			continue
		}
		path := "/apps/soc/" + n
		if n == "index.html" {
			path = "/apps/soc/"
		}
		rec := doRequest(t, s.Handler(), http.MethodGet, path, "", "localhost")
		if rec.Code != http.StatusOK || rec.Body.String() != string(want) {
			t.Errorf("%s = %d, served %d bytes, embedded %d", path, rec.Code, rec.Body.Len(), len(want))
		}
	}

	index, err := fs.ReadFile(socpage.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href|action)="([^"]*)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Fatal("index.html references nothing: the pattern or the page is wrong")
	}
	for _, m := range refs {
		ref := m[1]
		if !strings.HasPrefix(ref, "/apps/soc/") || strings.Contains(ref, "//") {
			t.Errorf("index.html references %q, which is not a same-origin page path", ref)
			continue
		}
		rec := doRequest(t, s.Handler(), http.MethodGet, ref, "", "localhost")
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != socPageCSP {
			t.Errorf("index.html references %s: status %d, page CSP %q", ref, rec.Code, rec.Header().Get("Content-Security-Policy"))
		}
	}
	if strings.Contains(string(index), "<script>") || strings.Contains(string(index), " style=") {
		t.Error("index.html carries inline script or style, which the CSP blocks")
	}
}

func getOutput(t *testing.T, s *Server, query string) (outputResponse, *httptest.ResponseRecorder) {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output"+query, "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("output%s = %d, body %s", query, rec.Code, rec.Body.String())
	}
	var got outputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	return got, rec
}

func nSamples(n int) []telemetryhistory.Sample {
	out := make([]telemetryhistory.Sample, n)
	for i := range out {
		out[i] = telemetryhistory.Sample{At: 1_700_000_000 + int64(i), Value: float64(i)}
	}
	return out
}

func TestOutputDefaultHoldsA49DeviceFleet(t *testing.T) {
	t.Parallel()
	const devices, attrs = 49, 7
	var series []telemetryhistory.SeriesSnapshot
	for d := range devices {
		for a := range attrs {
			series = append(series, histSeries(fmt.Sprintf("m-%02d", d), fmt.Sprintf("DERStatus.attr%d", a), nSamples(3)...))
		}
	}
	s := pageServer(t, &outputHistory{series: series})
	got, _ := getOutput(t, s, "")
	if got.TotalSeries != devices*attrs || got.SeriesTruncated || len(got.Series) != devices*attrs {
		t.Fatalf("total %d truncated %v kept %d, want all %d", got.TotalSeries, got.SeriesTruncated, len(got.Series), devices*attrs)
	}
	seen := map[string]int{}
	for _, sr := range got.Series {
		seen[sr.MRID]++
	}
	if seen["m-48"] != attrs {
		t.Errorf("last device m-48 has %d series, want %d", seen["m-48"], attrs)
	}
	if defaultOutputSeries*defaultOutputPoints > maxOutputResponsePoints {
		t.Errorf("default %d x %d exceeds the %d-point body bound", defaultOutputSeries, defaultOutputPoints, maxOutputResponsePoints)
	}
}

func TestOutputBuildSlotIsReleasedAfterEachRequest(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	for i := range 3 * maxOutputBuilds {
		if rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output", "", "localhost"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rec.Code)
		}
	}
	if n := len(s.outputBuilds); n != 0 {
		t.Errorf("%d build slots still held", n)
	}
}

func TestOutputCeilings(t *testing.T) {
	t.Parallel()
	t.Run("series ceiling", func(t *testing.T) {
		t.Parallel()
		var series []telemetryhistory.SeriesSnapshot
		for i := range maxOutputSeries + 5 {
			series = append(series, histSeries(fmt.Sprintf("m-%04d", i), attrSoC, nSamples(1)...))
		}
		s := pageServer(t, &outputHistory{series: series})
		got, _ := getOutput(t, s, fmt.Sprintf("?series=%d", maxOutputSeries+1))
		if len(got.Series) != maxOutputSeries || got.TotalSeries != maxOutputSeries+5 || !got.SeriesTruncated {
			t.Errorf("kept %d of %d truncated %v, want %d of %d", len(got.Series), got.TotalSeries, got.SeriesTruncated, maxOutputSeries, maxOutputSeries+5)
		}
	})
	t.Run("points per series ceiling", func(t *testing.T) {
		t.Parallel()
		s := pageServer(t, &outputHistory{series: []telemetryhistory.SeriesSnapshot{histSeries("m-a", attrSoC, nSamples(maxOutputPointsSeries+100)...)}})
		got, _ := getOutput(t, s, fmt.Sprintf("?series=1&points=%d", maxOutputPointsSeries+1))
		sr := got.Series[0]
		if len(sr.Points) != maxOutputPointsSeries || sr.Total != maxOutputPointsSeries+100 || !sr.Truncated {
			t.Errorf("served %d of %d truncated %v, want %d of %d", len(sr.Points), sr.Total, sr.Truncated, maxOutputPointsSeries, maxOutputPointsSeries+100)
		}
	})
	t.Run("response points ceiling", func(t *testing.T) {
		t.Parallel()
		per := maxOutputResponsePoints/maxOutputSeries + 10
		var series []telemetryhistory.SeriesSnapshot
		for i := range maxOutputSeries {
			series = append(series, histSeries(fmt.Sprintf("m-%04d", i), attrSoC, nSamples(per)...))
		}
		s := pageServer(t, &outputHistory{series: series})
		got, _ := getOutput(t, s, fmt.Sprintf("?series=%d&points=%d", maxOutputSeries, maxOutputPointsSeries))
		served := 0
		for _, sr := range got.Series {
			served += len(sr.Points)
		}
		if served > maxOutputResponsePoints || len(got.Series[0].Points) != maxOutputResponsePoints/maxOutputSeries {
			t.Errorf("served %d points, %d per series; want at most %d, %d per series", served, len(got.Series[0].Points), maxOutputResponsePoints, maxOutputResponsePoints/maxOutputSeries)
		}
	})
}

func TestOutputExactPointsIsNotTruncatedAndZeroMeansDefault(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{series: []telemetryhistory.SeriesSnapshot{histSeries("m-a", attrSoC, nSamples(3)...)}})
	got, _ := getOutput(t, s, "?points=3")
	if sr := got.Series[0]; sr.Truncated || len(sr.Points) != 3 || sr.Total != 3 {
		t.Errorf("series = %+v, want 3 of 3 untruncated", sr)
	}
	got, _ = getOutput(t, s, "?series=0&points=0")
	if len(got.Series) != 1 || len(got.Series[0].Points) != 3 {
		t.Errorf("series=0 points=0 = %+v, want the default window", got.Series)
	}
}

func TestOutputDropsNonFiniteValuesAndKeepsReportedPrefixDot(t *testing.T) {
	t.Parallel()
	h := &outputHistory{series: []telemetryhistory.SeriesSnapshot{
		histSeries("m-a", attrSoC,
			telemetryhistory.Sample{At: 1_700_000_001, Value: math.NaN()},
			telemetryhistory.Sample{At: 1_700_000_002, Value: 7},
			telemetryhistory.Sample{At: 1_700_000_003, Value: math.Inf(1)},
			telemetryhistory.Sample{At: 1_700_000_004, Value: math.Inf(-1)}),
		histSeries("m-a", "DERStatusX.stateOfChargeStatus", nSamples(1)...),
	}}
	s := pageServer(t, h)
	got, _ := getOutput(t, s, "")
	if len(got.Series) != 1 || got.Series[0].Attribute != attrSoC || got.Series[0].Total != 1 || len(got.Series[0].Points) != 1 || got.Series[0].Points[0] != (outputPointResponse{T: 1_700_000_002, V: 7}) {
		t.Errorf("series = %+v, want only the finite point of the DERStatus. series", got.Series)
	}
}

func TestOutputEmptySeriesIsAnEmptyArray(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	_, rec := getOutput(t, s, "")
	if !strings.Contains(rec.Body.String(), `"series":[]`) {
		t.Errorf("body = %s, want series as []", rec.Body.String())
	}
}

func TestPageServedWithSoCOnly(t *testing.T) {
	t.Parallel()
	src := testSources()
	src.SoC = newFakeSoC()
	s := newServer(t, Config{Key: testKey}, src)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/", "", "localhost")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "loading") {
		t.Errorf("/apps/soc/ with SoC only = %d", rec.Code)
	}
	assertSocHeaders(t, rec.Header())
}

func TestRefusedRepliesCarryTheSecurityHeaders(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	forbidden := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/output", "", "evil.example")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("foreign Host = %d, want 403", forbidden.Code)
	}
	assertSocHeaders(t, forbidden.Header())
	notAllowed := doRequest(t, s.Handler(), http.MethodPost, "/apps/soc/api/output", "", "localhost")
	if notAllowed.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", notAllowed.Code)
	}
	assertSocHeaders(t, notAllowed.Header())
}
