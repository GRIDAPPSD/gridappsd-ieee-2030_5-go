package adminui

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// panelGraphInput charts device status: the DERStatus reports the
// bridge's own publisher puts on the application output topic, plus
// commanded setpoints from the control path.
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
	// mrid is the battery's identity, the key a picker selection carries;
	// name is only its label.
	mrid    string
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
			series = append(series, chartSeries{mrid: h.Key.Object, name: label, samples: kept})
		}
	}
	return series, dropped
}

func (s *Server) graphInputView(context.Context) (sep2admin.Descriptor, error) {
	return s.graphDescriptor(nil), nil
}

// graphChoices offers every battery with a chartable series. A battery
// whose mRID the plane's selection pattern refuses is left out rather than
// failing the whole panel with a 500; the default view still charts it.
// Labels are the series labels, unique unless a name collides with another
// battery's mRID, and then the later one is left out.
func (s *Server) graphChoices(context.Context) ([]sep2admin.Choice, error) {
	series, _ := s.socSeries()
	choices := make([]sep2admin.Choice, 0, len(series))
	seen := map[string]bool{}
	for _, cs := range series {
		label := cs.name
		if utf8.RuneCountInString(label) > sep2admin.MaxChoiceLabel {
			label = cs.mrid
		}
		if sep2admin.ValidateSelectionIDs([]string{cs.mrid}) != nil || seen[label] {
			continue
		}
		seen[label] = true
		choices = append(choices, sep2admin.Choice{ID: cs.mrid, Label: label})
	}
	sort.SliceStable(choices, func(a, b int) bool { return choices[a].Label < choices[b].Label })
	if len(choices) > sep2admin.MaxChoices {
		choices = choices[:sep2admin.MaxChoices]
	}
	return choices, nil
}

func (s *Server) graphSelectView(_ context.Context, sel sep2admin.Selection) (sep2admin.Descriptor, error) {
	return s.graphDescriptor(sel.IDs()), nil
}

// graphDescriptor charts the batteries in selected, or the newest ones
// when it is empty. The latest table always lists every battery, so the
// operator can see what there is to pick, and marks the charted ones.
func (s *Server) graphDescriptor(selected []string) sep2admin.Descriptor {
	all, dropped := s.socSeries()
	pool := all
	if len(selected) > 0 {
		want := make(map[string]bool, len(selected))
		for _, id := range selected {
			want[id] = true
		}
		pool = nil
		for _, cs := range all {
			if want[cs.mrid] {
				pool = append(pool, cs)
			}
		}
	}
	series, notes := fitChart(pool, planeChartLimits)
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d samples were not charted: unstamped, non-finite or out of order.", dropped))
	}

	charted := make(map[string]bool, len(series))
	wire := make([]sep2admin.ChartSeries, 0, len(series))
	for _, cs := range series {
		charted[cs.mrid] = true
		points := make([]sep2admin.ChartPoint, 0, len(cs.samples))
		for _, p := range cs.samples {
			points = append(points, sep2admin.ChartPoint{At: time.Unix(p.At, 0), Value: p.Value})
		}
		wire = append(wire, sep2admin.ChartSeries{Name: cs.name, Points: points})
	}
	rows := make([]sep2admin.Row, 0, len(all))
	now := s.now()
	for _, cs := range all {
		last := cs.samples[len(cs.samples)-1]
		mark := "No"
		if charted[cs.mrid] {
			mark = "Yes"
		}
		rows = append(rows, sep2admin.Row{
			text(cs.name),
			text(strconv.FormatFloat(last.Value, 'f', -1, 64)),
			timeCell(time.Unix(last.At, 0)),
			text(age(now, time.Unix(last.At, 0))),
			text(mark),
		})
	}

	prose := append([]string{
		"Battery state of charge as the bridge last published it. A line ends at its last report: a quiet battery is either unchanged or gone, and the panel cannot tell which. Read only.",
	}, notes...)
	chart := sep2admin.Section{
		Heading: "State of charge",
		Prose:   prose,
		Empty:   "No state of charge samples yet.",
		Body:    sep2admin.NewChartBody(sep2admin.ChartBody{Unit: "%", Series: wire}),
	}
	latest := tables(table("Latest state of charge", "No state of charge samples yet.",
		[]string{"The newest report for each battery, how long ago it arrived, and whether the chart shows it."},
		[]string{"Series", "Value (%)", "As of", "Age", "Charted"}, rows))
	return descriptor(append([]sep2admin.Section{chart}, latest.Sections...)...)
}

// age is how long before now t was, to the second. A report stamped ahead
// of the local clock (the history tolerates a few minutes of skew) reads
// as 0s rather than a negative age.
func age(now, t time.Time) string {
	d := max(now.Sub(t), 0)
	return d.Round(time.Second).String()
}
