package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/activity"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

type fakeStatus struct {
	snaps []sep2embed.DERStatusSnapshot
	err   error
}

func (f *fakeStatus) DERStatusSnapshots(context.Context) ([]sep2embed.DERStatusSnapshot, error) {
	return f.snaps, f.err
}

// cardsHarness is a control harness with an activity recorder on the test
// clock and a status source.
type cardsHarness struct {
	*ctlHarness
	rec    *activity.Recorder
	status *fakeStatus
}

func newCardsHarness(t *testing.T) *cardsHarness {
	t.Helper()
	h := &cardsHarness{ctlHarness: newCtlHarness(t, Config{})}
	h.rec = activity.NewWithClock(h.clk.now)
	h.status = &fakeStatus{}
	h.s.activity = h.rec
	h.s.status = h.status
	return h
}

func lfdiOf(n int) string { return fmt.Sprintf("%040X", n) }

func (h *cardsHarness) device(mrid, name string, n int) {
	reg := h.s.registry.(*fakeRegistry)
	reg.entries = append(reg.entries, registry.Entry{MRID: mrid, Name: name, LFDI: lfdiOf(n)})
}

func (h *cardsHarness) seen(n int) { h.rec.Record(lfdiOf(n)) }

func (h *cardsHarness) active(mrid, id, ctlMRID string, start int64, duration uint32, watts int16) {
	if h.ctl.controls == nil {
		h.ctl.controls = map[string][]sep2embed.DERControlSnapshot{}
	}
	h.ctl.controls[mrid] = append(h.ctl.controls[mrid], sep2embed.DERControlSnapshot{
		ID: id, MRID: ctlMRID, CurrentStatus: sep2.EventStatusActive, Start: start, Duration: duration, Base: targetW(0, watts),
	})
}

func getCards(t *testing.T, h *cardsHarness) cardsResponse {
	t.Helper()
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cards = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != socPageCSP {
		t.Errorf("CSP = %q, want the page policy", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var out cardsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %v: %s", err, rec.Body)
	}
	return out
}

func oneCard(t *testing.T, h *cardsHarness) deviceCard {
	t.Helper()
	got := getCards(t, h)
	if len(got.Cards) != 1 {
		t.Fatalf("cards = %+v, want 1", got.Cards)
	}
	return got.Cards[0]
}

func TestCardForANeverSeenDeviceIsGreyWithNulls(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)

	got := getCards(t, h)
	if got.Now != t0 || got.Total != 1 || got.Truncated || got.SkippedDevices != 0 || got.StatusUnavailable {
		t.Errorf("envelope = %+v, want now %d total 1 and no flags", got, t0)
	}
	c := got.Cards[0]
	if c.Name != "Battery 1" || c.MRID != "_bat-1" || c.Kind != "device" {
		t.Errorf("identity = %q %q %q", c.Name, c.MRID, c.Kind)
	}
	if c.Stripe != "grey" || c.StripeLabel != "NOT CONNECTED" || c.Connected {
		t.Errorf("stripe %q %q connected %v, want grey NOT CONNECTED false", c.Stripe, c.StripeLabel, c.Connected)
	}
	if c.LastSeenAt != nil || c.LastSeenAgeSeconds != nil || c.Status != nil || c.Control != nil {
		t.Errorf("never-seen card has values: %+v", c)
	}
}

func TestCardForAConnectedIdleDeviceIsGreen(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	h.clk.set(t0 + 30)

	c := oneCard(t, h)
	if c.Stripe != "green" || c.StripeLabel != "CONNECTED" || !c.Connected || c.Control != nil {
		t.Errorf("card = %+v, want green CONNECTED with no control", c)
	}
	if c.LastSeenAt == nil || *c.LastSeenAt != t0 || *c.LastSeenAgeSeconds != 30 {
		t.Errorf("last seen = %v age %v, want %d and 30", c.LastSeenAt, c.LastSeenAgeSeconds, t0)
	}
}

// Connected is strictly younger than the threshold the Devices tab uses.
func TestCardConnectedFollowsTheIdleThresholdExactly(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	idle := int64(DefaultClientIdleAfter.Seconds())

	for _, tc := range []struct {
		age       int64
		connected bool
		stripe    string
	}{{idle - 1, true, "green"}, {idle, false, "grey"}, {idle + 1, false, "grey"}} {
		h.clk.set(t0 + tc.age)
		c := oneCard(t, h)
		if c.Connected != tc.connected || c.Stripe != tc.stripe || *c.LastSeenAgeSeconds != tc.age {
			t.Errorf("age %d: connected %v stripe %q age %d, want %v %q", tc.age, c.Connected, c.Stripe, *c.LastSeenAgeSeconds, tc.connected, tc.stripe)
		}
	}
}

