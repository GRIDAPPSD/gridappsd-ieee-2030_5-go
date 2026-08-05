package telemetrypub

import (
	"errors"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// This file moved here from internal/sep2embed/telemetry.go unchanged.
// The mapping itself is deliberately untouched: only its
// home moved, so that the embedded IEEE 2030.5 server carries no
// GridAPPS-D wire-shape code at all and the whole projection from
// protocol resource to platform message lives in one package.

// derStatusAttributePrefix names the dot-path family
// MapDERStatusToDifferences emits, paralleling the DOWN path's
// "DERControl.DERControlBase." prefix (see cmd/bridge's control
// subscriber): distinct prefixes ("DERStatus." vs
// "DERControl.DERControlBase.") mean the two directions can never be
// confused with each other even if a future revision routes them over
// the same topic.
const derStatusAttributePrefix = "DERStatus."

// derStatusStateOfChargeAttribute names the stateOfChargeStatus
// difference. Reversed from an earlier raw-passthrough choice: this
// field publishes a SCALED value, in percent, under the plain sep.xsd
// element name, and every other mapped field keeps publishing raw.
//
// sep.xsd types stateOfChargeStatus/value as PerCent (sep.xsd:4566-4582,
// sep.xsd:5945-5952): a UInt16 documented "specified in hundredths of a
// percent, 0 - 10000. (10000 = 100%)". That scale factor is fixed by the
// type itself: there is no multiplier element on the wire, unlike
// UnitValueType (sep.xsd:6169), which is how 2030.5 expresses a value
// whose scale genuinely varies and carries an explicit multiplier of
// PowerOfTenMultiplierType. The standard distinguishes the two cases
// deliberately, so PerCent is a scaled quantity whose scale is known
// unambiguously at the point of mapping, not an unresolved unit for a
// downstream consumer to guess at from a naming convention.
//
// Integer division would have been the wrong way to convert (it silently
// discards the two fractional digits the type exists to carry), which is
// why the original design rejected conversion outright and published
// the raw hundredths-of-a-percent integer instead, named accordingly.
// A float64 division is not lossy the same way: it carries the fractional digits,
// and diff.Difference.Value is already `any`, so no plumbing changes.
// 6500 (hundredths of a percent) becomes 65.0 (percent) here.
const derStatusStateOfChargeAttribute = derStatusAttributePrefix + "stateOfChargeStatus"

// derStatusStateOfChargePerCentScale converts a sep2.PerCent wire value
// (hundredths of a percent, per sep.xsd:5945-5952) to percent.
const derStatusStateOfChargePerCentScale = 100.0

// MapDERStatusToDifferences projects the DERStatus fields this bridge
// relays into diff.Difference entries, Object=mrid for every entry (the
// CIM device the status belongs to; mrid is resolved by
// sep2embed.Embed.DERStatusSnapshots through the same index allocator
// that assigned the device its URL segment, never guessed here).
//
// Coverage: every field core's sep2.DERStatus models is mapped
// (readingTime, genConnectStatus, inverterStatus, operationalModeStatus,
// stateOfChargeStatus, storageModeStatus, alarmStatus). Before this
// mapping's coverage was extended, only the middle three of those
// existed here, which made a DERStatus populating none of them a
// total silent no-op: the publish path's
// empty-slice short-circuit returned nil with nothing published and
// nothing logged. That was not theoretical: the EPRI client sends
// readingTime plus stateOfChargeStatus and nothing else, so e2e run 10
// completed 36 DERStatus PUTs, every one answered 204, and produced zero
// bus frames.
//
// sep.xsd's DERStatus also carries localControlModeStatus
// (sep.xsd:4167), manufacturerStatus (sep.xsd:4173) and storConnectStatus
// (sep.xsd:4201). Core v0.10.0 does not model any of the three, so there
// is nothing here to read; they need a core change first and are tracked
// separately. This mapping is complete with respect to core, not with
// respect to sep.xsd.
//
// Field order: the three original attributes keep their exact
// relative order at the head of the slice, ahead of the four added here,
// rather than being re-sorted into sep.xsd sequence order. The order is
// observable in the published forward_differences array, so preserving
// it keeps existing consumers byte-identical (pinned by
// TestMapDERStatusToDifferencesExistingThreeAreByteIdentical).
//
// Only fields present on status produce an entry; a DERStatus with none
// of the mapped fields set yields an empty, non-nil slice, not an error,
// since the client's PUT is otherwise still valid. Presence is a nil
// pointer check for every field except readingTime: core models that one
// as a non-pointer int64 with omitempty, so absent and epoch-0 are
// indistinguishable at this layer and a zero is therefore treated as
// absent. That is the fail-closed direction: publishing a synthesized
// 1970-01-01 reading timestamp would be a plausible-looking value
// downstream, which is exactly the invisible corruption a nil guard
// exists to prevent. A device that genuinely means epoch 0 loses one
// implausible reading; nothing else is affected.
//
// The per-field dateTime is deliberately dropped. ConnectStatusType and
// its siblings each carry a dateTime alongside their value, but
// diff.Difference is a flat Object/Attribute/Value triple with no place
// for a per-value timestamp, and the envelope already stamps its own
// publish time. Flattening dateTime into a second synthetic attribute
// would also mean republishing whatever the device sent: the EPRI client
// observed in e2e run 10 reports stateOfChargeStatus/dateTime =
// -5838048000, a negative epoch landing around the year 1785. That value
// is harmless only because it is dropped; on the bus a consumer could
// reasonably read it as a real observation time. Do not "helpfully"
// restore dateTime here without first fixing the source of values like
// that one. Pinned by
// TestMapDERStatusToDifferencesDropsPerFieldDateTime.
//
// The reverse value on each Difference is set equal to the forward
// value by the caller (see DiffMessageBuilder). This deliberately
// differs from the DOWN path's forward/reverse pair (which anticipates a
// genuine future undo): a status report is an observation, not a
// revertible command, and this bridge does not track the device's prior
// status to synthesize a real reverse. Documented here rather than
// silently reusing diff.Builder's undo-oriented contract without
// comment.
//
// Raw passthrough, unbounded (Leon LOW, PR #9 review): every
// value below is carried through exactly as the device reported it, at
// its full wire type range (genConnectStatus is sep2.HexBinary8,
// alarmStatus is sep2.HexBinary32; operationalModeStatus,
// inverterStatus and storageModeStatus are plain uint8; readingTime is
// int64; genConnectStatus and alarmStatus are on the
// hexBinary family, operationalModeStatus was and stays a plain UInt8
// per sep.xsd), with no plausibility or range check against what a real
// device could sanely report. stateOfChargeStatus is the one exception:
// its wire type (PerCent, sep.xsd:5945-5952) fixes its scale by
// definition, so it is published scaled to percent rather than raw; see
// derStatusStateOfChargeAttribute's doc comment. This is a deliberate
// discovery-stage choice, not an oversight: these are device-SUPPLIED values from an
// already ACL-scoped, mTLS-authenticated caller, so an out-of-range
// value is a malfunctioning-or-malicious device signal worth seeing
// unmodified on the bus rather than silently clamped. Adding a runtime
// bound (a sane alarmStatus bitmask, a known genConnectStatus/
// operationalModeStatus enum range) is a defense-in-depth follow-up,
// not part of this PR.
func MapDERStatusToDifferences(mrid string, status sep2.DERStatus) ([]diff.Difference, error) {
	if mrid == "" {
		return nil, errors.New("telemetrypub: MapDERStatusToDifferences: empty mrid")
	}

	diffs := make([]diff.Difference, 0, 7)

	if status.GenConnectStatus != nil {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusAttributePrefix + "genConnectStatus",
			Value:     status.GenConnectStatus.Value,
		})
	}
	if status.OperationalModeStatus != nil {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusAttributePrefix + "operationalModeStatus",
			Value:     status.OperationalModeStatus.Value,
		})
	}
	if status.AlarmStatus != nil {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusAttributePrefix + "alarmStatus",
			Value:     *status.AlarmStatus,
		})
	}

	// Fields added later, appended after the three above so the
	// pre-existing output stays byte-identical. See the doc comment for
	// why readingTime's guard is a zero check rather than a nil check,
	// and for the units and dateTime decisions.
	if status.ReadingTime != 0 {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusAttributePrefix + "readingTime",
			Value:     status.ReadingTime,
		})
	}
	if status.InverterStatus != nil {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusAttributePrefix + "inverterStatus",
			Value:     status.InverterStatus.Value,
		})
	}
	if status.StateOfChargeStatus != nil {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusStateOfChargeAttribute,
			Value:     float64(status.StateOfChargeStatus.Value) / derStatusStateOfChargePerCentScale,
		})
	}
	if status.StorageModeStatus != nil {
		diffs = append(diffs, diff.Difference{
			Object:    mrid,
			Attribute: derStatusAttributePrefix + "storageModeStatus",
			Value:     status.StorageModeStatus.Value,
		})
	}

	return diffs, nil
}
