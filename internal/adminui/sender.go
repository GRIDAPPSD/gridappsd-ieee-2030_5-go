package adminui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sender"
)

// The switch has a panel of its own: the plane runs one call per panel at
// a time, so on the sends' panel an off flip would wait behind the very
// send it is meant to stop, which can block in go-stomp for 10 s.
const (
	panelPublishing = "gridappsd-publishing"
	panelSender     = "gridappsd-sender"
)

// stillDeliverable is shown with every off flip: go-stomp's Send ignores
// cancellation, so the switch cannot recall a frame it already holds.
const stillDeliverable = "A send already handed to the STOMP client can still reach the bus after the switch is off."

type remoteKey struct{}

// withRemote keeps the caller's address on the request context, where a
// panel action, which never sees the request, reads it for the audit.
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

// senderPanel offers the publish switch, the four control forms and the
// raw-JSON box, and shows the switch, the recent sends and the refusals.
type senderPanel struct {
	s *sender.Sender
}

func (p *senderPanel) panel(slot int) sep2admin.Panel {
	device := sep2admin.ActionField{Name: "device", Label: "Device", Kind: sep2admin.ActionChoice, Choices: p.deviceChoices}
	multiplier := sep2admin.ActionField{Name: "multiplier", Label: "Multiplier (power of ten)", Kind: sep2admin.ActionInteger,
		Min: sender.MinMultiplier, Max: sender.MaxMultiplier}
	value := func(unit string) sep2admin.ActionField {
		return sep2admin.ActionField{Name: "value", Label: "Value (target = value x 10^multiplier " + unit + ")", Kind: sep2admin.ActionInteger,
			Min: math.MinInt16, Max: math.MaxInt16}
	}
	return sep2admin.Panel{
		ID:                panelSender,
		Label:             "Bus sender",
		Placement:         sep2admin.ExtensionSlot(slot),
		DescriptorVersion: sep2admin.CurrentDescriptorVersion,
		View:              p.view,
		Actions: []sep2admin.Action{
			{ID: "active-power", Label: "Set active power target", Run: p.activePower,
				Fields: []sep2admin.ActionField{device, multiplier, value("W")}},
			{ID: "reactive-power", Label: "Set reactive power target", Run: p.reactivePower,
				Fields: []sep2admin.ActionField{device, multiplier, value("var")}},
			{ID: "connect", Label: "Connect or disconnect", Run: p.connect,
				Fields: []sep2admin.ActionField{device, {Name: "connect", Label: "Connect", Kind: sep2admin.ActionBoolean}}},
			{ID: "energize", Label: "Energize or de-energize", Run: p.energize,
				Fields: []sep2admin.ActionField{device, {Name: "energize", Label: "Energize", Kind: sep2admin.ActionBoolean}}},
			{ID: "raw", Label: "Send raw JSON", Run: p.raw,
				Fields: []sep2admin.ActionField{{Name: "json", Label: "Message JSON (difference_mrid required)", Kind: sep2admin.ActionText, MaxLen: sep2admin.MaxActionTextLen}}},
		},
	}
}

func (p *senderPanel) switchPanel(slot int) sep2admin.Panel {
	return sep2admin.Panel{
		ID:                panelPublishing,
		Label:             "Bus publishing",
		Placement:         sep2admin.ExtensionSlot(slot),
		DescriptorVersion: sep2admin.CurrentDescriptorVersion,
		View: func(context.Context) (sep2admin.Descriptor, error) {
			return descriptor(p.switchSection()), nil
		},
		Actions: []sep2admin.Action{{ID: "publishing", Label: "Publishing switch", Run: p.flip,
			Fields: []sep2admin.ActionField{{Name: "on", Label: "Publishing on", Kind: sep2admin.ActionToggle}}}},
	}
}

// deviceChoices offers each registered device by name and submits its
// mRID, with the graph picker's rules for ids and labels.
func (p *senderPanel) deviceChoices(context.Context) ([]sep2admin.Choice, error) {
	devs := p.s.Devices()
	// pickableChoices reads only the mRID and name of a series.
	items := make([]chartSeries, 0, len(devs))
	for _, d := range devs {
		items = append(items, chartSeries{mrid: d.MRID, name: d.Name})
	}
	return pickableChoices(items), nil
}

func (p *senderPanel) flip(ctx context.Context, v sep2admin.ActionValues) (sep2admin.ActionResult, error) {
	st := p.s.SetPublishing(v.Bool("on"), remoteFrom(ctx))
	if st.On {
		return sep2admin.ActionResult{Message: "Publishing is ON."}, nil
	}
	return sep2admin.ActionResult{Message: fmt.Sprintf("Publishing is OFF. %d send(s) were still in flight when the wait ended. %s",
		st.StillInFlight, stillDeliverable)}, nil
}

func (p *senderPanel) activePower(ctx context.Context, v sep2admin.ActionValues) (sep2admin.ActionResult, error) {
	return sent(p.s.SendActivePower(ctx, remoteFrom(ctx), v.String("device"), int(v.Int("multiplier")), int(v.Int("value"))))
}

func (p *senderPanel) reactivePower(ctx context.Context, v sep2admin.ActionValues) (sep2admin.ActionResult, error) {
	return sent(p.s.SendReactivePower(ctx, remoteFrom(ctx), v.String("device"), int(v.Int("multiplier")), int(v.Int("value"))))
}

