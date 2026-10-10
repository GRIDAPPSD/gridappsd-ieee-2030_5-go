package adminui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

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

func TestDevicesListsNameMridLfdiSortedAndNothingElse(t *testing.T) {
	t.Parallel()
	s := pageServer(t, &outputHistory{})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/devices", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `[{"mrid":"m-a","name":"bat-a","lfdi":"AAAA"},{"mrid":"m-b","name":"pv-b","lfdi":"BBBB"}]`+"\n" {
		t.Errorf("body = %s", got)
	}
	for _, banned := range []string{"sfdi", "111111111", "222222222", "placeholder"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("body carries %q: %s", banned, rec.Body.String())
		}
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	assertSocHeaders(t, rec.Header())
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
	for _, p := range []string{"/apps/soc", "/apps/soc/", "/apps/soc/app.js", "/apps/soc/style.css", "/apps/soc/api/devices", "/apps/soc/api/output", "/apps/soc/api/mirror"} {
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
