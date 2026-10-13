package adminui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/graphpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

type graphScenario struct {
	Body      json.RawMessage            `json:"body,omitempty"`
	Bodies    map[string]json.RawMessage `json:"bodies,omitempty"`
	Status    int                        `json:"status,omitempty"`
	Hidden    bool                       `json:"hidden,omitempty"`
	NoTimeout bool                       `json:"noTimeout,omitempty"`
	NoChart   bool                       `json:"noChart,omitempty"`
	Steps     []string                   `json:"steps,omitempty"`
}

type graphInput struct {
	Value    string `json:"value"`
	Checked  bool   `json:"checked"`
	Disabled bool   `json:"disabled"`
}

type graphRun struct {
	URLs        []string     `json:"urls"`
	Title       string       `json:"title"`
	DocTitle    string       `json:"docTitle"`
	Status      string       `json:"status"`
	StatusClass string       `json:"statusClass"`
	Prose       []string     `json:"prose"`
	Hint        string       `json:"hint"`
	Inputs      []graphInput `json:"inputs"`
	Labels      []string     `json:"labels"`
	ChartText   string       `json:"chartText"`
	Summary     string       `json:"summary"`
	Chart       struct {
		Created   int          `json:"created"`
		Destroyed int          `json:"destroyed"`
		SetData   int          `json:"setData"`
		Data      [][]*float64 `json:"data"`
		Labels    []string     `json:"labels"`
		Gaps      []bool       `json:"gaps"`
		Strokes   []string     `json:"strokes"`
		YLabel    string       `json:"yLabel"`
	} `json:"chart"`
	Latest struct {
		Heading string   `json:"heading"`
		Prose   []string `json:"prose"`
		Empty   string   `json:"empty"`
		Columns []string `json:"columns"`
		Rows    [][]struct {
			Text     string `json:"text"`
			DateTime string `json:"dateTime"`
		} `json:"rows"`
	} `json:"latest"`
	Steps      []nodeStep `json:"steps"`
	Timers     int        `json:"timers"`
	InnerHTML  int        `json:"innerHTML"`
	Style      int        `json:"style"`
	TimeoutCal int        `json:"timeoutCalls"`
	Controller int        `json:"controllers"`
}

func graphPageSource(t *testing.T) string {
	t.Helper()
	b, err := graphpage.Files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	return string(b)
}

// runGraphPage runs source against the scenarios on a fake DOM with a fake
// chart library (testdata/graph_page.js). A missing node skips the test
// locally and fails it under CI, as the other page tests do.
func runGraphPage(t *testing.T, source string, scenarios map[string]graphScenario) map[string]graphRun {
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
	cmd := exec.Command(node, "testdata/graph_page.js")
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var runs map[string]graphRun
	if err := json.Unmarshal(out, &runs); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return runs
}

func rawGraph(t *testing.T, s *Server, query string) json.RawMessage {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, graphPath("data")+query, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET graph data %s: %d %s", query, rec.Code, rec.Body.String())
	}
	return slices.Clone(rec.Body.Bytes())
}

// twoBatteries is the fixture most page scenarios render: m-1 reports at
// two instants and m-2 at a third between them, so the shared time axis
// needs gaps.
func twoBatteries(t *testing.T) *Server {
	t.Helper()
	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("m-1", sample(graphEpoch, 65.5), sample(graphEpoch+15, 64)),
		soc("m-2", sample(graphEpoch+5, 30)),
	}}
	return graphServer(t, h, time.Unix(graphEpoch+75, 0), registry.Entry{MRID: "m-1", Name: "Battery One"})
}

