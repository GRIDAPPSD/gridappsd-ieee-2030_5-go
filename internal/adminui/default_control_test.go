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

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

type defaultCall struct {
	mrid  string
	watts int64
	// named holds the probe ids the page predicate answered true for.
	named []string
}

// defaultProbeIDs are the store ids the fake asks the page predicate about.
var defaultProbeIDs = []string{"ctl-1", "fo-1", "bus-1", "other"}

func (f *fakeControl) SetDefaultTargetW(_ context.Context, mrid string, watts int64, pageIssued func(string) bool) (sep2embed.DefaultTargetSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := defaultCall{mrid: mrid, watts: watts}
	for _, id := range defaultProbeIDs {
		if pageIssued != nil && pageIssued(id) {
			call.named = append(call.named, id)
		}
	}
	f.sets = append(f.sets, call)
	set := sep2embed.DefaultTargetSet{Cancelled: f.setCancelled}
	if f.setErr != nil && !errors.Is(f.setErr, sep2embed.ErrDefaultCancelIncomplete) {
		return sep2embed.DefaultTargetSet{}, f.setErr
	}
	if f.defaults == nil {
		f.defaults = map[string]*sep2.ActivePower{}
	}
	f.defaults[mrid] = &sep2.ActivePower{Value: int16(watts)}
	return set, f.setErr
}

func (f *fakeControl) ClearDefaultTargetW(_ context.Context, mrid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears = append(f.clears, mrid)
	if f.clearErr != nil {
		return f.clearErr
	}
	delete(f.defaults, mrid)
	return nil
}

func (f *fakeControl) DefaultTargetW(_ context.Context, mrid string) (*sep2.ActivePower, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.defaultErrFor[mrid]; err != nil {
		return nil, err
	}
	return f.defaults[mrid], nil
}

func postDefaultFrom(t *testing.T, h http.Handler, remote, contentType, body, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apps/soc/api/default", strings.NewReader(body))
	req.RemoteAddr = remote
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postDefault(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postDefaultFrom(t, h, "127.0.0.1:40000", "application/json", body, "localhost")
}

func decodeRaw(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body: %v: %s", err, rec.Body)
	}
	return raw
}

func TestDefaultSetPassesWattsAndNamesOnlyPageControls(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.send(t, `{"mrid":"_bat-1","watts":2000}`)
	h.ctl.setCancelled = []string{"ctl-1", "fo-1"}

	rec := postDefault(t, h.s.Handler(), `{"mrid":"_bat-1","watts":-1500}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(h.ctl.sets) != 1 {
		t.Fatalf("set calls = %+v, want 1", h.ctl.sets)
	}
	call := h.ctl.sets[0]
	if call.mrid != "_bat-1" || call.watts != -1500 {
		t.Errorf("set call = %+v, want _bat-1 -1500", call)
	}
	// The page ledger holds the send's control and follow-on; nothing else
	// may be named, bus or unknown.
	if want := []string{"ctl-1", "fo-1"}; !slices.Equal(call.named, want) {
		t.Errorf("predicate named %v, want %v", call.named, want)
	}
	raw := decodeRaw(t, rec)
	if raw["mrid"] != "_bat-1" || raw["default"] != -1500.0 || raw["cancelledControls"] != 2.0 {
		t.Errorf("body = %v, want mrid _bat-1, default -1500, cancelledControls 2", raw)
	}
	if _, ok := raw["warning"]; ok {
		t.Errorf("clean set carries a warning: %v", raw)
	}
}

func TestDefaultClearCallsClearAndReadsBackNull(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.defaults = map[string]*sep2.ActivePower{"_bat-1": {Value: 900}}

	rec := postDefault(t, h.s.Handler(), `{"mrid":"_bat-1","clear":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !slices.Equal(h.ctl.clears, []string{"_bat-1"}) || len(h.ctl.sets) != 0 {
		t.Errorf("clears %v sets %v, want one clear and no set", h.ctl.clears, h.ctl.sets)
	}
	raw := decodeRaw(t, rec)
	if v, ok := raw["default"]; !ok || v != nil {
		t.Errorf("default = %#v (present %v), want null", v, ok)
	}
	if raw["cancelledControls"] != 0.0 {
		t.Errorf("cancelledControls = %v, want 0", raw["cancelledControls"])
	}
}

func TestDefaultRefusesBadRequestsAndCallsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contentType, body string
		want                    int
	}{
		{"missing mrid", "application/json", `{"watts":5}`, http.StatusBadRequest},
		{"neither watts nor clear", "application/json", `{"mrid":"_b"}`, http.StatusBadRequest},
		{"clear false and no watts", "application/json", `{"mrid":"_b","clear":false}`, http.StatusBadRequest},
		{"both watts and clear", "application/json", `{"mrid":"_b","watts":5,"clear":true}`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"mrid":"_b","watts":5,"durationSeconds":60}`, http.StatusBadRequest},
		{"fractional watts", "application/json", `{"mrid":"_b","watts":1.5}`, http.StatusBadRequest},
		{"two objects", "application/json", `{"mrid":"_b","watts":5}{"mrid":"_b","watts":6}`, http.StatusBadRequest},
		{"form content type", "application/x-www-form-urlencoded", `mrid=_b&watts=5`, http.StatusUnsupportedMediaType},
		{"too large", "application/json", `{"mrid":"` + strings.Repeat("x", maxControlBody) + `","watts":5}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			rec := postDefaultFrom(t, h.s.Handler(), "127.0.0.1:1", tc.contentType, tc.body, "localhost")
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if len(h.ctl.sets) != 0 || len(h.ctl.clears) != 0 {
				t.Errorf("embed called: sets %v clears %v", h.ctl.sets, h.ctl.clears)
			}
		})
	}
}

