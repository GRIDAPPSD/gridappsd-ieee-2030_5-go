package adminui

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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
	assertColumns(t, table, "Series", "Value (%)", "As of", "Age")
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
	if rows := section(t, d, "Latest state of charge").Body.Rows; len(rows) != 16 {
		t.Errorf("latest rows = %d, want 16 (the charted series)", len(rows))
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
