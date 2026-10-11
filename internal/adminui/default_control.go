package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"mime"
	"net/http"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// defaultSendPattern sets or clears a device's DefaultDERControl
// opModTargetW. It sits outside the Bearer gate like the watts control route
// and shares its Host allowlist, JSON requirement and rate limit.
const defaultSendPattern = "POST /apps/soc/api/default"

const (
	defaultCancelWarning   = "the default is set, but a page control was not cancelled; it holds until its end"
	defaultReadBackWarning = "the default was written but could not be read back"
)

// defaultRequest is the POST body: mrid with watts to set, or mrid with
// clear to restore the fleet default. Watts is a pointer so a missing value
// is told apart from 0, which is a valid default.
type defaultRequest struct {
	MRID  string `json:"mrid"`
	Watts *int64 `json:"watts"`
	Clear bool   `json:"clear"`
}

// defaultResponse is the POST answer. Default is the device's default as
// read back after the write, null when it serves none.
type defaultResponse struct {
	MRID              string   `json:"mrid"`
	Default           *float64 `json:"default"`
	CancelledControls int      `json:"cancelledControls"`
	Warning           string   `json:"warning,omitempty"`
}

func (c defaultRequest) check() string {
	switch {
	case c.MRID == "":
		return "mrid is required"
	case c.Clear && c.Watts != nil:
		return "a clear takes no watts"
	case !c.Clear && c.Watts == nil:
		return "watts is required, or clear"
	}
	return ""
}

func (s *Server) handleDefaultSend(w http.ResponseWriter, r *http.Request) {
	if !s.controlLimit.allow(s.now()) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorResponse{controlRateLimited})
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{"content type must be application/json"})
		return
	}
	var req defaultRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{"request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{"body must be a JSON object with mrid and watts, or mrid and clear"})
		return
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, errorResponse{"body must hold one JSON object"})
		return
	}
	if msg := req.check(); msg != "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{msg})
		return
	}

	out := defaultResponse{MRID: req.MRID}
	if req.Clear {
		if err := s.control.ClearDefaultTargetW(r.Context(), req.MRID); err != nil {
			s.writeDefaultFailure(w, req.MRID, "cleared", err)
			return
		}
		log.Printf("adminui: default control accepted from %s: device=%s clear", remoteFrom(r.Context()), req.MRID)
	} else {
		page := s.controls.pageControlIDs()
		set, err := s.control.SetDefaultTargetW(r.Context(), req.MRID, *req.Watts, func(id string) bool {
			_, ok := page[id]
			return ok
		})
		switch {
		case err == nil:
		case errors.Is(err, sep2embed.ErrDefaultCancelIncomplete):
			log.Printf("adminui: default control for %s set with a cancel failure: %v", req.MRID, err)
			out.Warning = defaultCancelWarning
		default:
			s.writeDefaultFailure(w, req.MRID, "set", err)
			return
		}
		out.CancelledControls = len(set.Cancelled)
		// The route takes no credential, so this line is the only record of
		// who set the default.
		log.Printf("adminui: default control accepted from %s: device=%s watts=%d cancelled=%d",
			remoteFrom(r.Context()), req.MRID, *req.Watts, out.CancelledControls)
	}

	watts, err := s.defaultWatts(r.Context(), req.MRID)
	if err != nil {
		log.Printf("adminui: default control for %s: read back: %v", req.MRID, err)
		out.Warning = defaultReadBackWarning
	}
	out.Default = watts
	writeJSON(w, http.StatusOK, out)
}

// writeDefaultFailure answers a refused or failed default write with a fixed
// message; verb is "set" or "cleared".
func (s *Server) writeDefaultFailure(w http.ResponseWriter, mrid, verb string, err error) {
	code, msg := controlFailure(err)
	if code == http.StatusInternalServerError {
		log.Printf("adminui: default control for %s not %s: %v", mrid, verb, err)
		msg = "default could not be " + verb
	}
	writeJSON(w, code, errorResponse{msg})
}

// defaultWatts returns the device's default opModTargetW in watts, nil when
// it serves none.
func (s *Server) defaultWatts(ctx context.Context, mrid string) (*float64, error) {
	p, err := s.control.DefaultTargetW(ctx, mrid)
	if err != nil || p == nil {
		return nil, err
	}
	watts := activePowerWatts(*p)
	return &watts, nil
}

// activePowerWatts is value times ten to the multiplier.
func activePowerWatts(p sep2.ActivePower) float64 {
	return float64(p.Value) * math.Pow10(int(p.Multiplier))
}
