package adminui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// Watts control routes. They sit outside the Bearer gate by decision: anyone
// who reaches the admin port can send a real DERControl to any registered
// device. The Host allowlist still applies, and a send must be JSON, which a
// cross-site form cannot produce without a preflight this server never
// answers.
const (
	controlSendPattern   = "POST /apps/soc/api/control"
	controlStatusPattern = "GET /apps/soc/api/control/{id}"

	// maxControlBody bounds a send request; a real one is under 100 bytes.
	maxControlBody = 1 << 10

	// controlBurst and controlRefillEvery bound how fast the unauthenticated
	// route can issue controls: a burst of controlBurst, then one request per
	// controlRefillEvery, for all callers together.
	controlBurst       = 10
	controlRefillEvery = time.Second

	controlRateLimited = "too many control requests; wait a moment and retry"

	// The duration range a send offers. A stop is a 0 W control held for the
	// default duration; the device then follows the 0 W follow-on anyway.
	minControlSeconds     = 60
	maxControlSeconds     = 3600
	defaultControlSeconds = 300

	// notMovingSeconds is how long after the first report the device may sit
	// still before the verdict says so; the control's own duration caps it.
	notMovingSeconds = 180

	// maxControlRecords bounds the in-memory ledger of sends the status route
	// answers from; the oldest is dropped first.
	maxControlRecords = 256

	// maxStatusReports bounds the SoC reports one status body carries.
	maxStatusReports = 200
)

// Verdicts of a send, judged by the reported state of charge.
const (
	verdictWaiting   = "waiting"
	verdictMoving    = "moving"
	verdictReached   = "reached"
	verdictNotMoving = "not_moving"
	verdictWrongWay  = "wrong_way"
	// verdictStopped marks a 0 W send, which has no direction to judge.
	verdictStopped = "stopped"
)

// tokenBucket is a mutex-guarded token bucket on an injected clock.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newTokenBucket(now time.Time) *tokenBucket {
	return &tokenBucket{tokens: controlBurst, last: now}
}

// allow takes one token if there is one.
func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(float64(controlBurst), b.tokens+float64(el)/float64(controlRefillEvery))
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ControlSource is what the watts routes need from the embedded server.
type ControlSource interface {
	ApplyControlFor(ctx context.Context, delta sep2embed.ControlDelta, durationSeconds uint32) (sep2embed.ControlSend, error)
	ControlSnapshot(ctx context.Context, deviceMRID, controlID string) (sep2embed.DERControlSnapshot, bool, error)
	ResponsesFor(ctx context.Context, subject string, since int64) ([]sep2embed.ResponseSnapshot, error)
}

