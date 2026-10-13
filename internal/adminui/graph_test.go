package adminui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/graphpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// wireGraph is the graph data route's document as the page reads it.
type wireGraph struct {
	Title   string   `json:"title"`
	Unit    string   `json:"unit"`
	Max     int      `json:"max"`
	Prose   []string `json:"prose"`
	Empty   string   `json:"empty"`
	Choices []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"choices"`
	Series []struct {
		MRID   string       `json:"mrid"`
		Name   string       `json:"name"`
		Points [][2]float64 `json:"points"`
	} `json:"series"`
	Latest struct {
		Heading string        `json:"heading"`
		Prose   []string      `json:"prose"`
		Empty   string        `json:"empty"`
		Body    pageTableBody `json:"body"`
	} `json:"latest"`
}

// fakeHistory is a HistorySource that returns what a test preloaded, so a
// test can serve samples the real store would have refused.
type fakeHistory struct {
	snap []telemetryhistory.SeriesSnapshot
}

func (f *fakeHistory) Snapshot() []telemetryhistory.SeriesSnapshot { return f.snap }
func (f *fakeHistory) Evictions() uint64                           { return 0 }

func (f *fakeHistory) Series(key telemetryhistory.SeriesKey) ([]telemetryhistory.Sample, bool) {
	for _, ss := range f.snap {
		if ss.Key == key {
			return ss.Samples, true
		}
	}
	return nil, false
}

// graphEpoch is a fixed instant the tests stamp samples from, so wire
// milliseconds are literals.
const graphEpoch = int64(1_759_000_000)

func soc(object string, samples ...telemetryhistory.Sample) telemetryhistory.SeriesSnapshot {
	return telemetryhistory.SeriesSnapshot{Key: telemetryhistory.SeriesKey{Object: object, Attribute: socAttribute}, Samples: samples}
}

func sample(at int64, v float64) telemetryhistory.Sample {
	return telemetryhistory.Sample{At: at, Value: v}
}

// graphServer serves the graph panel over src, with names in the registry
// and the clock fixed at now.
func graphServer(t *testing.T, history HistorySource, now time.Time, names ...registry.Entry) *Server {
	t.Helper()
	src := testSources()
	src.History = history
	src.Registry = &fakeRegistry{entries: names}
	s := newServer(t, Config{Key: testKey}, src)
	s.now = func() time.Time { return now }
	return s
}

func graphPath(file string) string { return "/apps/" + panelGraphInput + "/" + file }

// getGraph fetches the data route through the full listener handler, as the
// page does, with query such as "?sel=M-01".
func getGraph(t *testing.T, s *Server, query string) wireGraph {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, graphPath("data")+query, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET graph data %s: %d %s", query, rec.Code, rec.Body.String())
	}
	var g wireGraph
	decodeJSON(t, rec.Body.Bytes(), &g)
	return g
}

func (g wireGraph) names() []string {
	var out []string
	for _, s := range g.Series {
		out = append(out, s.Name)
	}
	return out
}

// TestGraphDataChartsTwoBatteriesWithALatestTable: one line per battery, in
// percent, and a table of each series' newest value and its age.
func TestGraphDataChartsTwoBatteriesWithALatestTable(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("m-1", sample(graphEpoch, 65.5), sample(graphEpoch+15, 64)),
		soc("m-2", sample(graphEpoch+5, 30)),
		{Key: telemetryhistory.SeriesKey{Object: "m-1", Attribute: "DERControl.DERControlBase.opModTargetW"},
			Samples: []telemetryhistory.Sample{sample(graphEpoch, 5000)}},
	}}
	now := time.Unix(graphEpoch+75, 0)
	s := graphServer(t, h, now, registry.Entry{MRID: "m-1", Name: "Battery One"})
	g := getGraph(t, s, "")

	if g.Unit != "%" || g.Title != "Graph: device status" || g.Max != 16 {
		t.Errorf("unit %q, title %q, max %d; want %%, the tab label and 16", g.Unit, g.Title, g.Max)
	}
	type line = struct {
		MRID   string       `json:"mrid"`
		Name   string       `json:"name"`
		Points [][2]float64 `json:"points"`
	}
	want := []line{
		{MRID: "m-1", Name: "Battery One", Points: [][2]float64{{float64(graphEpoch), 65.5}, {float64(graphEpoch + 15), 64}}},
		{MRID: "m-2", Name: "m-2", Points: [][2]float64{{float64(graphEpoch + 5), 30}}},
	}
	if len(g.Series) != len(want) {
		t.Fatalf("series = %+v, want %+v (the setpoint series must not be charted)", g.Series, want)
	}
	for i, w := range want {
		got := g.Series[i]
		if got.MRID != w.MRID || got.Name != w.Name || !slices.Equal(got.Points, w.Points) {
			t.Errorf("series[%d] = %+v, want %+v", i, got, w)
		}
	}

	if !slices.Equal(g.Latest.Body.Columns, []string{"Series", "Value (%)", "As of", "Age", "Charted"}) {
		t.Errorf("latest columns = %q", g.Latest.Body.Columns)
	}
	rows := g.Latest.Body.Rows
	if len(rows) != 2 {
		t.Fatalf("latest rows = %d, want 2", len(rows))
	}
	cells := func(r []pageCell, idx ...int) []string {
		var out []string
		for _, i := range idx {
			out = append(out, r[i].Text)
		}
		return out
	}
	if got := cells(rows[0], 0, 1, 3, 4); !slices.Equal(got, []string{"Battery One", "64", "1m0s", "Yes"}) {
		t.Errorf("row 0 = %q, want Battery One 64 1m0s Yes", got)
	}
	if got := cells(rows[1], 0, 1, 3, 4); !slices.Equal(got, []string{"m-2", "30", "1m10s", "Yes"}) {
		t.Errorf("row 1 = %q, want m-2 30 1m10s Yes", got)
	}
	if rows[0][2].Kind != "time" || rows[0][2].DateTime != time.Unix(graphEpoch+15, 0).UTC().Format(time.RFC3339) {
		t.Errorf("row 0 as-of = %+v, want a time cell at the newest sample", rows[0][2])
	}
	wantChoices := []string{"Battery One", "m-2"}
	var gotChoices []string
	for _, c := range g.Choices {
		gotChoices = append(gotChoices, c.Label)
	}
	if !slices.Equal(gotChoices, wantChoices) {
		t.Errorf("choices = %q, want %q", gotChoices, wantChoices)
	}
}

// TestGraphDataServesTheNewest720: 2000 appended samples leave 1440 in the
// real store, of which the page is sent the newest 720.
func TestGraphDataServesTheNewest720(t *testing.T) {
	t.Parallel()

	var store telemetryhistory.Store
	base := time.Now().Unix() - 2000
	key := telemetryhistory.SeriesKey{Object: "m-1", Attribute: socAttribute}
	for i := 0; i < 2000; i++ {
		if err := store.Append(key, sample(base+int64(i), float64(i))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	s := graphServer(t, &store, time.Now())
	g := getGraph(t, s, "")

	pts := g.Series[0].Points
	if len(pts) != 720 {
		t.Fatalf("points = %d, want 720", len(pts))
	}
	if last := pts[len(pts)-1]; last != [2]float64{float64(base + 1999), 1999} {
		t.Errorf("last point = %v, want the newest sample %v", last, [2]float64{float64(base + 1999), 1999})
	}
	if first := pts[0]; first[1] != 1280 {
		t.Errorf("first point value = %v, want 1280 (the 720th newest)", first[1])
	}
	for i := 1; i < len(pts); i++ {
		if pts[i][0] <= pts[i-1][0] {
			t.Fatalf("points[%d] time %v not after %v", i, pts[i][0], pts[i-1][0])
		}
	}
	if !strings.Contains(strings.Join(g.Prose, " "), "newest 720 samples") {
		t.Errorf("prose %q does not say the line was cut to 720", g.Prose)
	}
}

// TestGraphDataDropsSamplesTheChartRefuses: the store refuses these at
// ingest, so a fake source stands in for a store that did not.
func TestGraphDataDropsSamplesTheChartRefuses(t *testing.T) {
	t.Parallel()

	nan := 0.0
	nan = nan / nan
	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("m-1",
			sample(0, 50),
			sample(graphEpoch, nan),
			sample(graphEpoch+1, 51),
			sample(graphEpoch+1, 52),
			sample(graphEpoch, 53),
			sample(graphEpoch+2, 54)),
		soc("m-2", sample(0, 1)),
	}}
	s := graphServer(t, h, time.Unix(graphEpoch+10, 0))
	g := getGraph(t, s, "")

	if len(g.Series) != 1 || g.Series[0].Name != "m-1" {
		t.Fatalf("series = %+v, want only m-1 (m-2 has no chartable sample)", g.Series)
	}
	want := [][2]float64{{float64(graphEpoch + 1), 51}, {float64(graphEpoch + 2), 54}}
	if got := g.Series[0].Points; !slices.Equal(got, want) {
		t.Errorf("points = %v, want %v", got, want)
	}
	if !strings.Contains(strings.Join(g.Prose, " "), "5 samples were not charted") {
		t.Errorf("prose %q does not count the 5 dropped samples", g.Prose)
	}
	if rows := g.Latest.Body.Rows; len(rows) != 1 {
		t.Errorf("latest rows = %d, want 1", len(rows))
	}
}

func TestGraphDataLabelsSeries(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("m-1", sample(graphEpoch, 1)),
		soc("m-2", sample(graphEpoch, 2)),
		soc("m-3", sample(graphEpoch, 3)),
		soc("m-4", sample(graphEpoch, 4)),
	}}
	s := graphServer(t, h, time.Unix(graphEpoch, 0),
		registry.Entry{MRID: "m-1", Name: "Same"},
		registry.Entry{MRID: "m-2", Name: "Same"},
		registry.Entry{MRID: "m-3", Name: "<img src=x onerror=alert(1)>"},
		registry.Entry{MRID: "m-4"})
	g := getGraph(t, s, "")

	// Shared names fall back to the mRID, an empty name falls back too, and
	// a name is passed through as data for the page to write as text.
	if want := []string{"m-1", "m-2", "<img src=x onerror=alert(1)>", "m-4"}; !slices.Equal(g.names(), want) {
		t.Errorf("names = %q, want %q", g.names(), want)
	}
}

// TestGraphDataSendsEmptyAsEmpty reads the serialized bytes: no samples is
// empty arrays, never null, and the empty text travels with the data.
func TestGraphDataSendsEmptyAsEmpty(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Now())
	rec := doRequest(t, s.Handler(), http.MethodGet, graphPath("data"), "Bearer "+testKey, "localhost")
	body := rec.Body.String()
	for _, w := range []string{`"series":[]`, `"choices":[]`, `"rows":[]`, `"empty":"No state of charge samples yet."`} {
		if !strings.Contains(body, w) {
			t.Errorf("data %s lacks %s", body, w)
		}
	}
	if strings.Contains(body, "null") {
		t.Errorf("data %s carries a null", body)
	}
}

func TestGraphDataAgeNeverReadsNegative(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{soc("m-1", sample(graphEpoch+120, 1))}}
	s := graphServer(t, h, time.Unix(graphEpoch, 0))
	if got := getGraph(t, s, "").Latest.Body.Rows[0][3].Text; got != "0s" {
		t.Errorf("age of a report stamped ahead of the clock = %q, want 0s", got)
	}
}

// TestGraphDataFitsTheChartLimits: 40 batteries, each with 1440 samples, is
// over the series limit and, untrimmed, over the point limit.
func TestGraphDataFitsTheChartLimits(t *testing.T) {
	t.Parallel()

	var snap []telemetryhistory.SeriesSnapshot
	for i := 0; i < 40; i++ {
		var ss []telemetryhistory.Sample
		for j := 0; j < telemetryhistory.SamplesPerSeries; j++ {
			ss = append(ss, sample(graphEpoch+int64(j), float64(j)))
		}
		// Battery 0 is the quietest: its newest sample is the oldest.
		snap = append(snap, soc(fmt.Sprintf("m-%02d", i), ss[:len(ss)-i]...))
	}
	s := graphServer(t, &fakeHistory{snap: snap}, time.Unix(graphEpoch+2000, 0))
	g := getGraph(t, s, "")

	if len(g.Series) != 16 {
		t.Fatalf("series = %d, want 16", len(g.Series))
	}
	// The 16 with the newest samples are batteries 0..15 here (fewest
	// samples dropped from the tail), in their original order.
	for i, ser := range g.Series {
		if want := fmt.Sprintf("m-%02d", i); ser.Name != want {
			t.Errorf("series[%d] = %q, want %q", i, ser.Name, want)
		}
	}
	if !strings.Contains(strings.Join(g.Prose, " "), "Showing 16 of 40 series") {
		t.Errorf("prose %q does not say 24 series were cut", g.Prose)
	}
	rows := g.Latest.Body.Rows
	if len(rows) != 40 {
		t.Fatalf("latest rows = %d, want 40 (every battery)", len(rows))
	}
	for i, r := range rows {
		if want := map[bool]string{true: "Yes", false: "No"}[i < 16]; r[4].Text != want {
			t.Errorf("row %d charted = %q, want %q", i, r[4].Text, want)
		}
	}
}

func TestFitChart(t *testing.T) {
	t.Parallel()

	mk := func(name string, newest int64, n int) chartSeries {
		var ss []telemetryhistory.Sample
		for i := 0; i < n; i++ {
			ss = append(ss, sample(newest-int64(n-1-i), float64(i)))
		}
		return chartSeries{name: name, samples: ss}
	}
	names := func(cs []chartSeries) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.name)
		}
		return out
	}

	t.Run("series over the limit keep the newest, in order", func(t *testing.T) {
		t.Parallel()
		in := []chartSeries{mk("a", 100, 1), mk("b", 300, 1), mk("c", 200, 1), mk("d", 400, 1)}
		got, notes := fitChart(in, chartLimits{series: 2, seriesPoints: 10, descriptorPoints: 100})
		if want := []string{"b", "d"}; !slices.Equal(names(got), want) {
			t.Errorf("kept %q, want %q", names(got), want)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "Showing 2 of 4 series") {
			t.Errorf("notes = %q", notes)
		}
	})

	t.Run("the Descriptor point budget is shared and the newest points survive", func(t *testing.T) {
		t.Parallel()
		in := []chartSeries{mk("a", 100, 9), mk("b", 100, 3)}
		got, notes := fitChart(in, chartLimits{series: 4, seriesPoints: 8, descriptorPoints: 10})
		if len(got[0].samples) != 5 || len(got[1].samples) != 3 {
			t.Fatalf("points = %d and %d, want 5 and 3", len(got[0].samples), len(got[1].samples))
		}
		if last := got[0].samples[4]; last.At != 100 || last.Value != 8 {
			t.Errorf("last kept sample of a = %+v, want the newest (100, 8)", last)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "newest 5 samples") {
			t.Errorf("notes = %q", notes)
		}
	})

	t.Run("within the limits nothing is cut and nothing is said", func(t *testing.T) {
		t.Parallel()
		got, notes := fitChart([]chartSeries{mk("a", 100, 4)}, planeChartLimits)
		if len(got[0].samples) != 4 || len(notes) != 0 {
			t.Errorf("got %d samples and notes %q", len(got[0].samples), notes)
		}
	})
}

// pickerHistory is n batteries "M-00".."M-nn", battery i last reporting
// at graphEpoch+i, so a higher index is more recent.
func pickerHistory(n int) []telemetryhistory.SeriesSnapshot {
	var snap []telemetryhistory.SeriesSnapshot
	for i := 0; i < n; i++ {
		snap = append(snap, soc(fmt.Sprintf("M-%02d", i), sample(graphEpoch+int64(i), float64(i))))
	}
	return snap
}

func TestGraphSelectionChartsExactlyTheSelectedBatteries(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0))
	g := getGraph(t, s, "?sel=M-01&sel=M-05&sel=M-03")
	if got, want := g.names(), []string{"M-01", "M-03", "M-05"}; !slices.Equal(got, want) {
		t.Errorf("series = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(g.Prose, " "), "Showing") {
		t.Errorf("prose %q carries a cut note for a selection that fits", g.Prose)
	}
	rows := g.Latest.Body.Rows
	if len(rows) != 20 {
		t.Fatalf("latest rows = %d, want 20", len(rows))
	}
	for _, r := range rows {
		want := "No"
		if r[0].Text == "M-01" || r[0].Text == "M-03" || r[0].Text == "M-05" {
			want = "Yes"
		}
		if r[4].Text != want {
			t.Errorf("%s charted = %q, want %q", r[0].Text, r[4].Text, want)
		}
	}
}

func TestGraphNoSelectionChartsTheSixteenMostRecent(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(49)}, time.Unix(graphEpoch+100, 0))
	g := getGraph(t, s, "")
	names := g.names()
	if len(names) != 16 || names[0] != "M-33" || names[15] != "M-48" {
		t.Errorf("series = %q, want M-33..M-48", names)
	}
	rows := g.Latest.Body.Rows
	if len(rows) != 49 {
		t.Fatalf("latest rows = %d, want all 49", len(rows))
	}
	yes := 0
	for _, r := range rows {
		if r[4].Text == "Yes" {
			yes++
		}
	}
	if yes != 16 {
		t.Errorf("charted rows = %d, want 16", yes)
	}
}

// TestGraphLatestTableTrimsPastTheRowCap: 1200 batteries cannot fit one
// answer; the table says what it cut.
func TestGraphLatestTableTrimsPastTheRowCap(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(1200)}, time.Unix(graphEpoch+2000, 0))
	g := getGraph(t, s, "")
	if len(g.Latest.Body.Rows) != 1000 {
		t.Errorf("rows = %d, want the cap of 1000", len(g.Latest.Body.Rows))
	}
	if !strings.Contains(strings.Join(g.Latest.Prose, " "), "Showing 1000 of 1200 rows") {
		t.Errorf("prose %q carries no trim notice", g.Latest.Prose)
	}
}

func TestGraphEmptyRegistryAndHistoryGiveNoChoicesAndTheDefaultView(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Unix(graphEpoch, 0))
	g := getGraph(t, s, "")
	if len(g.Choices) != 0 || len(g.Series) != 0 || g.Empty != "No state of charge samples yet." {
		t.Errorf("graph = %+v, want no choices, no series and the empty text", g)
	}
	if len(g.Latest.Body.Rows) != 0 || g.Latest.Empty != "No state of charge samples yet." {
		t.Errorf("latest = %+v, want no rows and the empty text", g.Latest)
	}
}

// TestGraphPanelIsAPagePanel: the tab keeps its id, label and position, is
// served as a page, and the plane mounts no descriptor, picker or choices
// route for it.
func TestGraphPanelIsAPagePanel(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: pickerHistory(20)}
	s := graphServer(t, h, time.Unix(graphEpoch+100, 0))
	p := s.panels()[4]
	if p.ID != panelGraphInput || p.Label != "Graph: device status" || p.Page == nil || p.View != nil || p.Picker != nil {
		t.Errorf("panel 5 = %q %q page %t view %t picker %t, want the graph page panel", p.ID, p.Label, p.Page != nil, p.View != nil, p.Picker != nil)
	}
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "localhost")
	var manifest []struct {
		ID     string          `json:"id"`
		Page   string          `json:"page"`
		Picker json.RawMessage `json:"picker"`
	}
	decodeJSON(t, rec.Body.Bytes(), &manifest)
	if len(manifest) < 5 || manifest[4].ID != panelGraphInput || manifest[4].Page != graphPath("") || len(manifest[4].Picker) != 0 {
		t.Errorf("manifest[4] = %+v, want %s with page %s and no picker", manifest, panelGraphInput, graphPath(""))
	}
	for _, route := range []string{"", "/choices"} {
		if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelGraphInput+route, "Bearer "+testKey, "localhost"); rec.Code == http.StatusOK {
			t.Errorf("descriptor route %q = %d, want it not served", route, rec.Code)
		}
	}
}

// TestGraphChoicesFollowTheHistory: a battery that leaves the history is gone
// from the choices on the next request.
func TestGraphChoicesFollowTheHistory(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: pickerHistory(20)}
	s := graphServer(t, h, time.Unix(graphEpoch+100, 0), registry.Entry{MRID: "M-02", Name: "Two"})
	g := getGraph(t, s, "")
	if len(g.Choices) != 20 || g.Choices[0].ID != "M-00" || g.Choices[19].ID != "M-02" || g.Choices[19].Label != "Two" {
		t.Fatalf("choices = %+v, want 20 sorted by label with M-02 named Two last", g.Choices)
	}
	h.snap = h.snap[:19]
	for _, c := range getGraph(t, s, "").Choices {
		if c.ID == "M-19" {
			t.Errorf("M-19 still offered after it left the history")
		}
	}
}

func TestGraphPageServesItsFilesAndTheSharedChartLibrary(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Now())
	for _, f := range []struct{ path, ctype, marker string }{
		{"", "text/html", `src="uplot.min.js"`},
		{"app.js", "text/javascript", "fetch("},
		{"style.css", "text/css", "body"},
		{"uplot.min.js", "text/javascript", "uPlot"},
		{"uplot.min.css", "text/css", ".uplot"},
	} {
		rec := doRequest(t, s.Handler(), http.MethodGet, graphPath(f.path), "Bearer "+testKey, "localhost")
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
		csp := strings.Join(rec.Header().Values("Content-Security-Policy"), "; ")
		for _, want := range []string{"script-src 'self'", "style-src 'self'", "frame-ancestors 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %q: CSP %q lacks %q", f.path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("GET %q: CSP %q allows inline", f.path, csp)
		}
	}
	// One copy of the library: the page serves the SoC page's bytes.
	want, err := fs.ReadFile(socpage.Files, "uplot.min.js")
	if err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s.Handler(), http.MethodGet, graphPath("uplot.min.js"), "Bearer "+testKey, "localhost")
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Error("uplot.min.js is not the SoC page's vendored copy")
	}
	for _, name := range []string{"uplot.min.js", "uplot.min.css"} {
		if _, err := fs.Stat(graphpage.Files, name); err == nil {
			t.Errorf("graphpage embeds its own %s", name)
		}
	}
}

func TestGraphPageNeedsTheCredentialAndRefusesWrites(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Now())
	for _, auth := range []string{"", "Bearer wrong-key-wrong-key"} {
		for _, f := range []string{"", "data", "app.js", "uplot.min.js"} {
			rec := doRequest(t, s.Handler(), http.MethodGet, graphPath(f), auth, "localhost")
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden && rec.Code != http.StatusSeeOther {
				t.Errorf("GET %q with auth %q = %d, want a refusal", f, auth, rec.Code)
			}
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if rec := doRequest(t, s.Handler(), m, graphPath("data"), "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s data = %d, want 405: the page is read only", m, rec.Code)
		}
	}
	if rec := doRequest(t, s.Handler(), http.MethodGet, graphPath("nope"), "Bearer "+testKey, "localhost"); rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown file = %d, want 404", rec.Code)
	}
}

// choicesFor serves the batteries in snap and returns their choices.
func choicesFor(t *testing.T, snap []telemetryhistory.SeriesSnapshot, names ...registry.Entry) []sep2admin.Choice {
	t.Helper()
	s := graphServer(t, &fakeHistory{snap: snap}, time.Unix(graphEpoch+10, 0), names...)
	series, _ := s.socSeries()
	got := pickableChoices(series)
	if err := sep2admin.ValidateChoices(got); err != nil {
		t.Fatalf("ValidateChoices: %v for %+v", err, got)
	}
	return got
}

// TestGraphChoicesPassTheValidatorForTheLiveShape: upper-case GUID mRIDs, a
// name two batteries share, a name over the label limit and an mRID outside
// the id pattern. Every shape must come out valid.
func TestGraphChoicesPassTheValidatorForTheLiveShape(t *testing.T) {
	t.Parallel()

	guid1 := "3F2504E0-4F89-11D3-9A0C-0305E82C3301"
	guid2 := "3F2504E0-4F89-11D3-9A0C-0305E82C3302"
	guid3 := "3F2504E0-4F89-11D3-9A0C-0305E82C3303"
	got := choicesFor(t, []telemetryhistory.SeriesSnapshot{
		soc(guid1, sample(graphEpoch, 1)),
		soc(guid2, sample(graphEpoch, 2)),
		soc(guid3, sample(graphEpoch, 3)),
		soc("{bad id}", sample(graphEpoch, 4)),
	}, registry.Entry{MRID: guid1, Name: "Twin"},
		registry.Entry{MRID: guid2, Name: "Twin"},
		registry.Entry{MRID: guid3, Name: strings.Repeat("n", 129)})
	want := []sep2admin.Choice{{ID: guid1, Label: guid1}, {ID: guid2, Label: guid2}, {ID: guid3, Label: guid3}}
	if !slices.Equal(got, want) {
		t.Errorf("choices = %+v, want %+v (shared and overlong names fall back to the mRID, the bad id is left out)", got, want)
	}
}

func TestGraphChoicesFallBackToTheMRIDForANameThatIsNotUTF8(t *testing.T) {
	t.Parallel()

	got := choicesFor(t, []telemetryhistory.SeriesSnapshot{soc("M-1", sample(graphEpoch, 1))},
		registry.Entry{MRID: "M-1", Name: "bad\xffname"})
	if want := []sep2admin.Choice{{ID: "M-1", Label: "M-1"}}; !slices.Equal(got, want) {
		t.Errorf("choices = %+v, want %+v", got, want)
	}
}

func TestGraphChoicesLabelLengthBoundary(t *testing.T) {
	t.Parallel()

	keep, over := strings.Repeat("k", 128), strings.Repeat("o", 129)
	got := choicesFor(t, []telemetryhistory.SeriesSnapshot{
		soc("M-1", sample(graphEpoch, 1)), soc("M-2", sample(graphEpoch, 2)),
	}, registry.Entry{MRID: "M-1", Name: keep}, registry.Entry{MRID: "M-2", Name: over})
	want := []sep2admin.Choice{{ID: "M-2", Label: "M-2"}, {ID: "M-1", Label: keep}}
	if !slices.Equal(got, want) {
		t.Errorf("choices = %+v, want %+v", got, want)
	}
}

// TestGraphChoicesKeepEveryBatteryWhenALabelCollides: battery A is named
// like battery B's mRID. Both stay pickable and no label repeats.
func TestGraphChoicesKeepEveryBatteryWhenALabelCollides(t *testing.T) {
	t.Parallel()

	got := choicesFor(t, []telemetryhistory.SeriesSnapshot{
		soc("M-A", sample(graphEpoch, 1)), soc("M-B", sample(graphEpoch, 2)),
	}, registry.Entry{MRID: "M-A", Name: "M-B"}, registry.Entry{MRID: "M-B", Name: "Bee"})
	ids := map[string]string{}
	for _, c := range got {
		ids[c.ID] = c.Label
	}
	if len(got) != 2 || ids["M-A"] == "" || ids["M-B"] == "" {
		t.Fatalf("choices = %+v, want both batteries", got)
	}
	if ids["M-A"] == "M-B" {
		t.Errorf("M-A is labelled %q, which is another battery's mRID", ids["M-A"])
	}
}

// TestGraphChoicesCapAtTheLimit: 300 batteries are cut to the choice limit
// and the page says how many cannot be picked; a selection still charts.
func TestGraphChoicesCapAtTheLimit(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(300)}, time.Unix(graphEpoch+1000, 0))
	g := getGraph(t, s, "")
	if len(g.Choices) != sep2admin.MaxChoices {
		t.Errorf("choices = %d, want %d", len(g.Choices), sep2admin.MaxChoices)
	}
	if !strings.Contains(strings.Join(g.Prose, " "), "44 of 300 batteries are not offered") {
		t.Errorf("prose %q does not say 44 batteries cannot be picked", g.Prose)
	}
	if got := getGraph(t, s, "?sel=M-10").names(); !slices.Equal(got, []string{"M-10"}) {
		t.Errorf("selection over 300 batteries charted %q, want M-10", got)
	}
}

func TestGraphBatteryWithAnUnpickableIDIsStillChartedByDefault(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("{bad id}", sample(graphEpoch, 4)), soc("M-1", sample(graphEpoch, 5)),
	}}, time.Unix(graphEpoch+1, 0))
	g := getGraph(t, s, "")
	if !slices.Equal(g.names(), []string{"{bad id}", "M-1"}) {
		t.Errorf("series = %q, want both batteries", g.names())
	}
	if len(g.Latest.Body.Rows) != 2 || len(g.Choices) != 1 {
		t.Errorf("rows = %d, choices = %d; want 2 rows and the one pickable choice", len(g.Latest.Body.Rows), len(g.Choices))
	}
}

func TestGraphSelectionOfDepartedBatteries(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0))
	if names := getGraph(t, s, "?sel=GONE-1&sel=GONE-2").names(); len(names) != 16 || names[15] != "M-19" {
		t.Errorf("all-departed selection charted %q, want the default 16 newest", names)
	}
	if got := getGraph(t, s, "?sel=GONE-1&sel=M-04").names(); !slices.Equal(got, []string{"M-04"}) {
		t.Errorf("partly-departed selection charted %q, want only M-04", got)
	}
}

func TestGraphRefusesAMalformedSelection(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0))
	var seventeen []string
	for i := 0; i < 17; i++ {
		seventeen = append(seventeen, fmt.Sprintf("sel=M-%02d", i))
	}
	for name, q := range map[string]string{
		"17 ids":      "?" + strings.Join(seventeen, "&"),
		"unknown key": "?other=M-01",
		"bad id":      "?sel=a%20b",
		"duplicate":   "?sel=M-01&sel=M-01",
	} {
		rec := doRequest(t, s.Handler(), http.MethodGet, graphPath("data")+q, "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "M-01") || strings.Contains(rec.Body.String(), "other") {
			t.Errorf("%s: body %q echoes the request", name, rec.Body.String())
		}
	}
}

// TestPickableChoicesNeverRepeatALabel: the series labels socSeries builds
// are unique today, but the choices contract is checked here on its own,
// with two series that share a name.
func TestPickableChoicesNeverRepeatALabel(t *testing.T) {
	t.Parallel()

	got := pickableChoices([]chartSeries{{mrid: "M-1", name: "Twin"}, {mrid: "M-2", name: "Twin"}})
	if err := sep2admin.ValidateChoices(got); err != nil {
		t.Fatalf("ValidateChoices: %v for %+v", err, got)
	}
	ids := []string{}
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"M-1", "M-2"}) {
		t.Errorf("choices = %+v, want both batteries kept", got)
	}
}