func TestGraphPageDrawsTheChartPickerAndTable(t *testing.T) {
	t.Parallel()

	s := twoBatteries(t)
	run := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"page": {Body: rawGraph(t, s, "")}})["page"]

	if run.Title != "Graph: device status" || run.DocTitle != "Graph: device status" || !strings.HasPrefix(run.Status, "updated ") {
		t.Errorf("title %q, document title %q, status %q", run.Title, run.DocTitle, run.Status)
	}
	if run.Chart.Created != 1 || run.Chart.YLabel != "%" {
		t.Fatalf("chart created %d times with y label %q, want once and %%", run.Chart.Created, run.Chart.YLabel)
	}
	e := float64(graphEpoch)
	f := func(v float64) *float64 { return &v }
	want := [][]*float64{{f(e), f(e + 5), f(e + 15)}, {f(65.5), nil, f(64)}, {nil, f(30), nil}}
	if !reflect.DeepEqual(run.Chart.Data, want) {
		t.Fatalf("chart data = %s, want the shared time axis then one row per battery, null where a battery has no point", string(mustJSON(t, run.Chart.Data)))
	}
	if !slices.Equal(run.Chart.Labels, []string{"", "Battery One", "m-2"}) || !slices.Equal(run.Chart.Gaps, []bool{false, true, true}) {
		t.Errorf("series labels %q, span gaps %v", run.Chart.Labels, run.Chart.Gaps)
	}
	if run.Chart.Strokes[1] == run.Chart.Strokes[2] || run.Chart.Strokes[1] == "" {
		t.Errorf("strokes = %q, want two distinct colors", run.Chart.Strokes)
	}
	if !strings.Contains(run.Summary, "2 series. Latest values: Battery One: 64; m-2: 30") {
		t.Errorf("summary = %q", run.Summary)
	}

	if !slices.Equal(run.Latest.Columns, []string{"Series", "Value (%)", "As of", "Age", "Charted"}) || len(run.Latest.Rows) != 2 {
		t.Fatalf("latest = %+v, want the five columns and two rows", run.Latest)
	}
	row := run.Latest.Rows[0]
	if row[0].Text != "Battery One" || row[1].Text != "64" || row[3].Text != "1m0s" || row[4].Text != "Yes" ||
		row[2].DateTime != time.Unix(graphEpoch+15, 0).UTC().Format(time.RFC3339) {
		t.Errorf("latest row 0 = %+v", row)
	}
	if want := []graphInput{{Value: "m-1"}, {Value: "m-2"}}; !slices.Equal(run.Inputs, want) {
		t.Errorf("checkboxes = %+v, want %+v", run.Inputs, want)
	}
	if want := []string{" Battery One", " m-2"}; !slices.Equal(run.Labels, want) {
		t.Errorf("checkbox labels = %q, want %q", run.Labels, want)
	}
	if !slices.Equal(run.URLs, []string{"data"}) || run.Timers != 1 {
		t.Errorf("fetched %q with %d timers, want one fetch of the data route and one poll scheduled", run.URLs, run.Timers)
	}
	if !strings.Contains(strings.Join(run.Prose, " "), "Battery state of charge as the bridge last published it") {
		t.Errorf("prose = %q", run.Prose)
	}
}

// TestGraphPageSelectionDrivesTheQuery: picking, unpicking and clearing
// each fetch at once with the selection the server is to chart.
func TestGraphPageSelectionDrivesTheQuery(t *testing.T) {
	t.Parallel()

	s := twoBatteries(t)
	run := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"pick": {
		Body: rawGraph(t, s, ""),
		Bodies: map[string]json.RawMessage{
			"?sel=m-2":         rawGraph(t, s, "?sel=m-2"),
			"?sel=m-2&sel=m-1": rawGraph(t, s, "?sel=m-2&sel=m-1"),
			"?sel=m-1":         rawGraph(t, s, "?sel=m-1"),
		},
		Steps: []string{"pick:m-2", "pick:m-1", "unpick:m-2", "clear"},
	}})["pick"]

	want := []string{"data", "data?sel=m-2", "data?sel=m-2&sel=m-1", "data?sel=m-1", "data"}
	if !slices.Equal(run.URLs, want) {
		t.Fatalf("fetched %q, want %q", run.URLs, want)
	}
	for i, st := range run.Steps {
		if st.Fetches != i+2 {
			t.Errorf("after %q: %d fetches, want %d (a pick loads at once)", st.Step, st.Fetches, i+2)
		}
	}
	for _, in := range run.Inputs {
		if in.Checked {
			t.Errorf("checkbox %s still checked after Clear selection", in.Value)
		}
	}
	if run.Hint == "" || !strings.Contains(run.Hint, "Nothing picked") {
		t.Errorf("hint after clear = %q", run.Hint)
	}
}