func (p *senderPanel) connect(ctx context.Context, v sep2admin.ActionValues) (sep2admin.ActionResult, error) {
	return sent(p.s.SendConnect(ctx, remoteFrom(ctx), v.String("device"), v.Bool("connect")))
}

func (p *senderPanel) energize(ctx context.Context, v sep2admin.ActionValues) (sep2admin.ActionResult, error) {
	return sent(p.s.SendEnergize(ctx, remoteFrom(ctx), v.String("device"), v.Bool("energize")))
}

func (p *senderPanel) raw(ctx context.Context, v sep2admin.ActionValues) (sep2admin.ActionResult, error) {
	return sent(p.s.SendRaw(ctx, remoteFrom(ctx), []byte(v.String("json"))))
}

// sent answers a send. A refusal (the switch, the rate, the first failing
// JSON path) is written for the operator and shown as one. A bus failure is
// a failure: the plane logs its detail, which is transport text, and
// answers a bare 500; the send's row reads failed.
func sent(res sender.Result, err error) (sep2admin.ActionResult, error) {
	if errors.Is(err, sender.ErrPublishFailed) {
		return sep2admin.ActionResult{}, fmt.Errorf("bus sender: %w", err)
	}
	if err != nil {
		return sep2admin.ActionResult{}, &sep2admin.ActionRefusal{Reason: strings.TrimPrefix(err.Error(), "sender: ")}
	}
	return sep2admin.ActionResult{Message: fmt.Sprintf("Published difference_mrid %s to %s. Its outcome appears under Recent sends.",
		res.DifferenceMRID, res.Destination)}, nil
}

func (p *senderPanel) switchSection() sep2admin.Section {
	st := p.s.Publishing()
	state := sep2admin.BadgeCell(sep2admin.BadgeNeutral, "OFF")
	if st.On {
		state = sep2admin.BadgeCell(sep2admin.BadgeWarn, "ON")
	}
	entries := []sep2admin.DefinitionEntry{
		{Key: "Publishing", Value: state},
		{Key: "Last changed", Value: timeCell(st.ChangedAt)},
		{Key: "Changed from", Value: text(st.ChangedBy)},
	}
	if !st.On && st.StillInFlight > 0 {
		entries = append(entries, sep2admin.DefinitionEntry{Key: "Still in flight at the off flip",
			Value: sep2admin.BadgeCell(sep2admin.BadgeWarn, sep2admin.Value(fmt.Sprintf("%d send(s); they may still be delivered", st.StillInFlight)))})
	}
	return sep2admin.Section{
		Heading: "Publishing switch",
		Prose: []string{
			"Publishing is off after every start unless SEP2_ADMIN_UI_BUS_PUBLISH_AT_START is true. While it is off every send is refused. The switch is on the Bus publishing tab.",
			"Switching off refuses new sends at once and waits a few seconds for sends in flight. " + stillDeliverable,
		},
		Body: sep2admin.NewDefinitionListBody(sep2admin.DefinitionListBody{Groups: []sep2admin.DefinitionGroup{{Entries: entries}}}),
	}
}

func (p *senderPanel) view(context.Context) (sep2admin.Descriptor, error) {
	sw := p.switchSection()

	recent := p.s.Recent()
	recentRows := make([]sep2admin.Row, 0, len(recent))
	for _, e := range recent {
		recentRows = append(recentRows, sep2admin.Row{
			timeCell(e.Time), text(e.Kind), orDash(e.Remote), orDash(e.DifferenceMRID), orDash(deltasText(e.Deltas)),
			outcomeCell(e), orDash(e.Reason),
		})
	}
	refusals := p.s.Refusals()
	refusalRows := make([]sep2admin.Row, 0, len(refusals))
	for _, e := range refusals {
		refusalRows = append(refusalRows, sep2admin.Row{timeCell(e.Time), text(e.Kind), orDash(e.Remote), orDash(e.DifferenceMRID), text(e.Reason)})
	}
	d := tables(
		table("Recent sends", "Nothing sent or switched yet.",
			[]string{fmt.Sprintf("The newest %d sends and switch flips, newest first. A send's outcome is what the control path did with it.", sender.MaxRecent)},
			[]string{"Time", "Kind", "From", "difference_mrid", "Changes", "Outcome", "Reason"}, recentRows),
		table("Refusals", "No send has been refused.",
			[]string{"Sends turned away before anything was published, newest first."},
			[]string{"Time", "Kind", "From", "difference_mrid", "Reason"}, refusalRows),
	)
	d.Sections = append([]sep2admin.Section{sw}, d.Sections...)
	return d, nil
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

func outcomeCell(e sender.Entry) sep2admin.Cell {
	variant := sep2admin.BadgeNeutral
	switch e.Outcome {
	case sender.OutcomeIssued, sender.OutcomeRestated, "on":
		variant = sep2admin.BadgeOK
	case sender.OutcomeRefused, sender.OutcomeFailed:
		variant = sep2admin.BadgeError
	case sender.OutcomeNone, sender.OutcomePending:
		variant = sep2admin.BadgeWarn
	}
	return sep2admin.BadgeCell(variant, sep2admin.Value(e.Outcome))
}
