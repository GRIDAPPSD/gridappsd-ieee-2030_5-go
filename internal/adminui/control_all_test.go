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
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

var errBoom = errors.New("boom: secret detail")

const allRoute = "/apps/soc/api/control/all"

func allHarness(t *testing.T, mrids ...string) *ctlHarness {
	t.Helper()
	h := newCtlHarness(t, Config{})
	reg := &fakeRegistry{}
	for _, m := range mrids {
		reg.entries = append(reg.entries, registry.Entry{MRID: m, Name: "name-" + m})
	}
	h.s.registry = reg
	return h
}

func postAllFrom(t *testing.T, h http.Handler, remote, contentType, body, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, allRoute, strings.NewReader(body))
	req.RemoteAddr = remote
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (h *ctlHarness) sendAll(t *testing.T, body string) controlAllResponse {
	t.Helper()
	rec := postAllFrom(t, h.s.Handler(), "127.0.0.1:40000", "application/json", body, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST all %s = %d: %s", body, rec.Code, rec.Body)
	}
	var out controlAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST all body: %v: %s", err, rec.Body)
	}
	return out
}

func TestControlAllGivesEveryDeviceItsOwnControlWithTheSameWattsAndDuration(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a", "_b", "_c")

	out := h.sendAll(t, `{"watts":100,"durationSeconds":120}`)

	want := []controlCall{{"_a", 100, 120}, {"_b", 100, 120}, {"_c", 100, 120}}
	if !slices.Equal(h.ctl.calls, want) {
		t.Errorf("calls = %+v, want %+v", h.ctl.calls, want)
	}
	for i, d := range h.ctl.deltas {
		if d.Attribute != "DERControl.DERControlBase.opModTargetW" || d.Value.(map[string]any)["multiplier"] != 0.0 {
			t.Errorf("delta %d = %+v, want opModTargetW with multiplier 0", i, d)
		}
	}
	if out.Total != 3 || out.Succeeded != 3 || out.Failed != 0 || out.Watts != 100 || out.DurationSeconds != 120 || out.Stop {
		t.Errorf("summary = %+v", out)
	}
	if len(out.Results) != 3 {
		t.Fatalf("results = %+v, want 3", out.Results)
	}
	seen := map[string]bool{}
	for i, r := range out.Results {
		if r.MRID != want[i].mrid || r.Name != "name-"+want[i].mrid || !r.OK || r.Error != "" || len(r.ID) != 16 {
			t.Errorf("result %d = %+v", i, r)
		}
		if seen[r.ID] {
			t.Errorf("result %d reuses status id %s", i, r.ID)
		}
		seen[r.ID] = true
		st := h.status(t, r.ID, "")
		if st.MRID != r.MRID || st.Watts != 100 || st.DurationSeconds != 120 {
			t.Errorf("status of %s = mrid %s watts %d duration %d", r.ID, st.MRID, st.Watts, st.DurationSeconds)
		}
	}
}

func TestControlAllDefaultsTheDurationAndStopSendsZeroWattsToEveryDevice(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a", "_b")

	h.sendAll(t, `{"watts":-250}`)
	out := h.sendAll(t, `{"stop":true}`)

	want := []controlCall{{"_a", -250, 300}, {"_b", -250, 300}, {"_a", 0, 300}, {"_b", 0, 300}}
	if !slices.Equal(h.ctl.calls, want) {
		t.Errorf("calls = %+v, want %+v", h.ctl.calls, want)
	}
	if !out.Stop || out.Watts != 0 {
		t.Errorf("stop summary = %+v", out)
	}
}

func TestControlAllReportsEachFailureAndKeepsGoing(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a", "_b", "_c")
	h.ctl.errFor = map[string]error{"_b": sep2embed.ErrUnknownControlDevice}

	out := h.sendAll(t, `{"watts":100}`)

	if len(h.ctl.calls) != 3 {
		t.Fatalf("embed called %d times, want 3: a failed device must not stop the rest", len(h.ctl.calls))
	}
	if out.Total != 3 || out.Succeeded != 2 || out.Failed != 1 {
		t.Errorf("summary = %+v, want 3 total, 2 succeeded, 1 failed", out)
	}
	b := out.Results[1]
	if b.MRID != "_b" || b.OK || b.ID != "" || b.Error != "unknown device" {
		t.Errorf("failed result = %+v, want _b not ok, no id, the fixed unknown-device message", b)
	}
	for _, i := range []int{0, 2} {
		if r := out.Results[i]; !r.OK || r.ID == "" || r.Error != "" {
			t.Errorf("result %d = %+v, want ok with an id", i, r)
		}
	}
}

