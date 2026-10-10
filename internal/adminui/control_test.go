package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

const (
	brokerSecret = "broker-user=bridge broker-password=hunter2"
	t0           = int64(1_700_000_000)
)

type controlCall struct {
	mrid    string
	watts   int64
	seconds uint32
}

type fakeControl struct {
	mu       sync.Mutex
	calls    []controlCall
	deltas   []sep2embed.ControlDelta
	err      error
	partial  bool
	noFollow bool
	start    int64
	snap     *sep2embed.DERControlSnapshot
	snapErr  error
	// snapFn, when set, answers the n-th ControlSnapshot call (from 1)
	// instead of snap.
	snapFn    func(n int) *sep2embed.DERControlSnapshot
	snapCalls int
	responses []sep2embed.ResponseSnapshot
	respErr   error
	askedFor  []string
	since     []int64
}

func newFakeControl() *fakeControl {
	return &fakeControl{start: t0, snap: &sep2embed.DERControlSnapshot{ID: "ctl-1", MRID: "ctl-mrid-1", CurrentStatus: sep2.EventStatusActive}}
}

func (f *fakeControl) ApplyControlFor(_ context.Context, d sep2embed.ControlDelta, seconds uint32) (sep2embed.ControlSend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	val, _ := d.Value.(map[string]any)["value"].(float64)
	f.calls = append(f.calls, controlCall{d.Object, int64(val), seconds})
	f.deltas = append(f.deltas, d)
	send := sep2embed.ControlSend{ControlID: "ctl-1", FollowOnID: "fo-1", Start: f.start, End: f.start + int64(seconds)}
	if f.err != nil {
		if f.noFollow {
			send.FollowOnID = ""
		}
		if f.partial {
			return send, f.err
		}
		return sep2embed.ControlSend{}, f.err
	}
	return send, nil
}

func (f *fakeControl) ControlSnapshot(_ context.Context, _, controlID string) (sep2embed.DERControlSnapshot, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapCalls++
	if f.snapErr != nil {
		return sep2embed.DERControlSnapshot{}, false, f.snapErr
	}
	snap := f.snap
	if f.snapFn != nil {
		snap = f.snapFn(f.snapCalls)
	}
	if snap == nil || snap.ID != controlID {
		return sep2embed.DERControlSnapshot{}, false, nil
	}
	return *snap, true, nil
}

func (f *fakeControl) ResponsesFor(_ context.Context, subject string, since int64) ([]sep2embed.ResponseSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askedFor = append(f.askedFor, subject)
	f.since = append(f.since, since)
	if f.respErr != nil {
		return nil, f.respErr
	}
	var out []sep2embed.ResponseSnapshot
	for _, r := range f.responses {
		if r.Subject == subject {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeControl) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *stepClock) set(unix int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = time.Unix(unix, 0)
}

// ctlHarness is a server with a fake control source, a history the test
// fills, and a clock the test moves.
type ctlHarness struct {
	s    *Server
	ctl  *fakeControl
	hist *outputHistory
	clk  *stepClock
}

func newCtlHarness(t *testing.T, cfg Config) *ctlHarness {
	t.Helper()
	if cfg.Key == "" && !cfg.InsecureNoKey {
		cfg.Key = testKey
	}
	h := &ctlHarness{ctl: newFakeControl(), hist: &outputHistory{}, clk: &stepClock{t: time.Unix(t0, 0)}}
	src := testSources()
	src.Control = h.ctl
	src.History = h.hist
	h.s = newServer(t, cfg, src)
	h.s.now = h.clk.now
	h.s.controlLimit = newTokenBucket(h.clk.now())
	return h
}

func (h *ctlHarness) reports(mrid string, samples ...telemetryhistory.Sample) {
	h.hist.series = []telemetryhistory.SeriesSnapshot{histSeries(mrid, attrSoC, samples...)}
}

func postControl(t *testing.T, h http.Handler, contentType, body, host string) *httptest.ResponseRecorder {
	t.Helper()
	return postControlFrom(t, h, "127.0.0.1:40000", contentType, body, host)
}

func postControlFrom(t *testing.T, h http.Handler, remote, contentType, body, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apps/soc/api/control", strings.NewReader(body))
	req.RemoteAddr = remote
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (h *ctlHarness) send(t *testing.T, body string) controlStatusResponse {
	t.Helper()
	rec := postControl(t, h.s.Handler(), "application/json", body, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", body, rec.Code, rec.Body)
	}
	var st controlStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("POST body: %v: %s", err, rec.Body)
	}
	return st
}

func (h *ctlHarness) status(t *testing.T, id, query string) controlStatusResponse {
	t.Helper()
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+id+query, "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body)
	}
	var st controlStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("GET body: %v: %s", err, rec.Body)
	}
	return st
}

func TestControlSendNeedsNoCredentialAndPassesWattsAndDefaultDuration(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})

	rec := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_bat-1","watts":2000}`, "localhost")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with no Authorization header: %s", rec.Code, rec.Body)
	}
	if want := []controlCall{{"_bat-1", 2000, 300}}; !slices.Equal(h.ctl.calls, want) {
		t.Errorf("calls = %+v, want %+v", h.ctl.calls, want)
	}
	d := h.ctl.deltas[0]
	if d.Attribute != "DERControl.DERControlBase.opModTargetW" || d.Value.(map[string]any)["multiplier"] != 0.0 {
		t.Errorf("delta = %+v, want opModTargetW with multiplier 0", d)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body: %v", err)
	}
	for k, want := range map[string]any{
		"mrid": "_bat-1", "watts": 2000.0, "stop": false, "durationSeconds": 300.0,
		"startedAt": float64(t0), "endsAt": float64(t0 + 300), "now": float64(t0),
		"controlState": "active", "received": false, "reportCount": 0.0, "verdict": "waiting",
	} {
		if raw[k] != want {
			t.Errorf("%s = %v, want %v", k, raw[k], want)
		}
	}
	if id, _ := raw["id"].(string); len(id) != 16 {
		t.Errorf("id = %q, want 16 hex characters", id)
	}
	if arr, ok := raw["responseStatuses"].([]any); !ok || len(arr) != 0 {
		t.Errorf("responseStatuses = %#v, want an empty array", raw["responseStatuses"])
	}
	if arr, ok := raw["reports"].([]any); !ok || len(arr) != 0 {
		t.Errorf("reports = %#v, want an empty array", raw["reports"])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
}

