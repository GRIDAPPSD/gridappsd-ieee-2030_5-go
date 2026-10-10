package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/socsend"
)

const brokerSecret = "broker-user=bridge broker-password=hunter2"

type socCall struct {
	kind    string
	mrid    string
	percent int
	hold    time.Duration
}

type fakeSoC struct {
	mu      sync.Mutex
	calls   []socCall
	err     error
	status  map[string]socsend.Status
	started chan struct{}
	stopped chan struct{}
}

func newFakeSoC() *fakeSoC {
	return &fakeSoC{status: map[string]socsend.Status{}, started: make(chan struct{}), stopped: make(chan struct{})}
}

func (f *fakeSoC) record(c socCall) (socsend.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.err != nil {
		return socsend.Status{}, f.err
	}
	return socsend.Status{ID: "id-1", MRID: c.mrid, Kind: c.kind, Percent: c.percent, HoldSeconds: int(c.hold / time.Second), Verdict: socsend.VerdictPending}, nil
}

func (f *fakeSoC) Send(_ context.Context, mrid string, percent int, hold time.Duration) (socsend.Status, error) {
	return f.record(socCall{"send", mrid, percent, hold})
}

func (f *fakeSoC) Clear(_ context.Context, mrid string) (socsend.Status, error) {
	return f.record(socCall{"clear", mrid, 0, 0})
}

func (f *fakeSoC) Status(_ context.Context, id string) (socsend.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.status[id]
	return st, ok
}

func (f *fakeSoC) Run(ctx context.Context) error {
	close(f.started)
	<-ctx.Done()
	close(f.stopped)
	return nil
}

func (f *fakeSoC) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func socServer(t *testing.T, cfg Config, soc SoCSource) *Server {
	t.Helper()
	if cfg.Key == "" && !cfg.InsecureNoKey {
		cfg.Key = testKey
	}
	src := testSources()
	src.SoC = soc
	return newServer(t, cfg, src)
}

func postSoC(t *testing.T, h http.Handler, contentType, body, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apps/soc/api/soc", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSoCSendNeedsNoCredentialAndPassesValueAndDefaultHold(t *testing.T) {
	t.Parallel()
	f := newFakeSoC()
	s := socServer(t, Config{}, f)

	rec := postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","percent":80}`, "localhost")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with no Authorization header: %s", rec.Code, rec.Body)
	}
	want := []socCall{{"send", "_pv-1", 80, 60 * time.Second}}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %+v, want %+v", f.calls, want)
	}
	var st socsend.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.ID != "id-1" || st.Percent != 80 || st.HoldSeconds != 60 {
		t.Errorf("body = %s (%v), want the service's status", rec.Body, err)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
}

func TestSoCSendHoldAndPercentZeroAreCarriedThrough(t *testing.T) {
	t.Parallel()
	f := newFakeSoC()
	s := socServer(t, Config{}, f)

	rec := postSoC(t, s.Handler(), "application/json; charset=utf-8", `{"mrid":"_pv-1","percent":0,"holdSeconds":3600}`, "localhost:8444")

	if rec.Code != http.StatusOK || !slices.Equal(f.calls, []socCall{{"send", "_pv-1", 0, time.Hour}}) {
		t.Errorf("status %d calls %+v, want 200 and percent 0 hold 1h", rec.Code, f.calls)
	}
}

func TestSoCClearCallsClear(t *testing.T) {
	t.Parallel()
	f := newFakeSoC()
	s := socServer(t, Config{}, f)

	rec := postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","clear":true}`, "localhost")

	if rec.Code != http.StatusOK || !slices.Equal(f.calls, []socCall{{"clear", "_pv-1", 0, 0}}) {
		t.Errorf("status %d calls %+v, want 200 and one clear", rec.Code, f.calls)
	}
}

func TestSoCSendRefusesMalformedRequestsWithoutCallingTheService(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contentType, body string
		want                    int
	}{
		{"text/plain form post", "text/plain", `{"mrid":"_pv-1","percent":80}`, http.StatusUnsupportedMediaType},
		{"urlencoded form post", "application/x-www-form-urlencoded", `mrid=_pv-1&percent=80`, http.StatusUnsupportedMediaType},
		{"no content type", "", `{"mrid":"_pv-1","percent":80}`, http.StatusUnsupportedMediaType},
		{"percent missing", "application/json", `{"mrid":"_pv-1"}`, http.StatusBadRequest},
		{"fractional percent", "application/json", `{"mrid":"_pv-1","percent":50.5}`, http.StatusBadRequest},
		{"percent as string", "application/json", `{"mrid":"_pv-1","percent":"80"}`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"mrid":"_pv-1","percent":80,"x":1}`, http.StatusBadRequest},
		{"clear with percent", "application/json", `{"mrid":"_pv-1","clear":true,"percent":80}`, http.StatusBadRequest},
		{"clear with hold", "application/json", `{"mrid":"_pv-1","clear":true,"holdSeconds":5}`, http.StatusBadRequest},
		{"negative hold", "application/json", `{"mrid":"_pv-1","percent":80,"holdSeconds":-1}`, http.StatusBadRequest},
		{"hold past an hour", "application/json", `{"mrid":"_pv-1","percent":80,"holdSeconds":3601}`, http.StatusBadRequest},
		{"hold that overflows a duration", "application/json", `{"mrid":"_pv-1","percent":80,"holdSeconds":9223372036854775807}`, http.StatusBadRequest},
		{"not json", "application/json", `percent=80`, http.StatusBadRequest},
		{"two objects", "application/json", `{"mrid":"_pv-1","percent":80}{"mrid":"_pv-2","percent":1}`, http.StatusBadRequest},
		{"oversized body", "application/json", `{"mrid":"` + strings.Repeat("a", maxSoCBody) + `","percent":80}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSoC()
			s := socServer(t, Config{}, f)

			rec := postSoC(t, s.Handler(), tc.contentType, tc.body, "localhost")

			if rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if f.callCount() != 0 {
				t.Errorf("service called %d times, want 0", f.callCount())
			}
		})
	}
}

