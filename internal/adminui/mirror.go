package adminui

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// mirrorRoute serves the posted mirror readings as series. It is one of the
// routes the SoC and readings page reads, and by operator decision it takes
// no credential: it keeps the Host allowlist, which stops a web page the
// operator visits from reading it through DNS rebinding, and the GET-only
// gate. Anyone who can reach the admin port can read every series.
const mirrorRoute = "/apps/soc/api/mirror"

// Defaults and ceiling for one mirror response. maxMirrorResponsePoints
// bounds the whole body, not one series, so asking for many series cannot
// multiply the per-series point limit.
//
// The default series count is above the 147 series a 49-device fleet
// produces, and the default point count is the most that many series can
// carry inside the response bound.
const (
	defaultMirrorSeries     = 200
	defaultMirrorPoints     = 250
	maxMirrorResponsePoints = 50000
)

// maxMirrorBuilds is how many series builds may run at once. The route takes
// no credential, so a burst of polls is bounded here rather than left to
// multiply the cost of one; the rest are refused with mirrorBusy.
const (
	maxMirrorBuilds = 4
	mirrorBusy      = "mirror readings busy, retry shortly"
)

// MirrorSource is the read surface Server needs from *sep2embed.Embed for
// the mirror reading series.
type MirrorSource interface {
	MirrorSeries(ctx context.Context, q sep2embed.MirrorQuery) (sep2embed.MirrorSeriesResult, error)
}

type mirrorPointResponse struct {
	// ID is stable across polls, so a page that resumes from an inclusive
	// since drops the points whose ID it already holds.
	ID string  `json:"id"`
	T  int64   `json:"t"`
	V  float64 `json:"v"`
}

type mirrorSeriesResponse struct {
	DeviceLFDI string `json:"deviceLfdi"`
	// Mirror is the mirror id and is set only when the mirror names no
	// device LFDI.
	Mirror string `json:"mirror,omitempty"`

	// Registered is true when the registry holds a device with this LFDI;
	// MRID and Name are then its identity. An unregistered device is still
	// listed, by its LFDI alone.
	Registered bool   `json:"registered"`
	MRID       string `json:"mrid,omitempty"`
	Name       string `json:"name,omitempty"`

	Uom         uint8  `json:"uom"`
	Unit        string `json:"unit"`
	Kind        uint8  `json:"kind"`
	Phase       uint8  `json:"phase"`
	Description string `json:"description"`

	// Total counts every point that matched, Truncated says Points holds
	// only the newest of them.
	Total     int                   `json:"total"`
	Truncated bool                  `json:"truncated"`
	Points    []mirrorPointResponse `json:"points"`
}

type mirrorResponse struct {
	// Now is the server clock in Unix seconds read before the series were
	// built, so a poller sets its next since to it, without trusting its own
	// clock, and cannot skip a reading stored while the answer was built. The
	// poll after it repeats the points stamped at Now; drop them by ID.
	Now             int64                  `json:"now"`
	Since           int64                  `json:"since"`
	TotalSeries     int                    `json:"totalSeries"`
	SeriesTruncated bool                   `json:"seriesTruncated"`
	Series          []mirrorSeriesResponse `json:"series"`
}

// uomUnit names the units the clients post. Any other code is reported as
// "uom <n>" rather than guessed.
func uomUnit(uom uint8) string {
	switch uom {
	case 29:
		return "V"
	case 38:
		return "W"
	case 63:
		return "var"
	}
	return fmt.Sprintf("uom %d", uom)
}

// handleMirror answers GET /apps/soc/api/mirror?since=<unix s>&series=<n>&points=<n>&device=<lfdi>&uom=<n>.
// since is optional and inclusive; series and points lower the defaults and
// cannot raise the ceilings. device keeps one device's series (its LFDI, or
// "mirror:<id>" for a mirror with none) and uom one unit code, so a series
// past the series cap can still be read. A numeric parameter that is not a
// non-negative integer is a 400, never a silent default.
func (s *Server) handleMirror(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := queryInt(q.Get("since"), 0)
	if err != nil {
		http.Error(w, "since: "+err.Error(), http.StatusBadRequest)
		return
	}
	maxSeries, err := queryInt(q.Get("series"), defaultMirrorSeries)
	if err != nil {
		http.Error(w, "series: "+err.Error(), http.StatusBadRequest)
		return
	}
	maxPoints, err := queryInt(q.Get("points"), defaultMirrorPoints)
	if err != nil {
		http.Error(w, "points: "+err.Error(), http.StatusBadRequest)
		return
	}
	uom, err := queryInt(q.Get("uom"), -1)
	if err != nil || uom > math.MaxUint8 {
		http.Error(w, "uom: must be an integer from 0 to 255", http.StatusBadRequest)
		return
	}
	if maxSeries == 0 {
		maxSeries = defaultMirrorSeries
	}
	if maxPoints == 0 {
		maxPoints = defaultMirrorPoints
	}
	maxSeries = min(maxSeries, sep2embed.MaxMirrorSeries)
	maxPoints = min(maxPoints, sep2embed.MaxMirrorPointsSeries, max(1, maxMirrorResponsePoints/maxSeries))

	query := sep2embed.MirrorQuery{Since: int64(since), Device: q.Get("device"), MaxSeries: maxSeries, MaxPoints: maxPoints}
	if uom >= 0 {
		u := uint8(uom)
		query.Uom = &u
	}

	select {
	case s.mirrorBuilds <- struct{}{}:
		defer func() { <-s.mirrorBuilds }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, mirrorBusy, http.StatusServiceUnavailable)
		return
	}

	// Taken before the read: a reading stored while the series are built is
	// stamped no earlier than this second, so a poll resuming at it finds it.
	now := s.now().Unix()
	res, err := s.mirror.MirrorSeries(r.Context(), query)
	if err != nil {
		log.Printf("adminui: mirror series: %v", err)
		http.Error(w, "mirror readings unavailable", http.StatusInternalServerError)
		return
	}

	byLFDI := map[string]registryEntryResponse{}
	for _, e := range s.registry.Snapshot() {
		if e.LFDI != "" {
			byLFDI[strings.ToUpper(e.LFDI)] = registryEntryResponse{MRID: e.MRID, Name: e.Name}
		}
	}

	out := mirrorResponse{
		Now:             now,
		Since:           int64(since),
		TotalSeries:     res.TotalSeries,
		SeriesTruncated: res.SeriesTruncated,
		Series:          make([]mirrorSeriesResponse, 0, len(res.Series)),
	}
	for _, ms := range res.Series {
		sr := mirrorSeriesResponse{
			DeviceLFDI:  ms.DeviceLFDI,
			Mirror:      ms.Mirror,
			Uom:         ms.Uom,
			Unit:        uomUnit(ms.Uom),
			Kind:        ms.Kind,
			Phase:       ms.Phase,
			Description: ms.Description,
			Total:       ms.Total,
			Truncated:   ms.Truncated,
			Points:      make([]mirrorPointResponse, 0, len(ms.Points)),
		}
		if e, ok := byLFDI[strings.ToUpper(ms.DeviceLFDI)]; ok {
			sr.Registered, sr.MRID, sr.Name = true, e.MRID, e.Name
		}
		for _, p := range ms.Points {
			sr.Points = append(sr.Points, mirrorPointResponse{ID: p.ID, T: p.Time, V: p.Value})
		}
		out.Series = append(out.Series, sr)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// queryInt parses an optional non-negative integer query value.
func queryInt(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("must be a non-negative integer, got %q", raw)
	}
	return v, nil
}
