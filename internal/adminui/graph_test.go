package adminui

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

type wireChartSeries struct {
	Name   string       `json:"name"`
	Points [][2]float64 `json:"points"`
}

type wireChartSection struct {
	Kind    string   `json:"kind"`
	Heading string   `json:"heading"`
	Prose   []string `json:"prose"`
	Empty   string   `json:"empty"`
	Body    struct {
		Unit   string            `json:"unit"`
		Series []wireChartSeries `json:"series"`
	} `json:"body"`
}

type wireChartDescriptor struct {
	Sections []wireChartSection `json:"sections"`
}

// fakeHistory is a HistorySource that returns what a test preloaded, so a
// test can serve samples the real store would have refused.
type fakeHistory struct {
	snap []telemetryhistory.SeriesSnapshot
}

func (f *fakeHistory) Snapshot() []telemetryhistory.SeriesSnapshot { return f.snap }

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

func getGraph(t *testing.T, s *Server) (chart wireChartSection, d wireDescriptor) {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelGraphInput, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET panel %s: %d %s", panelGraphInput, rec.Code, rec.Body.String())
	}
	var cd wireChartDescriptor
	decodeJSON(t, rec.Body.Bytes(), &cd)
	decodeJSON(t, rec.Body.Bytes(), &d)
	for _, sec := range cd.Sections {
		if sec.Kind == "chart" {
			return sec, d
		}
	}
	t.Fatalf("no chart section in %s", rec.Body.String())
	return
}

// TestGraphInputPanelChartsTwoBatteriesWithALatestTable is the issue's
// first done-when line: one chart, one series per battery, in percent,
// and a table of each series' newest value and its age.
func TestGraphInputPanelChartsTwoBatteriesWithALatestTable(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("m-1", sample(graphEpoch, 65.5), sample(graphEpoch+15, 64)),
		soc("m-2", sample(graphEpoch+5, 30)),
		{Key: telemetryhistory.SeriesKey{Object: "m-1", Attribute: "DERControl.DERControlBase.opModTargetW"},
			Samples: []telemetryhistory.Sample{sample(graphEpoch, 5000)}},
	}}
	now := time.Unix(graphEpoch+75, 0)
	s := graphServer(t, h, now, registry.Entry{MRID: "m-1", Name: "Battery One"})
	chart, d := getGraph(t, s)

	if chart.Body.Unit != "%" {
		t.Errorf("unit = %q, want %%", chart.Body.Unit)
	}
	want := []wireChartSeries{
		{Name: "Battery One", Points: [][2]float64{{float64(graphEpoch) * 1000, 65.5}, {float64(graphEpoch+15) * 1000, 64}}},
		{Name: "m-2", Points: [][2]float64{{float64(graphEpoch+5) * 1000, 30}}},
	}
	if len(chart.Body.Series) != len(want) {
		t.Fatalf("series = %+v, want %+v (the setpoint series must not be charted)", chart.Body.Series, want)
	}
	for i, w := range want {
		g := chart.Body.Series[i]
		if g.Name != w.Name || !slices.Equal(g.Points, w.Points) {
			t.Errorf("series[%d] = %+v, want %+v", i, g, w)
		}
	}

	table := section(t, d, "Latest state of charge")
	assertColumns(t, table, "Series", "Value (%)", "As of", "Age", "Charted")
	if len(table.Body.Rows) != 2 {
		t.Fatalf("latest rows = %d, want 2", len(table.Body.Rows))
	}
	r0, r1 := table.Body.Rows[0], table.Body.Rows[1]
	if got := texts([]wireCell{r0[0], r0[1], r0[3]}); !slices.Equal(got, []string{"Battery One", "64", "1m0s"}) {
		t.Errorf("row 0 = %q, want Battery One 64 1m0s", got)
	}
	if got := texts([]wireCell{r1[0], r1[1], r1[3]}); !slices.Equal(got, []string{"m-2", "30", "1m10s"}) {
		t.Errorf("row 1 = %q, want m-2 30 1m10s", got)
	}
	if r0[2].Kind != "time" || r0[2].DateTime != time.Unix(graphEpoch+15, 0).UTC().Format(time.RFC3339) {
		t.Errorf("row 0 as-of = %+v, want a time cell at the newest sample", r0[2])
	}
}