// controlRequest is the POST body. Watts and DurationSeconds are pointers so
// a missing value is told apart from 0.
type controlRequest struct {
	MRID            string `json:"mrid"`
	Watts           *int64 `json:"watts"`
	DurationSeconds *int64 `json:"durationSeconds"`
	Stop            bool   `json:"stop"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// controlRecord is what the bridge remembers of one send.
type controlRecord struct {
	id, mrid    string
	watts       int64
	seconds     uint32
	start, end  int64
	controlID   string
	controlMRID string
}

// controlLedger holds the last maxControlRecords sends.
type controlLedger struct {
	mu    sync.Mutex
	byID  map[string]*controlRecord
	order []string
}

func newControlLedger() *controlLedger {
	return &controlLedger{byID: map[string]*controlRecord{}}
}

func (l *controlLedger) add(r *controlRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID[r.id] = r
	l.order = append(l.order, r.id)
	for len(l.order) > maxControlRecords {
		delete(l.byID, l.order[0])
		l.order = l.order[1:]
	}
}

// get returns a copy so the caller reads it without the lock.
func (l *controlLedger) get(id string) (controlRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.byID[id]
	if !ok {
		return controlRecord{}, false
	}
	return *r, true
}

// learnMRID remembers the control's mRID the first time it is seen, because
// the control leaves the store after it ends and its responses are keyed by
// the mRID.
func (l *controlLedger) learnMRID(id, mrid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.byID[id]; ok && r.controlMRID == "" {
		r.controlMRID = mrid
	}
}

func newControlID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) mountControl(mux *http.ServeMux) {
	mux.Handle(controlSendPattern, socHeaders(s.hostAllowlist(withRemote(http.HandlerFunc(s.handleControlSend)))))
	mux.Handle(controlStatusPattern, socHeaders(s.hostAllowlist(http.HandlerFunc(s.handleControlStatus))))
}

func (s *Server) handleControlSend(w http.ResponseWriter, r *http.Request) {
	if !s.controlLimit.allow(s.now()) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorResponse{controlRateLimited})
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{"content type must be application/json"})
		return
	}
	var req controlRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{"request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{"body must be a JSON object with mrid and watts, or mrid and stop"})
		return
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, errorResponse{"body must hold one JSON object"})
		return
	}

	watts, seconds, msg := req.resolve()
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{msg})
		return
	}

	delta := sep2embed.ControlDelta{
		Object:    req.MRID,
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": float64(watts)},
	}
	send, err := s.control.ApplyControlFor(r.Context(), delta, seconds)
	warning := ""
	if err != nil {
		if send.ControlID == "" {
			code, msg := controlFailure(err)
			if code == http.StatusInternalServerError {
				log.Printf("adminui: watts control for %s refused: %v", req.MRID, err)
			}
			if code == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "1")
			}
			writeJSON(w, code, errorResponse{msg})
			return
		}
		// Both controls are in service; only the cancel of an older
		// scheduled control failed.
		log.Printf("adminui: watts control for %s issued with a cleanup failure: %v", req.MRID, err)
		warning = "an older scheduled control was not cancelled"
	}

	id, err := newControlID()
	if err != nil {
		log.Printf("adminui: watts control id: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{"control issued but no status id could be made"})
		return
	}
	rec := &controlRecord{
		id: id, mrid: req.MRID, watts: watts, seconds: seconds,
		start: send.Start, end: send.End, controlID: send.ControlID,
	}
	if snap, ok, err := s.control.ControlSnapshot(r.Context(), req.MRID, send.ControlID); err == nil && ok {
		rec.controlMRID = snap.MRID
	}
	s.controls.add(rec)

	// The route takes no credential, so this line is the only record of who
	// sent the control.
	log.Printf("adminui: watts control accepted from %s: device=%s watts=%d duration=%ds id=%s",
		remoteFrom(r.Context()), req.MRID, watts, seconds, id)

	st, serr := s.controlStatus(r.Context(), *rec, nil)
	if serr != nil {
		log.Printf("adminui: watts control %s status: %v", id, serr)
		st = controlStatusResponse{ID: id, MRID: req.MRID, Watts: watts, DurationSeconds: seconds,
			StartedAt: send.Start, EndsAt: send.End, Stop: watts == 0, ControlState: "unknown",
			ResponseStatuses: []int{}, Reports: []outputPointResponse{}, Verdict: verdictWaiting}
	}
	st.Warning = warning
	writeJSON(w, http.StatusOK, st)
}

// resolve checks the request's shape and returns the watts and the duration
// in seconds to issue, or a message for a 400.
func (c controlRequest) resolve() (watts int64, seconds uint32, msg string) {
	if c.MRID == "" {
		return 0, 0, "mrid is required"
	}
	if c.Stop {
		if c.Watts != nil || c.DurationSeconds != nil {
			return 0, 0, "a stop takes no watts or durationSeconds"
		}
		return 0, defaultControlSeconds, ""
	}
	if c.Watts == nil {
		return 0, 0, "watts is required"
	}
	seconds = defaultControlSeconds
	if c.DurationSeconds != nil {
		// Checked as an int64 before the narrowing to uint32.
		if *c.DurationSeconds < minControlSeconds || *c.DurationSeconds > maxControlSeconds {
			return 0, 0, "durationSeconds must be " + strconv.Itoa(minControlSeconds) + " to " + strconv.Itoa(maxControlSeconds)
		}
		seconds = uint32(*c.DurationSeconds)
	}
	return *c.Watts, seconds, ""
}

// controlFailure maps an embed error to a status and a fixed message. The
// message is never the error's own text.
func controlFailure(err error) (int, string) {
	switch {
	case errors.Is(err, sep2embed.ErrUnknownControlDevice):
		return http.StatusNotFound, "unknown device"
	case errors.Is(err, sep2embed.ErrControlValueInvalid):
		return http.StatusBadRequest, "watts is outside what a device control can carry"
	case errors.Is(err, sep2embed.ErrControlDurationInvalid):
		return http.StatusBadRequest, "duration is outside what the fleet policy allows"
	case errors.Is(err, sep2embed.ErrUnsupportedControlAttribute):
		return http.StatusBadRequest, "unsupported control"
	case errors.Is(err, sep2embed.ErrControlDeltaRateUnrepresentable):
		return http.StatusTooManyRequests, "too many controls for this device in one second; retry"
	default:
		return http.StatusInternalServerError, "control could not be issued"
	}
}

// controlStatusResponse is the status body, also returned by the POST.
type controlStatusResponse struct {
	ID              string `json:"id"`
	MRID            string `json:"mrid"`
	Watts           int64  `json:"watts"`
	Stop            bool   `json:"stop"`
	DurationSeconds uint32 `json:"durationSeconds"`
	StartedAt       int64  `json:"startedAt"`
	EndsAt          int64  `json:"endsAt"`
	Now             int64  `json:"now"`
	// ControlState is the served DERControl's state: scheduled, active,
	// cancelled, cancelled_randomized or superseded, or gone once it is no
	// longer served.
	ControlState string `json:"controlState"`
	// Received is true when the device posted a Received response for the
	// control; ResponseStatuses lists every status it posted.
	Received         bool  `json:"received"`
	ResponseStatuses []int `json:"responseStatuses"`
	// ReportCount counts the SoC reports at or after StartedAt; Reports
	// carries the newest maxStatusReports of them.
	ReportCount    int                   `json:"reportCount"`
	Reports        []outputPointResponse `json:"reports"`
	Verdict        string                `json:"verdict"`
	WatchPercent   *float64              `json:"watchPercent,omitempty"`
	WatchReachedAt *int64                `json:"watchReachedAt,omitempty"`
	Warning        string                `json:"warning,omitempty"`
}

func (s *Server) handleControlStatus(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.controls.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{"no such send"})
		return
	}
	var watch *float64
	if raw := r.URL.Query().Get("watch"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || v < 0 || v > 100 {
			writeJSON(w, http.StatusBadRequest, errorResponse{"watch must be a percent from 0 to 100"})
			return
		}
		watch = &v
	}
	st, err := s.controlStatus(r.Context(), rec, watch)
	if err != nil {
		log.Printf("adminui: watts control %s status: %v", rec.id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{"control status unavailable"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) controlStatus(ctx context.Context, rec controlRecord, watch *float64) (controlStatusResponse, error) {
	now := s.now().Unix()
	state := "gone"
	snap, found, err := s.control.ControlSnapshot(ctx, rec.mrid, rec.controlID)
	if err != nil {
		return controlStatusResponse{}, err
	}
	if found {
		state = controlStateName(snap.CurrentStatus)
		if rec.controlMRID == "" {
			rec.controlMRID = snap.MRID
			s.controls.learnMRID(rec.id, snap.MRID)
		}
	}

	statuses := []int{}
	received := false
	if rec.controlMRID != "" {
		resps, err := s.control.ResponsesFor(ctx, rec.controlMRID, rec.start)
		if err != nil {
			return controlStatusResponse{}, err
		}
		seen := map[uint8]bool{}
		for _, rs := range resps {
			if !seen[rs.Status] {
				seen[rs.Status] = true
				statuses = append(statuses, int(rs.Status))
			}
			if rs.Status == sep2.ResponseStatusEventReceived {
				received = true
			}
		}
		sort.Ints(statuses)
	}

	baseline, reports := s.socReports(rec.mrid, rec.start)
	judged := judge(rec.watts, int64(rec.seconds), rec.start, now, baseline, reports, watch)

	shown := reports
	if len(shown) > maxStatusReports {
		shown = shown[len(shown)-maxStatusReports:]
	}
	out := controlStatusResponse{
		ID: rec.id, MRID: rec.mrid, Watts: rec.watts, Stop: rec.watts == 0,
		DurationSeconds: rec.seconds, StartedAt: rec.start, EndsAt: rec.end, Now: now,
		ControlState: state, Received: received, ResponseStatuses: statuses,
		ReportCount: len(reports), Reports: append([]outputPointResponse{}, shown...),
		Verdict: judged.verdict, WatchPercent: watch, WatchReachedAt: judged.watchReachedAt,
	}
	return out, nil
}

// controlStateName names an EventStatus.currentStatus.
func controlStateName(status uint8) string {
	switch status {
	case sep2.EventStatusScheduled:
		return "scheduled"
	case sep2.EventStatusActive:
		return "active"
	case sep2.EventStatusCancelled:
		return "cancelled"
	case 3:
		return "cancelled_randomized"
	case sep2.EventStatusSuperseded:
		return "superseded"
	default:
		return "status_" + strconv.Itoa(int(status))
	}
}

// socReports reads the device's reported state of charge from the output
// history: the newest sample before start, if any, and every sample at or
// after it, oldest first.
func (s *Server) socReports(mrid string, start int64) (baseline *float64, reports []outputPointResponse) {
	for _, ss := range s.history.Snapshot() {
		if ss.Key.Object != mrid || ss.Key.Attribute != socAttribute {
			continue
		}
		return splitReports(ss.Samples, start)
	}
	return nil, nil
}

func splitReports(samples []telemetryhistory.Sample, start int64) (baseline *float64, reports []outputPointResponse) {
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		if sm.At < start {
			v := sm.Value
			baseline = &v
			continue
		}
		reports = append(reports, outputPointResponse{T: sm.At, V: sm.Value})
	}
	return baseline, reports
}

type judgement struct {
	verdict        string
	watchReachedAt *int64
}

// judge decides the verdict from what the device reported. Discharge (watts
// above 0) lowers the state of charge, charge raises it.
//
//   - reached: a report is at the limit (0 or 100) in the commanded
//     direction, or at the optional watch percent.
//   - moving, wrong_way: two successive steps, counted from the last report
//     before the send when there is one, both in or both against the
//     commanded direction. Once seen they hold, so a device that stops after
//     the control ends is still judged by what it did.
//   - not_moving: neither of those within notMovingSeconds or the duration,
//     whichever is shorter, counted from the first report after the send
//     (from the send itself while there is none).
//   - waiting: none of those yet.
func judge(watts, durationSeconds, start, now int64, baseline *float64, reports []outputPointResponse, watch *float64) judgement {
	if watts == 0 {
		return judgement{verdict: verdictStopped}
	}
	dir := -1.0 // discharge lowers the state of charge
	limit := 0.0
	if watts < 0 {
		dir, limit = 1, 100
	}
	atOrPast := func(v, target float64) bool { return (v-target)*dir >= 0 }

	var out judgement
	limitHit := false
	prev := baseline
	for _, p := range reports {
		if atOrPast(p.V, limit) {
			limitHit = true
		}
		// A watch is marked when a report crosses it. A level already past
		// it before this report (a watch set behind the start) is never
		// marked, so a device moving the wrong way cannot read as reaching it.
		if watch != nil && out.watchReachedAt == nil && atOrPast(p.V, *watch) && (prev == nil || !atOrPast(*prev, *watch)) {
			t := p.T
			out.watchReachedAt = &t
		}
		v := p.V
		prev = &v
	}
	if limitHit || out.watchReachedAt != nil {
		out.verdict = verdictReached
		return out
	}

	seq := make([]float64, 0, len(reports)+1)
	if baseline != nil {
		seq = append(seq, *baseline)
	}
	for _, p := range reports {
		seq = append(seq, p.V)
	}
	step := func(i int) float64 { // +1 in the commanded direction, -1 against, 0 flat
		switch d := (seq[i] - seq[i-1]) * dir; {
		case d > 0:
			return 1
		case d < 0:
			return -1
		}
		return 0
	}
	for i := 2; i < len(seq); i++ {
		a, b := step(i-1), step(i)
		if a == 1 && b == 1 {
			out.verdict = verdictMoving
			return out
		}
		if a == -1 && b == -1 {
			out.verdict = verdictWrongWay
			return out
		}
	}

	anchor := start
	if len(reports) > 0 {
		anchor = reports[0].T
	}
	if now >= anchor+min(int64(notMovingSeconds), durationSeconds) {
		out.verdict = verdictNotMoving
	} else {
		out.verdict = verdictWaiting
	}
	return out
}
