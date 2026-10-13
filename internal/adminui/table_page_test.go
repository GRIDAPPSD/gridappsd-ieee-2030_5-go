package adminui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/tablepage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// tablePanels are the tabs served as table pages, in tab order.
var tablePanels = []struct{ id, label string }{
	{panelConnections, "Connections"},
	{panelDERPrograms, "DER programs"},
	{panelControlFlow, "Control flow"},
}

func tablePath(id, file string) string { return "/apps/" + id + "/" + file }

func TestTablePanelsArePagePanels(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	byID := map[string]sep2admin.Panel{}
	for _, p := range s.panels() {
		byID[p.ID] = p
	}
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	var manifest []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Page  string `json:"page"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	for i, want := range tablePanels {
		p := byID[want.id]
		if p.Page == nil || p.View != nil {
			t.Errorf("%s: Page set = %t, View set = %t, want a page panel with no View", want.id, p.Page != nil, p.View != nil)
		}
		// Health is first, so the table tabs follow it in this order.
		got := manifest[i+1]
		if got.ID != want.id || got.Label != want.label || got.Page != tablePath(want.id, "") {
			t.Errorf("manifest[%d] = %+v, want %s %q with page %s", i+1, got, want.id, want.label, tablePath(want.id, ""))
		}
		if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+want.id, "Bearer "+testKey, "localhost"); rec.Code == http.StatusOK {
			t.Errorf("descriptor route for %s = %d, want it not served", want.id, rec.Code)
		}
	}
}

func TestTablePagesServeTheSharedFilesUnderTheirOwnPolicy(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, tp := range tablePanels {
		for _, f := range []struct{ path, ctype, marker string }{
			{"", "text/html", `src="table.js"`},
			{"table.js", "text/javascript", "fetch("},
			{"table.css", "text/css", "table"},
		} {
			rec := doRequest(t, s.Handler(), http.MethodGet, tablePath(tp.id, f.path), "Bearer "+testKey, "localhost")
			if rec.Code != http.StatusOK {
				t.Errorf("%s GET %q: status %d, want 200", tp.id, f.path, rec.Code)
				continue
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, f.ctype) {
				t.Errorf("%s GET %q: Content-Type %q, want prefix %q", tp.id, f.path, ct, f.ctype)
			}
			if !strings.Contains(rec.Body.String(), f.marker) {
				t.Errorf("%s GET %q: body lacks %q", tp.id, f.path, f.marker)
			}
			csp := strings.Join(rec.Header().Values("Content-Security-Policy"), "; ")
			for _, want := range []string{"script-src 'self'", "style-src 'self'", "frame-ancestors 'self'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("%s GET %q: CSP %q lacks %q", tp.id, f.path, csp, want)
				}
			}
			if strings.Contains(csp, "unsafe-inline") {
				t.Errorf("%s GET %q: CSP %q allows inline", tp.id, f.path, csp)
			}
		}
	}
}

func TestTablePagesNeedTheCredentialAndRefuseWrites(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, tp := range tablePanels {
		for _, f := range []string{"", "data", "table.js"} {
			rec := doRequest(t, s.Handler(), http.MethodGet, tablePath(tp.id, f), "", "localhost")
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden && rec.Code != http.StatusSeeOther {
				t.Errorf("%s GET %q with no credential = %d, want a refusal", tp.id, f, rec.Code)
			}
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if rec := doRequest(t, s.Handler(), m, tablePath(tp.id, "data"), "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s data = %d, want 405: the page is read only", tp.id, m, rec.Code)
			}
		}
		if rec := doRequest(t, s.Handler(), http.MethodGet, tablePath(tp.id, "nope"), "Bearer "+testKey, "localhost"); rec.Code != http.StatusNotFound {
			t.Errorf("%s GET unknown file = %d, want 404", tp.id, rec.Code)
		}
	}
}

// TestTableDataSendsEmptyAsEmpty reads the serialized bytes: an empty table
// and an empty definition list are empty arrays, never null or absent, and
// the page title travels with the data.
func TestTableDataSendsEmptyAsEmpty(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, tc := range []struct {
		id    string
		title string
		want  []string
	}{
		{panelConnections, "Connections", []string{`"rows":[]`}},
		{panelDERPrograms, "DER programs", []string{`"rows":[]`}},
		{panelControlFlow, "Control flow", []string{`"groups":[]`}},
	} {
		body := getPanelBody(t, s, tc.id)
		if !strings.Contains(string(body), `"title":"`+tc.title+`"`) {
			t.Errorf("%s data %s lacks title %q", tc.id, body, tc.title)
		}
		for _, w := range tc.want {
			if !strings.Contains(string(body), w) {
				t.Errorf("%s data %s lacks %s", tc.id, body, w)
			}
		}
		if strings.Contains(string(body), "null") {
			t.Errorf("%s data %s carries a null", tc.id, body)
		}
	}
}

// getPanelBody fetches a table page's data route through the full listener
// handler, as the page does.
func getPanelBody(t *testing.T, s *Server, id string) json.RawMessage {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, tablePath(id, "data"), "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s data: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s data Content-Type %q, want JSON", id, ct)
	}
	return slices.Clone(rec.Body.Bytes())
}

type nodeScenario struct {
	Status int             `json:"status,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
	Hidden bool            `json:"hidden,omitempty"`
	Steps  []string        `json:"steps,omitempty"`
	// NoTimeout runs the page in a browser whose AbortSignal has no timeout().
	NoTimeout bool `json:"noTimeout,omitempty"`
}

