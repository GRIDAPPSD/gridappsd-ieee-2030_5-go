package adminui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const healthPagePath = "/apps/" + panelHealth + "/"

func getHealthPage(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, s.Handler(), method, healthPagePath+path, "Bearer "+testKey, "localhost")
}

// getHealthData fetches the page's data route through the full listener
// handler, as the page does, and decodes it.
func getHealthData(t *testing.T, s *Server) healthData {
	t.Helper()
	rec := getHealthPage(t, s, http.MethodGet, "data")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET health data: status %d, body %s", rec.Code, rec.Body.String())
	}
	var d healthData
	decodeJSON(t, rec.Body.Bytes(), &d)
	return d
}

func TestHealthPanelIsAPagePanel(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	p := s.panels()[0]
	if p.ID != panelHealth || p.Label != "Bridge health" {
		t.Errorf("first panel = %q %q, want %q %q", p.ID, p.Label, panelHealth, "Bridge health")
	}
	if p.Page == nil || p.View != nil {
		t.Errorf("health panel Page set = %t, View set = %t, want a page panel with no View", p.Page != nil, p.View != nil)
	}

	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	var manifest []struct {
		ID   string `json:"id"`
		Page string `json:"page"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	if len(manifest) == 0 || manifest[0].ID != panelHealth || manifest[0].Page != healthPagePath {
		t.Errorf("manifest[0] = %+v, want %s with page %s", manifest, panelHealth, healthPagePath)
	}
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelHealth, "Bearer "+testKey, "localhost"); rec.Code == http.StatusOK {
		t.Errorf("descriptor route for the page panel = %d, want it not served", rec.Code)
	}
}

func TestHealthPageServesItsFiles(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, f := range []struct{ path, ctype, marker string }{
		{"", "text/html", `src="app.js"`},
		{"app.js", "text/javascript", "fetch("},
		{"style.css", "text/css", "body"},
	} {
		rec := getHealthPage(t, s, http.MethodGet, f.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %q: status %d, want 200", f.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, f.ctype) {
			t.Errorf("GET %q: Content-Type %q, want prefix %q", f.path, ct, f.ctype)
		}
		if !strings.Contains(rec.Body.String(), f.marker) {
			t.Errorf("GET %q: body lacks %q", f.path, f.marker)
		}
		if csp := strings.Join(rec.Header().Values("Content-Security-Policy"), "; "); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Errorf("GET %q: CSP %q, want the page policy plus the plane's frame-ancestors", f.path, csp)
		}
	}
}

func TestHealthPageNeedsTheCredentialAndRefusesWrites(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, path := range []string{"", "data", "app.js"} {
		if rec := doRequest(t, s.Handler(), http.MethodGet, healthPagePath+path, "", "localhost"); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden && rec.Code != http.StatusSeeOther {
			t.Errorf("GET %q with no credential = %d, want a refusal", path, rec.Code)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := getHealthPage(t, s, m, "data")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s data = %d, want 405: the page is read only", m, rec.Code)
		}
	}
	if rec := getHealthPage(t, s, http.MethodGet, "nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown file = %d, want 404", rec.Code)
	}
}

func TestHealthDataCarriesTheSenderSwitch(t *testing.T) {
	t.Parallel()

	snd := newTestSender(t, &recordingBus{}, true)
	s := senderServer(t, snd)
	pub := getHealthData(t, s).Publishing
	st := snd.Publishing()
	if pub == nil || pub.On != st.On || pub.ChangedBy != st.ChangedBy || pub.ChangedAt != st.ChangedAt.Format(timeFormat) {
		t.Errorf("publishing = %+v, want sender state %+v", pub, st)
	}

	raw := getHealthPage(t, newServer(t, Config{Key: testKey}, testSources()), http.MethodGet, "data")
	var m map[string]json.RawMessage
	decodeJSON(t, raw.Body.Bytes(), &m)
	if _, ok := m["publishing"]; ok {
		t.Errorf("data has a publishing key with no sender: %s", raw.Body.String())
	}
	for _, k := range []string{"status", "stompConnected", "sorLink", "uptimeSeconds"} {
		if _, ok := m[k]; !ok {
			t.Errorf("data lacks %q: %s", k, raw.Body.String())
		}
	}
}

type healthRender struct {
	Rows    map[string]string `json:"rows"`
	Link    string            `json:"link"`
	LinkSet bool              `json:"linkSet"`
	Status  string            `json:"status"`
	Polls   int               `json:"polls"`
}

type healthRun struct {
	Full   healthRender `json:"full"`
	Unsafe healthRender `json:"unsafe"`
	Bare   healthRender `json:"bare"`
	Failed healthRender `json:"failed"`
}

// runHealthPage runs the shipped app.js against a fake DOM and fake fetch
// (testdata/health_page.js). A missing node skips the test locally and fails
// it under CI, as the cards page test does.
func runHealthPage(t *testing.T) healthRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node not installed, and CI must run the page test: %v", err)
		}
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "testdata/health_page.js", "healthpage/app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var run healthRun
	if err := json.Unmarshal(out, &run); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return run
}

func TestHealthPageRendersEveryField(t *testing.T) {
	t.Parallel()
	full := runHealthPage(t).Full
	want := map[string]string{
		"Status":                         "ok",
		"STOMP connection":               "connected",
		"mTLS listener":                  "127.0.0.1:8443",
		"Server SFDI":                    "999888777",
		"Server LFDI":                    "FEDCBA9876543210",
		"Feeder mRID":                    "feeder-1",
		"Simulation ID":                  "sim-1",
		"Registry entries":               "3",
		"Placeholder identities":         "1",
		"Certificate derived identities": "2",
		"Uptime (seconds)":               "42",
		"Server of record":               "Server of record dashboard",
		"Publishing":                     "ON",
		"Last changed":                   "2026-07-18T12:30:00.000Z",
		"Changed from":                   "127.0.0.1:40000",
	}
	for k, v := range want {
		if full.Rows[k] != v {
			t.Errorf("%s = %q, want %q", k, full.Rows[k], v)
		}
	}
	if len(full.Rows) != len(want) {
		t.Errorf("rows = %v, want exactly the %d fields", full.Rows, len(want))
	}
	if !full.LinkSet || full.Link != "https://sor.example/dash" {
		t.Errorf("server of record link = %q (set %t), want the configured https URL", full.Link, full.LinkSet)
	}
	if full.Polls < 1 {
		t.Errorf("polls = %d, want the page to poll the data route", full.Polls)
	}
}

func TestHealthPageRefusesUnsafeLinks(t *testing.T) {
	t.Parallel()
	unsafe := runHealthPage(t).Unsafe
	if unsafe.LinkSet {
		t.Errorf("javascript: value became a link to %q, want plain text", unsafe.Link)
	}
	if unsafe.Rows["Server of record"] != "javascript:alert(1)" {
		t.Errorf("Server of record = %q, want the raw value as text", unsafe.Rows["Server of record"])
	}
}

func TestHealthPageShowsAbsentAndFailedStates(t *testing.T) {
	t.Parallel()
	run := runHealthPage(t)
	if got := run.Bare.Rows["Server of record"]; got != "not set" {
		t.Errorf("unset link row = %q, want %q", got, "not set")
	}
	if got := run.Bare.Rows["STOMP connection"]; got != "disconnected" {
		t.Errorf("STOMP connection = %q, want disconnected", got)
	}
	for _, k := range []string{"Publishing", "Last changed", "Changed from"} {
		if _, ok := run.Bare.Rows[k]; ok {
			t.Errorf("%s shown with no sender", k)
		}
	}
	if !strings.Contains(run.Failed.Status, "unavailable") || len(run.Failed.Rows) != 0 {
		t.Errorf("failed poll shows status %q and %d rows, want an unavailable notice and no stale rows", run.Failed.Status, len(run.Failed.Rows))
	}
}