func TestDefaultMapsEmbedErrorsToFixedMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want int
		msg  string
	}{
		{"unknown device", fmt.Errorf("x: %w", sep2embed.ErrUnknownControlDevice), http.StatusNotFound, "unknown device"},
		{"value out of range", fmt.Errorf("x: %w", sep2embed.ErrControlValueInvalid), http.StatusBadRequest, "watts is outside what a device control can carry"},
		{"store failure", errors.New("secret store detail"), http.StatusInternalServerError, "default could not be set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			h.ctl.setErr = tc.err
			rec := postDefault(t, h.s.Handler(), `{"mrid":"_b","watts":5}`)
			if rec.Code != tc.want || decodeRaw(t, rec)["error"] != tc.msg {
				t.Errorf("set: status %d body %s, want %d %q", rec.Code, rec.Body, tc.want, tc.msg)
			}
			h.ctl.clearErr = tc.err
			rec = postDefault(t, h.s.Handler(), `{"mrid":"_b","clear":true}`)
			want := strings.Replace(tc.msg, "set", "cleared", 1)
			if rec.Code != tc.want || decodeRaw(t, rec)["error"] != want {
				t.Errorf("clear: status %d body %s, want %d %q", rec.Code, rec.Body, tc.want, want)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("error text leaked: %s", rec.Body)
			}
		})
	}
}

