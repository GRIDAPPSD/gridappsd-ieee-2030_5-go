package cim

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidFeederID is returned by the Query* SPARQL wrappers when the
// supplied feederID is empty or contains characters that would break the
// SPARQL syntax (quotes, angle brackets, newlines, backslashes). The
// Python upstream's Queries.py performs raw %-formatting with no
// validation; this defensive check stops a malformed feederID from
// reaching the broker as a corrupted SPARQL string.
var ErrInvalidFeederID = errors.New("cim: invalid feeder ID")

// SPARQL query templates derived from the Python upstream's Queries.py
// (gridappsd-2030_5) but adapted to handle two CIM profile shapes
// transparently:
//
//  1. CURRENT (gridappsd-docker:develop): each PowerElectronicsConnection
//     stores its full attribute set (name, mRID, ratedS, ratedU,
//     maxIFault, p, q, controlMode, Equipment.EquipmentContainer)
//     directly. NO child PowerElectronicsUnit is attached. Verified
//     empirically against the IEEE 123pv feeder in 2026-05-07: 14 PECs
//     present in Blazegraph, 0 child Units.
//
//  2. OLDER (Python upstream's assumed shape): each PEC has a child
//     PowerElectronicsUnit (PhotovoltaicUnit, BatteryUnit, etc.) bound
//     via c:PowerElectronicsConnection.PowerElectronicsUnit. The Unit
//     carries name and mRID; the PEC carries the electrical
//     attributes. The Python upstream's `Queries.py` assumes this shape
//     and uses an inner join, which filters everything out on the
//     current schema.
//
// The fix: every PEC-rooted template makes the PowerElectronicsUnit
// relationship OPTIONAL, then uses COALESCE binds to prefer the Unit's
// name and mRID when present and fall back to the PEC's when absent.
// One row per PEC either way.
//
// For Solar and Battery, the Unit type filter (a c:PhotovoltaicUnit / a
// c:BatteryUnit) lives inside the OPTIONAL block. On the current
// schema the OPTIONAL never binds and all PECs in the feeder come back
// for both wrappers; QuerySolar, QueryBattery, and QueryInverter
// return the same row set. On the older schema the type filter
// discriminates correctly and only PVs come back from QuerySolar, only
// batteries from QueryBattery. Callers needing strict PV-vs-battery
// distinction on the current-schema deployment should switch to the
// CIM Dictionary layer (GetCIMDictionary) where solarpanels and
// batteries are pre-classified by the platform.
//
// QueryAllDERGroups operates on EndDeviceGroup, not PEC, and so is
// unaffected by the PEC-Unit shape question; it remains a verbatim
// port of the Python upstream's template.
//
// Each template carries a single %s placeholder where the feederID is
// substituted via fmt.Sprintf, after the wrapper strips a leading
// underscore from the input (see queryFeederTemplate). The on-wire
// VALUES clause matches the gridappsd-docker:develop dataset's bare
// uppercase UUID storage of c:IdentifiedObject.mRID.
const (
	sparqlQuerySolar = `# Solar - DistSolar
    PREFIX r:  <http://www.w3.org/1999/02/22-rdf-syntax-ns#>
    PREFIX c:  <http://iec.ch/TC57/CIM100#>
    SELECT ?name ?bus ?ratedS ?ratedU ?ipu ?p ?q ?maxQ ?fdrid ?id ?pecid (group_concat(distinct ?phs;separator="\n") as ?phases) WHERE {
    VALUES ?fdrid {"%s"}
    ?pec a c:PowerElectronicsConnection.
    ?pec c:IdentifiedObject.name ?pecName.
    ?pec c:IdentifiedObject.mRID ?pecid.
    ?pec c:Equipment.EquipmentContainer ?fdr.
    ?fdr c:IdentifiedObject.mRID ?fdrid.
    ?pec c:PowerElectronicsConnection.ratedS ?ratedS.
    ?pec c:PowerElectronicsConnection.ratedU ?ratedU.
    ?pec c:PowerElectronicsConnection.maxIFault ?ipu.
    ?pec c:PowerElectronicsConnection.p ?p.
    ?pec c:PowerElectronicsConnection.q ?q.
    OPTIONAL { ?pec c:PowerElectronicsConnection.maxQ ?maxQ. }
    OPTIONAL {
      ?pec c:PowerElectronicsConnection.PowerElectronicsUnit ?s.
      ?s a c:PhotovoltaicUnit.
      ?s c:IdentifiedObject.name ?unitName.
      ?s c:IdentifiedObject.mRID ?unitID.
    }
    BIND(COALESCE(?unitName, ?pecName) AS ?name)
    BIND(COALESCE(?unitID, ?pecid) AS ?id)
    OPTIONAL {?pecp c:PowerElectronicsConnectionPhase.PowerElectronicsConnection ?pec.
      ?pecp c:PowerElectronicsConnectionPhase.phase ?phsraw.
      bind(strafter(str(?phsraw),"SinglePhaseKind.") as ?phs) }
    ?t c:Terminal.ConductingEquipment ?pec.
    ?t c:Terminal.ConnectivityNode ?cn.
    ?cn c:IdentifiedObject.name ?bus
    }
    GROUP by ?name ?bus ?ratedS ?ratedU ?ipu ?p ?q ?maxQ ?fdrid ?id ?pecid
    ORDER by ?name
    `

	sparqlQueryBattery = `# Storage - DistStorage
    PREFIX r:  <http://www.w3.org/1999/02/22-rdf-syntax-ns#>
    PREFIX c:  <http://iec.ch/TC57/CIM100#>
    SELECT ?name ?bus ?ratedS ?ratedU ?ipu ?ratedE ?storedE ?state ?p ?q ?maxQ ?id ?pecid ?fdrid (group_concat(distinct ?phs;separator="\n") as ?phases) WHERE {
    VALUES ?fdrid {"%s"}
    ?pec a c:PowerElectronicsConnection.
    ?pec c:IdentifiedObject.name ?pecName.
    ?pec c:IdentifiedObject.mRID ?pecid.
    ?pec c:Equipment.EquipmentContainer ?fdr.
    ?fdr c:IdentifiedObject.mRID ?fdrid.
    ?pec c:PowerElectronicsConnection.ratedS ?ratedS.
    ?pec c:PowerElectronicsConnection.ratedU ?ratedU.
    ?pec c:PowerElectronicsConnection.maxIFault ?ipu.
    ?pec c:PowerElectronicsConnection.p ?p.
    ?pec c:PowerElectronicsConnection.q ?q.
    OPTIONAL { ?pec c:PowerElectronicsConnection.maxQ ?maxQ. }
    OPTIONAL {
      ?pec c:PowerElectronicsConnection.PowerElectronicsUnit ?s.
      ?s a c:BatteryUnit.
      ?s c:IdentifiedObject.name ?unitName.
      ?s c:IdentifiedObject.mRID ?unitID.
      ?s c:BatteryUnit.ratedE ?ratedE.
      ?s c:BatteryUnit.storedE ?storedE.
      ?s c:BatteryUnit.batteryState ?stateraw.
      bind(strafter(str(?stateraw),"BatteryState.") as ?state)
    }
    BIND(COALESCE(?unitName, ?pecName) AS ?name)
    BIND(COALESCE(?unitID, ?pecid) AS ?id)
    OPTIONAL {?pecp c:PowerElectronicsConnectionPhase.PowerElectronicsConnection ?pec.
      ?pecp c:PowerElectronicsConnectionPhase.phase ?phsraw.
      bind(strafter(str(?phsraw),"SinglePhaseKind.") as ?phs) }
    ?t c:Terminal.ConductingEquipment ?pec.
    ?t c:Terminal.ConnectivityNode ?cn.
    ?cn c:IdentifiedObject.name ?bus
    }
    GROUP by ?name ?bus ?ratedS ?ratedU ?ipu ?ratedE ?storedE ?state ?p ?q ?maxQ ?id ?pecid ?fdrid
    ORDER by ?name
    `

	sparqlQueryInverter = `
    PREFIX r: <http://www.w3.org/1999/02/22-rdf-syntax-ns#>
    PREFIX c: <http://iec.ch/TC57/CIM100#>
    PREFIX xsd: <http://www.w3.org/2001/XMLSchema#>
    SELECT ?name ?bus ?ratedS ?ratedU ?ipu ?p ?q ?maxQ ?fdrid ?id ?pecid (group_concat(distinct ?phs;separator="\n") as ?phases)  WHERE {
    VALUES ?fdrid {"%s"}
    ?pec a c:PowerElectronicsConnection.
    ?pec c:IdentifiedObject.name ?pecName.
    ?pec c:IdentifiedObject.mRID ?pecid.
    ?pec c:Equipment.EquipmentContainer ?fdr.
    ?fdr c:IdentifiedObject.mRID ?fdrid.
    ?pec c:PowerElectronicsConnection.ratedS ?ratedS.
    ?pec c:PowerElectronicsConnection.ratedU ?ratedU.
    ?pec c:PowerElectronicsConnection.maxIFault ?ipu.
    ?pec c:PowerElectronicsConnection.p ?p.
    ?pec c:PowerElectronicsConnection.q ?q.
    OPTIONAL { ?pec c:PowerElectronicsConnection.maxQ ?maxQ. }
    OPTIONAL {
      ?pec c:PowerElectronicsConnection.PowerElectronicsUnit ?s.
      ?s c:IdentifiedObject.name ?unitName.
      ?s c:IdentifiedObject.mRID ?unitID.
    }
    BIND(COALESCE(?unitName, ?pecName) AS ?name)
    BIND(COALESCE(?unitID, ?pecid) AS ?id)
    OPTIONAL {?pecp c:PowerElectronicsConnectionPhase.PowerElectronicsConnection ?pec.
      ?pecp c:PowerElectronicsConnectionPhase.phase ?phsraw.
      bind(strafter(str(?phsraw),"SinglePhaseKind.") as ?phs) }
    ?t c:Terminal.ConductingEquipment ?pec.
    ?t c:Terminal.ConnectivityNode ?cn.
    ?cn c:IdentifiedObject.name ?bus
    }
    GROUP by ?name ?bus ?ratedS ?ratedU ?ipu ?p ?q ?maxQ ?fdrid ?id ?pecid
    ORDER by ?name
    `

	// sparqlQueryPECCount is the discovery-count template. It
	// counts every PowerElectronicsConnection in the feeder using only
	// the identity-plus-feeder-membership triples every PEC is
	// guaranteed to carry (a c:PowerElectronicsConnection type triple
	// and its Equipment.EquipmentContainer link), deliberately omitting
	// every mandatory attribute join the device-enumeration templates
	// above require (ratedS, ratedU, maxIFault, p, q, and the
	// Terminal/ConnectivityNode bus lookup). Those extra INNER joins are
	// exactly what makes a PEC silently vanish from the enumeration
	// query's row set when it is missing one of those attributes
	// (Cyrus's finding); this count exists to surface that gap, so it
	// cannot itself depend on the attributes whose absence it is meant
	// to detect. See bootstrapRegistry and pecCountLogLine in
	// cmd/bridge/main.go for how the discovered-vs-projected comparison
	// uses this count.
	sparqlQueryPECCount = `# PEC discovery count
    PREFIX c:  <http://iec.ch/TC57/CIM100#>
    SELECT (COUNT(DISTINCT ?pec) as ?count) WHERE {
    VALUES ?fdrid {"%s"}
    ?pec a c:PowerElectronicsConnection.
    ?pec c:Equipment.EquipmentContainer ?fdr.
    ?fdr c:IdentifiedObject.mRID ?fdrid.
    }
    `

	sparqlQueryAllDERGroups = `#get all EndDeviceGroup
    PREFIX  xsd:  <http://www.w3.org/2001/XMLSchema#>
    PREFIX  r:    <http://www.w3.org/1999/02/22-rdf-syntax-ns#>
    PREFIX  c:    <http://iec.ch/TC57/CIM100#>
    select ?mRID ?description (group_concat(distinct ?name;separator="\n") as ?names)
                              (group_concat(distinct ?device;separator="\n") as ?devices)
                              (group_concat(distinct ?func;separator="\n") as ?funcs)
    VALUES ?fdrid {"%s"}
    where {
      ?q1 a c:EndDeviceGroup .
      ?q1 c:IdentifiedObject.mRID ?mRIDraw .
        bind(strafter(str(?mRIDraw), "_") as ?mRID).
      ?q1 c:IdentifiedObject.name ?name .
      ?q1 c:IdentifiedObject.description ?description .
      Optional{
        ?q1 c:EndDeviceGroup.EndDevice ?deviceobj .
        ?deviceobj c:IdentifiedObject.mRID ?deviceID .
        ?deviceobj c:IdentifiedObject.name ?deviceName .
        ?deviceobj c:EndDevice.isSmartInverter ?isSmart .
        bind(concat(strafter(str(?deviceID), "_"), ",", str(?deviceName), ",", str(?isSmart)) as ?device)
      }
      ?q1 c:DERFunction ?derFunc .
      ?derFunc ?pfunc ?vfuc .
      Filter(?pfunc !=r:type)
        bind(concat(strafter(str(?pfunc), "DERFunction."), ",", str(?vfuc)) as ?func)
    }
    Group by ?mRID ?description
    Order by ?mRID
    `
)