type nodeCell struct {
	Text     string `json:"text"`
	Class    string `json:"cls"`
	DateTime string `json:"dateTime"`
}

type nodeEntry struct {
	Key   string   `json:"key"`
	Value nodeCell `json:"value"`
}

type nodeGroup struct {
	Heading string      `json:"heading"`
	Entries []nodeEntry `json:"entries"`
}

type nodeSection struct {
	Heading string       `json:"heading"`
	Prose   []string     `json:"prose"`
	Empty   string       `json:"empty"`
	Columns []string     `json:"columns"`
	Rows    [][]nodeCell `json:"rows"`
	Groups  []nodeGroup  `json:"groups"`
}

type nodeStep struct {
	Step    string `json:"step"`
	Fetches int    `json:"fetches"`
	Timers  int    `json:"timers"`
}

type nodeRun struct {
	Title        string        `json:"title"`
	DocTitle     string        `json:"docTitle"`
	Status       string        `json:"status"`
	StatusClass  string        `json:"statusClass"`
	Sections     []nodeSection `json:"sections"`
	Fetches      int           `json:"fetches"`
	Timers       int           `json:"timers"`
	Steps        []nodeStep    `json:"steps"`
	InnerHTML    int           `json:"innerHTML"`
	Style        int           `json:"style"`
	TimeoutCalls int           `json:"timeoutCalls"`
	Controllers  int           `json:"controllers"`
	Cleared      int           `json:"cleared"`
}

func tablePageSource(t *testing.T) string {
	t.Helper()
	b, err := tablepage.Files.ReadFile("table.js")
	if err != nil {
		t.Fatalf("read table.js: %v", err)
	}
	return string(b)
}

// runTablePage runs source against the scenarios on a fake DOM
// (testdata/table_page.js). A missing node skips the test locally and fails
// it under CI, as the other page tests do.
func runTablePage(t *testing.T, source string, scenarios map[string]nodeScenario) map[string]nodeRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node not installed, and CI must run the page test: %v", err)
		}
		t.Skip("node not installed")
	}
	in, err := json.Marshal(map[string]any{"source": source, "scenarios": scenarios})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "testdata/table_page.js")
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var runs map[string]nodeRun
	if err := json.Unmarshal(out, &runs); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return runs
}

// tablePageSources fills every table with distinct values.
func tablePageSources() Sources {
	lastSeen := time.Date(2026, 7, 27, 9, 15, 30, 0, time.UTC)
	src := testSources()
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{
		Clients: []connobs.ClientSnapshot{{LFDI: "LFDIX", LastSeen: lastSeen, Age: time.Minute, RequestCount: 7, Paths: []string{"/dcap", "/edev"}}},
		Handshakes: []connobs.HandshakeAttempt{
			{LFDI: "LFDIX", RemoteAddr: "10.0.0.5:54321", Accepted: false, Reason: "x509: unknown authority", At: lastSeen},
		},
	}}
	src.Devices = &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{{ID: "edev-1"}}}
	src.Programs = &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{
		"edev-1": {{ID: "1", Href: "/edev/1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 5}},
	}}
	src.Flow = &fakeFlow{snap: controlobs.Snapshot{
		Applied: 4, Restated: 3, Skipped: 1, EmptyFrames: 2,
		Last:        &controlobs.LastDelta{Object: "mrid-inv-1", Attribute: "opModTargetW", Value: 4200.0, AppliedAt: lastSeen},
		OutputTopic: "/topic/out", InputTopic: "/topic/in",
	}}
	return src
}