func TestControlAllFailureMessagesAreTheFixedOnesNeverTheErrorText(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a")
	h.ctl.errFor = map[string]error{"_a": errBoom}

	out := h.sendAll(t, `{"watts":100}`)

	if out.Succeeded != 0 || out.Failed != 1 || out.Results[0].Error != "control could not be issued" {
		t.Errorf("out = %+v", out)
	}
}

func TestControlAllKeepsAFollowOnWarningPerDevice(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a")
	h.ctl.err, h.ctl.partial, h.ctl.noFollow = sep2embed.ErrControlFollowOnNotWritten, true, true

	out := h.sendAll(t, `{"watts":100}`)

	if r := out.Results[0]; !r.OK || r.Warning != controlNoStopWarning || r.ID == "" {
		t.Errorf("result = %+v, want ok with the no-stop warning", r)
	}
}

func TestControlAllTakesOneRateLimitToken(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a", "_b", "_c", "_d", "_e")

	for i := 0; i < controlBurst-1; i++ {
		h.sendAll(t, `{"watts":5}`)
	}
	if got, want := h.ctl.callCount(), 5*(controlBurst-1); got != want {
		t.Fatalf("embed calls = %d, want %d", got, want)
	}
	// One token is left: a single send takes it, then both routes are limited.
	h.send(t, `{"mrid":"_a","watts":5}`)
	all := postAllFrom(t, h.s.Handler(), "203.0.113.9:1", "application/json", `{"watts":5}`, "localhost")
	one := postControl(t, h.s.Handler(), "application/json", `{"mrid":"_a","watts":5}`, "localhost")
	for name, rec := range map[string]*httptest.ResponseRecorder{"all": all, "single": one} {
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), controlRateLimited) || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("%s past the burst: status %d body %s", name, rec.Code, rec.Body)
		}
	}
	if got, want := h.ctl.callCount(), 5*(controlBurst-1)+1; got != want {
		t.Errorf("embed calls = %d, want %d: a limited request must not issue", got, want)
	}
}

func TestControlAllKeepsTheHostJSONAndBodyRules(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a")
	for _, tc := range []struct {
		name, ctype, body, host string
		want                    int
	}{
		{"foreign host", "application/json", `{"watts":5}`, "evil.example", http.StatusForbidden},
		{"form content type", "application/x-www-form-urlencoded", `{"watts":5}`, "localhost", http.StatusUnsupportedMediaType},
		{"no content type", "", `{"watts":5}`, "localhost", http.StatusUnsupportedMediaType},
		{"oversize", "application/json", `{"watts":5,"pad":"` + strings.Repeat("x", maxControlBody) + `"}`, "localhost", http.StatusRequestEntityTooLarge},
		{"unknown field mrid", "application/json", `{"mrid":"_a","watts":5}`, "localhost", http.StatusBadRequest},
		{"two objects", "application/json", `{"watts":5}{"watts":6}`, "localhost", http.StatusBadRequest},
		{"no watts", "application/json", `{}`, "localhost", http.StatusBadRequest},
		{"stop with watts", "application/json", `{"stop":true,"watts":5}`, "localhost", http.StatusBadRequest},
		{"duration too short", "application/json", `{"watts":5,"durationSeconds":59}`, "localhost", http.StatusBadRequest},
		{"duration too long", "application/json", `{"watts":5,"durationSeconds":3601}`, "localhost", http.StatusBadRequest},
	} {
		rec := postAllFrom(t, h.s.Handler(), "127.0.0.1:40000", tc.ctype, tc.body, tc.host)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.name, rec.Code, tc.want, rec.Body)
		}
	}
	if h.ctl.callCount() != 0 {
		t.Errorf("embed called %d times for refused requests", h.ctl.callCount())
	}
}

func TestControlAllWithNoRegisteredDevicesSendsNothing(t *testing.T) {
	t.Parallel()
	h := allHarness(t)

	rec := postAllFrom(t, h.s.Handler(), "127.0.0.1:40000", "application/json", `{"watts":5}`, "localhost")

	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no registered devices") {
		t.Errorf("status %d body %s, want 404 no registered devices", rec.Code, rec.Body)
	}
	if h.ctl.callCount() != 0 {
		t.Errorf("embed called %d times", h.ctl.callCount())
	}
}