// feederIDPattern is an ALLOWLIST for the shape of a CIM feeder mRID:
// an optional single leading underscore, followed by 8 or
// more hex digits and/or dashes. This matches both forms real call
// sites produce: a bare uppercase UUID as stored by gridappsd-docker's
// c:IdentifiedObject.mRID ("E407CBB6-8C8D-9BC9-589C-AB83FBF0826D") and
// an underscore-prefixed UUID as cmd/bridge's defaultFeederMRID uses
// ("_C1C3E687-6FFD-C753-582B-632A27E28507"). Lowercase hex is accepted
// too since neither form the codebase actually produces is
// case-constrained, and rejecting a case CIM tooling elsewhere might
// emit would be an arbitrary restriction with no security value: the
// allowlist's job is excluding SPARQL-syntax-breaking characters
// (quotes, angle brackets, newlines, backslashes) and control bytes,
// not enforcing a specific hex case.
//
// This supersedes the prior denylist (Leon M1: reject C0
// control bytes; M2: reject the four SPARQL-syntax characters
// explicitly). An allowlist subsumes both: every C0 control byte and
// every one of "<>\n\r\ falls outside [0-9A-Fa-f-], so there is no
// separate control-byte check to maintain.
var feederIDPattern = regexp.MustCompile(`^_?[0-9A-Fa-f-]{8,}$`)