func TestCardForAStaleDeviceGoesGreyEvenUnderControl(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	h.active("_bat-1", "bus-1", "m-bus", t0, 3600, -1500)
	h.clk.set(t0 + 10)
	if c := oneCard(t, h); c.Stripe != "blue" {
		t.Fatalf("connected charging card = %q, want blue", c.Stripe)
	}

	h.clk.set(t0 + 600)
	c := oneCard(t, h)
	if c.Stripe != "grey" || c.StripeLabel != "NOT CONNECTED" || c.Connected {
		t.Errorf("stale card = %q %q connected %v, want grey NOT CONNECTED false", c.Stripe, c.StripeLabel, c.Connected)
	}
	if c.Control == nil || c.Control.Watts != -1500 {
		t.Errorf("a stale device still shows its control, got %+v", c.Control)
	}
}

func TestCardChargingFromAPageSend(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	h.send(t, `{"mrid":"_bat-1","watts":-2000,"durationSeconds":300}`)
	h.active("_bat-1", "ctl-1", "m-page", t0, 300, -2000)
	h.ctl.responses = []sep2embed.ResponseSnapshot{{Subject: "m-page", Status: sep2.ResponseStatusEventReceived}}
	h.clk.set(t0 + 10)

	c := oneCard(t, h)
	want := cardControl{Watts: -2000, Direction: "charge", Source: "page", State: "active",
		SecondsLeft: 290, EndsAt: t0 + 300, Received: true, ControlMRID: "m-page"}
	if c.Stripe != "blue" || c.StripeLabel != "CHARGING" {
		t.Errorf("stripe = %q %q, want blue CHARGING", c.Stripe, c.StripeLabel)
	}
	if c.Control == nil || *c.Control != want {
		t.Errorf("control = %+v, want %+v", c.Control, want)
	}
}

func TestCardDischargingFromABusControl(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	h.active("_bat-1", "bus-1", "m-bus", t0-60, 600, 3000)
	h.ctl.bus = map[string]bool{"bus-1": true}
	h.clk.set(t0 + 10)

	c := oneCard(t, h)
	want := cardControl{Watts: 3000, Direction: "discharge", Source: "bus", State: "active",
		SecondsLeft: 530, EndsAt: t0 + 540, Received: false, ControlMRID: "m-bus"}
	if c.Stripe != "yellow" || c.StripeLabel != "DISCHARGING" {
		t.Errorf("stripe = %q %q, want yellow DISCHARGING", c.Stripe, c.StripeLabel)
	}
	if c.Control == nil || *c.Control != want {
		t.Errorf("control = %+v, want %+v", c.Control, want)
	}
}

// Pink comes from the embedded server's latch and outranks the direction of
// the target in force, so it is kept through a 0 W hold and with no control.
func TestCardPinkIsKeptThroughAStop(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	h.ctl.pink = map[string]bool{"_bat-1": true}
	h.active("_bat-1", "ctl-1", "m-stop", t0, 300, 0)
	h.clk.set(t0 + 5)

	c := oneCard(t, h)
	if c.Stripe != "pink" || c.StripeLabel != "CHARGE TO DISCHARGE" {
		t.Errorf("pink with a 0 W control = %q %q", c.Stripe, c.StripeLabel)
	}
	if c.Control == nil || c.Control.Direction != "hold" || c.Control.Watts != 0 {
		t.Errorf("the 0 W control must still be shown as hold, got %+v", c.Control)
	}

	delete(h.ctl.controls, "_bat-1")
	if c := oneCard(t, h); c.Stripe != "pink" || c.Control != nil {
		t.Errorf("pink with no control = %q control %+v", c.Stripe, c.Control)
	}

	// Not connected outranks pink.
	h.clk.set(t0 + 600)
	if c := oneCard(t, h); c.Stripe != "grey" {
		t.Errorf("stale pink device = %q, want grey", c.Stripe)
	}
}

// The scheduled follow-on and an ended control are not the control in force.
func TestCardShowsOnlyTheActiveControl(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	h.active("_bat-1", "ctl-1", "m-now", t0, 300, -2000)
	h.ctl.controls["_bat-1"] = append(h.ctl.controls["_bat-1"],
		sep2embed.DERControlSnapshot{ID: "fo-1", MRID: "m-fo", CurrentStatus: sep2.EventStatusScheduled, Start: t0 + 300, Duration: 900, Base: targetW(0, 0)},
		sep2embed.DERControlSnapshot{ID: "old", MRID: "m-old", CurrentStatus: sep2.EventStatusSuperseded, Start: t0 - 100, Duration: 300, Base: targetW(0, 500)},
	)
	h.clk.set(t0 + 10)
	if c := oneCard(t, h); c.Control == nil || c.Control.ControlMRID != "m-now" || c.Stripe != "blue" {
		t.Errorf("card = %+v control %+v, want the active -2000 W control", c, c.Control)
	}

	// After the end only the follow-on remains, and it is not in force yet.
	h.clk.set(t0 + 300)
	h.seen(1)
	if c := oneCard(t, h); c.Control != nil || c.Stripe != "green" {
		t.Errorf("after the end: stripe %q control %+v, want green and none", c.Stripe, c.Control)
	}
}

