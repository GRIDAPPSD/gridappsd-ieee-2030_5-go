package adminui

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/activity"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const (
	// cardsRoute lists one card per registered device for the SoC page.
	cardsRoute = "/apps/soc/api/cards"

	// maxCards bounds the cards of one response.
	maxCards = 500

	cardsBusy = "cards busy, retry shortly"

	stripeGrey   = "grey"
	stripeGreen  = "green"
	stripeBlue   = "blue"
	stripeYellow = "yellow"
	stripePink   = "pink"

	kindBattery = "battery"
	kindDevice  = "device"

	// pvPrefix marks the devices this setup treats as batteries.
	pvPrefix = "pv_"

	absent = "-"
)

// StatusSource is the read surface Server needs from *sep2embed.Embed for the
// latest stored DERStatus of each device.
type StatusSource interface {
	DERStatusSnapshots(ctx context.Context) ([]sep2embed.DERStatusSnapshot, error)
}

// deviceKind is the one place that decides a device's kind label: a name
// starting with pv_ is a battery here. The registered name is never changed.
func deviceKind(name string) string {
	if strings.HasPrefix(name, pvPrefix) {
		return kindBattery
	}
	return kindDevice
}

// cardControl is the control in force on a card.
type cardControl struct {
	// Watts is the commanded target; Direction is "discharge" above 0,
	// "charge" below 0 and "hold" at 0.
	Watts       float64 `json:"watts"`
	Direction   string  `json:"direction"`
	Source      string  `json:"source"`
	State       string  `json:"state"`
	SecondsLeft int64   `json:"secondsLeft"`
	EndsAt      int64   `json:"endsAt"`
	Received    bool    `json:"received"`
	ControlMRID string  `json:"controlMrid"`
}

// cardStatus is a device's latest stored DERStatus. A field the device did not
// report reads "-" (StateOfChargePercent is null).
type cardStatus struct {
	// StateOfChargePercent is the reported value in percent, 0 to 100.
	StateOfChargePercent *float64 `json:"stateOfChargePercent"`
	Inverter             string   `json:"inverterStatus"`
	OperationalMode      string   `json:"operationalModeStatus"`
	StorageMode          string   `json:"storageModeStatus"`
	GenConnect           string   `json:"genConnectStatus"`
	StorConnect          string   `json:"storConnectStatus"`
	// Alarm is the alarmStatus bitmap in hex.
	Alarm string `json:"alarmStatus"`
	// ReadingTime is the device's reported time, 0 when it sent none, and
	// ReadingAgeSeconds its age at the response's now (null when ReadingTime
	// is 0).
	ReadingTime       int64  `json:"readingTime"`
	ReadingAgeSeconds *int64 `json:"readingAgeSeconds"`
}

// deviceCard is one registered device. The LFDI is never served.
type deviceCard struct {
	Name string `json:"name"`
	MRID string `json:"mrid"`
	Kind string `json:"kind"`
	// Stripe is grey (not connected), pink (last direction change was charge
	// to discharge), yellow (discharging), blue (charging) or green, checked
	// in that order; StripeLabel is its text.
	Stripe      string `json:"stripe"`
	StripeLabel string `json:"stripeLabel"`
	Connected   bool   `json:"connected"`
	// LastSeenAt and LastSeenAgeSeconds are null for a device with no
	// accepted request since the bridge started.
	LastSeenAt         *int64 `json:"lastSeenAt"`
	LastSeenAgeSeconds *int64 `json:"lastSeenAgeSeconds"`
	// Status is null for a device that never reported a DERStatus.
	Status  *cardStatus  `json:"status"`
	Control *cardControl `json:"control"`
}

type cardsResponse struct {
	Now int64 `json:"now"`
	// Total counts every registered device; Truncated says Cards holds only
	// the first maxCards in name order.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// SkippedDevices counts devices left out because their controls could not
	// be read, so a short list is not mistaken for a complete one.
	SkippedDevices int `json:"skippedDevices"`
	// StatusUnavailable is true when the DERStatus read failed, in which case
	// every Status is null.
	StatusUnavailable bool         `json:"statusUnavailable"`
	Cards             []deviceCard `json:"cards"`
}