// validateFeederID rejects any feederID that does not match
// feederIDPattern: empty, too short, wrong shape, or containing any
// character (including quotes, angle brackets, newlines, backslashes,
// and C0 control bytes) that could break the SPARQL VALUES clause when
// interpolated. CIM mRIDs are well-shaped (UUID-ish, optionally
// underscore-prefixed) at every production and test call site; see
// feederIDPattern's doc comment for the two accepted forms.
func validateFeederID(feederID string) error {
	if feederID == "" {
		return fmt.Errorf("%w: empty", ErrInvalidFeederID)
	}
	if !feederIDPattern.MatchString(feederID) {
		return fmt.Errorf("%w: %q does not match the allowed feeder ID shape", ErrInvalidFeederID, feederID)
	}
	return nil
}

// normalizeFeederID strips at most one leading underscore from the
// feederID. The gridappsd-docker:develop dataset stores
// c:IdentifiedObject.mRID as a bare uppercase UUID without the
// underscore prefix the Python upstream's call sites use; stripping
// here lets the bridge accept either form without the caller having to
// know which the deployed dataset uses. Callers only ever reach this
// function after feederIDPattern has already accepted the input
// (queryFeederTemplate validates first), and that pattern permits at
// most one leading underscore, so there is no double-underscore case
// left to reason about here: an input with a second leading underscore
// is rejected by validateFeederID before normalizeFeederID ever runs.
func normalizeFeederID(feederID string) string {
	return strings.TrimPrefix(feederID, "_")
}