func TestControlSendChargeIsNegativeAndDurationIsCarriedThrough(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		want       controlCall
	}{
		{"charge", `{"mrid":"_bat-1","watts":-2500,"durationSeconds":60}`, controlCall{"_bat-1", -2500, 60}},
		{"longest", `{"mrid":"_bat-1","watts":1,"durationSeconds":3600}`, controlCall{"_bat-1", 1, 3600}},
		{"zero watts", `{"mrid":"_bat-1","watts":0,"durationSeconds":120}`, controlCall{"_bat-1", 0, 120}},
		{"stop", `{"mrid":"_bat-1","stop":true}`, controlCall{"_bat-1", 0, 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			st := h.send(t, tc.body)
			if !slices.Equal(h.ctl.calls, []controlCall{tc.want}) {
				t.Errorf("calls = %+v, want %+v", h.ctl.calls, tc.want)
			}
			if st.Watts != tc.want.watts || st.DurationSeconds != tc.want.seconds || st.Stop != (tc.want.watts == 0) {
				t.Errorf("status = watts %d duration %d stop %v, want %d %d %v", st.Watts, st.DurationSeconds, st.Stop, tc.want.watts, tc.want.seconds, tc.want.watts == 0)
			}
			if tc.want.watts == 0 && st.Verdict != verdictStopped {
				t.Errorf("verdict = %q, want %q for a 0 W send", st.Verdict, verdictStopped)
			}
		})
	}
}

func TestControlSendRefusesMalformedRequestsWithoutCallingTheEmbed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contentType, body string
		want                    int
	}{
		{"text/plain form post", "text/plain", `{"mrid":"_b","watts":5}`, http.StatusUnsupportedMediaType},
		{"urlencoded form post", "application/x-www-form-urlencoded", `mrid=_b&watts=5`, http.StatusUnsupportedMediaType},
		{"no content type", "", `{"mrid":"_b","watts":5}`, http.StatusUnsupportedMediaType},
		{"mrid missing", "application/json", `{"watts":5}`, http.StatusBadRequest},
		{"watts missing", "application/json", `{"mrid":"_b"}`, http.StatusBadRequest},
		{"fractional watts", "application/json", `{"mrid":"_b","watts":5.5}`, http.StatusBadRequest},
		{"watts as string", "application/json", `{"mrid":"_b","watts":"5"}`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"mrid":"_b","watts":5,"x":1}`, http.StatusBadRequest},
		{"the old percent field", "application/json", `{"mrid":"_b","percent":50}`, http.StatusBadRequest},
		{"stop with watts", "application/json", `{"mrid":"_b","stop":true,"watts":5}`, http.StatusBadRequest},
		{"stop with duration", "application/json", `{"mrid":"_b","stop":true,"durationSeconds":60}`, http.StatusBadRequest},
		{"duration under a minute", "application/json", `{"mrid":"_b","watts":5,"durationSeconds":59}`, http.StatusBadRequest},
		{"duration zero", "application/json", `{"mrid":"_b","watts":5,"durationSeconds":0}`, http.StatusBadRequest},
		{"negative duration", "application/json", `{"mrid":"_b","watts":5,"durationSeconds":-1}`, http.StatusBadRequest},
		{"duration past an hour", "application/json", `{"mrid":"_b","watts":5,"durationSeconds":3601}`, http.StatusBadRequest},
		{"duration that wraps a uint32", "application/json", `{"mrid":"_b","watts":5,"durationSeconds":4294967356}`, http.StatusBadRequest},
		{"not json", "application/json", `watts=5`, http.StatusBadRequest},
		{"two objects", "application/json", `{"mrid":"_b","watts":5}{"mrid":"_c","watts":1}`, http.StatusBadRequest},
		{"oversized body", "application/json", `{"mrid":"` + strings.Repeat("a", maxControlBody) + `","watts":5}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})

			rec := postControl(t, h.s.Handler(), tc.contentType, tc.body, "localhost")

			if rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if h.ctl.callCount() != 0 {
				t.Errorf("embed called %d times, want 0", h.ctl.callCount())
			}
		})
	}
}

func TestControlSendMapsEmbedErrorsToFixedMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"unknown device", fmt.Errorf("%w: x", sep2embed.ErrUnknownControlDevice), http.StatusNotFound, "unknown device"},
		{"value refused", errors.Join(sep2embed.ErrControlValueInvalid, errors.New(brokerSecret)), http.StatusBadRequest, "watts is outside what a device control can carry"},
		{"duration refused", sep2embed.ErrControlDurationInvalid, http.StatusBadRequest, "duration is outside what the fleet policy allows"},
		{"attribute refused", sep2embed.ErrUnsupportedControlAttribute, http.StatusBadRequest, "unsupported control"},
		{"creation time budget spent", sep2embed.ErrControlDeltaRateUnrepresentable, http.StatusTooManyRequests, "too many controls for this device in one second; retry"},
		{"store failure", errors.New(brokerSecret), http.StatusInternalServerError, "control could not be issued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			h.ctl.err = tc.err

			rec := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost")

			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body: %v: %s", err, rec.Body)
			}
			if rec.Code != tc.code || body.Error != tc.msg {
				t.Errorf("status %d error %q, want %d %q", rec.Code, body.Error, tc.code, tc.msg)
			}
			if tc.code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("body %s carries the error text", rec.Body)
			}
		})
	}
}

