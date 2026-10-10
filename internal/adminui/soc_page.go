package adminui

import (
	"io/fs"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/socpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// Routes of the SoC and readings page. Like the mirror and SoC routes they
// take no credential by decision and keep the Host allowlist. Every pattern
// is exact, so the plane keeps any other path under /apps.
const (
	socPageRoot     = "/apps/soc"
	socPageIndex    = "/apps/soc/{$}"
	socPageScript   = "/apps/soc/app.js"
	socPageStyle    = "/apps/soc/style.css"
	socPageUPlotJS  = "/apps/soc/uplot.min.js"
	socPageUPlotCSS = "/apps/soc/uplot.min.css"
	devicesRoute    = "/apps/soc/api/devices"
	outputRoute     = "/apps/soc/api/output"
	reportedPrefix  = "DERStatus."
)

// The page loads only its own files and talks only to its own origin. The
// route takes no credential, so these headers are what keep a page
// the operator opens from framing it or running a script from elsewhere.
const socPageCSP = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Defaults and ceilings for one output response, with the same meaning as
// the mirror's: maxOutputResponsePoints bounds the whole body. A 49-device
// fleet reports 7 DERStatus attributes each, 343 series; the default holds
// them and the points default is what that many series can carry inside the
// body bound. The history store below holds twice the fleet's series.
const (
	defaultOutputSeries     = 500
	defaultOutputPoints     = 100
	maxOutputSeries         = 1000
	maxOutputPointsSeries   = 2000
	maxOutputResponsePoints = 50000

	maxOutputBuilds = 4
	outputBusy      = "output readings busy, retry shortly"
)

var outputLimits = seriesLimits{
	defSeries: defaultOutputSeries, defPoints: defaultOutputPoints,
	maxSeries: maxOutputSeries, maxSeriesPoints: maxOutputPointsSeries,
	maxResponsePoints: maxOutputResponsePoints,
}

// socHeaders sets the page's security headers before next runs, so error
// responses carry them too.
func socHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", socPageCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// socGetGate is the chain every /apps/soc route shares: headers outermost,
// then the Host allowlist, then GET only.
func (s *Server) socGetGate(next http.Handler) http.Handler {
	return socHeaders(s.hostAllowlist(requireGET(next)))
}

// mountSoCPage mounts the page, its four assets (the script and style, and
// the vendored chart library's script and style) and the devices and output
// routes.
func (s *Server) mountSoCPage(mux *http.ServeMux) {
	mux.Handle(devicesRoute, s.socGetGate(http.HandlerFunc(s.handleDevices)))
	mux.Handle(outputRoute, s.socGetGate(http.HandlerFunc(s.handleOutput)))
	mux.Handle(socPageIndex, s.socGetGate(staticFile("index.html", "text/html; charset=utf-8")))
	mux.Handle(socPageScript, s.socGetGate(staticFile("app.js", "text/javascript; charset=utf-8")))
	mux.Handle(socPageStyle, s.socGetGate(staticFile("style.css", "text/css; charset=utf-8")))
	mux.Handle(socPageUPlotJS, s.socGetGate(staticFile("uplot.min.js", "text/javascript; charset=utf-8")))
	mux.Handle(socPageUPlotCSS, s.socGetGate(staticFile("uplot.min.css", "text/css; charset=utf-8")))
	mux.Handle(socPageRoot, s.socGetGate(http.RedirectHandler(socPageRoot+"/", http.StatusTemporaryRedirect)))
}

// staticFile serves one embedded file. It is read per request: the files are
// small and the embed cannot change.
func staticFile(name, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, err := fs.ReadFile(socpage.Files, name)
		if err != nil {
			log.Printf("adminui: page file %s: %v", name, err)
			http.Error(w, "page file unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		// The status is sent; a failed write is the client leaving.
		_, _ = w.Write(b)
	})
}

type deviceResponse struct {
	MRID string `json:"mrid"`
	Name string `json:"name"`
}

// handleDevices answers GET /apps/soc/api/devices with the registered
// devices by name. Only the two fields the page needs are served; the
// LFDI, SFDI and the placeholder flag stay behind the Bearer-gated registry route.
func (s *Server) handleDevices(w http.ResponseWriter, _ *http.Request) {
	entries := s.registry.Snapshot()
	out := make([]deviceResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, deviceResponse{MRID: e.MRID, Name: e.Name})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].MRID < out[j].MRID
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

type outputPointResponse struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

type outputSeriesResponse struct {
	MRID      string `json:"mrid"`
	Name      string `json:"name,omitempty"`
	Attribute string `json:"attribute"`
	// Total counts the points at or after since; Truncated says Points holds
	// only the newest of them.
	Total     int                   `json:"total"`
	Truncated bool                  `json:"truncated"`
	Points    []outputPointResponse `json:"points"`
}

type outputResponse struct {
	// Now is read before the snapshot, with the mirror route's cursor rule:
	// poll again with since=now and drop the points stamped at now that you
	// already hold.
	Now             int64 `json:"now"`
	Since           int64 `json:"since"`
	TotalSeries     int   `json:"totalSeries"`
	SeriesTruncated bool  `json:"seriesTruncated"`
	// Evictions counts the series the history store has dropped to stay
	// under its cap; non-zero means the page is missing history.
	Evictions uint64                 `json:"evictions"`
	Series    []outputSeriesResponse `json:"series"`
}

// handleOutput answers GET /apps/soc/api/output?since=<unix s>&device=<mrid>&series=<n>&points=<n>.
// It serves the reported-state series the bridge published on its output
// topic (attributes under DERStatus.), never the commanded setpoints held in
// the same store. A series with no point at or after since is left out.
func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, maxSeries, maxPoints, err := queryWindow(q, outputLimits)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	device := q.Get("device")

	select {
	case s.outputBuilds <- struct{}{}:
		defer func() { <-s.outputBuilds }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, outputBusy, http.StatusServiceUnavailable)
		return
	}

	now := s.now().Unix()
	snap := s.history.Snapshot()
	evictions := s.history.Evictions()

	names := map[string]string{}
	for _, e := range s.registry.Snapshot() {
		names[e.MRID] = e.Name
	}

	out := outputResponse{Now: now, Since: int64(since), Evictions: evictions, Series: []outputSeriesResponse{}}
	for _, ss := range snap {
		if !strings.HasPrefix(ss.Key.Attribute, reportedPrefix) {
			continue
		}
		if device != "" && ss.Key.Object != device {
			continue
		}
		pts := pointsSince(ss.Samples, int64(since))
		if len(pts) == 0 {
			continue
		}
		out.TotalSeries++
		if len(out.Series) >= maxSeries {
			out.SeriesTruncated = true
			continue
		}
		sr := outputSeriesResponse{
			MRID:      ss.Key.Object,
			Name:      names[ss.Key.Object],
			Attribute: ss.Key.Attribute,
			Total:     len(pts),
		}
		if len(pts) > maxPoints {
			pts = pts[len(pts)-maxPoints:]
			sr.Truncated = true
		}
		sr.Points = pts
		out.Series = append(out.Series, sr)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// pointsSince returns the samples at or after since as points, oldest first.
// A value that cannot be encoded in JSON is dropped rather than failing the
// response; the store refuses NaN and infinity, so none is expected.
func pointsSince(samples []telemetryhistory.Sample, since int64) []outputPointResponse {
	pts := make([]outputPointResponse, 0, len(samples))
	for _, sm := range samples {
		if sm.At < since || math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		pts = append(pts, outputPointResponse{T: sm.At, V: sm.Value})
	}
	return pts
}