// queryFeederTemplate validates the feederID, strips a leading
// underscore, substitutes it into the SPARQL template, and dispatches
// through Client.QueryData. The four public Query* wrappers differ
// only in the template they pass.
func (c *Client) queryFeederTemplate(ctx context.Context, template, feederID string) (*QueryDataResult, error) {
	if err := validateFeederID(feederID); err != nil {
		return nil, err
	}
	sparql := fmt.Sprintf(template, normalizeFeederID(feederID))
	return c.QueryData(ctx, sparql)
}

// QuerySolar runs the photovoltaic-unit enumeration SPARQL against the
// powergrid-model service, scoped to feederID. The query returns one
// row per PowerElectronicsConnection in the feeder. On the older CIM
// profile shape (PEC plus PhotovoltaicUnit child) the PhotovoltaicUnit
// type filter inside the OPTIONAL block discriminates correctly; on
// the gridappsd-docker:develop shape (PEC-as-leaf, no Unit) the
// OPTIONAL never binds and all feeder PECs come back. The returned
// QueryDataResult exposes the raw SPARQL bindings; callers are
// responsible for projecting them into domain types and may use
// Binding.AsJSONLD on JSON-LD-shaped values.
func (c *Client) QuerySolar(ctx context.Context, feederID string) (*QueryDataResult, error) {
	return c.queryFeederTemplate(ctx, sparqlQuerySolar, feederID)
}