// A send whose controls are in service but whose old-schedule cancel failed
// is a success with a warning, and its status is readable.
func TestControlSendWithACleanupFailureStillAnswersAndWarns(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.err, h.ctl.partial = errors.New(brokerSecret), true

	rec := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	var st controlStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Warning != "an older scheduled control was not cancelled" || st.ID == "" {
		t.Errorf("body = %s (%v), want a warning and an id", rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("body carries the error text: %s", rec.Body)
	}
}

// A send whose control is in service but whose 0 W follow-on was not written
// is recorded and answered as in service, with a warning that nothing stops
// it at its end.
func TestControlSendWithAFailedFollowOnRecordsTheControlAndWarns(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.err = fmt.Errorf("%w: %w", sep2embed.ErrControlFollowOnNotWritten, errors.New(brokerSecret))
	h.ctl.partial, h.ctl.noFollow = true, true

	rec := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_bat-1","watts":2000,"durationSeconds":900}`, "localhost")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	var st controlStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("body: %v: %s", err, rec.Body)
	}
	if st.Warning != controlNoStopWarning || st.ControlState != "active" || st.Watts != 2000 || st.EndsAt != t0+900 {
		t.Errorf("body = %+v, want the control active until t0+900 with the no-stop warning", st)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("body carries the error text: %s", rec.Body)
	}
	got := h.status(t, st.ID, "")
	if got.ID != st.ID || got.ControlState != "active" || got.Watts != 2000 || got.Warning != controlNoStopWarning {
		t.Errorf("status read = %+v, want the recorded send", got)
	}
}

func TestControlRoutesKeepTheHostAllowlist(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	st := h.send(t, `{"mrid":"_b","watts":5}`)
	before := h.ctl.callCount()

	post := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "evil.example")
	get := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+st.ID, "", "evil.example")

	if post.Code != http.StatusForbidden || get.Code != http.StatusForbidden {
		t.Errorf("POST %d GET %d from a foreign Host, want 403 for both", post.Code, get.Code)
	}
	if h.ctl.callCount() != before {
		t.Errorf("embed called for a foreign Host")
	}
}

func TestControlRoutesAbsentWithoutASource(t *testing.T) {
	t.Parallel()
	src := testSources()
	s := newServer(t, Config{Key: testKey}, src)

	rec := postControl(t, s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost")

	if rec.Code == http.StatusOK {
		t.Errorf("POST answered 200 with no control source configured")
	}
}

func TestControlResponsesNeverCarryTheAdminKey(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{{Key: testKey}, {InsecureNoKey: true}} {
		h := newCtlHarness(t, cfg)
		key := h.s.cfg.Key
		st := h.send(t, `{"mrid":"_b","watts":5}`)
		bodies := []string{
			postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost").Body.String(),
			postControl(t, h.s.Handler(), "text/plain", `x`, "localhost").Body.String(),
			doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+st.ID, "", "localhost").Body.String(),
			doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/nope", "", "localhost").Body.String(),
		}
		for _, b := range bodies {
			if strings.Contains(b, key) {
				t.Errorf("insecure=%v: a response carries the admin key: %s", cfg.InsecureNoKey, b)
			}
		}
	}
}

func TestPlaneMountsNoControlRoute(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	for _, p := range h.s.planePatterns {
		if strings.Contains(p, "/apps/soc") {
			t.Errorf("the plane mounts %q, which the control route would shadow or be shadowed by", p)
		}
	}
}

// Not parallel: it swaps the process-wide log writer.
func TestControlAcceptedSendIsLoggedWithTheCallerAndRefusalsAreNot(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := newCtlHarness(t, Config{})

	ok := postControlFrom(t, h.s.Handler(), "203.0.113.9:51234", "application/json", `{"mrid":"_pv-1","watts":-700,"durationSeconds":90}`, "localhost")
	stop := postControlFrom(t, h.s.Handler(), "198.51.100.7:40000", "application/json", `{"mrid":"_pv-2","stop":true}`, "localhost")
	refused := postControlFrom(t, h.s.Handler(), "192.0.2.1:1", "application/json", `{"mrid":"_pv-3"}`, "localhost")

	if ok.Code != http.StatusOK || stop.Code != http.StatusOK || refused.Code != http.StatusBadRequest {
		t.Fatalf("statuses %d %d %d, want 200 200 400", ok.Code, stop.Code, refused.Code)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, "watts control accepted") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d accepted lines, want 2: %q", len(lines), lines)
	}
	for i, want := range [][]string{
		{"203.0.113.9:51234", "device=_pv-1", "watts=-700", "duration=90s"},
		{"198.51.100.7:40000", "device=_pv-2", "watts=0", "duration=300s"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d %q lacks %q", i, lines[i], w)
			}
		}
	}
	if strings.Contains(buf.String(), "192.0.2.1") {
		t.Errorf("a refused request was logged as accepted: %s", buf.String())
	}
}

