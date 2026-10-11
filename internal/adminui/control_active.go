package adminui

import (
	"context"
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

	sourcePage    = "page"
	sourceBus     = "bus"
	sourceUnknown = "unknown"
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
	// route (or its 0 W follow-on), "bus" for one it issued from the bus,
	// and "unknown" when neither in-memory record holds it.
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
	// Total counts every active or scheduled control read; Truncated says
	// Controls holds only the first maxActiveControls in the sorted order.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// SkippedDevices counts registered devices whose controls could not be
	// read, so a short list is not mistaken for a complete one.
	SkippedDevices int             `json:"skippedDevices"`
	Controls       []activeControl `json:"controls"`
}

// handleControlActive answers GET /apps/soc/api/control/active with every
// registered device's active or scheduled opModTargetW control. A control the
// lifecycle sweep has not yet removed after its end is left out by its end
// time, not by its stored status.
//
// The store does not mark who issued a control, so the source comes from two
// in-memory, bounded records: the page's send ledger and the embedded
// server's bus record. A control neither holds, such as one issued before a
// restart, reads "unknown" rather than a guess.
//
// A device whose controls cannot be read is logged and counted in
// SkippedDevices; the route fails only when no device could be read.
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
	devices := s.registry.Snapshot()
	type candidate struct {
		row   activeControl
		since int64
	}
	var rows []candidate
	for _, e := range devices {
		snaps, err := s.control.ControlsFor(r.Context(), e.MRID)
		if err != nil {
			log.Printf("adminui: active controls for %s: %v", e.MRID, err)
			out.SkippedDevices++
			continue
		}
		for _, sn := range snaps {
			row, ok := activeRow(sn, now)
			if !ok {
				continue
			}
			row.Name, row.MRID = e.Name, e.MRID
			row.Source = controlSource(sn.ID, page, s.control.BusIssued)
			rows = append(rows, candidate{row, sn.ResponsesSince()})
		}
	}
	if len(devices) > 0 && out.SkippedDevices == len(devices) {
		writeJSON(w, http.StatusInternalServerError, errorResponse{"active controls unavailable"})
		return
	}

	// Sorted before the cap so the kept rows are the same on every poll, and
	// responses are read only for kept rows.
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].row, rows[j].row
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.MRID != b.MRID {
			return a.MRID < b.MRID
		}
		return a.StartsAt < b.StartsAt
	})
	out.Total = len(rows)
	if len(rows) > maxActiveControls {
		rows = rows[:maxActiveControls]
		out.Truncated = true
	}
	for _, c := range rows {
		received, err := s.receivedFor(r.Context(), c.row.ControlMRID, c.since)
		if err != nil {
			log.Printf("adminui: active controls for %s: responses: %v", c.row.MRID, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{"active controls unavailable"})
			return
		}
		c.row.Received = received
		out.Controls = append(out.Controls, c.row)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// controlSource names who issued the control with the given store id.
func controlSource(id string, page map[string]struct{}, busIssued func(string) bool) string {
	if _, ok := page[id]; ok {
		return sourcePage
	}
	if busIssued(id) {
		return sourceBus
	}
	return sourceUnknown
}

// receivedFor reports whether the device posted a Received response for the
// control since the given Unix second.
func (s *Server) receivedFor(ctx context.Context, controlMRID string, since int64) (bool, error) {
	resps, err := s.control.ResponsesFor(ctx, controlMRID, since)
	if err != nil {
		return false, err
	}
	for _, rs := range resps {
		if rs.Status == sep2.ResponseStatusEventReceived {
			return true, nil
		}
	}
	return false, nil
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