func TestGraphPageHoldsTheSelectionAtTheMaximum(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0))
	var steps []string
	for i := range 16 {
		steps = append(steps, fmt.Sprintf("pick:M-%02d", i))
	}
	run := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"max": {Body: rawGraph(t, s, ""), Steps: steps}})["max"]

	checked, disabled := 0, 0
	for _, in := range run.Inputs {
		if in.Checked {
			checked++
			if in.Disabled {
				t.Errorf("picked %s is disabled, so it could not be unpicked", in.Value)
			}
		}
		if in.Disabled {
			disabled++
		}
	}
	if len(run.Inputs) != 20 || checked != 16 || disabled != 4 {
		t.Errorf("%d checkboxes, %d checked, %d disabled; want 20, 16 and the 4 unpicked disabled", len(run.Inputs), checked, disabled)
	}
	if !strings.Contains(run.Hint, "16 of 16 picked") {
		t.Errorf("hint = %q", run.Hint)
	}
}

// TestGraphPageDropsABatteryThatLeft: a picked battery the server no longer
// offers leaves the selection, so the next poll asks for no stale id.
func TestGraphPageDropsABatteryThatLeft(t *testing.T) {
	t.Parallel()

	both := twoBatteries(t)
	only2 := graphServer(t, &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{soc("m-2", sample(graphEpoch+5, 30))}}, time.Unix(graphEpoch+75, 0))
	run := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"left": {
		Body:   rawGraph(t, both, ""),
		Bodies: map[string]json.RawMessage{"?sel=m-1": rawGraph(t, only2, "")},
		Steps:  []string{"pick:m-1", "tick"},
	}})["left"]
	if want := []string{"data", "data?sel=m-1", "data"}; !slices.Equal(run.URLs, want) {
		t.Errorf("fetched %q, want %q", run.URLs, want)
	}
}

func TestGraphPageRepollKeepsTheChart(t *testing.T) {
	t.Parallel()

	body := rawGraph(t, twoBatteries(t), "")
	run := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"repoll": {Body: body, Steps: []string{"tick"}}})["repoll"]
	if len(run.URLs) != 2 || run.Chart.Created != 1 || run.Chart.SetData != 1 || run.Chart.Destroyed != 0 {
		t.Errorf("after a second poll: %d fetches, chart created %d, setData %d, destroyed %d; want 2, 1, 1 and 0", len(run.URLs), run.Chart.Created, run.Chart.SetData, run.Chart.Destroyed)
	}
}

func TestGraphPageShowsEmptyFailedAndNoLibraryStates(t *testing.T) {
	t.Parallel()

	empty := rawGraph(t, graphServer(t, &fakeHistory{}, time.Now()), "")
	runs := runGraphPage(t, graphPageSource(t), map[string]graphScenario{
		"empty":   {Body: empty},
		"failed":  {Status: 500, Body: json.RawMessage(`{"error":"boom"}`)},
		"nolib":   {Body: rawGraph(t, twoBatteries(t), ""), NoChart: true},
		"badjson": {Body: json.RawMessage(`{"title":"x"}`)},
	})
	e := runs["empty"]
	if e.ChartText != "No state of charge samples yet." || e.Chart.Created != 0 || e.Latest.Empty != "No state of charge samples yet." || e.Latest.Columns != nil || len(e.Inputs) != 0 {
		t.Errorf("empty page = chart text %q, %d charts, latest %+v, %d checkboxes", e.ChartText, e.Chart.Created, e.Latest, len(e.Inputs))
	}
	f := runs["failed"]
	if !strings.Contains(f.Status, "unavailable") || f.StatusClass != "error" || f.Latest.Columns != nil || f.Chart.Created != 0 {
		t.Errorf("failed poll shows status %q (%q), latest %+v; want an unavailable notice and no stale content", f.Status, f.StatusClass, f.Latest)
	}
	n := runs["nolib"]
	if n.ChartText != "The chart library did not load." || len(n.Latest.Rows) != 2 || !strings.HasPrefix(n.Status, "updated") {
		t.Errorf("no chart library: chart text %q, %d latest rows, status %q; want the notice and the table still drawn", n.ChartText, len(n.Latest.Rows), n.Status)
	}
	if b := runs["badjson"]; !strings.Contains(b.Status, "unavailable") {
		t.Errorf("a document without its fields shows status %q, want an unavailable notice", b.Status)
	}
}