func TestControlSendRateLimitIsSharedAndStatusReadsAreNotLimited(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	var first controlStatusResponse
	for i := 0; i < controlBurst; i++ {
		st := h.send(t, `{"mrid":"_b","watts":5}`)
		if i == 0 {
			first = st
		}
	}
	over := postControlFrom(t, h.s.Handler(), "203.0.113.9:1", "application/json", `{"mrid":"_b","watts":5}`, "localhost")
	overOther := postControlFrom(t, h.s.Handler(), "203.0.113.10:1", "application/json", `{"mrid":"_c","stop":true}`, "localhost")

	for name, rec := range map[string]*httptest.ResponseRecorder{"same caller": over, "other caller": overOther} {
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), controlRateLimited) || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("%s past the burst: status %d body %s Retry-After %q", name, rec.Code, rec.Body, rec.Header().Get("Retry-After"))
		}
	}
	if h.ctl.callCount() != controlBurst {
		t.Errorf("embed called %d times, want %d: a limited request must not issue", h.ctl.callCount(), controlBurst)
	}
	if st := h.status(t, first.ID, ""); st.ID != first.ID {
		t.Errorf("status read after the limit was hit returned %q", st.ID)
	}

	h.clk.advance(controlRefillEvery)
	if rec := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost"); rec.Code != http.StatusOK {
		t.Errorf("after one refill interval: status %d, want 200", rec.Code)
	}
	if rec := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a second request in the same interval: status %d, want 429", rec.Code)
	}
}

func TestControlRateLimitNeverBanksMoreThanTheBurst(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.clk.advance(24 * time.Hour)

	ok := 0
	for i := 0; i < controlBurst+5; i++ {
		if postControl(t, h.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost").Code == http.StatusOK {
			ok++
		}
	}
	if ok != controlBurst {
		t.Errorf("%d requests passed after a long idle, want exactly the burst of %d", ok, controlBurst)
	}
}

func TestControlLedgerDropsTheOldestSendPastItsBound(t *testing.T) {
	t.Parallel()
	l := newControlLedger()
	for i := 0; i < maxControlRecords+3; i++ {
		l.add(&controlRecord{id: fmt.Sprintf("id-%d", i)})
	}
	for i := 0; i < 3; i++ {
		if _, ok := l.get(fmt.Sprintf("id-%d", i)); ok {
			t.Errorf("id-%d still held past the bound", i)
		}
	}
	if _, ok := l.get("id-3"); !ok {
		t.Error("id-3, the oldest within the bound, was dropped")
	}
	if len(l.byID) != maxControlRecords || len(l.order) != maxControlRecords {
		t.Errorf("ledger holds %d / %d, want %d", len(l.byID), len(l.order), maxControlRecords)
	}
}

func samples(pairs ...float64) []telemetryhistory.Sample {
	var out []telemetryhistory.Sample
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, telemetryhistory.Sample{At: int64(pairs[i]), Value: pairs[i+1]})
	}
	return out
}

func TestControlStatusVerdicts(t *testing.T) {
	t.Parallel()
	const dev = "_bat-1"
	for _, tc := range []struct {
		name    string
		body    string
		reports []telemetryhistory.Sample
		after   time.Duration // clock advance past the send before reading
		query   string
		want    string
		count   int
	}{
		// With no Received response, reports are judged from one poll
		// (controlPickupSeconds) after the start.
		{"discharge falling twice is moving", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48), 75 * time.Second, "", verdictMoving, 2},
		{"charge rising twice is moving", `{"mrid":"_bat-1","watts":-2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 51, float64(t0+70), 52), 75 * time.Second, "", verdictMoving, 2},
		{"moving holds after the control ends and the device goes flat", `{"mrid":"_bat-1","watts":2000,"durationSeconds":120}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48, float64(t0+100), 48, float64(t0+130), 48), 200 * time.Second, "", verdictMoving, 4},
		{"discharge to zero is reached", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 2, float64(t0+40), 1, float64(t0+70), 0), 75 * time.Second, "", verdictReached, 2},
		{"charge to 100 is reached", `{"mrid":"_bat-1","watts":-2000}`,
			samples(float64(t0+40), 99, float64(t0+70), 100), 75 * time.Second, "", verdictReached, 2},
		{"a watch percent crossed is reached", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48), 75 * time.Second, "?watch=48.5", verdictReached, 2},
		{"a watch percent not yet crossed leaves the verdict alone", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48), 75 * time.Second, "?watch=40", verdictMoving, 2},
		{"a watch of 0 is accepted and reached at empty", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 1, float64(t0+40), 0), 45 * time.Second, "?watch=0", verdictReached, 1},
		{"a watch of 100 is accepted and not reached by a discharge", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48), 75 * time.Second, "?watch=100", verdictMoving, 2},
		{"discharge rising twice is wrong way", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 51, float64(t0+70), 52), 75 * time.Second, "", verdictWrongWay, 2},
		{"charge falling twice is wrong way", `{"mrid":"_bat-1","watts":-2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48), 75 * time.Second, "", verdictWrongWay, 2},
		{"flat for 180 s is not moving", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 50, float64(t0+70), 50), 221 * time.Second, "", verdictNotMoving, 2},
		{"no report at all for 180 s after the pickup is not moving", `{"mrid":"_bat-1","watts":2000}`,
			nil, (controlPickupSeconds + notMovingSeconds) * time.Second, "", verdictNotMoving, 0},
		{"one second short of the pickup and 180 s with no report is still waiting", `{"mrid":"_bat-1","watts":2000,"durationSeconds":3600}`,
			nil, (controlPickupSeconds+notMovingSeconds)*time.Second - time.Second, "", verdictWaiting, 0},
		{"a report exactly at the end of the control is judged", `{"mrid":"_bat-1","watts":2000,"durationSeconds":60}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+60), 48), 70 * time.Second, "", verdictMoving, 2},
		{"flat inside the window is still waiting", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 50), 130 * time.Second, "", verdictWaiting, 1},
		{"a 60 s control gives up 60 s after its first report, not 180", `{"mrid":"_bat-1","watts":2000,"durationSeconds":60}`,
			samples(float64(t0-30), 50, float64(t0+35), 50), 96 * time.Second, "", verdictNotMoving, 1},
		{"a report before the send is not a report since it", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0-5), 40), 10 * time.Second, "", verdictWaiting, 0},
		// A discharge sent while the device was charging: it keeps charging
		// until its poll picks the control up, then discharges.
		{"reports before the pickup do not count against the new direction", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 48, float64(t0+5), 50, float64(t0+15), 52, float64(t0+25), 54,
				float64(t0+40), 52, float64(t0+70), 50, float64(t0+100), 48, float64(t0+130), 46), 135 * time.Second, "", verdictMoving, 7},
		{"a device still going the wrong way after the pickup is wrong way", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 48, float64(t0+5), 50, float64(t0+15), 52, float64(t0+25), 54,
				float64(t0+40), 56, float64(t0+70), 58), 75 * time.Second, "", verdictWrongWay, 5},
		{"a wrong way verdict is revised while the control is live", `{"mrid":"_bat-1","watts":2000}`,
			samples(float64(t0-30), 50, float64(t0+40), 51, float64(t0+70), 52, float64(t0+100), 51, float64(t0+130), 50), 135 * time.Second, "", verdictMoving, 4},
		{"reports after the control ends are not judged", `{"mrid":"_bat-1","watts":2000,"durationSeconds":60}`,
			samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 50, float64(t0+100), 51), 200 * time.Second, "", verdictNotMoving, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			h.reports(dev, tc.reports...)
			sent := h.send(t, tc.body)
			h.clk.advance(tc.after)

			st := h.status(t, sent.ID, tc.query)

			if st.Verdict != tc.want || st.ReportCount != tc.count {
				t.Errorf("verdict %q with %d reports, want %q with %d", st.Verdict, st.ReportCount, tc.want, tc.count)
			}
			if st.Now != t0+int64(tc.after/time.Second) || st.StartedAt != t0 {
				t.Errorf("now %d startedAt %d", st.Now, st.StartedAt)
			}
		})
	}
}