// TestGraphInputPanelServesTheNewest720 is the second done-when line,
// through the real store: 2000 appended samples leave 1440, of which the
// panel serves the newest 720.
func TestGraphInputPanelServesTheNewest720(t *testing.T) {
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
	chart, _ := getGraph(t, s)

	pts := chart.Body.Series[0].Points
	if len(pts) != 720 {
		t.Fatalf("points = %d, want 720", len(pts))
	}
	if last := pts[len(pts)-1]; last != [2]float64{float64(base+1999) * 1000, 1999} {
		t.Errorf("last point = %v, want the newest sample %v", last, [2]float64{float64(base+1999) * 1000, 1999})
	}
	if first := pts[0]; first[1] != 1280 {
		t.Errorf("first point value = %v, want 1280 (the 720th newest)", first[1])
	}
	for i := 1; i < len(pts); i++ {
		if pts[i][0] <= pts[i-1][0] {
			t.Fatalf("points[%d] time %v not after %v", i, pts[i][0], pts[i-1][0])
		}
	}
	if !strings.Contains(strings.Join(chart.Prose, " "), "newest 720 samples") {
		t.Errorf("prose %q does not say the line was cut to 720", chart.Prose)
	}
}

func TestGraphInputPanelAnswers401WithoutACredential(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Now())
	for _, auth := range []string{"", "Bearer wrong-key-wrong-key"} {
		rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelGraphInput, auth, "localhost")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: %d, want 401", auth, rec.Code)
		}
	}
}

// TestGraphInputPanelDropsSamplesTheChartRefuses: the store refuses these
// at ingest, so a fake source stands in for a store that did not.
func TestGraphInputPanelDropsSamplesTheChartRefuses(t *testing.T) {
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
	chart, d := getGraph(t, s)

	if len(chart.Body.Series) != 1 || chart.Body.Series[0].Name != "m-1" {
		t.Fatalf("series = %+v, want only m-1 (m-2 has no chartable sample)", chart.Body.Series)
	}
	want := [][2]float64{{float64(graphEpoch+1) * 1000, 51}, {float64(graphEpoch+2) * 1000, 54}}
	if got := chart.Body.Series[0].Points; !slices.Equal(got, want) {
		t.Errorf("points = %v, want %v", got, want)
	}
	if !strings.Contains(strings.Join(chart.Prose, " "), "5 samples were not charted") {
		t.Errorf("prose %q does not count the 5 dropped samples", chart.Prose)
	}
	if rows := section(t, d, "Latest state of charge").Body.Rows; len(rows) != 1 {
		t.Errorf("latest rows = %d, want 1", len(rows))
	}
}

func TestGraphInputPanelLabelsSeries(t *testing.T) {
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
	chart, _ := getGraph(t, s)

	var got []string
	for _, ser := range chart.Body.Series {
		got = append(got, ser.Name)
	}
	// Shared names fall back to the mRID, an empty name falls back too, and
	// a name is passed through as data for the shell to render as text.
	if want := []string{"m-1", "m-2", "<img src=x onerror=alert(1)>", "m-4"}; !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}

func TestGraphInputPanelWithNoSamplesSaysSo(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Now())
	chart, d := getGraph(t, s)
	if len(chart.Body.Series) != 0 || chart.Empty != "No state of charge samples yet." {
		t.Errorf("chart = %+v, want no series and the empty text", chart)
	}
	if rows := section(t, d, "Latest state of charge").Body.Rows; len(rows) != 0 {
		t.Errorf("latest rows = %d, want 0", len(rows))
	}
}