func TestPVDevicesAreLabelledBattery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, kind string }{
		{"pv_house_12", "battery"},
		{"pv_", "battery"},
		{"tl_house_3", "device"},
		{"xpv_1", "device"},
		{"PV_1", "device"},
		{"", "device"},
	} {
		if got := deviceKind(tc.name); got != tc.kind {
			t.Errorf("deviceKind(%q) = %q, want %q", tc.name, got, tc.kind)
		}
	}

	h := newCardsHarness(t)
	h.device("_pv-1", "pv_house_12", 1)
	c := oneCard(t, h)
	if c.Kind != "battery" || c.Name != "pv_house_12" {
		t.Errorf("card kind %q name %q, want battery and the registered name", c.Kind, c.Name)
	}
}

func TestCardsCapSortAndCountEveryDevice(t *testing.T) {
	t.Parallel()
	for _, n := range []int{maxCards, maxCards + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			h := newCardsHarness(t)
			// Registered newest name first, so registry order is the reverse
			// of the response order.
			for i := n - 1; i >= 0; i-- {
				h.device(fmt.Sprintf("_d%04d", i), fmt.Sprintf("dev%04d", i), i+1)
			}
			got := getCards(t, h)
			wantTruncated := n > maxCards
			if got.Total != n || got.Truncated != wantTruncated || len(got.Cards) != min(n, maxCards) {
				t.Fatalf("total %d truncated %v cards %d, want %d %v %d", got.Total, got.Truncated, len(got.Cards), n, wantTruncated, min(n, maxCards))
			}
			for i, c := range got.Cards {
				if c.Name != fmt.Sprintf("dev%04d", i) {
					t.Fatalf("card %d = %q, want dev%04d", i, c.Name, i)
				}
			}
			if wantTruncated && len(h.ctl.askedFor) != 0 {
				t.Errorf("responses read for %v, want none without controls", h.ctl.askedFor)
			}
		})
	}
}

func TestCardsStatusValuesAndAbsentFields(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.device("_bat-2", "Battery 2", 2)
	h.device("_bat-3", "Battery 3", 3)
	alarm := sep2.HexBinary32(0x0000A001)
	h.status.snaps = []sep2embed.DERStatusSnapshot{
		{MRID: "_bat-1", Status: sep2.DERStatus{
			ReadingTime:           t0 - 20,
			StateOfChargeStatus:   &sep2.StateOfChargeStatusType{Value: 5525},
			InverterStatus:        &sep2.InverterStatusType{Value: 4},
			OperationalModeStatus: &sep2.OperationalModeStatusType{Value: 2},
			StorageModeStatus:     &sep2.StorageModeStatusType{Value: 1},
			GenConnectStatus:      &sep2.ConnectStatusType{Value: 0x05},
			StorConnectStatus:     &sep2.ConnectStatusType{Value: 0x00},
			AlarmStatus:           &alarm,
		}},
		// A report with only SoC and no readingTime: every other field is "-".
		{MRID: "_bat-2", Status: sep2.DERStatus{StateOfChargeStatus: &sep2.StateOfChargeStatusType{Value: 0}}},
	}

	got := getCards(t, h)
	s1, s2 := got.Cards[0].Status, got.Cards[1].Status
	if s1 == nil || s1.StateOfChargePercent == nil || *s1.StateOfChargePercent != 55.25 ||
		s1.Inverter != "Tracking MPPT" || s1.OperationalMode != "Operational" || s1.StorageMode != "Discharging" ||
		s1.GenConnect != "Connected, Operating" || s1.StorConnect != "None" || s1.Alarm != "A001" ||
		s1.ReadingTime != t0-20 || s1.ReadingAgeSeconds == nil || *s1.ReadingAgeSeconds != 20 {
		t.Errorf("status 1 = %+v", s1)
	}
	if s2 == nil || s2.StateOfChargePercent == nil || *s2.StateOfChargePercent != 0 ||
		s2.Inverter != "-" || s2.OperationalMode != "-" || s2.StorageMode != "-" ||
		s2.GenConnect != "-" || s2.StorConnect != "-" || s2.Alarm != "-" || s2.ReadingAgeSeconds != nil {
		t.Errorf("status 2 = %+v, want 0 percent and dashes", s2)
	}
	if got.Cards[2].Status != nil {
		t.Errorf("a device that never reported has status %+v, want null", got.Cards[2].Status)
	}
}

