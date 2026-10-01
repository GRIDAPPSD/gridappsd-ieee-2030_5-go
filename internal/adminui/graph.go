package adminui

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// panelGraphInput charts the input topic: the DERStatus reports the
// bridge's own publisher puts on the bus.
const panelGraphInput = "gridappsd-graph-input"

// socAttribute is the history series attribute that carries battery state
// of charge in percent; telemetryhistory's decoder retains it as received.
const socAttribute = "DERStatus.stateOfChargeStatus"

// chartLimits are the plane's chart bounds. They copy sep2admin's
// exported constants so a test can pass smaller ones and reach the
// Descriptor-wide trim, which the real constants make unreachable.
type chartLimits struct {
	series, seriesPoints, descriptorPoints int
}

var planeChartLimits = chartLimits{
	series:           sep2admin.MaxChartSeries,
	seriesPoints:     sep2admin.MaxChartSeriesPoints,
	descriptorPoints: sep2admin.MaxDescriptorChartPoints,
}

// chartSeries is one line before it is fitted to the limits.
type chartSeries struct {
	name    string
	samples []telemetryhistory.Sample
}

func (c chartSeries) newest() int64 { return c.samples[len(c.samples)-1].At }

// fitChart keeps at most limits.series series, the ones whose newest
// sample is latest, and at most the newest per-series share of the
// Descriptor point budget in each, so the last point of a line is always
// its newest sample. Order among kept series is preserved. The returned
// notes say what was cut.
func fitChart(in []chartSeries, limits chartLimits) ([]chartSeries, []string) {
	var notes []string
	kept := in
	if len(kept) > limits.series {
		order := make([]int, len(kept))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return kept[order[a]].newest() > kept[order[b]].newest() })
		order = order[:limits.series]
		sort.Ints(order)
		fitted := make([]chartSeries, 0, len(order))
		for _, i := range order {
			fitted = append(fitted, kept[i])
		}
		notes = append(notes, fmt.Sprintf("Showing %d of %d series, the ones with the newest samples: a chart holds at most %d series.",
			len(fitted), len(in), limits.series))
		kept = fitted
	}
	if len(kept) == 0 {
		return kept, notes
	}
	per := min(limits.seriesPoints, limits.descriptorPoints/len(kept))
	out := make([]chartSeries, len(kept))
	cut := false
	for i, s := range kept {
		if len(s.samples) > per {
			s.samples = s.samples[len(s.samples)-per:]
			cut = true
		}
		out[i] = s
	}
	if cut {
		notes = append(notes, fmt.Sprintf("Each line shows its newest %d samples: a chart holds at most %d points per series and %d in all.",
			per, limits.seriesPoints, limits.descriptorPoints))
	}
	return out, notes
}

// socSeries turns the history into chart series, one per battery. A
// sample the chart encoder would refuse (an unstamped time, a non-finite
// value, a time not after the previous one) is dropped and counted, so a
// bad history can shorten a line but never fail the panel.
func (s *Server) socSeries() (series []chartSeries, dropped int) {
	snap := s.history.Snapshot()
	names := map[string]int{}
	byMRID := map[string]string{}
	for _, e := range s.registry.Snapshot() {
		byMRID[e.MRID] = e.Name
	}
	var socs []telemetryhistory.SeriesSnapshot
	for _, h := range snap {
		if h.Key.Attribute != socAttribute {
			continue
		}
		socs = append(socs, h)
		if n := byMRID[h.Key.Object]; n != "" {
			names[n]++
		}
	}
	for _, h := range socs {
		// A name two batteries share would draw two lines the operator
		// cannot tell apart, so those fall back to the mRID.
		label := byMRID[h.Key.Object]
		if label == "" || names[label] > 1 {
			label = h.Key.Object
		}
		kept := make([]telemetryhistory.Sample, 0, len(h.Samples))
		for _, p := range h.Samples {
			if p.At <= 0 || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) ||
				(len(kept) > 0 && p.At <= kept[len(kept)-1].At) {
				dropped++
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) > 0 {
			series = append(series, chartSeries{name: label, samples: kept})
		}
	}
	return series, dropped
}

func (s *Server) graphInputView(context.Context) (sep2admin.Descriptor, error) {
	series, dropped := s.socSeries()
	series, notes := fitChart(series, planeChartLimits)
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d samples were not charted: unstamped, non-finite or out of order.", dropped))
	}

	wire := make([]sep2admin.ChartSeries, 0, len(series))
	rows := make([]sep2admin.Row, 0, len(series))
	now := s.now()
	for _, cs := range series {
		points := make([]sep2admin.ChartPoint, 0, len(cs.samples))
		for _, p := range cs.samples {
			points = append(points, sep2admin.ChartPoint{At: time.Unix(p.At, 0), Value: p.Value})
		}
		wire = append(wire, sep2admin.ChartSeries{Name: cs.name, Points: points})
		last := cs.samples[len(cs.samples)-1]
		rows = append(rows, sep2admin.Row{
			text(cs.name),
			text(strconv.FormatFloat(last.Value, 'f', -1, 64)),
			timeCell(time.Unix(last.At, 0)),
			text(age(now, time.Unix(last.At, 0))),
		})
	}

	prose := append([]string{
		"Battery state of charge as reported on the simulation input topic. A line ends at its last report: a quiet battery is either unchanged or gone, and the panel cannot tell which. Read only.",
	}, notes...)
	chart := sep2admin.Section{
		Heading: "State of charge",
		Prose:   prose,
		Empty:   "No state of charge samples yet.",
		Body:    sep2admin.NewChartBody(sep2admin.ChartBody{Unit: "%", Series: wire}),
	}
	latest := tables(table("Latest state of charge", "No state of charge samples yet.",
		[]string{"The newest report for each charted battery, and how long ago it arrived."},
		[]string{"Series", "Value (%)", "As of", "Age"}, rows))
	return descriptor(append([]sep2admin.Section{chart}, latest.Sections...)...), nil
}

// age is how long before now t was, to the second. A report stamped ahead
// of the local clock (the history tolerates a few minutes of skew) reads
// as 0s rather than a negative age.
func age(now, t time.Time) string {
	d := max(now.Sub(t), 0)
	return d.Round(time.Second).String()
}
