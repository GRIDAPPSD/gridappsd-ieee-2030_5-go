package telemetryhistory

import (
	"math"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// Lane identifies which direction a decoded attribute represents.
type Lane string

const (
	// LaneReportedState is what a device reports it IS: the "DERStatus."
	// prefix, whose value on the wire is a bare scalar.
	LaneReportedState Lane = "reported-state"

	// LaneCommandedSetpoint is what the platform TOLD a device to be:
	// the "DERControl.DERControlBase." prefix, whose value on the wire
	// is a {multiplier, value} object.
	LaneCommandedSetpoint Lane = "commanded-setpoint"
)

const (
	derStatusPrefix  = "DERStatus."
	derControlPrefix = "DERControl.DERControlBase."
)

// defaultAllowlist names the attributes this decoder retains as
// plottable lines. DERStatus. also carries alarmStatus (a HexBinary32
// bitmap, not a scalar), genConnectStatus / operationalModeStatus /
// storageModeStatus (enums, tracked separately for a future analog
// remap), and readingTime (a device-reported timestamp, never used as
// the sample's own timestamp: see DecodeMessage's doc comment). None of
// those belong on a graph as-is, so only the two attributes below are
// retained. The allowlist doubles as the series-budget control referred
// to in the store's memory-ceiling accounting.
var defaultAllowlist = map[string]struct{}{
	derStatusPrefix + "stateOfChargeStatus": {},
	derControlPrefix + "opModTargetW":       {},
}

// classifyLane derives the Lane from attribute's dotted-path prefix. The
// two prefixes are deliberately distinct
// (internal/telemetrypub/mapping.go:17-24) specifically so a lane can
// never be confused with the other even if a future revision puts both
// directions on the same destination. An attribute matching neither
// known prefix returns ("", false): it is never defaulted into either
// lane.
func classifyLane(attribute string) (Lane, bool) {
	switch {
	case strings.HasPrefix(attribute, derStatusPrefix):
		return LaneReportedState, true
	case strings.HasPrefix(attribute, derControlPrefix):
		return LaneCommandedSetpoint, true
	default:
		return "", false
	}
}

// isAllowed reports whether attribute is on the retained allowlist.
func isAllowed(attribute string) bool {
	_, ok := defaultAllowlist[attribute]
	return ok
}

// decodeValue extracts a float64 quantity from a Difference's
// polymorphic Value field (diff.Difference.Value is `any`). Two shapes
// are recognized:
//
//   - a bare scalar: the value IS the quantity, unscaled. This is what
//     telemetrypub.MapDERStatusToDifferences emits for the UP lane
//     (mapping.go:192).
//   - an object shape {"multiplier": N, "value": V}: this is the DOWN
//     lane's shape. The multiplier is a power of ten and MUST be
//     applied: quantity = value * 10^multiplier. The one frame this
//     project has ever captured happens to carry multiplier 0, at which
//     10^0 == 1 and an implementation that ignores the field entirely
//     produces the same answer as this one, on that fixture alone. It
//     is wrong by orders of magnitude on any feeder that uses a
//     non-zero multiplier: see testdata/README.md and the two synthetic
//     fixtures that exist specifically to catch that, including a
//     negative multiplier so a reversed sign (10^-multiplier instead of
//     10^multiplier) cannot pass by accident either.
//
// encoding/json decodes both "multiplier" and "value" into float64 when
// unmarshaling into `any`, regardless of whether the wire integer was
// written as "0" or "0.0", so both type assertions below are float64.
//
// Any other shape, or an object missing either key, returns (0, false);
// the caller must skip the sample rather than treat 0 as real data.
func decodeValue(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case map[string]any:
		mRaw, hasM := val["multiplier"]
		vRaw, hasV := val["value"]
		if !hasM || !hasV {
			return 0, false
		}
		m, ok := mRaw.(float64)
		if !ok {
			return 0, false
		}
		vv, ok := vRaw.(float64)
		if !ok {
			return 0, false
		}
		return vv * math.Pow(10, m), true
	default:
		return 0, false
	}
}

// Decoded is one retainable sample extracted from a difference message,
// paired with the Lane its attribute prefix identifies.
type Decoded struct {
	Key    SeriesKey
	Sample Sample
	Lane   Lane
}

// DecodeMessage extracts the retainable Samples from msg. Only
// msg.Input.Message.ForwardDifferences are considered: reverse_differences
// are never plotted. On the commanded-setpoint lane, reverse is the
// UNDO value (0 in the real capture, against a forward of 5000), so
// plotting it would draw a phantom return to zero after every command.
// On the reported-state lane, reverse is set EQUAL to forward by design
// (internal/telemetrypub's DiffMessageBuilder, message.go:83-88: "a
// status report is an observation, not a revertible command"), so
// plotting both would double-plot identical points.
//
// A Difference is skipped, and never contributes a zero-valued sample,
// when:
//   - its attribute matches neither known lane prefix (classifyLane)
//   - its attribute is not on the retained allowlist (isAllowed)
//   - its Value is a shape decodeValue does not recognize
//
// Skips are reported to logf (nil-safe: pass nil to discard) with a
// human-readable reason describing the object and attribute; they never
// surface as an error, since a partially-decodable frame is expected
// traffic (DERStatus carries several non-numeric-line fields alongside
// the allowlisted ones), not a failure.
//
// The sample timestamp is always msg.Input.Message.Timestamp, the
// envelope's own publisher-stamped time. A device-reported per-field
// dateTime inside Value, if present, is never read: an observed EPRI
// client reported stateOfChargeStatus/dateTime landing in the year
// 1785, and a decoded sample must never carry that.
func DecodeMessage(msg diff.Message, logf func(format string, args ...any)) []Decoded {
	log := logf
	if log == nil {
		log = func(string, ...any) {}
	}

	fwd := msg.Input.Message.ForwardDifferences
	ts := msg.Input.Message.Timestamp
	out := make([]Decoded, 0, len(fwd))

	for _, d := range fwd {
		lane, ok := classifyLane(d.Attribute)
		if !ok {
			log("telemetryhistory: skip object=%q attribute=%q: unrecognized attribute prefix", d.Object, d.Attribute)
			continue
		}
		if !isAllowed(d.Attribute) {
			log("telemetryhistory: skip object=%q attribute=%q: not on retained allowlist", d.Object, d.Attribute)
			continue
		}
		value, ok := decodeValue(d.Value)
		if !ok {
			log("telemetryhistory: skip object=%q attribute=%q: unrecognized value shape %T", d.Object, d.Attribute, d.Value)
			continue
		}
		out = append(out, Decoded{
			Key:    SeriesKey{Object: d.Object, Attribute: d.Attribute},
			Sample: Sample{At: ts, Value: value},
			Lane:   lane,
		})
	}
	return out
}
