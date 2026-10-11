package adminui

import (
	"log"
	"math"
	"net/http"
	"sort"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const (
	// maxActiveControls bounds the rows of one active-controls response.
	maxActiveControls = 500

	activeBusy = "active controls busy, retry shortly"

	sourcePage = "page"
	sourceBus  = "bus"
)

// activeControl is one device's active or scheduled opModTargetW control.
type activeControl struct {
	Name string `json:"name"`
	MRID string `json:"mrid"`
	// Watts is the commanded target; Direction is "discharge" above 0,
	// "charge" below 0 and "hold" at 0.
	Watts     float64 `json:"watts"`
	Direction string  `json:"direction"`
	// Source is "page" for a control this process issued through the watts
	// route (or its 0 W follow-on) and "bus" for any other.
	Source      string `json:"source"`
	State       string `json:"state"`
	StartsAt    int64  `json:"startsAt"`
	EndsAt      int64  `json:"endsAt"`
	SecondsLeft int64  `json:"secondsLeft"`
	// Received is true when the device posted a Received response for the
	// control.
	Received    bool   `json:"received"`
	ControlMRID string `json:"controlMrid"`
}

type activeControlsResponse struct {
	Now int64 `json:"now"`
	// Total counts every active or scheduled control; Truncated says
	// Controls holds only the first maxActiveControls.
	Total     int             `json:"total"`
	Truncated bool            `json:"truncated"`
	Controls  []activeControl `json:"controls"`
}

// handleControlActive answers GET /apps/soc/api/control/active with every
// registered device's active or scheduled opModTargetW control. A control the
// lifecycle sweep has not yet removed after its end is left out by its end
// time, not by its stored status.
//
// The store does not mark who issued a control, so the source is decided by
// the page's send ledger: a control it issued is "page". The ledger lives in
// memory and holds the last maxControlRecords sends, so a page control sent
// before a bridge restart, or pushed out of the ledger, reads as "bus".
func (s *Server) handleControlActive(w http.ResponseWriter, r *http.Request) {
	select {
	case s.outputBuilds <- struct{}{}:
		defer func() { <-s.outputBuilds }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, activeBusy, http.StatusServiceUnavailable)
		return
	}

	now := s.now().Unix()
	page := s.controls.pageControlIDs()
	out := activeControlsResponse{Now: now, Controls: []activeControl{}}
	for _, e := range s.registry.Snapshot() {
		snaps, err := s.control.ControlsFor(r.Context(), e.MRID)
		if err != nil {
			log.Printf("adminui: active controls for %s: %v", e.MRID, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{"active controls unavailable"})
			return
		}
		for _, sn := range snaps {
			row, ok := activeRow(sn, now)
			if !ok {
				continue
			}
			row.Name, row.MRID = e.Name, e.MRID
			row.Source = sourceBus
			if _, ok := page[sn.ID]; ok {
				row.Source = sourcePage
			}
			resps, err := s.control.ResponsesFor(r.Context(), sn.MRID, sn.Start)
			if err != nil {
				log.Printf("adminui: active controls for %s: responses: %v", e.MRID, err)
				writeJSON(w, http.StatusInternalServerError, errorResponse{"active controls unavailable"})
				return
			}
			for _, rs := range resps {
				if rs.Status == sep2.ResponseStatusEventReceived {
					row.Received = true
					break
				}
			}
			out.Total++
			if len(out.Controls) >= maxActiveControls {
				out.Truncated = true
				continue
			}
			out.Controls = append(out.Controls, row)
		}
	}
	sort.SliceStable(out.Controls, func(i, j int) bool {
		a, b := out.Controls[i], out.Controls[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.MRID != b.MRID {
			return a.MRID < b.MRID
		}
		return a.StartsAt < b.StartsAt
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// activeRow turns a stored control into a row when it is an opModTargetW
// control that is scheduled or active and has not ended at now.
func activeRow(sn sep2embed.DERControlSnapshot, now int64) (activeControl, bool) {
	if sn.CurrentStatus != sep2.EventStatusScheduled && sn.CurrentStatus != sep2.EventStatusActive {
		return activeControl{}, false
	}
	if sn.Base == nil || sn.Base.OpModTargetW == nil || sn.Duration == 0 {
		return activeControl{}, false
	}
	end := sn.Start + int64(sn.Duration)
	if end <= now {
		return activeControl{}, false
	}
	p := sn.Base.OpModTargetW
	watts := float64(p.Value) * math.Pow10(int(p.Multiplier))
	if math.IsNaN(watts) || math.IsInf(watts, 0) {
		log.Printf("adminui: active controls: control %s has an unrepresentable target (value %d, multiplier %d)", sn.ID, p.Value, p.Multiplier)
		return activeControl{}, false
	}
	dir := "hold"
	switch {
	case watts > 0:
		dir = "discharge"
	case watts < 0:
		dir = "charge"
	}
	return activeControl{
		Watts: watts, Direction: dir, State: controlStateName(sn.CurrentStatus),
		StartsAt: sn.Start, EndsAt: end, SecondsLeft: end - now, ControlMRID: sn.MRID,
	}, true
}