func TestControlAllAcceptedSendIsLoggedOnceWithTheCaller(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := allHarness(t, "_a", "_b")

	rec := postAllFrom(t, h.s.Handler(), "203.0.113.9:51234", "application/json", `{"watts":-700,"durationSeconds":90}`, "localhost")
	refused := postAllFrom(t, h.s.Handler(), "192.0.2.1:1", "application/json", `{}`, "localhost")

	if rec.Code != http.StatusOK || refused.Code != http.StatusBadRequest {
		t.Fatalf("statuses %d %d, want 200 400", rec.Code, refused.Code)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, "watts control accepted") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d accepted lines, want 1: %q", len(lines), lines)
	}
	for _, w := range []string{"203.0.113.9:51234", "device=all", "devices=2", "ok=2", "watts=-700", "duration=90s"} {
		if !strings.Contains(lines[0], w) {
			t.Errorf("line %q lacks %q", lines[0], w)
		}
	}
	if strings.Contains(buf.String(), "192.0.2.1") {
		t.Errorf("a refused request was logged as accepted: %s", buf.String())
	}
}

// The single route is not changed by the all route: a device whose mRID is
// "all" is still reached through it, and its reply keeps the status shape.
func TestControlSingleSendIsUnchangedByTheAllRoute(t *testing.T) {
	t.Parallel()
	h := allHarness(t, "_a", "_b")

	st := h.send(t, `{"mrid":"all","watts":2000,"durationSeconds":90}`)

	if want := []controlCall{{"all", 2000, 90}}; !slices.Equal(h.ctl.calls, want) {
		t.Errorf("calls = %+v, want %+v: the single route must send to the one device only", h.ctl.calls, want)
	}
	if st.MRID != "all" || st.Watts != 2000 || st.DurationSeconds != 90 || len(st.ID) != 16 {
		t.Errorf("status = %+v", st)
	}
	rec := postControl(t, h.s.Handler(), "application/json", `{"watts":5}`, "localhost")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "mrid is required") {
		t.Errorf("single send without mrid: %d %s, want the unchanged 400", rec.Code, rec.Body)
	}
}

// cancelAfterFirst cancels the request context once the first device is sent
// and records whether each send still had a live context.
type cancelAfterFirst struct {
	ControlSource
	cancel context.CancelFunc
	live   []bool
}

func (c *cancelAfterFirst) ApplyControlFor(ctx context.Context, d sep2embed.ControlDelta, seconds uint32) (sep2embed.ControlSend, error) {
	c.live = append(c.live, ctx.Err() == nil)
	send, err := c.ControlSource.ApplyControlFor(ctx, d, seconds)
	if len(c.live) == 1 {
		c.cancel()
	}
	return send, err
}

func TestControlAllCompletesEveryDeviceWhenTheRequestIsCancelledMidLoop(t *testing.T) {
	h := allHarness(t, "_a", "_b", "_c")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrap := &cancelAfterFirst{ControlSource: h.ctl, cancel: cancel}
	h.s.control = wrap

	req := httptest.NewRequest(http.MethodPost, allRoute, strings.NewReader(`{"watts":100}`)).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)

	var out controlAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v: %s", err, rec.Body)
	}
	if len(h.ctl.calls) != 3 || out.Succeeded != 3 || out.Failed != 0 {
		t.Errorf("calls=%d summary=%+v, want all 3 devices sent", len(h.ctl.calls), out)
	}
	for i, ok := range wrap.live {
		if !ok {
			t.Errorf("send %d ran on a cancelled context", i)
		}
	}
}

func TestControlAllLogLineCarriesEveryDeviceIDOrFailure(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := allHarness(t, "_a", "_b", "_c")
	h.ctl.errFor = map[string]error{"_b": errBoom}

	out := h.sendAll(t, `{"watts":100}`)

	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "watts control accepted") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no accepted line: %s", buf.String())
	}
	for _, w := range []string{"_a=" + out.Results[0].ID, "_b=failed", "_c=" + out.Results[2].ID} {
		if !strings.Contains(line, w) {
			t.Errorf("line %q lacks %q", line, w)
		}
	}
	if strings.Contains(line, "secret detail") {
		t.Errorf("line carries the error text: %q", line)
	}
}

func TestControlHistoryHoldsSeveralFleetWideSends(t *testing.T) {
	const devices, sends = 60, 5
	mrids := make([]string, devices)
	for i := range mrids {
		mrids[i] = fmt.Sprintf("_d%02d", i)
	}
	h := allHarness(t, mrids...)
	h.s.controlLimit = newTokenBucket(h.clk.now())
	var ids []string
	for i := 0; i < sends; i++ {
		h.clk.t = h.clk.t.Add(10 * time.Second)
		for _, r := range h.sendAll(t, `{"watts":100}`).Results {
			ids = append(ids, r.ID)
		}
	}
	for i, id := range ids {
		if _, ok := h.s.controls.get(id); !ok {
			t.Fatalf("record %d of %d was evicted", i, len(ids))
		}
	}
}
