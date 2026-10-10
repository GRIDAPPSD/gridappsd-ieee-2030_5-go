package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/socsend"
)

// SoC routes. They sit outside the Bearer gate by decision: anyone who
// reaches the admin port can send a state of charge to any registered
// device. The Host allowlist still applies, and a send must be JSON, which
// a cross-site form cannot produce without a preflight this server never
// answers.
const (
	socSendPattern   = "POST /apps/soc/api/soc"
	socStatusPattern = "GET /apps/soc/api/soc/{id}"

	// maxSoCBody bounds a send request; a real one is under 100 bytes.
	maxSoCBody = 1 << 10

	// socBurst and socRefillEvery bound how fast the unauthenticated send
	// and clear route can publish to the shared input topic: a burst of
	// socBurst, then one request per socRefillEvery, for all callers
	// together.
	socBurst       = 10
	socRefillEvery = time.Second

	socRateLimited = "too many state of charge requests; wait a moment and retry"
)

// tokenBucket is a mutex-guarded token bucket on an injected clock.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newTokenBucket(now time.Time) *tokenBucket {
	return &tokenBucket{tokens: socBurst, last: now}
}

// allow takes one token if there is one.
func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(float64(socBurst), b.tokens+float64(el)/float64(socRefillEvery))
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// SoCSource is what the SoC routes need from *socsend.Service.
type SoCSource interface {
	Send(ctx context.Context, mrid string, percent int, hold time.Duration) (socsend.Status, error)
	Clear(ctx context.Context, mrid string) (socsend.Status, error)
	Status(ctx context.Context, id string) (socsend.Status, bool)
	Run(ctx context.Context) error
}

// socSendRequest is the POST body. Percent is a pointer so a missing value
// is told apart from 0.
type socSendRequest struct {
	MRID        string `json:"mrid"`
	Percent     *int   `json:"percent"`
	HoldSeconds *int   `json:"holdSeconds"`
	Clear       bool   `json:"clear"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) mountSoC(mux *http.ServeMux) {
	mux.Handle(socSendPattern, socHeaders(s.hostAllowlist(withRemote(http.HandlerFunc(s.handleSoCSend)))))
	mux.Handle(socStatusPattern, socHeaders(s.hostAllowlist(http.HandlerFunc(s.handleSoCStatus))))
}

func (s *Server) handleSoCSend(w http.ResponseWriter, r *http.Request) {
	if !s.socLimit.allow(s.now()) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorResponse{socRateLimited})
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{"content type must be application/json"})
		return
	}
	var req socSendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSoCBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{"request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{"body must be a JSON object with mrid and percent, or mrid and clear"})
		return
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, errorResponse{"body must hold one JSON object"})
		return
	}

	var (
		st  socsend.Status
		err error
	)
	switch {
	case req.Clear && (req.Percent != nil || req.HoldSeconds != nil):
		writeJSON(w, http.StatusBadRequest, errorResponse{"a clear takes no percent or holdSeconds"})
		return
	case req.Clear:
		st, err = s.soc.Clear(r.Context(), req.MRID)
	case req.Percent == nil:
		writeJSON(w, http.StatusBadRequest, errorResponse{"percent is required"})
		return
	default:
		hold := socsend.DefaultHold
		if req.HoldSeconds != nil {
			// Checked before the multiplication, which would overflow.
			if *req.HoldSeconds < 0 || *req.HoldSeconds > int(socsend.MaxHold/time.Second) {
				writeJSON(w, http.StatusBadRequest, errorResponse{socsend.ErrHoldRange.Error()})
				return
			}
			hold = time.Duration(*req.HoldSeconds) * time.Second
		}
		st, err = s.soc.Send(r.Context(), req.MRID, *req.Percent, hold)
	}
	if err != nil {
		code, msg := socFailure(err)
		writeJSON(w, code, errorResponse{msg})
		return
	}
	// The route takes no credential, so this line is the only record of who
	// published.
	log.Printf("adminui: state of charge %s accepted from %s: device=%s percent=%d hold=%ds",
		st.Kind, remoteFrom(r.Context()), st.MRID, st.Percent, st.HoldSeconds)
	writeJSON(w, http.StatusOK, st)
}

// socFailure maps a service error to a status and a fixed message. The
// message is never the error's own text: a bus error can name the broker.
func socFailure(err error) (int, string) {
	switch {
	case errors.Is(err, socsend.ErrPercentRange):
		return http.StatusBadRequest, socsend.ErrPercentRange.Error()
	case errors.Is(err, socsend.ErrHoldRange):
		return http.StatusBadRequest, socsend.ErrHoldRange.Error()
	case errors.Is(err, socsend.ErrUnknownDevice):
		return http.StatusNotFound, socsend.ErrUnknownDevice.Error()
	default:
		return http.StatusBadGateway, socsend.ErrPublishFailed.Error()
	}
}

func (s *Server) handleSoCStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.soc.Status(r.Context(), r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{"no such send"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