func pageRows(sec nodeSection) [][]string {
	out := make([][]string, 0, len(sec.Rows))
	for _, r := range sec.Rows {
		row := make([]string, 0, len(r))
		for _, c := range r {
			row = append(row, c.Text)
		}
		out = append(out, row)
	}
	return out
}

func TestTablePageRendersEachPagesData(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, tablePageSources())
	runs := runTablePage(t, tablePageSource(t), map[string]nodeScenario{
		panelConnections: {Body: getPanelBody(t, s, panelConnections)},
		panelDERPrograms: {Body: getPanelBody(t, s, panelDERPrograms)},
		panelControlFlow: {Body: getPanelBody(t, s, panelControlFlow)},
	})

	conn := runs[panelConnections]
	if conn.Title != "Connections" || conn.DocTitle != "Connections" || len(conn.Sections) != 2 {
		t.Fatalf("connections page = %+v, want its title and 2 sections", conn)
	}
	seen := conn.Sections[0]
	if seen.Heading != "LFDIs seen with no EndDevice" || !slices.Equal(seen.Columns, []string{"LFDI", "Status", "Last seen", "Age", "Requests", "Paths touched"}) {
		t.Errorf("strangers section = %+v", seen)
	}
	if got := pageRows(seen); !slices.EqualFunc(got, [][]string{{"LFDIX", "connected", "2026-07-27T09:15:30.000Z", "1m0s", "7", "/dcap, /edev"}}, slices.Equal) {
		t.Errorf("strangers rows = %q", got)
	}
	if c := seen.Rows[0][1]; c.Class != "badge badge-ok" {
		t.Errorf("status cell class = %q, want the ok badge", c.Class)
	}
	if c := seen.Rows[0][2]; c.DateTime != "2026-07-27T09:15:30Z" {
		t.Errorf("last seen datetime = %q, want RFC3339", c.DateTime)
	}
	hs := conn.Sections[1]
	if hs.Heading != "Handshake attempts (cert validity)" || hs.Rows[0][2].Text != "rejected" || hs.Rows[0][2].Class != "badge badge-error" {
		t.Errorf("handshake section = %+v", hs)
	}

	der := runs[panelDERPrograms].Sections[0]
	if got := pageRows(der); !slices.EqualFunc(got, [][]string{{"edev-1", "1", "derp-mrid-1", "default", "5", "/edev/1/fsa/1/derp/1", "absent"}}, slices.Equal) {
		t.Errorf("DER program rows = %q", got)
	}

	flow := runs[panelControlFlow].Sections
	if len(flow) != 2 || flow[0].Heading != "Control flow" || len(flow[0].Groups) != 2 {
		t.Fatalf("control flow sections = %+v, want the state section with its two groups", flow)
	}
	if g := flow[0].Groups[0]; g.Heading != "Topics" || g.Entries[0].Key != "Simulation output topic" || g.Entries[0].Value.Text != "/topic/out" {
		t.Errorf("topics group = %+v", g)
	}
	if g := flow[0].Groups[1]; g.Heading != "Counters" || len(g.Entries) != 4 || g.Entries[0].Value.Text != "4" {
		t.Errorf("counters group = %+v", g)
	}
	last := flow[1]
	if last.Heading != "Last applied delta" || len(last.Groups) != 1 || last.Groups[0].Entries[2].Value.Text != "4200" || last.Groups[0].Entries[3].Value.DateTime != "2026-07-27T09:15:30Z" {
		t.Errorf("last delta section = %+v", last)
	}
}

