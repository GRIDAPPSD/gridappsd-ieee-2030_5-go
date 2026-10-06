package adminui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// The wire shape of a served Descriptor, decoded the way the shell reads
// it, so the tests assert what reaches the browser rather than the Go
// values a View built.
type wireCell struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Badge    string `json:"badge"`
	DateTime string `json:"datetime"`
	Href     string `json:"href"`
}

type wireEntry struct {
	Key   string   `json:"key"`
	Value wireCell `json:"value"`
}

type wireGroup struct {
	Heading string      `json:"heading"`
	Entries []wireEntry `json:"entries"`
}

type wireBody struct {
	Columns []string     `json:"columns"`
	Rows    [][]wireCell `json:"rows"`
	Groups  []wireGroup  `json:"groups"`
}

type wireSection struct {
	Kind    string   `json:"kind"`
	Heading string   `json:"heading"`
	Prose   []string `json:"prose"`
	Empty   string   `json:"empty"`
	Body    wireBody `json:"body"`
}

type wireDescriptor struct {
	Version  int           `json:"version"`
	Sections []wireSection `json:"sections"`
}

// getPanel fetches a panel the way the shell does, through the full
// listener handler, and fails unless it answers 200.
func getPanel(t *testing.T, s *Server, id string) wireDescriptor {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+id, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET panel %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	var d wireDescriptor
	decodeJSON(t, rec.Body.Bytes(), &d)
	if d.Version != 2 {
		t.Fatalf("panel %s: version %d, want 2", id, d.Version)
	}
	return d
}

// section returns the section with heading, failing when it is absent.
func section(t *testing.T, d wireDescriptor, heading string) wireSection {
	t.Helper()
	for _, s := range d.Sections {
		if s.Heading == heading {
			return s
		}
	}
	t.Fatalf("no section %q in %+v", heading, d)
	return wireSection{}
}

// entries flattens a definition-list section into key -> cell.
func entries(t *testing.T, s wireSection) map[string]wireCell {
	t.Helper()
	if s.Kind != "definitionList" {
		t.Fatalf("section %q: kind %q, want definitionList", s.Heading, s.Kind)
	}
	out := map[string]wireCell{}
	for _, g := range s.Body.Groups {
		for _, e := range g.Entries {
			out[e.Key] = e.Value
		}
	}
	return out
}

// texts is a table row's cell texts.
func texts(row []wireCell) []string {
	out := make([]string, 0, len(row))
	for _, c := range row {
		out = append(out, c.Text)
	}
	return out
}

func assertColumns(t *testing.T, s wireSection, want ...string) {
	t.Helper()
	if s.Kind != "table" {
		t.Fatalf("section %q: kind %q, want table", s.Heading, s.Kind)
	}
	if !slices.Equal(s.Body.Columns, want) {
		t.Fatalf("section %q columns = %q, want %q", s.Heading, s.Body.Columns, want)
	}
}

func assertBadge(t *testing.T, where string, c wireCell, variant, text string) {
	t.Helper()
	if c.Kind != "badge" || c.Badge != variant || c.Text != text {
		t.Errorf("%s = %+v, want badge %q %q", where, c, variant, text)
	}
}

// TestHealthPanelCarriesEveryHealthField: each /api/health field appears
// in the panel with the same value the JSON route serves.
func TestHealthPanelCarriesEveryHealthField(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Registry = &fakeRegistry{entries: []registry.Entry{{MRID: "a"}, {MRID: "b", Placeholder: true}, {MRID: "c"}}}
	src.Identity = &fakeIdentity{addr: "127.0.0.1:8443", identity: sep2srv.Identity{SFDI: "999888777", LFDI: "FEDCBA9876543210"}}
	src.Stomp = &fakeStomp{connected: true}
	s := newServer(t, Config{Key: testKey, FeederMRID: "feeder-1", SimulationID: "sim-1", SORLink: "https://sor.example/dash"}, src)

	got := entries(t, section(t, getPanel(t, s, panelHealth), "Bridge"))
	want := map[string]string{
		"mTLS listener":                  "127.0.0.1:8443",
		"Server SFDI":                    "999888777",
		"Server LFDI":                    "FEDCBA9876543210",
		"Feeder mRID":                    "feeder-1",
		"Simulation ID":                  "sim-1",
		"Registry entries":               "3",
		"Placeholder identities":         "1",
		"Certificate derived identities": "2",
	}
	for k, v := range want {
		if got[k].Text != v {
			t.Errorf("%s = %+v, want %q", k, got[k], v)
		}
	}
	assertBadge(t, "Status", got["Status"], "ok", "ok")
	assertBadge(t, "STOMP connection", got["STOMP connection"], "ok", "connected")
	if _, err := time.ParseDuration(got["Uptime (seconds)"].Text + "s"); err != nil {
		t.Errorf("Uptime (seconds) = %+v, want a whole number", got["Uptime (seconds)"])
	}
	if sor := got["Server of record"]; sor.Kind != "link" || sor.Href != "https://sor.example/dash" {
		t.Errorf("Server of record = %+v, want a link to the configured URL", sor)
	}
}