// hostileGraph is data whose strings are markup, to prove the page writes
// them as text.
const hostileGraph = `{"title":"<b>t</b>","unit":"<i>%</i>","max":16,"prose":["<script>alert(1)</script>"],"empty":"",
"choices":[{"id":"m-1","label":"<img src=x onerror=alert(1)>"}],
"series":[{"mrid":"m-1","name":"<img src=x onerror=alert(1)>","points":[[1759000000,5]]}],
"latest":{"kind":"table","heading":"<u>h</u>","prose":["<svg onload=alert(1)>"],"empty":"",
"body":{"columns":["<i>c</i>"],"rows":[[{"kind":"text","text":"<b>x</b>"}]]}}}`

func TestGraphPageWritesServerStringsAsText(t *testing.T) {
	t.Parallel()

	h := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"hostile": {Body: json.RawMessage(hostileGraph)}})["hostile"]
	if h.InnerHTML != 0 || h.Style != 0 {
		t.Errorf("innerHTML writes = %d, style writes = %d, want none", h.InnerHTML, h.Style)
	}
	if h.Title != "<b>t</b>" || h.Labels[0] != " <img src=x onerror=alert(1)>" || h.Chart.Labels[1] != "<img src=x onerror=alert(1)>" ||
		h.Latest.Heading != "<u>h</u>" || h.Latest.Rows[0][0].Text != "<b>x</b>" || h.Chart.YLabel != "<i>%</i>" {
		t.Errorf("hostile page = title %q labels %q chart labels %q latest %+v, want the raw strings as text", h.Title, h.Labels, h.Chart.Labels, h.Latest)
	}
}

func graphPollScenarios(body json.RawMessage) map[string]graphScenario {
	return map[string]graphScenario{
		"visible":     {Body: body},
		"hideLater":   {Body: body, Steps: []string{"hide", "tick", "show", "tick"}},
		"hiddenStart": {Body: body, Hidden: true, Steps: []string{"tick", "show"}},
	}
}