func TestTablePageShowsEmptyAndFailedStates(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	runs := runTablePage(t, tablePageSource(t), map[string]nodeScenario{
		"idle":   {Body: getPanelBody(t, s, panelControlFlow)},
		"derp":   {Body: getPanelBody(t, s, panelDERPrograms)},
		"failed": {Status: 500, Body: json.RawMessage(`{"error":"boom"}`)},
	})
	if last := runs["idle"].Sections[1]; last.Empty != "No control delta has been applied yet." || len(last.Groups) != 0 {
		t.Errorf("idle last delta = %+v, want only the empty text", last)
	}
	if der := runs["derp"].Sections[0]; der.Empty != "No DERPrograms served." || der.Rows != nil {
		t.Errorf("empty programs = %+v, want only the empty text and no table", der)
	}
	f := runs["failed"]
	if !strings.Contains(f.Status, "unavailable") || f.StatusClass != "error" || len(f.Sections) != 0 {
		t.Errorf("failed poll shows status %q (%q) and %d sections, want an unavailable notice and no stale content", f.Status, f.StatusClass, len(f.Sections))
	}
}

// hostileBody is data whose strings are markup, to prove the page writes
// them as text.
const hostileBody = `{"title":"<b>t</b>","sections":[{"kind":"table","heading":"<img src=x onerror=alert(1)>",
"prose":["<script>alert(1)</script>"],"empty":"","body":{"columns":["<i>c</i>"],"rows":[[
{"kind":"badge","text":"<u>x</u>","badge":"\" onmouseover=\"alert(1)"},{"kind":"text","text":"<svg onload=alert(1)>"}]]}}]}`

func TestTablePageWritesServerStringsAsText(t *testing.T) {
	t.Parallel()

	runs := runTablePage(t, tablePageSource(t), map[string]nodeScenario{"hostile": {Body: json.RawMessage(hostileBody)}})
	h := runs["hostile"]
	if h.InnerHTML != 0 || h.Style != 0 {
		t.Errorf("innerHTML writes = %d, style writes = %d, want none", h.InnerHTML, h.Style)
	}
	if h.Title != "<b>t</b>" || len(h.Sections) != 1 {
		t.Fatalf("hostile run = %+v", h)
	}
	sec := h.Sections[0]
	if sec.Heading != "<img src=x onerror=alert(1)>" || sec.Columns[0] != "<i>c</i>" || sec.Rows[0][1].Text != "<svg onload=alert(1)>" {
		t.Errorf("section = %+v, want the raw strings as text", sec)
	}
	if cls := sec.Rows[0][0].Class; strings.Contains(cls, "onmouseover") || strings.Contains(cls, `"`) {
		t.Errorf("unknown badge variant became class %q, want it dropped", cls)
	}
}

func TestTablePagePollsOnlyWhileVisible(t *testing.T) {
	t.Parallel()

	body := getPanelBody(t, newServer(t, Config{Key: testKey}, testSources()), panelDERPrograms)
	runs := runTablePage(t, tablePageSource(t), tablePollScenarios(body))
	checkTablePolling(t, runs)
}

func tablePollScenarios(body json.RawMessage) map[string]nodeScenario {
	return map[string]nodeScenario{
		"visible":     {Body: body},
		"hideLater":   {Body: body, Steps: []string{"hide", "tick", "show", "tick"}},
		"hiddenStart": {Body: body, Hidden: true, Steps: []string{"tick", "show"}},
	}
}

func checkTablePolling(t *testing.T, runs map[string]nodeRun) {
	t.Helper()
	if v := runs["visible"]; v.Fetches != 1 || v.Timers != 1 {
		t.Errorf("visible start: %d fetches, %d timers, want 1 and 1", v.Fetches, v.Timers)
	}
	h := runs["hideLater"]
	if len(h.Steps) != 4 {
		t.Fatalf("hideLater steps = %+v", h.Steps)
	}
	if s := h.Steps[1]; s.Fetches != 1 || s.Timers != 0 {
		t.Errorf("tick while hidden: %d fetches, %d timers, want no fetch and no new timer", s.Fetches, s.Timers)
	}
	if s := h.Steps[2]; s.Fetches != 2 || s.Timers != 1 {
		t.Errorf("after show: %d fetches, %d timers, want polling resumed", s.Fetches, s.Timers)
	}
	hs := runs["hiddenStart"]
	if hs.Steps[0].Fetches != 0 {
		t.Errorf("hidden start fetched %d times, want none", hs.Steps[0].Fetches)
	}
	if hs.Steps[1].Fetches != 1 || hs.Fetches != 1 {
		t.Errorf("after show from hidden start: %d fetches, want 1", hs.Steps[1].Fetches)
	}
}