func TestGraphInputPanelAgeNeverReadsNegative(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{soc("m-1", sample(graphEpoch+120, 1))}}
	s := graphServer(t, h, time.Unix(graphEpoch, 0))
	_, d := getGraph(t, s)
	if got := section(t, d, "Latest state of charge").Body.Rows[0][3].Text; got != "0s" {
		t.Errorf("age of a report stamped ahead of the clock = %q, want 0s", got)
	}
}

// TestGraphInputPanelNeverFailsOnSeriesCount: 40 batteries, each with 1440
// samples, is over the series limit and, untrimmed, over the Descriptor
// point limit. The plane would answer 500 to either.
func TestGraphInputPanelNeverFailsOnSeriesCount(t *testing.T) {
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
	chart, d := getGraph(t, s)

	if len(chart.Body.Series) != 16 {
		t.Fatalf("series = %d, want 16", len(chart.Body.Series))
	}
	// The 16 with the newest samples are batteries 0..15 here (fewest
	// samples dropped from the tail), in their original order.
	for i, ser := range chart.Body.Series {
		if want := fmt.Sprintf("m-%02d", i); ser.Name != want {
			t.Errorf("series[%d] = %q, want %q", i, ser.Name, want)
		}
	}
	if !strings.Contains(strings.Join(chart.Prose, " "), "Showing 16 of 40 series") {
		t.Errorf("prose %q does not say 24 series were cut", chart.Prose)
	}
	rows := section(t, d, "Latest state of charge").Body.Rows
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

func chartNames(c wireChartSection) []string {
	var out []string
	for _, s := range c.Body.Series {
		out = append(out, s.Name)
	}
	return out
}

func getSelected(t *testing.T, s *Server, query string) (wireChartSection, wireDescriptor) {
	t.Helper()
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelGraphInput+query, "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", query, rec.Code, rec.Body.String())
	}
	var cd wireChartDescriptor
	var d wireDescriptor
	decodeJSON(t, rec.Body.Bytes(), &cd)
	decodeJSON(t, rec.Body.Bytes(), &d)
	for _, sec := range cd.Sections {
		if sec.Kind == "chart" {
			return sec, d
		}
	}
	t.Fatalf("no chart in %s", rec.Body.String())
	return wireChartSection{}, d
}

// TestGraphChoicesPassTheServersValidatorForTheLiveShape: upper-case GUID
// mRIDs, a name two batteries share, a name over the label limit and an
// mRID outside the id pattern. The plane answers 500 to a choice list
// ValidateChoices refuses, so every shape must come out valid.
func TestGraphChoicesPassTheServersValidatorForTheLiveShape(t *testing.T) {
	t.Parallel()

	guid1 := "3F2504E0-4F89-11D3-9A0C-0305E82C3301"
	guid2 := "3F2504E0-4F89-11D3-9A0C-0305E82C3302"
	guid3 := "3F2504E0-4F89-11D3-9A0C-0305E82C3303"
	h := &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc(guid1, sample(graphEpoch, 1)),
		soc(guid2, sample(graphEpoch, 2)),
		soc(guid3, sample(graphEpoch, 3)),
		soc("{bad id}", sample(graphEpoch, 4)),
	}}
	s := graphServer(t, h, time.Unix(graphEpoch+1, 0),
		registry.Entry{MRID: guid1, Name: "Twin"},
		registry.Entry{MRID: guid2, Name: "Twin"},
		registry.Entry{MRID: guid3, Name: strings.Repeat("n", 129)})
	got, err := s.graphChoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sep2admin.ValidateChoices(got); err != nil {
		t.Fatalf("ValidateChoices: %v for %+v", err, got)
	}
	want := []sep2admin.Choice{{ID: guid1, Label: guid1}, {ID: guid2, Label: guid2}, {ID: guid3, Label: guid3}}
	if !slices.Equal(got, want) {
		t.Errorf("choices = %+v, want %+v (shared and overlong names fall back to the mRID, the bad id is left out)", got, want)
	}
}