// handleCards answers GET /apps/soc/api/cards with one card per registered
// device, sorted by name then mRID and capped at maxCards.
//
// Connected means the device's last accepted request is younger than the idle
// threshold the Devices tab uses. The control shown is the one in force now,
// not the scheduled follow-on. Pink comes from the embedded server's record of
// the controls it issued, which is in memory, so a restart clears it.
func (s *Server) handleCards(w http.ResponseWriter, r *http.Request) {
	select {
	case s.outputBuilds <- struct{}{}:
		defer func() { <-s.outputBuilds }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, cardsBusy, http.StatusServiceUnavailable)
		return
	}

	now := s.now()
	devices := slices.Clone(s.registry.Snapshot())
	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].Name != devices[j].Name {
			return devices[i].Name < devices[j].Name
		}
		return devices[i].MRID < devices[j].MRID
	})
	out := cardsResponse{Now: now.Unix(), Total: len(devices), Cards: []deviceCard{}}
	if len(devices) > maxCards {
		devices = devices[:maxCards]
		out.Truncated = true
	}

	statuses, statusErr := s.statusByMRID(r.Context())
	if statusErr != nil {
		log.Printf("adminui: cards: der status: %v", statusErr)
		out.StatusUnavailable = true
	}

	page := s.controls.pageControlIDs()
	for _, e := range devices {
		snaps, err := s.control.ControlsFor(r.Context(), e.MRID)
		if err != nil {
			log.Printf("adminui: cards: controls for %s: %v", e.MRID, err)
			out.SkippedDevices++
			continue
		}
		card, err := s.buildCard(r.Context(), e, snaps, page, now)
		if err != nil {
			log.Printf("adminui: cards: %s: responses: %v", e.MRID, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{"cards unavailable"})
			return
		}
		if st, ok := statuses[e.MRID]; ok {
			card.Status = cardStatusOf(st, now.Unix())
		}
		out.Cards = append(out.Cards, card)
	}
	if len(devices) > 0 && out.SkippedDevices == len(devices) {
		writeJSON(w, http.StatusInternalServerError, errorResponse{"cards unavailable"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// statusByMRID reads the latest DERStatus of every device that reported one.
// With no StatusSource wired it returns an empty map and no error.
func (s *Server) statusByMRID(ctx context.Context) (map[string]sep2.DERStatus, error) {
	if s.status == nil {
		return nil, nil
	}
	snaps, err := s.status.DERStatusSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]sep2.DERStatus, len(snaps))
	for _, sn := range snaps {
		m[sn.MRID] = sn.Status
	}
	return m, nil
}

func (s *Server) buildCard(ctx context.Context, e registry.Entry, snaps []sep2embed.DERControlSnapshot, page map[string]struct{}, now time.Time) (deviceCard, error) {
	card := deviceCard{Name: e.Name, MRID: e.MRID, Kind: deviceKind(e.Name)}

	if last, _, ok := s.activity.Last(e.LFDI); ok {
		at := last.Unix()
		age := max(int64(now.Sub(last)/time.Second), 0)
		card.LastSeenAt, card.LastSeenAgeSeconds = &at, &age
	}
	card.Connected = s.activity.State(e.LFDI, now, s.idleAfter) == activity.Online

	var best *sep2embed.DERControlSnapshot
	var row activeControl
	for i := range snaps {
		sn := snaps[i]
		if sn.CurrentStatus != sep2.EventStatusActive {
			continue
		}
		r, ok := activeRow(sn, now.Unix())
		if !ok || (best != nil && sn.Start < best.Start) {
			continue
		}
		best, row = &snaps[i], r
	}
	if best != nil {
		received, err := s.receivedFor(ctx, row.ControlMRID, best.ResponsesSince())
		if err != nil {
			return deviceCard{}, err
		}
		card.Control = &cardControl{
			Watts: row.Watts, Direction: row.Direction,
			Source: controlSource(best.ID, page, s.control.BusIssued),
			State:  row.State, SecondsLeft: row.SecondsLeft, EndsAt: row.EndsAt,
			Received: received, ControlMRID: row.ControlMRID,
		}
	}

	var watts float64
	if card.Control != nil {
		watts = card.Control.Watts
	}
	card.Stripe, card.StripeLabel = stripeOf(card.Connected, s.control.DirectionPink(e.MRID), watts)
	return card, nil
}

// stripeOf orders the checks as the EPRI log UI does: not connected, then
// pink, then the direction of the target in force, otherwise green.
func stripeOf(connected, pink bool, watts float64) (stripe, label string) {
	switch {
	case !connected:
		return stripeGrey, "NOT CONNECTED"
	case pink:
		return stripePink, "CHARGE TO DISCHARGE"
	case watts > 0:
		return stripeYellow, "DISCHARGING"
	case watts < 0:
		return stripeBlue, "CHARGING"
	}
	return stripeGreen, "CONNECTED"
}

func cardStatusOf(d sep2.DERStatus, now int64) *cardStatus {
	cs := &cardStatus{
		Inverter: absent, OperationalMode: absent, StorageMode: absent,
		GenConnect: absent, StorConnect: absent, Alarm: absent,
		ReadingTime: d.ReadingTime,
	}
	if v := d.StateOfChargeStatus; v != nil {
		pct := float64(v.Value) / 100
		cs.StateOfChargePercent = &pct
	}
	if v := d.InverterStatus; v != nil {
		cs.Inverter = enumName(inverterNames, v.Value)
	}
	if v := d.OperationalModeStatus; v != nil {
		cs.OperationalMode = enumName(operationalModeNames, v.Value)
	}
	if v := d.StorageModeStatus; v != nil {
		cs.StorageMode = enumName(storageModeNames, v.Value)
	}
	if v := d.GenConnectStatus; v != nil {
		cs.GenConnect = connectNames(uint8(v.Value))
	}
	if v := d.StorConnectStatus; v != nil {
		cs.StorConnect = connectNames(uint8(v.Value))
	}
	if v := d.AlarmStatus; v != nil {
		cs.Alarm = fmt.Sprintf("%X", uint32(*v))
	}
	if d.ReadingTime != 0 {
		age := max(now-d.ReadingTime, 0)
		cs.ReadingAgeSeconds = &age
	}
	return cs
}

// The value names of IEEE 2030.5-2018 section 10.10 (DER status).
var (
	inverterNames = map[uint8]string{
		0: "N/A", 1: "Off", 2: "Sleeping", 3: "Starting", 4: "Tracking MPPT",
		5: "Derating", 6: "Shutting down", 7: "Fault", 8: "Standby", 9: "Test",
		10: "Manufacturer defined",
	}
	operationalModeNames = map[uint8]string{0: "N/A", 1: "Off", 2: "Operational", 3: "Test"}
	storageModeNames     = map[uint8]string{0: "Charging", 1: "Discharging", 2: "Holding"}
	connectBitNames      = [...]string{"Connected", "Available", "Operating", "Test", "Fault"}
)

func enumName(names map[uint8]string, v uint8) string {
	if n, ok := names[v]; ok {
		return n
	}
	return fmt.Sprintf("reserved (%d)", v)
}

// connectNames names the set bits of a ConnectStatus bitmap, "None" for 0.
func connectNames(v uint8) string {
	var set []string
	for i := range 8 {
		if v&(1<<i) == 0 {
			continue
		}
		if i < len(connectBitNames) {
			set = append(set, connectBitNames[i])
		} else {
			set = append(set, fmt.Sprintf("reserved bit %d", i))
		}
	}
	if len(set) == 0 {
		return "None"
	}
	return strings.Join(set, ", ")
}