func TestSoCSendMapsServiceErrorsToFixedMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"percent range", socsend.ErrPercentRange, http.StatusBadRequest},
		{"hold range", socsend.ErrHoldRange, http.StatusBadRequest},
		{"unknown device", socsend.ErrUnknownDevice, http.StatusNotFound},
		{"publish failed", errors.Join(socsend.ErrPublishFailed, errors.New(brokerSecret)), http.StatusBadGateway},
		{"anything else", errors.New(brokerSecret), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSoC()
			f.err = tc.err
			s := socServer(t, Config{}, f)

			rec := postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","percent":80}`, "localhost")

			if rec.Code != tc.code {
				t.Errorf("status %d, want %d", rec.Code, tc.code)
			}
			if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "broker-user") {
				t.Errorf("body %s carries the transport error text", rec.Body)
			}
		})
	}
}

func TestSoCRoutesKeepTheHostAllowlist(t *testing.T) {
	t.Parallel()
	f := newFakeSoC()
	f.status["id-1"] = socsend.Status{ID: "id-1"}
	s := socServer(t, Config{}, f)

	post := postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","percent":80}`, "evil.example")
	get := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/soc/id-1", "", "evil.example")

	if post.Code != http.StatusForbidden || get.Code != http.StatusForbidden {
		t.Errorf("POST %d GET %d from a foreign Host, want 403 for both", post.Code, get.Code)
	}
	if f.callCount() != 0 {
		t.Errorf("service called %d times for a foreign Host, want 0", f.callCount())
	}
}

func TestSoCStatusReturnsTheLedgerEntryWithoutCredential(t *testing.T) {
	t.Parallel()
	f := newFakeSoC()
	posted := time.Date(2026, 10, 9, 12, 0, 5, 0, time.UTC)
	f.status["id-1"] = socsend.Status{ID: "id-1", MRID: "_pv-1", Kind: "send", Percent: 80, HoldSeconds: 60,
		SentAt: posted.Add(-5 * time.Second), PostedAt: &posted, Verdict: socsend.VerdictPending}
	s := socServer(t, Config{}, f)

	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/soc/id-1", "", "localhost")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	for k, want := range map[string]any{"id": "id-1", "mrid": "_pv-1", "percent": 80.0, "holdSeconds": 60.0, "verdict": "pending", "postedAt": "2026-10-09T12:00:05Z"} {
		if raw[k] != want {
			t.Errorf("%s = %v, want %v", k, raw[k], want)
		}
	}
	if _, present := raw["seenAt"]; present {
		t.Errorf("seenAt present for a stage not reached: %v", raw["seenAt"])
	}
	if miss := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/soc/nope", "", "localhost"); miss.Code != http.StatusNotFound {
		t.Errorf("unknown id status %d, want 404", miss.Code)
	}
}

func TestSoCRoutesAbsentWithoutAService(t *testing.T) {
	t.Parallel()
	s := socServer(t, Config{}, nil)

	rec := postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","percent":80}`, "localhost")

	if rec.Code == http.StatusOK {
		t.Errorf("POST answered 200 with no SoC service configured")
	}
}

func TestSoCResponsesNeverCarryTheAdminKey(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{{Key: testKey}, {InsecureNoKey: true}} {
		f := newFakeSoC()
		f.status["id-1"] = socsend.Status{ID: "id-1", MRID: "_pv-1"}
		s := socServer(t, cfg, f)
		key := s.cfg.Key
		bodies := []string{
			postSoC(t, s.Handler(), "application/json", `{"mrid":"_pv-1","percent":80}`, "localhost").Body.String(),
			postSoC(t, s.Handler(), "text/plain", `x`, "localhost").Body.String(),
			doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/soc/id-1", "", "localhost").Body.String(),
			doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/soc/nope", "", "localhost").Body.String(),
		}
		for _, b := range bodies {
			if strings.Contains(b, key) {
				t.Errorf("insecure=%v: a response carries the admin key: %s", cfg.InsecureNoKey, b)
			}
		}
	}
}

func TestPlaneMountsNoSoCRoute(t *testing.T) {
	t.Parallel()
	s := socServer(t, Config{}, newFakeSoC())
	for _, p := range s.planePatterns {
		if strings.Contains(p, "/apps/soc") {
			t.Errorf("the plane mounts %q, which the SoC route would shadow or be shadowed by", p)
		}
	}
}