func TestGraphSelectChartsExactlyTheSelectedBatteries(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0))
	chart, d := getSelected(t, s, "?sel=M-01&sel=M-05&sel=M-03")
	if got, want := chartNames(chart), []string{"M-01", "M-03", "M-05"}; !slices.Equal(got, want) {
		t.Errorf("series = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(chart.Prose, " "), "Showing") {
		t.Errorf("prose %q carries a cut note for a selection that fits", chart.Prose)
	}
	rows := section(t, d, "Latest state of charge").Body.Rows
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
	chart, d := getSelected(t, s, "")
	names := chartNames(chart)
	if len(names) != 16 || names[0] != "M-33" || names[15] != "M-48" {
		t.Errorf("series = %q, want M-33..M-48", names)
	}
	rows := section(t, d, "Latest state of charge").Body.Rows
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
// panel; the table says what it cut instead of the plane answering 500.
func TestGraphLatestTableTrimsPastTheRowCap(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(1200)}, time.Unix(graphEpoch+2000, 0))
	_, d := getSelected(t, s, "")
	sec := section(t, d, "Latest state of charge")
	if len(sec.Body.Rows) != 1000 {
		t.Errorf("rows = %d, want the cap of 1000", len(sec.Body.Rows))
	}
	if !strings.Contains(strings.Join(sec.Prose, " "), "Showing 1000 of 1200 rows") {
		t.Errorf("prose %q carries no trim notice", sec.Prose)
	}
}

func TestGraphEmptyRegistryAndHistoryGiveNoChoicesAndTheDefaultView(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{}, time.Unix(graphEpoch, 0))
	choices, err := s.graphChoices(context.Background())
	if err != nil || len(choices) != 0 {
		t.Fatalf("choices = %+v, %v; want none and no error", choices, err)
	}
	chart, d := getSelected(t, s, "")
	if len(chart.Body.Series) != 0 || chart.Empty != "No state of charge samples yet." {
		t.Errorf("chart = %+v, want the empty default view", chart)
	}
	if rows := section(t, d, "Latest state of charge").Body.Rows; len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// TestGraphPickerThroughThePlane drives the plane's own routes: the list