func TestControlStatusReportsAreTheSinceStartSamplesWithTheirValues(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0), 49.5, float64(t0+30), 49)...)
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.clk.advance(40 * time.Second)

	st := h.status(t, sent.ID, "")

	want := []outputPointResponse{{T: t0, V: 49.5}, {T: t0 + 30, V: 49}}
	if !slices.Equal(st.Reports, want) || st.ReportCount != 2 {
		t.Errorf("reports = %+v (count %d), want %+v", st.Reports, st.ReportCount, want)
	}
}

func TestControlStatusCapsTheReportsItCarriesButCountsThemAll(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	var ss []telemetryhistory.Sample
	for i := 0; i < maxStatusReports+50; i++ {
		ss = append(ss, telemetryhistory.Sample{At: t0 + int64(i), Value: 50})
	}
	h.reports("_bat-1", ss...)
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)

	st := h.status(t, sent.ID, "")

	if st.ReportCount != maxStatusReports+50 || len(st.Reports) != maxStatusReports {
		t.Fatalf("count %d carried %d, want %d and %d", st.ReportCount, len(st.Reports), maxStatusReports+50, maxStatusReports)
	}
	if st.Reports[len(st.Reports)-1].T != t0+int64(maxStatusReports+49) {
		t.Errorf("newest carried report at %d, want the newest sample", st.Reports[len(st.Reports)-1].T)
	}
}

func TestControlStatusReadsOnlyTheSocSeriesOfTheSentDevice(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.hist.series = []telemetryhistory.SeriesSnapshot{
		histSeries("_other", attrSoC, samples(float64(t0+10), 10, float64(t0+20), 9)...),
		histSeries("_bat-1", attrSetpoint, samples(float64(t0+10), 2000)...),
		histSeries("_bat-1", "DERStatus.readingTime", samples(float64(t0+10), 5)...),
	}
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.clk.advance(20 * time.Second)

	st := h.status(t, sent.ID, "")

	if st.ReportCount != 0 || st.Verdict != verdictWaiting {
		t.Errorf("count %d verdict %q from another device's or attribute's samples, want 0 and waiting", st.ReportCount, st.Verdict)
	}
}

func TestControlStatusWatchPercentIsMarkedOnlyWhenReached(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+10), 49, float64(t0+40), 47.5)...)
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.clk.advance(45 * time.Second)

	reached := h.status(t, sent.ID, "?watch=48")
	if reached.WatchPercent == nil || *reached.WatchPercent != 48 || reached.WatchReachedAt == nil || *reached.WatchReachedAt != t0+40 {
		t.Errorf("watch 48: percent %v reachedAt %v, want 48 and %d", reached.WatchPercent, reached.WatchReachedAt, t0+40)
	}
	notYet := h.status(t, sent.ID, "?watch=10")
	if notYet.WatchPercent == nil || *notYet.WatchPercent != 10 || notYet.WatchReachedAt != nil {
		t.Errorf("watch 10: percent %v reachedAt %v, want 10 and none", notYet.WatchPercent, notYet.WatchReachedAt)
	}
	none := h.status(t, sent.ID, "")
	if none.WatchPercent != nil || none.WatchReachedAt != nil {
		t.Errorf("no watch: percent %v reachedAt %v, want both absent", none.WatchPercent, none.WatchReachedAt)
	}

	for _, q := range []string{"?watch=abc", "?watch=-1", "?watch=101", "?watch=NaN", "?watch=Inf"} {
		rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+sent.ID+q, "", "localhost")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rec.Code)
		}
	}
}