func TestDefaultSetWithAFailedCancelWarns(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.setErr = fmt.Errorf("%w: cancel control x: boom", sep2embed.ErrDefaultCancelIncomplete)
	h.ctl.setCancelled = []string{"ctl-1"}

	rec := postDefault(t, h.s.Handler(), `{"mrid":"_b","watts":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	raw := decodeRaw(t, rec)
	if raw["default"] != 300.0 || raw["cancelledControls"] != 1.0 || raw["warning"] != defaultCancelWarning {
		t.Errorf("body = %v, want default 300, 1 cancelled and the cancel warning", raw)
	}
}

// A read-back failure after a successful write is reported, not shown as no
// default.
func TestDefaultReadBackFailureWarnsInsteadOfServingNull(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	h.ctl.defaultErrFor = map[string]error{"_b": errors.New("boom")}

	rec := postDefault(t, h.s.Handler(), `{"mrid":"_b","watts":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	raw := decodeRaw(t, rec)
	if raw["warning"] != defaultReadBackWarning {
		t.Errorf("warning = %v, want the read-back warning", raw["warning"])
	}
}

func TestDefaultSharesTheControlRateLimit(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	for range controlBurst {
		h.send(t, `{"mrid":"_b","watts":5}`)
	}
	rec := postDefault(t, h.s.Handler(), `{"mrid":"_b","watts":5}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), controlRateLimited) {
		t.Errorf("default past the control burst: %d %q %s, want 429", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if len(h.ctl.sets) != 0 {
		t.Errorf("a limited request reached the embed: %v", h.ctl.sets)
	}

	h2 := newCtlHarness(t, Config{})
	for range controlBurst {
		if rec := postDefault(t, h2.s.Handler(), `{"mrid":"_b","clear":true}`); rec.Code != http.StatusOK {
			t.Fatalf("clear = %d: %s", rec.Code, rec.Body)
		}
	}
	if rec := postControl(t, h2.s.Handler(), "application/json", `{"mrid":"_b","watts":5}`, "localhost"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("control send past the default burst = %d, want 429", rec.Code)
	}
}

func TestDefaultKeepsTheHostGateAndPostOnly(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	if rec := postDefaultFrom(t, h.s.Handler(), "127.0.0.1:1", "application/json", `{"mrid":"_b","watts":5}`, "evil.example"); rec.Code == http.StatusOK {
		t.Errorf("foreign Host = %d, want a refusal", rec.Code)
	}
	if rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/default", "", "localhost"); rec.Code == http.StatusOK {
		t.Errorf("GET = %d, want a refusal", rec.Code)
	}
	if len(h.ctl.sets) != 0 {
		t.Errorf("refused requests reached the embed: %v", h.ctl.sets)
	}
}

func TestDefaultAcceptedRequestsAreLoggedWithTheCaller(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := newCtlHarness(t, Config{})

	postDefaultFrom(t, h.s.Handler(), "203.0.113.9:51234", "application/json", `{"mrid":"_pv-1","watts":-700}`, "localhost")
	postDefaultFrom(t, h.s.Handler(), "198.51.100.7:40000", "application/json", `{"mrid":"_pv-2","clear":true}`, "localhost")
	postDefaultFrom(t, h.s.Handler(), "192.0.2.1:1", "application/json", `{"mrid":"_pv-3"}`, "localhost")

	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "default control accepted") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("accepted lines = %q, want 2", lines)
	}
	for i, want := range [][]string{
		{"203.0.113.9:51234", "device=_pv-1", "watts=-700"},
		{"198.51.100.7:40000", "device=_pv-2", "clear"},
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

func TestCardCarriesTheDefaultAndWhatIsInForce(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.device("_bat-2", "Battery 2", 2)
	h.device("_bat-3", "Battery 3", 3)
	h.ctl.defaults = map[string]*sep2.ActivePower{
		"_bat-1": {Multiplier: 1, Value: -150},
		"_bat-2": {Value: 400},
	}
	h.active("_bat-2", "ctl-9", "m9", t0-10, 300, 2500)

	byMRID := map[string]map[string]any{}
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	var raw struct {
		Cards []map[string]any `json:"cards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body: %v: %s", err, rec.Body)
	}
	for _, c := range raw.Cards {
		byMRID[c["mrid"].(string)] = c
	}

	if c := byMRID["_bat-1"]; c["default"] != -1500.0 || !inForceIs(c, "default", -1500) {
		t.Errorf("_bat-1 default %v inForce %v, want -1500 and default -1500", c["default"], c["inForce"])
	}
	if c := byMRID["_bat-2"]; c["default"] != 400.0 || !inForceIs(c, "event", 2500) {
		t.Errorf("_bat-2 default %v inForce %v, want 400 and event 2500", c["default"], c["inForce"])
	}
	c := byMRID["_bat-3"]
	if v, ok := c["default"]; !ok || v != nil {
		t.Errorf("_bat-3 default = %#v (present %v), want null", v, ok)
	}
	if v, ok := c["inForce"]; !ok || v != nil {
		t.Errorf("_bat-3 inForce = %#v (present %v), want null", v, ok)
	}
}

func inForceIs(card map[string]any, kind string, watts float64) bool {
	f, ok := card["inForce"].(map[string]any)
	return ok && f["kind"] == kind && f["watts"] == watts
}

func TestCardsSkipADeviceWhoseDefaultCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.device("_bat-2", "Battery 2", 2)
	h.ctl.defaultErrFor = map[string]error{"_bat-2": errors.New("boom")}

	got := getCards(t, h)
	if got.Total != 2 || got.SkippedDevices != 1 || len(got.Cards) != 1 || got.Cards[0].MRID != "_bat-1" {
		t.Errorf("response = %+v, want total 2, 1 skipped and only _bat-1", got)
	}
}