// TestTablePageFallsBackWhereAbortSignalHasNoTimeout: a browser without
// AbortSignal.timeout still loads the page, with its own controller and a
// timer it clears; a browser with it uses it and makes no controller.
func TestTablePageFallsBackWhereAbortSignalHasNoTimeout(t *testing.T) {
	t.Parallel()

	body := getPanelBody(t, newServer(t, Config{Key: testKey}, testSources()), panelDERPrograms)
	check := func(t *testing.T, source string) map[string]nodeRun {
		t.Helper()
		return runTablePage(t, source, map[string]nodeScenario{
			"old": {Body: body, NoTimeout: true},
			"new": {Body: body},
		})
	}
	runs := check(t, tablePageSource(t))
	old := runs["old"]
	if len(old.Sections) != 1 || old.Sections[0].Empty != "No DERPrograms served." || old.Fetches != 1 {
		t.Errorf("page without AbortSignal.timeout: %d sections, %d fetches, status %q; want it loaded", len(old.Sections), old.Fetches, old.Status)
	}
	if old.TimeoutCalls != 0 || old.Controllers != 1 || old.Cleared != 1 {
		t.Errorf("fallback: %d timeout calls, %d controllers, %d timers cleared; want 0, 1 and 1", old.TimeoutCalls, old.Controllers, old.Cleared)
	}
	if cur := runs["new"]; cur.TimeoutCalls != 1 || cur.Controllers != 0 {
		t.Errorf("with AbortSignal.timeout: %d calls, %d controllers; want 1 and 0", cur.TimeoutCalls, cur.Controllers)
	}
}

// TestTablePageMutantsAreCaught proves the page tests can fail: each
// mutant breaks one property of table.js, and the test that guards it must
// see the difference.
func TestTablePageMutantsAreCaught(t *testing.T) {
	t.Parallel()

	src := tablePageSource(t)
	mutate := func(old, repl string) string {
		if strings.Count(src, old) != 1 {
			t.Fatalf("mutant target %q occurs %d times in table.js, want 1", old, strings.Count(src, old))
		}
		return strings.Replace(src, old, repl, 1)
	}
	body := getPanelBody(t, newServer(t, Config{Key: testKey}, testSources()), panelDERPrograms)

	t.Run("calls AbortSignal.timeout without a fallback", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(`if (typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function") {`, `if (true) {`)
		old := runTablePage(t, mutant, map[string]nodeScenario{"old": {Body: body, NoTimeout: true}})["old"]
		if len(old.Sections) != 0 {
			t.Error("the fallback check passed against a page that always calls AbortSignal.timeout")
		}
	})
	t.Run("polls while hidden", func(t *testing.T) {
		t.Parallel()
		mutant := mutate("if (document.hidden) return;", "")
		runs := runTablePage(t, mutant, tablePollScenarios(body))
		if runs["hideLater"].Steps[1].Fetches == 1 && runs["hiddenStart"].Steps[0].Fetches == 0 {
			t.Error("the polling checks passed against a page that ignores document.hidden")
		}
	})
	t.Run("writes markup with innerHTML", func(t *testing.T) {
		t.Parallel()
		mutant := mutate("e.textContent = text;", "e.innerHTML = text;")
		runs := runTablePage(t, mutant, map[string]nodeScenario{"hostile": {Body: json.RawMessage(hostileBody)}})
		if runs["hostile"].InnerHTML == 0 {
			t.Error("the text-only check passed against a page that writes innerHTML")
		}
	})
	t.Run("keeps an unknown badge variant as a class", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(`BADGES.has(c.badge) ? " badge-" + c.badge : ""`, `" badge-" + c.badge`)
		runs := runTablePage(t, mutant, map[string]nodeScenario{"hostile": {Body: json.RawMessage(hostileBody)}})
		if !strings.Contains(runs["hostile"].Sections[0].Rows[0][0].Class, "onmouseover") {
			t.Error("the badge check passed against a page that trusts the variant")
		}
	})
	t.Run("drops group headings", func(t *testing.T) {
		t.Parallel()
		mutant := mutate("if (g.heading) root.append(el(\"h3\", g.heading));", "")
		s := newServer(t, Config{Key: testKey}, tablePageSources())
		runs := runTablePage(t, mutant, map[string]nodeScenario{"flow": {Body: getPanelBody(t, s, panelControlFlow)}})
		if runs["flow"].Sections[0].Groups[0].Heading != "" {
			t.Error("the group heading check passed against a page that drops headings")
		}
	})
}