func TestControlStatusControlStateAndResponses(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.responses = []sep2embed.ResponseSnapshot{
		{Subject: "ctl-mrid-1", EndDeviceLFDI: "L", Status: sep2.ResponseStatusEventStarted, CreatedDateTime: t0 + 20},
		{Subject: "ctl-mrid-1", EndDeviceLFDI: "L", Status: sep2.ResponseStatusEventReceived, CreatedDateTime: t0 + 15},
		{Subject: "ctl-mrid-1", EndDeviceLFDI: "L", Status: sep2.ResponseStatusEventReceived, CreatedDateTime: t0 + 16},
		{Subject: "someone-else", Status: sep2.ResponseStatusEventCompleted},
	}
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	// The fake already holds the responses at send time, so the POST shows them.
	if !sent.Received || !slices.Equal(sent.ResponseStatuses, []int{1, 2}) {
		t.Errorf("send-time received %v statuses %v, want true [1 2]", sent.Received, sent.ResponseStatuses)
	}
	if len(h.ctl.since) == 0 || h.ctl.since[0] != t0 {
		t.Errorf("responses read since %v, want the send's start %d", h.ctl.since, t0)
	}

	st := h.status(t, sent.ID, "")
	if st.ControlState != "active" || !st.Received || !slices.Equal(st.ResponseStatuses, []int{1, 2}) {
		t.Errorf("state %q received %v statuses %v, want active true [1 2]", st.ControlState, st.Received, st.ResponseStatuses)
	}

	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventStarted}}
	if st := h.status(t, sent.ID, ""); st.Received || !slices.Equal(st.ResponseStatuses, []int{2}) {
		t.Errorf("started without received: received %v statuses %v, want false [2]", st.Received, st.ResponseStatuses)
	}

	for status, want := range map[uint8]string{
		sep2.EventStatusScheduled: "scheduled", sep2.EventStatusActive: "active",
		sep2.EventStatusCancelled: "cancelled", 3: "cancelled_randomized",
		sep2.EventStatusSuperseded: "superseded", 9: "status_9",
	} {
		h.ctl.snap.CurrentStatus = status
		if st := h.status(t, sent.ID, ""); st.ControlState != want {
			t.Errorf("status %d served as %q, want %q", status, st.ControlState, want)
		}
	}
}

// Once the control leaves the store the status says so, and the responses
// are still found through the mRID read at send time.
func TestControlStatusGoneControlStillFindsItsResponses(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.ctl.snap = nil
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived}}

	st := h.status(t, sent.ID, "")

	if st.ControlState != "gone" || !st.Received {
		t.Errorf("state %q received %v, want gone true", st.ControlState, st.Received)
	}
}

func TestControlStatusLearnsTheControlMRIDWhenTheSendCouldNotSeeIt(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.snapErr = errors.New("store blip")
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	if sent.ControlState != "unknown" || sent.Now != t0 {
		t.Errorf("send-time state %q now %d, want unknown and %d after a snapshot failure", sent.ControlState, sent.Now, t0)
	}
	h.ctl.snapErr = nil
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived}}

	st := h.status(t, sent.ID, "")
	again := h.status(t, sent.ID, "")

	if st.ControlState != "active" || !st.Received || !again.Received {
		t.Errorf("state %q received %v/%v, want active true/true", st.ControlState, st.Received, again.Received)
	}
}

func TestControlStatusUnknownIDAndStoreFailures(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)

	if rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/nope", "", "localhost"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rec.Code)
	}
	h.ctl.snapErr = errors.New(brokerSecret)
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+sent.ID, "", "localhost")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("snapshot failure = %d %s, want 500 without the error text", rec.Code, rec.Body)
	}
	h.ctl.snapErr = nil
	h.ctl.respErr = errors.New(brokerSecret)
	rec = doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+sent.ID, "", "localhost")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("responses failure = %d %s, want 500 without the error text", rec.Code, rec.Body)
	}
}

func TestOldSoCRoutesAreGone(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/apps/soc/api/soc", strings.NewReader(`{"mrid":"_b","percent":50}`))
	req.Host, req.RemoteAddr = "localhost", "127.0.0.1:40000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || h.ctl.callCount() != 0 {
		t.Errorf("POST /apps/soc/api/soc = %d, calls %d, want it unrouted", rec.Code, h.ctl.callCount())
	}
	if rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/soc/x", "", "localhost"); rec.Code == http.StatusOK || rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "no such send") {
		t.Errorf("GET /apps/soc/api/soc/x = %d %s, want it unrouted", rec.Code, rec.Body)
	}
}