// TestHealthPanelShowsAnUnsafeSORLinkAsText: a link the descriptor would
// refuse must not fail the whole panel, and must not become a link.
func TestHealthPanelShowsAnUnsafeSORLinkAsText(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey, SORLink: "javascript:alert(1)"}, testSources())
	sor := entries(t, section(t, getPanel(t, s, panelHealth), "Bridge"))["Server of record"]
	if sor.Kind != "text" || sor.Text != "javascript:alert(1)" || sor.Href != "" {
		t.Errorf("Server of record = %+v, want the raw value as plain text", sor)
	}
}

// TestControlFlowPanelCarriesEverySnapshotField covers the topics, both
// counters and every field of the last delta.
func TestControlFlowPanelCarriesEverySnapshotField(t *testing.T) {
	t.Parallel()

	appliedAt := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	src := testSources()
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{
		Applied:     4,
		Restated:    3,
		Skipped:     1,
		EmptyFrames: 2,
		Last: &controlobs.LastDelta{
			Object:    "mrid-inv-1",
			Attribute: "DERControl.DERControlBase.opModTargetW",
			Value:     map[string]any{"multiplier": float64(0), "value": float64(4200)},
			AppliedAt: appliedAt,
		},
		OutputTopic: "/topic/goss.gridappsd.simulation.output.12345",
		InputTopic:  "/topic/goss.gridappsd.simulation.input.12345",
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelControlFlow)

	state := entries(t, section(t, d, "Control flow"))
	for k, v := range map[string]string{
		"Simulation output topic":   "/topic/goss.gridappsd.simulation.output.12345",
		"Control delta input topic": "/topic/goss.gridappsd.simulation.input.12345",
		"Applied":                   "4",
		"Restated":                  "3",
		"Skipped":                   "1",
		"Empty frames":              "2",
	} {
		if state[k].Text != v {
			t.Errorf("%s = %+v, want %q", k, state[k], v)
		}
	}
	last := entries(t, section(t, d, "Last applied delta"))
	for k, v := range map[string]string{
		"Object":     "mrid-inv-1",
		"Attribute":  "DERControl.DERControlBase.opModTargetW",
		"Value":      `{"multiplier":0,"value":4200}`,
		"Applied at": "2026-07-18T12:30:00.000Z",
	} {
		if last[k].Text != v {
			t.Errorf("%s = %+v, want %q", k, last[k], v)
		}
	}
	if at := last["Applied at"]; at.Kind != "time" || at.DateTime != "2026-07-18T12:30:00Z" {
		t.Errorf("Applied at = %+v, want a time cell at 2026-07-18T12:30:00Z", at)
	}
}

// TestControlFlowPanelShowsNoDeltaAsItsOwnState: with no delta applied,
// the section is empty with its explanation, not a delta of blanks.
func TestControlFlowPanelShowsNoDeltaAsItsOwnState(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	last := section(t, getPanel(t, s, panelControlFlow), "Last applied delta")
	if len(last.Body.Groups) != 0 || last.Empty != "No control delta has been applied yet." {
		t.Errorf("last delta section = %+v, want no groups and the idle text", last)
	}
}

// TestEveryRouteAnswers401WithoutACredential is the issue's last
// done-when line, over every route the listener mounts, from loopback:
// the bypass is off, so loopback is no exception. The two login routes
// are the only routes a caller with no credential can reach, by design,
// and neither of them hands out a session.
func TestEveryRouteAnswers401WithoutACredential(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	routes := append(slices.Clone(s.planePatterns), bridgeJSONRoutes...)
	if len(routes) < 10 {
		t.Fatalf("only %d routes: %q", len(routes), routes)
	}
	login := map[string]bool{"GET /login": true, "POST /auth/login": true}
	fill := strings.NewReplacer("{id}", "x", "{action}", "x", "{lfdi}", "x", "{edevId}", "x", "{frqId}", "x", "{$}", "")
	checked := 0
	for _, p := range routes {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			method, path = http.MethodGet, p
		}
		path = fill.Replace(path)
		if strings.Contains(path, "{") {
			t.Errorf("route %q has a wildcard this test does not fill", p)
			continue
		}
		rec := boundedRequest(t, s.Handler(), method, path)
		if login[p] {
			if c := rec.Header().Values("Set-Cookie"); len(c) != 0 {
				t.Errorf("%s with no credential set a cookie: %q", p, c)
			}
			continue
		}
		checked++
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: %d, want 401", method, path, rec.Code)
		}
	}
	t.Logf("%d routes answered 401, %d login routes checked for no session", checked, len(login))
}

// TestPlaneMountsNoBridgeJSONRoute: the bridge's exact JSON patterns sit
// in front of the plane, so a plane route on the same path would be
// silently shadowed.
func TestPlaneMountsNoBridgeJSONRoute(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, p := range s.planePatterns {
		_, path, _ := strings.Cut(p, " ")
		if slices.Contains(bridgeJSONRoutes, path) {
			t.Errorf("the plane mounts %q, which the bridge route shadows", p)
		}
	}
}

