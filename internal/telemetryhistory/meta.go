package telemetryhistory

// SeriesMeta is the fixed, attribute-derived metadata for one retained
// series: its Lane and its physical Unit. Both derive solely from the
// attribute's dotted-path prefix (mirroring classifyLane's own
// discriminator, internal/telemetrypub/mapping.go:17-24), never from the
// object mRID or from any wire value, so a caller can compute this
// metadata for an attribute string with no access to a live sample.
type SeriesMeta struct {
	// Lane is LaneReportedState or LaneCommandedSetpoint.
	Lane Lane

	// Unit names the physical quantity the decoded, already-scaled
	// Sample.Value carries: "percent" for
	// DERStatus.stateOfChargeStatus, "watts" for
	// DERControl.DERControlBase.opModTargetW. Percent and watts must
	// never share a y-axis (admin UI telemetry graphing plan, section
	// 1); this field is what lets a client enforce that without
	// inferring it from the attribute name.
	Unit string
}

// unitByAttribute pairs every entry on defaultAllowlist with the
// physical unit its decoded Sample.Value carries. Kept as its own map,
// deliberately not folded into defaultAllowlist's struct{} values, so a
// change to one is a visible, separate edit from a change to the other:
// the allowlist controls what DecodeMessage retains at all, this map
// only describes what a retained attribute MEANS.
var unitByAttribute = map[string]string{
	derStatusPrefix + "stateOfChargeStatus": "percent",
	derControlPrefix + "opModTargetW":       "watts",
}

// MetaFor returns the Lane and Unit for attribute, mirroring
// DecodeMessage's own two-step classification (classifyLane, then
// isAllowed). MetaFor's ok result is true if and only if DecodeMessage
// would have retained a sample carrying this exact attribute string, so
// a caller (the admin UI's history endpoint) can classify every series
// the Store actually holds. An attribute the Store could not possibly
// hold (an unrecognized prefix, or a recognized prefix that is not on
// the allowlist) returns SeriesMeta{}, false: callers must not fabricate
// a Lane or Unit for it.
func MetaFor(attribute string) (SeriesMeta, bool) {
	lane, ok := classifyLane(attribute)
	if !ok || !isAllowed(attribute) {
		return SeriesMeta{}, false
	}
	unit, ok := unitByAttribute[attribute]
	if !ok {
		return SeriesMeta{}, false
	}
	return SeriesMeta{Lane: lane, Unit: unit}, true
}