func TestCardsStatusReadFailureIsFlaggedNotFatal(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.status.err = errors.New("secret store detail")
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"statusUnavailable":true`) || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("status failure = %d %s, want 200 with statusUnavailable and no detail", rec.Code, rec.Body)
	}
}

func TestCardsSkipADeviceWhoseControlsCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.device("_bat-2", "Battery 2", 2)
	h.ctl.ctlErrFor = map[string]error{"_bat-2": errors.New("boom")}

	got := getCards(t, h)
	if got.Total != 2 || got.SkippedDevices != 1 || len(got.Cards) != 1 || got.Cards[0].MRID != "_bat-1" {
		t.Errorf("response = %+v, want total 2, 1 skipped and only _bat-1", got)
	}

	h.ctl.ctlErr = errors.New("secret store detail")
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("every device failing = %d %s, want 500 with a fixed message", rec.Code, rec.Body)
	}
}

func TestCardsServeNoLFDIAndKeepTheGates(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.seen(1)
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	if strings.Contains(strings.ToUpper(rec.Body.String()), lfdiOf(1)) {
		t.Errorf("response carries the LFDI: %s", rec.Body)
	}
	if rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "evil.example"); rec.Code == http.StatusOK {
		t.Errorf("foreign Host = %d, want a refusal", rec.Code)
	}
	if rec := doRequest(t, h.s.Handler(), http.MethodPost, cardsRoute, "", "localhost"); rec.Code == http.StatusOK {
		t.Errorf("POST = %d, want a refusal", rec.Code)
	}
}

func TestCardsBusyWhenEverySlotIsTaken(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	for range maxOutputBuilds {
		h.s.outputBuilds <- struct{}{}
	}
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("busy = %d retry-after %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestCardsEmptyRegistrySerializesAsAnEmptyList(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cards":[]`) {
		t.Errorf("empty registry = %d %s, want 200 and cards []", rec.Code, rec.Body)
	}
}

// Of two active controls on one device the later start is the one in force,
// whichever order the store lists them in.
func TestCardTakesTheLaterStartedOfTwoActiveControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		earlyFirst bool
	}{{"early listed first", true}, {"late listed first", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCardsHarness(t)
			h.device("_bat-1", "Battery 1", 1)
			h.seen(1)
			early := func() { h.active("_bat-1", "ctl-early", "m-early", t0-100, 600, 3000) }
			late := func() { h.active("_bat-1", "ctl-late", "m-late", t0, 300, -2000) }
			if tc.earlyFirst {
				early()
				late()
			} else {
				late()
				early()
			}
			h.clk.set(t0 + 10)

			c := oneCard(t, h)
			if c.Control == nil || c.Control.ControlMRID != "m-late" || c.Control.Direction != "charge" || c.Control.Watts != -2000 {
				t.Errorf("control = %+v, want the later-started -2000 W charge", c.Control)
			}
			if c.Stripe != "blue" || c.StripeLabel != "CHARGING" {
				t.Errorf("stripe = %q %q, want blue CHARGING", c.Stripe, c.StripeLabel)
			}
		})
	}
}

// A Responses read failure fails the whole list, unlike a Controls read
// failure, which skips only its device. The status is deliberate and pinned.
func TestCardsResponsesReadFailureFailsTheWholeList(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_bat-1", "Battery 1", 1)
	h.device("_bat-2", "Battery 2", 2)
	h.seen(1)
	h.active("_bat-1", "ctl-1", "m-1", t0, 300, -2000)
	h.clk.set(t0 + 10)
	h.ctl.respErr = errors.New("secret responses detail")

	rec := doRequest(t, h.s.Handler(), http.MethodGet, cardsRoute, "", "localhost")
	body := rec.Body.String()
	if rec.Code != http.StatusInternalServerError || strings.Contains(body, "secret") || strings.Contains(body, "_bat-2") {
		t.Errorf("responses failure = %d %s, want 500 with a fixed message and no card", rec.Code, body)
	}
}

// Devices with the same name are ordered by mRID, whatever the registry order.
func TestCardsSortSameNameDevicesByMRID(t *testing.T) {
	t.Parallel()
	h := newCardsHarness(t)
	h.device("_b", "Twin", 2)
	h.device("_a", "Twin", 1)
	h.device("_c", "Twin", 3)

	got := getCards(t, h)
	var order []string
	for _, c := range got.Cards {
		order = append(order, c.MRID)
	}
	if strings.Join(order, ",") != "_a,_b,_c" {
		t.Errorf("order = %v, want _a,_b,_c", order)
	}
}
