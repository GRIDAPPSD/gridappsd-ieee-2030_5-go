package adminui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/senderpage"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sender"
)

// The switch keeps a panel of its own, so the Bus publishing tab stays
// where it was and an off flip never shares a page with the sends it stops.
const (
	panelPublishing = "gridappsd-publishing"
	panelSender     = "gridappsd-sender"
)

// stillDeliverable is shown with every off flip: go-stomp's Send ignores
// cancellation, so the switch cannot recall a frame it already holds.
const stillDeliverable = "A send already handed to the STOMP client can still reach the bus after the switch is off."

type remoteKey struct{}

// withRemote keeps the caller's address on the request context, where the
// sender and control routes read it for the audit.
func withRemote(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteKey{}, r.RemoteAddr)))
	})
}

func remoteFrom(ctx context.Context) string {
	if v, ok := ctx.Value(remoteKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// senderPanel is the bus sender behind the Bus publishing and Bus sender
// pages.
type senderPanel struct {
	s *sender.Sender
}

// switchDoc is the data of the Bus publishing page. On is the state the
// page's button acts on.
type switchDoc struct {
	pageDoc
	On bool `json:"on"`
}

type senderDevice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// senderLimits are the bounds the forms enforce, sent so the page's inputs
// carry them. The server checks every submission against the same values.
type senderLimits struct {
	MultiplierMin int64 `json:"multiplierMin"`
	MultiplierMax int64 `json:"multiplierMax"`
	ValueMin      int64 `json:"valueMin"`
	ValueMax      int64 `json:"valueMax"`
	RawMax        int64 `json:"rawMax"`
}

// senderDoc is the data of the Bus sender page. Devices and Sections are
// arrays, never null.
type senderDoc struct {
	pageDoc
	On      bool           `json:"on"`
	Devices []senderDevice `json:"devices"`
	Limits  senderLimits   `json:"limits"`
}

// switchPage is the handler of the Bus publishing page panel.
func (s *Server) switchPage() http.Handler {
	p := s.sender
	return formPage(panelPublishing, senderpage.Files, "switch.html", "switch.js",
		func() any { return p.switchData() },
		map[string]formRoute{
			"publishing": {
				fields: []formField{{name: "on", kind: fieldBoolean}},
				run:    p.flip,
			},
		})
}

// senderPage is the handler of the Bus sender page panel.
func (s *Server) senderPage() http.Handler {
	p := s.sender
	device := formField{name: "device", kind: fieldChoice, choices: p.deviceIDs}
	multiplier := formField{name: "multiplier", kind: fieldInteger, min: sender.MinMultiplier, max: sender.MaxMultiplier}
	value := formField{name: "value", kind: fieldInteger, min: math.MinInt16, max: math.MaxInt16}
	return formPage(panelSender, senderpage.Files, "sender.html", "sender.js",
		func() any { return p.senderData() },
		map[string]formRoute{
			"active-power":   {fields: []formField{device, multiplier, value}, run: p.activePower},
			"reactive-power": {fields: []formField{device, multiplier, value}, run: p.reactivePower},
			"connect":        {fields: []formField{device, {name: "connect", kind: fieldBoolean}}, run: p.connect},
			"energize":       {fields: []formField{device, {name: "energize", kind: fieldBoolean}}, run: p.energize},
			"raw":            {fields: []formField{{name: "json", kind: fieldText, maxLen: formTextMax}}, run: p.raw},
		})
}

// deviceChoices offers each registered device by name and submits its
// mRID, with the graph picker's rules for ids and labels.
func (p *senderPanel) deviceChoices() []senderDevice {
	devs := p.s.Devices()
	// pickableChoices reads only the mRID and name of a series.
	items := make([]chartSeries, 0, len(devs))
	for _, d := range devs {
		items = append(items, chartSeries{mrid: d.MRID, name: d.Name})
	}
	choices := pickableChoices(items)
	out := make([]senderDevice, 0, len(choices))
	for _, c := range choices {
		out = append(out, senderDevice{ID: c.ID, Label: c.Label})
	}
	return out
}

// deviceIDs are the mRIDs a form may name, read at each submission.
func (p *senderPanel) deviceIDs() []string {
	choices := p.deviceChoices()
	ids := make([]string, 0, len(choices))
	for _, c := range choices {
		ids = append(ids, c.ID)
	}
	return ids
}

func (p *senderPanel) flip(ctx context.Context, remote string, v formValues) (string, error) {
	st := p.s.SetPublishing(v.boolean("on"), remote)
	if st.On {
		return "Publishing is ON.", nil
	}
	return fmt.Sprintf("Publishing is OFF. %d send(s) were still in flight when the wait ended. %s",
		st.StillInFlight, stillDeliverable), nil
}

func (p *senderPanel) activePower(ctx context.Context, remote string, v formValues) (string, error) {
	return sent(p.s.SendActivePower(ctx, remote, v.text("device"), int(v.integer("multiplier")), int(v.integer("value"))))
}

func (p *senderPanel) reactivePower(ctx context.Context, remote string, v formValues) (string, error) {
	return sent(p.s.SendReactivePower(ctx, remote, v.text("device"), int(v.integer("multiplier")), int(v.integer("value"))))
}

func (p *senderPanel) connect(ctx context.Context, remote string, v formValues) (string, error) {
	return sent(p.s.SendConnect(ctx, remote, v.text("device"), v.boolean("connect")))
}

func (p *senderPanel) energize(ctx context.Context, remote string, v formValues) (string, error) {
	return sent(p.s.SendEnergize(ctx, remote, v.text("device"), v.boolean("energize")))
}

func (p *senderPanel) raw(ctx context.Context, remote string, v formValues) (string, error) {
	return sent(p.s.SendRaw(ctx, remote, []byte(v.text("json"))))
}

// sent answers a send. A refusal (the switch, the rate, the first failing
// JSON path) is written for the operator and shown as one. A bus failure is
// a failure: the handler logs its detail, which is transport text, and
// answers a bare 500; the send's row reads failed.
func sent(res sender.Result, err error) (string, error) {
	if errors.Is(err, sender.ErrPublishFailed) {
		return "", fmt.Errorf("bus sender: %w", err)
	}
	if err != nil {
		return "", &formRefusal{reason: strings.TrimPrefix(err.Error(), "sender: ")}
	}
	return fmt.Sprintf("Published difference_mrid %s to %s. Its outcome appears under Recent sends.",
		res.DifferenceMRID, res.Destination), nil
}

func (p *senderPanel) switchSection() pageSection {
	st := p.s.Publishing()
	state := pBadge(badgeNeutral, "OFF")
	if st.On {
		state = pBadge(badgeWarn, "ON")
	}
	entries := []pageEntry{
		{Key: "Publishing", Value: state},
		{Key: "Last changed", Value: pTime(st.ChangedAt)},
		{Key: "Changed from", Value: pText(st.ChangedBy)},
	}
	if !st.On && st.StillInFlight > 0 {
		entries = append(entries, pageEntry{Key: "Still in flight at the off flip",
			Value: pBadge(badgeWarn, fmt.Sprintf("%d send(s); they may still be delivered", st.StillInFlight))})
	}
	sec := listSection("Publishing switch", "", pageGroup{Entries: entries})
	sec.Prose = []string{
		"Publishing is off after every start unless SEP2_ADMIN_UI_BUS_PUBLISH_AT_START is true. While it is off every send is refused. The switch is on the Bus publishing tab.",
		"Switching off refuses new sends at once and waits a few seconds for sends in flight. " + stillDeliverable,
	}
	return sec
}

func (p *senderPanel) switchData() switchDoc {
	return switchDoc{
		pageDoc: pageDoc{Title: "Bus publishing", Sections: []pageSection{p.switchSection()}},
		On:      p.s.Publishing().On,
	}
}

func (p *senderPanel) senderData() senderDoc {
	recent := p.s.Recent()
	recentRows := make([][]pageCell, 0, len(recent))
	for _, e := range recent {
		recentRows = append(recentRows, []pageCell{
			pTime(e.Time), pText(e.Kind), pOrDash(e.Remote), pOrDash(e.DifferenceMRID), pOrDash(deltasText(e.Deltas)),
			outcomeCell(e), pOrDash(e.Reason),
		})
	}
	refusals := p.s.Refusals()
	refusalRows := make([][]pageCell, 0, len(refusals))
	for _, e := range refusals {
		refusalRows = append(refusalRows, []pageCell{pTime(e.Time), pText(e.Kind), pOrDash(e.Remote), pOrDash(e.DifferenceMRID), pText(e.Reason)})
	}
	tableDoc := pageTables("Bus sender",
		pageTable("Recent sends", "Nothing sent or switched yet.",
			[]string{fmt.Sprintf("The newest %d sends and switch flips, newest first. A send's outcome is what the control path did with it.", sender.MaxRecent)},
			[]string{"Time", "Kind", "From", "difference_mrid", "Changes", "Outcome", "Reason"}, recentRows),
		pageTable("Refusals", "No send has been refused.",
			[]string{"Sends turned away before anything was published, newest first."},
			[]string{"Time", "Kind", "From", "difference_mrid", "Reason"}, refusalRows),
	)
	return senderDoc{
		pageDoc: pageDoc{Title: "Bus sender", Sections: append([]pageSection{p.switchSection()}, tableDoc.Sections...)},
		On:      p.s.Publishing().On,
		Devices: p.deviceChoices(),
		Limits: senderLimits{
			MultiplierMin: sender.MinMultiplier, MultiplierMax: sender.MaxMultiplier,
			ValueMin: math.MinInt16, ValueMax: math.MaxInt16, RawMax: formTextMax,
		},
	}
}

// deltasText is each change as object attribute=value, with the control
// path's result once it has one.
func deltasText(ds []sender.Delta) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		s := fmt.Sprintf("%s %s=%s", d.Object, d.Attribute, jsonText(d.Value))
		if d.Result != "" {
			s += " (" + d.Result
			if d.Reason != "" {
				s += ": " + d.Reason
			}
			s += ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

func outcomeCell(e sender.Entry) pageCell {
	variant := badgeNeutral
	switch e.Outcome {
	case sender.OutcomeIssued, sender.OutcomeRestated, "on":
		variant = badgeOK
	case sender.OutcomeRefused, sender.OutcomeFailed:
		variant = badgeError
	case sender.OutcomeNone, sender.OutcomePending:
		variant = badgeWarn
	}
	return pBadge(variant, e.Outcome)
}