// TestNoResponseEverContainsTheAdminKey drives the bridge JSON routes and
// every bridge panel with populated sources and asserts the key never
// reaches a body.
func TestNoResponseEverContainsTheAdminKey(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Registry = &fakeRegistry{entries: []registry.Entry{{MRID: "mrid-1", Name: "inverter-1", LFDI: "LFDI1", SFDI: "SFDI1"}}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1", LFDI: "LFDI1", DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/h1"}}}}}
	src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{"edev-1": {{ID: "1", Href: "/h1", MRID: "derp-1"}}}}
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{Applied: 1, Last: &controlobs.LastDelta{Object: "mrid-1", Value: 1.0, AppliedAt: time.Now()}}}
	s := newServer(t, Config{Key: testKey}, src)

	paths := slices.Clone(bridgeJSONRoutes)
	for _, p := range s.panels() {
		paths = append(paths, "/api/ui/panels/"+p.ID)
	}
	for _, path := range paths {
		rec := doRequest(t, s.Handler(), http.MethodGet, path, "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), testKey) {
			t.Errorf("GET %s body contains the admin key", path)
		}
	}
}

// boundedRequest is a loopback request with no credential that gives up
// after a second, so a stream (the dashboard's SSE) that wrongly admits
// it answers 200 instead of hanging the test.
func boundedRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, method, path, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNoControlWriteRouteIsMounted: the bridge issues DER controls itself,
// so the plane's DER control and flow reservation writes stay unmounted.
// The store set here mounts the flow reservation reads, so their writes
// would appear if ControlWrites were on.
func TestNoControlWriteRouteIsMounted(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	if !slices.Contains(s.planePatterns, "GET /api/derms/flow-reservations") {
		t.Fatalf("flow reservation reads are not mounted, so this test cannot see their writes: %q", s.planePatterns)
	}
	for _, p := range s.planePatterns {
		method, path, _ := strings.Cut(p, " ")
		if method == http.MethodGet {
			continue
		}
		if strings.HasPrefix(path, "/api/der/controls") || strings.HasPrefix(path, "/api/derms/flow-reservations") {
			t.Errorf("control write route %q is mounted", p)
		}
	}
}

// TestListenerMountsNoWriteRoute: the bridge seeds and writes the stores
// itself, so every route on the listener reads, except the two auth POSTs
// that login and the SSE ticket need and the panel action route, whose
// actions publish to the bus and write no store. A plane pattern with no
// method would accept every method, so it fails too; the bridge JSON
// routes are method-less but sit behind requireGET.
func TestListenerMountsNoWriteRoute(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	allowed := map[string]bool{"POST /auth/login": true, "POST /auth/ticket": true, "POST /api/ui/panels/{id}/actions/{action}": true}
	if len(s.planePatterns) == 0 {
		t.Fatal("the plane reports no patterns, so this test checks nothing")
	}
	for _, p := range s.planePatterns {
		method, _, ok := strings.Cut(p, " ")
		switch {
		case allowed[p]:
		case !ok || strings.HasPrefix(method, "/"):
			t.Errorf("plane pattern %q has no method, so it admits writes", p)
		case method != http.MethodGet && method != http.MethodHead:
			t.Errorf("write route %q is mounted", p)
		}
	}
	for _, route := range bridgeJSONRoutes {
		if rec := doRequest(t, s.Handler(), http.MethodPost, route, "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s with the key: %d, want 405", route, rec.Code)
		}
	}
}

// manyClients returns n connected clients.
func manyClients(n int) *fakeClientObserver {
	obs := &fakeClientObserver{}
	for i := 0; i < n; i++ {
		obs.snap.Clients = append(obs.snap.Clients, connobs.ClientSnapshot{LFDI: fmt.Sprintf("LFDI%04d", i), LastSeen: time.Unix(1700000000, 0), RequestCount: 1})
	}
	return obs
}

// notice reports whether a section says it shows only part of its rows.
func notice(s wireSection) string {
	for _, p := range s.Prose {
		if strings.HasPrefix(p, "Showing ") {
			return p
		}
	}
	return ""
}

func TestControlFlowPanelSaysNoSimulationIDInsteadOfBlank(t *testing.T) {
	src := testSources()
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{
		InputTopic: "/topic/goss.gridappsd.IEEE_2030_5.input",
	}}
	s := newServer(t, Config{Key: testKey}, src)
	d := getPanel(t, s, panelControlFlow)

	state := entries(t, section(t, d, "Control flow"))
	const want = "not subscribed (no simulation id configured)"
	if got := state["Simulation output topic"].Text; got != want {
		t.Errorf("Simulation output topic = %q, want %q", got, want)
	}
	if got := state["Control delta input topic"].Text; got != "/topic/goss.gridappsd.IEEE_2030_5.input" {
		t.Errorf("Control delta input topic = %q", got)
	}
}