// judge is the pure decision; these rows cover shapes the HTTP rows do not.
func TestJudgeRows(t *testing.T) {
	t.Parallel()
	pt := func(at int64, v float64) outputPointResponse { return outputPointResponse{T: at, V: v} }
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name      string
		watts     int64
		duration  int64
		now       int64
		baseline  *float64
		reports   []outputPointResponse
		watch     *float64
		want      string
		watchedAt *int64
	}{
		{"no baseline needs three reports for two steps", 1000, 300, t0 + 100, nil, []outputPointResponse{pt(t0+10, 50), pt(t0+40, 49)}, nil, verdictWaiting, nil},
		{"no baseline, three falling reports", 1000, 300, t0 + 100, nil, []outputPointResponse{pt(t0+10, 50), pt(t0+40, 49), pt(t0+70, 48)}, nil, verdictMoving, nil},
		{"one step each way is neither", 1000, 300, t0 + 100, f(50), []outputPointResponse{pt(t0+10, 49), pt(t0+40, 50)}, nil, verdictWaiting, nil},
		{"a flat step breaks a run", 1000, 300, t0 + 100, f(50), []outputPointResponse{pt(t0+10, 49), pt(t0+40, 49), pt(t0+70, 48)}, nil, verdictWaiting, nil},
		{"already at the limit and told to discharge is reached", 1000, 300, t0 + 10, f(0), []outputPointResponse{pt(t0+5, 0)}, nil, verdictReached, nil},
		{"the watch percent marks the first report that crosses it", 1000, 300, t0 + 100, f(50), []outputPointResponse{pt(t0+10, 45), pt(t0+40, 40)}, f(44), verdictReached, func() *int64 { v := t0 + 40; return &v }()},
		{"a charge watch is crossed upward", -1000, 300, t0 + 100, f(50), []outputPointResponse{pt(t0+10, 55), pt(t0+40, 60)}, f(58), verdictReached, func() *int64 { v := t0 + 40; return &v }()},
		{"a charge watch behind the start is never marked", -1000, 300, t0 + 100, f(50), []outputPointResponse{pt(t0+10, 51)}, f(40), verdictWaiting, nil},
		{"a discharge watch behind the start is never marked, even when the device goes the wrong way", 1000, 300, t0 + 100, f(50), []outputPointResponse{pt(t0+10, 51), pt(t0+40, 52)}, f(60), verdictWrongWay, nil},
		{"with no baseline the first report is judged against the watch", 1000, 300, t0 + 100, nil, []outputPointResponse{pt(t0+10, 44)}, f(45), verdictReached, func() *int64 { v := t0 + 10; return &v }()},
		{"the window is counted from the first report", 1000, 3600, t0 + 250, f(50), []outputPointResponse{pt(t0+100, 50)}, nil, verdictWaiting, nil},
		{"the window ends 180 s after the first report", 1000, 3600, t0 + 280, f(50), []outputPointResponse{pt(t0+100, 50)}, nil, verdictNotMoving, nil},
		{"a watch crossed again after dipping back is marked at the second crossing", 1000, 300, t0 + 100, f(44), []outputPointResponse{pt(t0+10, 46), pt(t0+40, 44)}, f(45), verdictReached, func() *int64 { v := t0 + 40; return &v }()},
		{"zero watts has no direction", 0, 300, t0 + 999, f(50), []outputPointResponse{pt(t0+10, 10)}, nil, verdictStopped, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := judge(tc.watts, tc.duration, t0, tc.now, tc.baseline, tc.reports, tc.watch)
			if got.verdict != tc.want {
				t.Errorf("verdict %q, want %q", got.verdict, tc.want)
			}
			switch {
			case tc.watchedAt == nil && got.watchReachedAt != nil:
				t.Errorf("watchReachedAt = %d, want none", *got.watchReachedAt)
			case tc.watchedAt != nil && (got.watchReachedAt == nil || *got.watchReachedAt != *tc.watchedAt):
				t.Errorf("watchReachedAt = %v, want %d", got.watchReachedAt, *tc.watchedAt)
			}
		})
	}
}

// The control's mRID is captured at send time when the control is visible
// then, so its responses are found after the control leaves the store even
// though no status read ever saw it.
func TestControlStatusUsesTheMRIDCapturedAtSend(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	visible := *h.ctl.snap
	h.ctl.snapFn = func(n int) *sep2embed.DERControlSnapshot {
		if n == 1 {
			return &visible
		}
		return nil
	}
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived}}

	st := h.status(t, sent.ID, "")

	if st.ControlState != "gone" || !st.Received {
		t.Errorf("state %q received %v, want gone true", st.ControlState, st.Received)
	}
}

// A control first seen by a status read has its mRID remembered, so a later
// read finds its responses after it leaves the store.
func TestControlStatusRemembersTheMRIDFirstSeenByAStatusRead(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	visible := *h.ctl.snap
	h.ctl.snapFn = func(n int) *sep2embed.DERControlSnapshot {
		if n == 3 {
			return &visible
		}
		return nil
	}
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived}}

	first := h.status(t, sent.ID, "")
	second := h.status(t, sent.ID, "")

	if first.ControlState != "active" || second.ControlState != "gone" || !second.Received {
		t.Errorf("states %q then %q, second received %v; want active, gone, true", first.ControlState, second.ControlState, second.Received)
	}
}

// A Received response sets when judging starts: here it comes before the
// default pickup, so reports from it on count.
func TestControlStatusJudgesFromTheReceivedResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		received bool
		want     string
	}{
		{"received at t0+5", true, verdictMoving},
		{"no received response", false, verdictWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+10), 49, float64(t0+20), 48)...)
			if tc.received {
				h.ctl.responses = []sep2embed.ResponseSnapshot{
					{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived, CreatedDateTime: t0 + 5},
				}
			}
			sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
			h.clk.advance(25 * time.Second)

			if st := h.status(t, sent.ID, ""); st.Verdict != tc.want {
				t.Errorf("verdict %q, want %q", st.Verdict, tc.want)
			}
		})
	}
}

