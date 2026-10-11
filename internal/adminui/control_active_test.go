package adminui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
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
	h.ctl.bus = map[string]bool{"bus-1": true}
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

// Every device failing is a store failure, not an empty fleet.
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

// A Received posted when the device fetched a scheduled control, before its
// start, still counts: the 0 W follow-on is fetched with the send.
func TestActiveControlsSeesAReceivedPostedBeforeTheStart(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{
		"_bat-1": {{ID: "fo-1", MRID: "m-fo", CurrentStatus: sep2.EventStatusScheduled, CreationTime: t0 + 1,
			Start: t0 + 300, Duration: 900, Base: targetW(0, 0)}},
	}
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "m-fo", Status: sep2.ResponseStatusEventReceived}}
	h.ctl.receivedAt = map[string]int64{"m-fo": t0 + 5}
	h.clk.set(t0 + 10)

	got := getActive(t, h)
	if len(got.Controls) != 1 || !got.Controls[0].Received || got.Controls[0].State != "scheduled" {
		t.Errorf("rows = %+v, want the scheduled follow-on with received true", got.Controls)
	}
}

// A control neither in-memory record holds, here a page send from before a
// restart, reads unknown; each record still vouches for its own.
func TestActiveControlsSourceIsUnknownWhenNeitherRecordHasIt(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	h.send(t, `{"mrid":"_bat-1","watts":1000}`)
	ctl := func(id string, start int64) sep2embed.DERControlSnapshot {
		return sep2embed.DERControlSnapshot{ID: id, MRID: "m-" + id, CurrentStatus: sep2.EventStatusActive, Start: start, Duration: 600, Base: targetW(0, 1)}
	}
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{
		"_bat-1": {ctl("ctl-1", t0), ctl("bus-1", t0+1), ctl("before-restart", t0+2)},
	}
	h.ctl.bus = map[string]bool{"bus-1": true}

	var got []string
	for _, row := range getActive(t, h).Controls {
		got = append(got, row.ControlMRID+"="+row.Source)
	}
	want := []string{"m-ctl-1=page", "m-bus-1=bus", "m-before-restart=unknown"}
	if !slices.Equal(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

// The cap keeps the first maxActiveControls rows by name whatever the
// registry order, counts every row in Total, and reads responses only for
// the kept rows.
func TestActiveControlsCapIsDeterministicAndCountsEveryRow(t *testing.T) {
	t.Parallel()
	for _, n := range []int{maxActiveControls, maxActiveControls + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			h := newCtlHarness(t, Config{})
			var entries []registry.Entry
			h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{}
			// Registered newest name first, so registry order is the reverse
			// of the response order.
			for i := n - 1; i >= 0; i-- {
				mrid := fmt.Sprintf("_bat-%04d", i)
				entries = append(entries, registry.Entry{MRID: mrid, Name: fmt.Sprintf("Battery %04d", i)})
				h.ctl.controls[mrid] = []sep2embed.DERControlSnapshot{{ID: "c" + mrid, MRID: "m" + mrid,
					CurrentStatus: sep2.EventStatusActive, Start: t0, Duration: 600, Base: targetW(0, 1)}}
			}
			setRegistry(h, entries...)

			got := getActive(t, h)
			kept := min(n, maxActiveControls)
			if got.Total != n || got.Truncated != (n > maxActiveControls) || len(got.Controls) != kept {
				t.Fatalf("total %d truncated %v rows %d, want %d %v %d", got.Total, got.Truncated, len(got.Controls), n, n > maxActiveControls, kept)
			}
			for i, row := range got.Controls {
				if want := fmt.Sprintf("Battery %04d", i); row.Name != want {
					t.Fatalf("row %d = %s, want %s", i, row.Name, want)
				}
			}
			if asked := len(h.ctl.askedFor); asked != kept {
				t.Errorf("responses read for %d rows, want %d", asked, kept)
			}
			if dropped := fmt.Sprintf("m_bat-%04d", maxActiveControls); slices.Contains(h.ctl.askedFor, dropped) {
				t.Errorf("responses read for the dropped row %s", dropped)
			}
		})
	}
}