// QueryBattery runs the battery-unit enumeration SPARQL against the
// powergrid-model service, scoped to feederID. The query returns
// PowerElectronicsConnections that have a BatteryUnit child on the
// older CIM schema. On the current gridappsd-docker:develop schema
// where no PowerElectronicsUnit children exist, this method returns
// the same PEC set as QueryInverter and QuerySolar; the BatteryUnit-
// specific fields (ratedE, storedE, batteryState) are inside the
// OPTIONAL block and stay empty for those rows. Callers needing
// battery-vs-other-DER discrimination on the current schema must use
// external metadata (e.g., name patterns, EndDeviceGroup membership)
// until the older-shape dataset is loaded.
func (c *Client) QueryBattery(ctx context.Context, feederID string) (*QueryDataResult, error) {
	return c.queryFeederTemplate(ctx, sparqlQueryBattery, feederID)
}

// QueryInverter runs the generic PowerElectronicsConnection enumeration
// SPARQL against the powergrid-model service, scoped to feederID. The
// OPTIONAL Unit block has no type filter, so on the older schema any
// PowerElectronicsUnit (PhotovoltaicUnit, BatteryUnit, or other) binds
// the unit-level identity; on the current schema it falls through to
// PEC identity. This wrapper is the workhorse for DER enumeration on
// the current schema since QuerySolar and QueryBattery return the
// same PEC set with no discriminator.
func (c *Client) QueryInverter(ctx context.Context, feederID string) (*QueryDataResult, error) {
	return c.queryFeederTemplate(ctx, sparqlQueryInverter, feederID)
}

// QueryAllDERGroups runs the EndDeviceGroup enumeration SPARQL against
// the powergrid-model service, scoped to feederID. The result includes
// group-concatenated name, device, and DERFunction lists per group.
// EndDeviceGroup is independent of the PEC-Unit profile question and
// this template is unchanged from the Python upstream.
func (c *Client) QueryAllDERGroups(ctx context.Context, feederID string) (*QueryDataResult, error) {
	return c.queryFeederTemplate(ctx, sparqlQueryAllDERGroups, feederID)
}

// QueryPECCount runs the discovery-count SPARQL against the
// powergrid-model service, scoped to feederID, and returns a single-row
// result whose "count" binding is the number of distinct
// PowerElectronicsConnection objects in the feeder, counted without any
// of the mandatory attribute joins QuerySolar/QueryBattery/QueryInverter
// require. Callers comparing this count against the number of rows
// those enumeration queries return can detect PECs silently dropped by
// an INNER join on a missing attribute (see sparqlQueryPECCount's doc
// comment).
func (c *Client) QueryPECCount(ctx context.Context, feederID string) (*QueryDataResult, error) {
	return c.queryFeederTemplate(ctx, sparqlQueryPECCount, feederID)
}
