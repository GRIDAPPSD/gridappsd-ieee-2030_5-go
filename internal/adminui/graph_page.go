package adminui

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/graphpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
)

// graphChoice is one battery the picker offers.
type graphChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// graphLine is one chart line. Each point is [unix seconds, percent].
type graphLine struct {
	MRID   string       `json:"mrid"`
	Name   string       `json:"name"`
	Points [][2]float64 `json:"points"`
}

// graphDoc is the data of the graph page. Choices, Series and Prose are
// arrays, never null, and Latest is a table section in the table pages' shape.
type graphDoc struct {
	Title string `json:"title"`
	Unit  string `json:"unit"`
	// Max is the most batteries one selection may name.
	Max     int           `json:"max"`
	Prose   []string      `json:"prose"`
	Empty   string        `json:"empty"`
	Choices []graphChoice `json:"choices"`
	Series  []graphLine   `json:"series"`
	Latest  pageSection   `json:"latest"`
}

const graphEmpty = "No state of charge samples yet."

// graphSelection reads the page's selection: repeated sel values and no
// other key. The error says what was wrong, never what was sent.
func graphSelection(q url.Values) ([]string, error) {
	for k := range q {
		if k != "sel" {
			return nil, fmt.Errorf("unknown query key")
		}
	}
	ids := q["sel"]
	if err := sep2admin.ValidateSelectionIDs(ids); err != nil {
		return nil, fmt.Errorf("bad selection")
	}
	return ids, nil
}

// graphData charts the batteries in selected, or the newest ones when none
// of them is in the history now. The latest table always lists every
// battery, so the operator can see what there is to pick, and marks the
// charted ones.
func (s *Server) graphData(selected []string) graphDoc {
	all, dropped := s.socSeries()
	pool := all
	if len(selected) > 0 {
		want := make(map[string]bool, len(selected))
		for _, id := range selected {
			want[id] = true
		}
		var picked []chartSeries
		for _, cs := range all {
			if want[cs.mrid] {
				picked = append(picked, cs)
			}
		}
		// A selection of batteries that have all left the history shows the
		// default view rather than an empty chart.
		if len(picked) > 0 {
			pool = picked
		}
	}
	series, notes := fitChart(pool, planeChartLimits)
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d samples were not charted: unstamped, non-finite or out of order.", dropped))
	}
	choices := pickableChoices(all)
	if len(choices) < len(all) {
		notes = append(notes, fmt.Sprintf("%d of %d batteries are not offered in the picker: it holds at most %d, and an id with unusual characters cannot be picked. The default view still charts the newest.",
			len(all)-len(choices), len(all), sep2admin.MaxChoices))
	}

	charted := make(map[string]bool, len(series))
	lines := make([]graphLine, 0, len(series))
	for _, cs := range series {
		charted[cs.mrid] = true
		points := make([][2]float64, 0, len(cs.samples))
		for _, p := range cs.samples {
			points = append(points, [2]float64{float64(p.At), p.Value})
		}
		lines = append(lines, graphLine{MRID: cs.mrid, Name: cs.name, Points: points})
	}
	out := make([]graphChoice, 0, len(choices))
	for _, c := range choices {
		out = append(out, graphChoice{ID: c.ID, Label: c.Label})
	}

	now := s.now()
	rows := make([][]pageCell, 0, len(all))
	for _, cs := range all {
		last := cs.samples[len(cs.samples)-1]
		mark := "No"
		if charted[cs.mrid] {
			mark = "Yes"
		}
		rows = append(rows, []pageCell{
			pText(cs.name),
			pText(strconv.FormatFloat(last.Value, 'f', -1, 64)),
			pTime(time.Unix(last.At, 0)),
			pText(age(now, time.Unix(last.At, 0))),
			pText(mark),
		})
	}
	latest := pageTables("", pageTable("Latest state of charge", graphEmpty,
		[]string{"The newest report for each battery, how long ago it arrived, and whether the chart shows it."},
		[]string{"Series", "Value (%)", "As of", "Age", "Charted"}, rows)).Sections[0]

	prose := append([]string{
		"Battery state of charge as the bridge last published it. A line ends at its last report: a quiet battery is either unchanged or gone, and the panel cannot tell which. Read only.",
	}, notes...)
	return graphDoc{
		Title: "Graph: device status", Unit: "%", Max: sep2admin.MaxSelection,
		Prose: prose, Empty: graphEmpty, Choices: out, Series: lines, Latest: latest,
	}
}

// graphPage is the handler of the graph page panel. The plane mounts it
// under /apps/gridappsd-graph-input/ behind its own authentication and
// strips that prefix. The chart library is served from the SoC page's
// vendored copy, so the repository holds one. A malformed selection is a
// 400 that does not echo the request.
func (s *Server) graphPage() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", pageFile(graphpage.Files, "index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /app.js", pageFile(graphpage.Files, "app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /style.css", pageFile(graphpage.Files, "style.css", "text/css; charset=utf-8"))
	mux.Handle("GET /uplot.min.js", pageFile(socpage.Files, "uplot.min.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /uplot.min.css", pageFile(socpage.Files, "uplot.min.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /data", func(w http.ResponseWriter, r *http.Request) {
		sel, err := graphSelection(r.URL.Query())
		if err != nil {
			log.Printf("adminui: graph page: %v", err)
			http.Error(w, "bad selection", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, s.graphData(sel))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", pageCSP)
		mux.ServeHTTP(w, r)
	})
}