// Only scheduled and active opModTargetW controls with an interval are rows;
// any other status, a control without opModTargetW, and a zero duration are
// not, and none of them panics.
func TestActiveControlsListsOnlyScheduledOrActiveTargetControls(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	ctl := func(id string, status uint8, base *sep2embed.DERControlBaseSnapshot, dur uint32) sep2embed.DERControlSnapshot {
		return sep2embed.DERControlSnapshot{ID: id, MRID: "m-" + id, CurrentStatus: status, Start: t0, Duration: dur, Base: base}
	}
	varOnly := &sep2embed.DERControlBaseSnapshot{OpModTargetVar: &sep2.ReactivePower{Value: 5}}
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{"_bat-1": {
		ctl("scheduled", sep2.EventStatusScheduled, targetW(0, 1), 600),
		ctl("active", sep2.EventStatusActive, targetW(0, 1), 600),
		ctl("cancelled", sep2.EventStatusCancelled, targetW(0, 1), 600),
		ctl("cancelled-rand", eventStatusCancelledRandomized, targetW(0, 1), 600),
		ctl("superseded", sep2.EventStatusSuperseded, targetW(0, 1), 600),
		ctl("status-5", 5, targetW(0, 1), 600),
		ctl("no-base", sep2.EventStatusActive, nil, 600),
		ctl("var-only", sep2.EventStatusActive, varOnly, 600),
		// Starts later, so only the duration check drops it.
		{ID: "no-duration", MRID: "m-no-duration", CurrentStatus: sep2.EventStatusScheduled, Start: t0 + 100, Base: targetW(0, 1)},
	}}

	got := getActive(t, h)
	var ids []string
	for _, row := range got.Controls {
		ids = append(ids, row.ControlMRID+"="+row.State)
	}
	// Equal starts keep the store order.
	if want := []string{"m-scheduled=scheduled", "m-active=active"}; !slices.Equal(ids, want) || got.Total != 2 {
		t.Errorf("rows = %v total %d, want %v total 2", ids, got.Total, want)
	}
}

// Rows are sorted by name, then mRID, then start, whatever order the
// registry and the store give them in.
func TestActiveControlsSortsByNameThenMRIDThenStart(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_b", Name: "Same"}, registry.Entry{MRID: "_z", Name: "Alpha"}, registry.Entry{MRID: "_a", Name: "Same"})
	ctl := func(id string, start int64) sep2embed.DERControlSnapshot {
		return sep2embed.DERControlSnapshot{ID: id, MRID: id, CurrentStatus: sep2.EventStatusScheduled, Start: start, Duration: 600, Base: targetW(0, 1)}
	}
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{
		"_b": {ctl("b-late", t0+100), ctl("b-early", t0+50)},
		"_z": {ctl("z", t0+200)},
		"_a": {ctl("a", t0+300)},
	}
	var got []string
	for _, row := range getActive(t, h).Controls {
		got = append(got, row.ControlMRID)
	}
	if want := []string{"z", "a", "b-early", "b-late"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Only a Received response sets received.
func TestActiveControlsReceivedNeedsAReceivedResponse(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{"_bat-1": {{ID: "c", MRID: "m-c",
		CurrentStatus: sep2.EventStatusActive, Start: t0, Duration: 600, Base: targetW(0, 1)}}}
	h.ctl.responses = []sep2embed.ResponseSnapshot{
		{Subject: "m-c", Status: sep2.ResponseStatusEventStarted},
		{Subject: "m-other", Status: sep2.ResponseStatusEventReceived},
	}
	if got := getActive(t, h); len(got.Controls) != 1 || got.Controls[0].Received {
		t.Errorf("rows = %+v, want one row with received false", got.Controls)
	}
}

// One device's store error skips that device and is counted; the rest
// still list.
func TestActiveControlsSkipsADeviceWhoseControlsCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bad", Name: "Bad"}, registry.Entry{MRID: "_good", Name: "Good"})
	h.ctl.ctlErrFor = map[string]error{"_bad": errors.New("store down")}
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{"_good": {{ID: "c", MRID: "m-c",
		CurrentStatus: sep2.EventStatusActive, Start: t0, Duration: 600, Base: targetW(0, 1)}}}

	got := getActive(t, h)
	if got.SkippedDevices != 1 || len(got.Controls) != 1 || got.Controls[0].MRID != "_good" {
		t.Errorf("skipped %d rows %+v, want 1 skipped and the good device's row", got.SkippedDevices, got.Controls)
	}
	if !strings.Contains(string(mustJSON(t, got)), `"skippedDevices":1`) {
		t.Errorf("body %s does not carry skippedDevices", mustJSON(t, got))
	}
}

func TestActiveControlsResponsesErrorIsAFixedMessage(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	setRegistry(h, registry.Entry{MRID: "_bat-1", Name: "Battery 1"})
	h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{"_bat-1": {{ID: "c", MRID: "m-c",
		CurrentStatus: sep2.EventStatusActive, Start: t0, Duration: 600, Base: targetW(0, 1)}}}
	h.ctl.respErr = errors.New("secret responses detail")
	rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "localhost")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("responses error = %d %s, want 500 with a fixed message", rec.Code, rec.Body)
	}
}

// Each request gives its build slot back, so more requests than slots all
// succeed one after another.
func TestActiveControlsReleasesItsSlot(t *testing.T) {
	t.Parallel()
	h := newCtlHarness(t, Config{})
	for i := range maxOutputBuilds + 2 {
		rec := doRequest(t, h.s.Handler(), http.MethodGet, "/apps/soc/api/control/active", "", "localhost")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rec.Code)
		}
	}
}