// entry, the choices route, and a selection. A battery that leaves the
// history is gone from the choices on the next request.
func TestGraphPickerThroughThePlane(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: pickerHistory(20)}
	s := graphServer(t, h, time.Unix(graphEpoch+100, 0), registry.Entry{MRID: "M-02", Name: "Two"})
	get := func(path string) []byte {
		t.Helper()
		rec := doRequest(t, s.Handler(), http.MethodGet, path, "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}

	var list []struct {
		ID     string `json:"id"`
		Picker *struct {
			Max int `json:"max"`
		} `json:"picker"`
	}
	decodeJSON(t, get("/api/ui/panels"), &list)
	for _, e := range list {
		hasPicker := e.Picker != nil
		if hasPicker != (e.ID == panelGraphInput) || (hasPicker && e.Picker.Max != 16) {
			t.Errorf("panel %s picker = %+v, want max 16 on %s only", e.ID, e.Picker, panelGraphInput)
		}
	}

	type choicesBody struct {
		Max     int `json:"max"`
		Choices []struct{ ID, Label string }
	}
	var cb choicesBody
	decodeJSON(t, get("/api/ui/panels/"+panelGraphInput+"/choices"), &cb)
	if cb.Max != 16 || len(cb.Choices) != 20 {
		t.Fatalf("choices = max %d, %d entries; want 16 and 20", cb.Max, len(cb.Choices))
	}
	last := cb.Choices[len(cb.Choices)-1]
	if cb.Choices[0].ID != "M-00" || last.ID != "M-02" || last.Label != "Two" {
		t.Errorf("choices = %+v ... %+v, want sorted by label with M-02 named Two last", cb.Choices[0], last)
	}

	var cd wireChartDescriptor
	decodeJSON(t, get("/api/ui/panels/"+panelGraphInput+"?sel=M-07&sel=M-19"), &cd)
	if got := chartNames(cd.Sections[0]); !slices.Equal(got, []string{"M-07", "M-19"}) {
		t.Errorf("selection chart = %q, want M-07 M-19", got)
	}

	h.snap = h.snap[:19]
	decodeJSON(t, get("/api/ui/panels/"+panelGraphInput+"/choices"), &cb)
	for _, c := range cb.Choices {
		if c.ID == "M-19" {
			t.Errorf("M-19 still offered after it left the history")
		}
	}
}

// choiceFor serves one battery named name and returns its choice.
func choicesFor(t *testing.T, snap []telemetryhistory.SeriesSnapshot, names ...registry.Entry) []sep2admin.Choice {
	t.Helper()
	s := graphServer(t, &fakeHistory{snap: snap}, time.Unix(graphEpoch+10, 0), names...)
	got, err := s.graphChoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sep2admin.ValidateChoices(got); err != nil {
		t.Fatalf("ValidateChoices: %v for %+v", err, got)
	}
	return got
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

// TestGraphChoicesCapAtTheServerLimit: 300 batteries would make the
// plane answer 500 to the choices route and to every selection.
func TestGraphChoicesCapAtTheServerLimit(t *testing.T) {
	t.Parallel()

	h := &fakeHistory{snap: pickerHistory(300)}
	s := graphServer(t, h, time.Unix(graphEpoch+1000, 0))
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelGraphInput+"/choices", "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("choices: %d %s", rec.Code, rec.Body.String())
	}
	var cb struct{ Choices []struct{ ID, Label string } }
	decodeJSON(t, rec.Body.Bytes(), &cb)
	if len(cb.Choices) != sep2admin.MaxChoices {
		t.Errorf("choices = %d, want %d", len(cb.Choices), sep2admin.MaxChoices)
	}
	chart, _ := getSelected(t, s, "?sel=M-10")
	if got := chartNames(chart); !slices.Equal(got, []string{"M-10"}) {
		t.Errorf("selection over 300 batteries charted %q, want M-10", got)
	}
	prose := strings.Join(getGraphChart(t, s).Prose, " ")
	if !strings.Contains(prose, "44 of 300 batteries are not offered") {
		t.Errorf("prose %q does not say 44 batteries cannot be picked", prose)
	}
}

func getGraphChart(t *testing.T, s *Server) wireChartSection {
	t.Helper()
	c, _ := getSelected(t, s, "")
	return c
}

func TestGraphBatteryWithAnUnpickableIDIsStillChartedByDefault(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: []telemetryhistory.SeriesSnapshot{
		soc("{bad id}", sample(graphEpoch, 4)), soc("M-1", sample(graphEpoch, 5)),
	}}, time.Unix(graphEpoch+1, 0))
	chart, d := getSelected(t, s, "")
	if got := chartNames(chart); !slices.Equal(got, []string{"{bad id}", "M-1"}) {
		t.Errorf("series = %q, want both batteries", got)
	}
	if rows := section(t, d, "Latest state of charge").Body.Rows; len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
}

func TestGraphSelectionOfDepartedBatteries(t *testing.T) {
	t.Parallel()

	s := graphServer(t, &fakeHistory{snap: pickerHistory(20)}, time.Unix(graphEpoch+100, 0))
	chart, _ := getSelected(t, s, "?sel=GONE-1&sel=GONE-2")
	if names := chartNames(chart); len(names) != 16 || names[15] != "M-19" {
		t.Errorf("all-departed selection charted %q, want the default 16 newest", names)
	}
	chart, _ = getSelected(t, s, "?sel=GONE-1&sel=M-04")
	if got := chartNames(chart); !slices.Equal(got, []string{"M-04"}) {
		t.Errorf("partly-departed selection charted %q, want only M-04", got)
	}
}

func TestGraphPickerRefusesAMalformedSelection(t *testing.T) {
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
		rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels/"+panelGraphInput+q, "Bearer "+testKey, "localhost")
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