// A status read copies only the sent device's state of charge series, never
// the whole history.
func TestControlStatusReadsOnlyTheDevicesSeries(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48)...)
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.hist.during = func() { t.Error("a control status read took a whole-history snapshot") }
	h.clk.advance(75 * time.Second)

	st := h.status(t, sent.ID, "")

	if st.ReportCount != 2 || st.Verdict != verdictMoving {
		t.Errorf("count %d verdict %q, want 2 and moving", st.ReportCount, st.Verdict)
	}
	if want := (telemetryhistory.SeriesKey{Object: "_bat-1", Attribute: attrSoC}); !slices.Contains(h.hist.asked, want) {
		t.Errorf("series asked for %+v, want %+v", h.hist.asked, want)
	}
}

func TestControlStatusIsNotCached(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)

	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/"+sent.ID, "", "localhost")

	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status %d Cache-Control %q, want 200 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// Not parallel: it swaps the process-wide log writer.
func TestControlSendLogsASnapshotFailure(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := newCtlHarness(t, Config{})
	h.ctl.snapErr = errors.New("store blip")

	h.send(t, `{"mrid":"_bat-1","watts":2000}`)

	if !strings.Contains(buf.String(), "control snapshot") || !strings.Contains(buf.String(), "store blip") {
		t.Errorf("log = %q, want the send-time snapshot failure", buf.String())
	}
}

// A newer control that supersedes or cancels a send ends the window its
// reports are judged in, so what the device does for the newer control
// cannot rewrite the older send's verdict, here or after the control leaves
// the store.
func TestControlStatusStopsJudgingWhenTheSendIsNoLongerInForce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status uint8
	}{
		{"superseded", sep2.EventStatusSuperseded},
		{"cancelled", sep2.EventStatusCancelled},
		{"cancelled with randomization", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48)...)
			sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
			h.clk.advance(75 * time.Second)
			if st := h.status(t, sent.ID, ""); st.Verdict != verdictMoving {
				t.Fatalf("before the newer control: verdict %q, want moving", st.Verdict)
			}

			// A charge send takes over at t0+80 and the device turns round.
			h.ctl.snap.CurrentStatus = tc.status
			h.ctl.snap.DateTime = t0 + 80
			h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48,
				float64(t0+100), 49, float64(t0+130), 50)...)
			h.clk.advance(60 * time.Second)

			st := h.status(t, sent.ID, "")
			if st.Verdict != verdictMoving || st.ReportCount != 4 {
				t.Errorf("after the newer control: verdict %q with %d reports, want moving with 4", st.Verdict, st.ReportCount)
			}

			h.ctl.snap = nil
			if st := h.status(t, sent.ID, ""); st.ControlState != "gone" || st.Verdict != verdictMoving {
				t.Errorf("after the control left the store: state %q verdict %q, want gone and moving", st.ControlState, st.Verdict)
			}
		})
	}
}

// A device moving the commanded way and later turning is revised to wrong
// way while the send is still in force: the latest two steps decide.
func TestControlStatusMovingLaterTurnsWrongWayWhileInForce(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48)...)
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.clk.advance(75 * time.Second)
	if st := h.status(t, sent.ID, ""); st.Verdict != verdictMoving {
		t.Fatalf("first read: verdict %q, want moving", st.Verdict)
	}

	h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+40), 49, float64(t0+70), 48,
		float64(t0+100), 49, float64(t0+130), 50)...)
	h.clk.advance(60 * time.Second)

	if st := h.status(t, sent.ID, ""); st.Verdict != verdictWrongWay || st.ControlState != "active" {
		t.Errorf("verdict %q state %q, want wrong_way while active", st.Verdict, st.ControlState)
	}
}

// Judging starts from the device's first Received response, not a later one.
func TestControlStatusJudgesFromTheEarliestReceivedResponse(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.reports("_bat-1", samples(float64(t0-30), 50, float64(t0+10), 49, float64(t0+20), 48)...)
	h.ctl.responses = []sep2embed.ResponseSnapshot{
		{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived, CreatedDateTime: t0 + 5},
		{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived, CreatedDateTime: t0 + 50},
	}
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.clk.advance(25 * time.Second)

	if st := h.status(t, sent.ID, ""); st.Verdict != verdictMoving {
		t.Errorf("verdict %q, want moving judged from t0+5", st.Verdict)
	}
}

// A Received response dated before the send does not widen the judged
// window back past the start: a report before it stays a baseline.
func TestControlStatusNeverJudgesFromBeforeTheStart(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	// A discharge: the report at 0 before the start would read as reached
	// if it were judged.
	h.reports("_bat-1", samples(float64(t0-150), 10, float64(t0-50), 0, float64(t0+10), 5)...)
	h.ctl.responses = []sep2embed.ResponseSnapshot{
		{Subject: "ctl-mrid-1", Status: sep2.ResponseStatusEventReceived, CreatedDateTime: t0 - 100},
	}
	sent := h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.clk.advance(20 * time.Second)

	if st := h.status(t, sent.ID, ""); st.Verdict != verdictWaiting {
		t.Errorf("verdict %q, want waiting", st.Verdict)
	}
}

// When the status read fails on a send that carries a warning, the fallback
// body still carries it.
func TestControlSendStatusFallbackKeepsTheWarning(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.err = fmt.Errorf("%w: %w", sep2embed.ErrControlFollowOnNotWritten, errors.New("store down"))
	h.ctl.partial, h.ctl.noFollow = true, true
	h.ctl.respErr = errors.New("responses unavailable")

	st := h.send(t, `{"mrid":"_bat-1","watts":2000}`)

	if st.ControlState != "unknown" || st.Warning != controlNoStopWarning {
		t.Errorf("state %q warning %q, want unknown with the no-stop warning", st.ControlState, st.Warning)
	}
}
