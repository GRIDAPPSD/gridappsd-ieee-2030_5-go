package adminui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

func targetW(mult int8, v int16) *sep2embed.DERControlBaseSnapshot {
	return &sep2embed.DERControlBaseSnapshot{OpModTargetW: &sep2.ActivePower{Multiplier: mult, Value: v}}
}

func getActive(t *testing.T, h *ctlHarness) activeControlsResponse {
	t.Helper()
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET active = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != socPageCSP {
		t.Errorf("CSP = %q, want the page policy", got)
	}
	var out activeControlsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v: %s", err, rec.Body)
	}
	return out
}

func setRegistry(h *ctlHarness, entries ...registry.Entry) {
	h.s.registry = &fakeRegistry{entries: entries}
}

func TestActiveControlsListsPageBusAndFollowOnWithFieldValues(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"}, registry.Entry{MRID: "_bat-2", Name: "Battery 2"})

	// A page send: the fake issues ctl-1 with follow-on fo-1.
	h.send(t, `{"mrid":"_bat-1","watts":-2000,"durationSeconds":300}`)
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{
		"_bat-1": {
			{ID: "ctl-1", MRID: "m-page", CurrentStatus: sep2.EventStatusActive, Start: t0, Duration: 300, Base: targetW(0, -2000)},
			{ID: "fo-1", MRID: "m-fo", CurrentStatus: sep2.EventStatusScheduled, Start: t0 + 300, Duration: 900, Base: targetW(0, 0)},
			// Superseded earlier control: not listed.
			{ID: "old", MRID: "m-old", CurrentStatus: sep2.EventStatusSuperseded, Start: t0 - 100, Duration: 300, Base: targetW(0, 500)},
		},
		"_bat-2": {
			{ID: "bus-1", MRID: "m-bus", CurrentStatus: sep2.EventStatusActive, Start: t0 - 60, Duration: 600, Base: targetW(3, 2)},
		},
	}
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "m-bus", Status: sep2.ResponseStatusEventReceived}}
	h.clk.set(t0 + 10)

	got := getActive(t, h)

	want := []activeControl{
		{Name: "Battery 1", MRID: "_bat-1", Watts: -2000, Direction: "charge", Source: "page", State: "active",
			StartsAt: t0, EndsAt: t0 + 300, SecondsLeft: 290, Received: false, ControlMRID: "m-page"},
		{Name: "Battery 1", MRID: "_bat-1", Watts: 0, Direction: "hold", Source: "page", State: "scheduled",
			StartsAt: t0 + 300, EndsAt: t0 + 1200, SecondsLeft: 1190, Received: false, ControlMRID: "m-fo"},
		{Name: "Battery 2", MRID: "_bat-2", Watts: 2000, Direction: "discharge", Source: "bus", State: "active",
			StartsAt: t0 - 60, EndsAt: t0 + 540, SecondsLeft: 530, Received: true, ControlMRID: "m-bus"},
	}
	if got.Now != t0+10 || got.Total != 3 || got.Truncated {
		t.Errorf("now %d total %d truncated %v, want %d 3 false", got.Now, got.Total, got.Truncated, t0+10)
	}
	if len(got.Controls) != len(want) {
		t.Fatalf("controls = %+v, want %d rows", got.Controls, len(want))
	}
	for i := range want {
		if got.Controls[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got.Controls[i], want[i])
		}
	}
}

func TestActiveControlsDropsAnEndedControlStillInTheStore(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{
		"_bat-1": {{ID: "bus-1", MRID: "m-bus", CurrentStatus: sep2.EventStatusActive, Start: t0, Duration: 300, Base: targetW(0, 100)}},
	}
	h.clk.set(t0 + 299)
	if got := getActive(t, h); len(got.Controls) != 1 || got.Controls[0].SecondsLeft != 1 {
		t.Fatalf("at end-1 = %+v, want one row with 1 s left", got.Controls)
	}
	h.clk.set(t0 + 300)
	got := getActive(t, h)
	if len(got.Controls) != 0 || got.Total != 0 {
		t.Errorf("at end = %+v total %d, want none", got.Controls, got.Total)
	}
	if !strings.Contains(string(mustJSON(t, got)), `"controls":[]`) {
		t.Errorf("an empty list must serialize as [], got %s", mustJSON(t, got))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActiveRouteDoesNotCollideWithTheStatusRoute(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	st := h.send(t, `{"mrid":"_bat-1","watts":1000}`)

	if got := getActive(t, h); got.Controls == nil {
		t.Error("active route did not answer with a list")
	}
	if got := h.status(t, st.ID, ""); got.ID != st.ID || got.MRID != "_bat-1" || got.Watts != 1000 {
		t.Errorf("status route = %+v, want the send %s", got, st.ID)
	}
	// "active" is not a send id, so the status route would have said so.
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "localhost")
	if strings.Contains(rec.Body.String(), "no such send") {
		t.Errorf("active was routed to the status handler: %s", rec.Body)
	}
}

func TestActiveControlsKeepsTheGates(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	if rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "evil.example"); rec.Code == http.StatusOK {
		t.Errorf("foreign Host = %d, want a refusal", rec.Code)
	}
	if rec := doRequest(t, h.s.Handler(), http.MethodPost, "/apps/soc/api/control/active", "", "localhost"); rec.Code == http.StatusOK {
		t.Errorf("POST = %d, want a refusal", rec.Code)
	}
}

func TestActiveControlsStoreErrorIsAFixedMessage(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	h.ctl.ctlErr = errors.New("secret store detail")
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "localhost")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("store error = %d %s, want 500 with a fixed message", rec.Code, rec.Body)
	}
}

func TestActiveControlsBusyWhenEverySlotIsTaken(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	for range maxOutputBuilds {
		h.s.outputBuilds <- struct{}{}
	}
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "localhost")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("busy = %d retry-after %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
}