func TestRunStartsAndStopsTheSoCFollowLoop(t *testing.T) {
	t.Parallel()
	f := newFakeSoC()
	s := socServer(t, Config{}, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not start the SoC follow loop")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case <-f.stopped:
	default:
		t.Errorf("Run returned before the follow loop stopped")
	}
}

func postSoCFrom(t *testing.T, h http.Handler, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apps/soc/api/soc", strings.NewReader(body))
	req.RemoteAddr = remote
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Not parallel: it swaps the process-wide log writer.
func TestSoCAcceptedSendAndClearAreLoggedWithTheCaller(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := socServer(t, Config{}, newFakeSoC())

	send := postSoCFrom(t, s.Handler(), "203.0.113.9:51234", `{"mrid":"_pv-1","percent":80,"holdSeconds":90}`)
	clr := postSoCFrom(t, s.Handler(), "198.51.100.7:40000", `{"mrid":"_pv-2","clear":true}`)
	refused := postSoCFrom(t, s.Handler(), "192.0.2.1:1", `{"mrid":"_pv-3"}`)

	if send.Code != http.StatusOK || clr.Code != http.StatusOK || refused.Code != http.StatusBadRequest {
		t.Fatalf("statuses %d %d %d, want 200 200 400", send.Code, clr.Code, refused.Code)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, "state of charge") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d state of charge log lines, want 2 (one per accepted request): %q", len(lines), lines)
	}
	for i, want := range [][]string{
		{"send accepted", "203.0.113.9:51234", "device=_pv-1", "percent=80", "hold=90s"},
		{"clear accepted", "198.51.100.7:40000", "device=_pv-2", "percent=0", "hold=0s"},
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

func limitedServer(t *testing.T) (*Server, *fakeSoC, *stepClock) {
	t.Helper()
	f := newFakeSoC()
	s := socServer(t, Config{}, f)
	clk := &stepClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	s.now = clk.now
	s.socLimit = newTokenBucket(clk.now())
	return s, f, clk
}

func TestSoCSendAndClearShareARateLimit(t *testing.T) {
	t.Parallel()
	s, f, clk := limitedServer(t)

	for i := 0; i < socBurst; i++ {
		body := `{"mrid":"_pv-1","percent":80}`
		if i%2 == 1 {
			body = `{"mrid":"_pv-1","clear":true}`
		}
		if rec := postSoCFrom(t, s.Handler(), "203.0.113.9:1", body); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 inside the burst", i, rec.Code)
		}
	}
	over := postSoCFrom(t, s.Handler(), "203.0.113.9:1", `{"mrid":"_pv-1","percent":80}`)
	overClear := postSoCFrom(t, s.Handler(), "203.0.113.10:1", `{"mrid":"_pv-1","clear":true}`)

	for name, rec := range map[string]*httptest.ResponseRecorder{"send": over, "clear": overClear} {
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s past the burst: status %d, want 429", name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), socRateLimited) || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("%s past the burst: body %s Retry-After %q", name, rec.Body, rec.Header().Get("Retry-After"))
		}
	}
	if f.callCount() != socBurst {
		t.Errorf("service called %d times, want %d: a limited request must not publish", f.callCount(), socBurst)
	}

	clk.advance(socRefillEvery)
	if rec := postSoCFrom(t, s.Handler(), "203.0.113.9:1", `{"mrid":"_pv-1","percent":80}`); rec.Code != http.StatusOK {
		t.Errorf("after one refill interval: status %d, want 200", rec.Code)
	}
	if rec := postSoCFrom(t, s.Handler(), "203.0.113.9:1", `{"mrid":"_pv-1","percent":80}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a second request in the same interval: status %d, want 429", rec.Code)
	}
}

func TestSoCRateLimitNeverBanksMoreThanTheBurst(t *testing.T) {
	t.Parallel()
	s, _, clk := limitedServer(t)
	clk.advance(24 * time.Hour)

	ok := 0
	for i := 0; i < socBurst+5; i++ {
		if postSoCFrom(t, s.Handler(), "203.0.113.9:1", `{"mrid":"_pv-1","percent":80}`).Code == http.StatusOK {
			ok++
		}
	}

	if ok != socBurst {
		t.Errorf("%d requests passed after a long idle, want exactly the burst of %d", ok, socBurst)
	}
}

func TestSoCStatusReadsAreNotRateLimited(t *testing.T) {
	t.Parallel()
	s, f, _ := limitedServer(t)
	f.status["id-1"] = socsend.Status{ID: "id-1"}
	for i := 0; i < socBurst+5; i++ {
		postSoCFrom(t, s.Handler(), "203.0.113.9:1", `{"mrid":"_pv-1","percent":80}`)
	}

	rec := doRequest(t, s.Handler(), http.MethodGet, "/apps/soc/api/soc/id-1", "", "localhost")

	if rec.Code != http.StatusOK {
		t.Errorf("status read after the send limit was hit: %d, want 200", rec.Code)
	}
}