func checkGraphPolling(t *testing.T, runs map[string]graphRun) {
	t.Helper()
	if v := runs["visible"]; len(v.URLs) != 1 || v.Timers != 1 {
		t.Errorf("visible start: %d fetches, %d timers, want 1 and 1", len(v.URLs), v.Timers)
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
	if hs.Steps[1].Fetches != 1 {
		t.Errorf("after show from hidden start: %d fetches, want 1", hs.Steps[1].Fetches)
	}
}

func TestGraphPagePollsOnlyWhileVisible(t *testing.T) {
	t.Parallel()
	checkGraphPolling(t, runGraphPage(t, graphPageSource(t), graphPollScenarios(rawGraph(t, twoBatteries(t), ""))))
}

func TestGraphPageFallsBackWhereAbortSignalHasNoTimeout(t *testing.T) {
	t.Parallel()

	body := rawGraph(t, twoBatteries(t), "")
	runs := runGraphPage(t, graphPageSource(t), map[string]graphScenario{"old": {Body: body, NoTimeout: true}, "new": {Body: body}})
	old := runs["old"]
	if old.Chart.Created != 1 || len(old.Latest.Rows) != 2 || old.TimeoutCal != 0 || old.Controller != 1 {
		t.Errorf("without AbortSignal.timeout: %d charts, %d rows, %d timeout calls, %d controllers; want the page loaded through one controller", old.Chart.Created, len(old.Latest.Rows), old.TimeoutCal, old.Controller)
	}
	if cur := runs["new"]; cur.TimeoutCal != 1 || cur.Controller != 0 {
		t.Errorf("with AbortSignal.timeout: %d calls, %d controllers; want 1 and 0", cur.TimeoutCal, cur.Controller)
	}
}

// TestGraphPageMutantsAreCaught proves the page tests can fail: each mutant
// breaks one property of app.js, and the test that guards it must see it.
func TestGraphPageMutantsAreCaught(t *testing.T) {
	t.Parallel()

	src := graphPageSource(t)
	mutate := func(old, repl string) string {
		if strings.Count(src, old) != 1 {
			t.Fatalf("mutant target %q occurs %d times in app.js, want 1", old, strings.Count(src, old))
		}
		return strings.Replace(src, old, repl, 1)
	}
	s := twoBatteries(t)
	body := rawGraph(t, s, "")

	t.Run("polls while hidden", func(t *testing.T) {
		t.Parallel()
		runs := runGraphPage(t, mutate("if (document.hidden) return;", ""), graphPollScenarios(body))
		if runs["hideLater"].Steps[1].Fetches == 1 && runs["hiddenStart"].Steps[0].Fetches == 0 {
			t.Error("the polling checks passed against a page that ignores document.hidden")
		}
	})
	t.Run("writes markup with innerHTML", func(t *testing.T) {
		t.Parallel()
		runs := runGraphPage(t, mutate("e.textContent = text;", "e.innerHTML = text;"), map[string]graphScenario{"hostile": {Body: json.RawMessage(hostileGraph)}})
		if runs["hostile"].InnerHTML == 0 {
			t.Error("the text-only check passed against a page that writes innerHTML")
		}
	})
	t.Run("calls AbortSignal.timeout without a fallback", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(`if (typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function") {`, `if (true) {`)
		if old := runGraphPage(t, mutant, map[string]graphScenario{"old": {Body: body, NoTimeout: true}})["old"]; old.Chart.Created != 0 {
			t.Error("the fallback check passed against a page that always calls AbortSignal.timeout")
		}
	})
	t.Run("sends no selection", func(t *testing.T) {
		t.Parallel()
		mutant := mutate(`return selected.length === 0 ? "" : "?" + selected.map`, `return "" && selected.map`)
		run := runGraphPage(t, mutant, map[string]graphScenario{"pick": {Body: body, Steps: []string{"pick:m-1"}}})["pick"]
		if slices.Contains(run.URLs, "data?sel=m-1") {
			t.Error("the selection check passed against a page that never sends it")
		}
	})
	t.Run("does not cap the selection", func(t *testing.T) {
		t.Parallel()
		big := rawGraph(t, graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0)), "")
		var steps []string
		for i := range 16 {
			steps = append(steps, fmt.Sprintf("pick:M-%02d", i))
		}
		run := runGraphPage(t, mutate("box.disabled = atMax && !box.checked;", "box.disabled = false;"), map[string]graphScenario{"max": {Body: big, Steps: steps}})["max"]
		for _, in := range run.Inputs {
			if in.Disabled {
				t.Fatal("the cap check passed against a page that never disables a checkbox")
			}
		}
	})
	t.Run("keeps a battery that left", func(t *testing.T) {
		t.Parallel()
		only2 := rawGraph(t, graphServer(t, &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{soc("m-2", sample(graphEpoch+5, 30))}}, time.Unix(graphEpoch+75, 0)), "")
		mutant := mutate("selected = selected.filter((id) => known.has(id));", "")
		run := runGraphPage(t, mutant, map[string]graphScenario{"left": {Body: body, Bodies: map[string]json.RawMessage{"?sel=m-1": only2}, Steps: []string{"pick:m-1", "tick"}}})["left"]
		if run.URLs[len(run.URLs)-1] == "data" {
			t.Error("the departed-battery check passed against a page that never prunes the selection")
		}
	})
	t.Run("redraws the chart on every poll", func(t *testing.T) {
		t.Parallel()
		runs := runGraphPage(t, mutate("if (chart && sig === chartSig) {", "if (false) {"), map[string]graphScenario{"repoll": {Body: body, Steps: []string{"tick"}}})
		if r := runs["repoll"]; r.Chart.Created == 1 && r.Chart.SetData == 1 {
			t.Error("the repoll check passed against a page that rebuilds the chart each time")
		}
	})
	t.Run("does not join gaps", func(t *testing.T) {
		t.Parallel()
		run := runGraphPage(t, mutate("spanGaps: true", "spanGaps: false"), map[string]graphScenario{"p": {Body: body}})["p"]
		if slices.Contains(run.Chart.Gaps, true) {
			t.Error("the gap check passed against a page that does not join gaps")
		}
	})
}
