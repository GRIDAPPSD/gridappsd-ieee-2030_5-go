package sep2embed

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// GAGO-034 UP path: SEP2 device telemetry -> GridAPPS-D bus.
//
// A device's DERStatus is a PUT-able resource (core's
// coreder.DERSingletonHandlers), already reachable at
// /edev/{id}/der/{derId}/ders under this bridge's existing ACL. This
// file adds a telemetry relay: after a successful, ACL-cleared DERStatus
// PUT, the just-written status is mapped to the same diff.Message wire
// shape internal/cim/diff already builds (Object/Attribute/Value) and
// published to the GridAPPS-D bus, so a device's own reported status
// becomes visible on the platform side.
//
// Deliberate transport drift from Noor's 2026-07-02 assessment: see
// BusPublisher's doc comment below.

// BusPublisher is the minimal surface PublishDERStatus needs from a
// connected message bus: fire-and-forget publish to a destination.
// Defined at the consumer (this package), per the workspace Go
// standards, rather than depending on gridappsd-go's fieldbus.MessageBus
// directly; *fieldbus.GridAPPSDMessageBus (and the fieldbus.MessageBus
// interface itself) already satisfies this method set unchanged.
//
// The assessment named "the existing cimstomp publisher"
// (internal/cimstomp.Publisher) as the UP-path transport. That was
// written before GAGO-039 replaced cimstomp's own STOMP connection with
// gridappsd-go's fieldbus.MessageBus as the bridge's live transport (see
// cmd/bridge/main.go's package doc comment: cimstomp "no longer backs
// the bridge's connection"). cimstomp.Publisher is not connected in the
// running bridge as of this card, so wiring the UP path through it would
// require standing up a second, independent STOMP session purely for
// this relay. BusPublisher instead lets the relay reuse the SAME
// fieldbus.MessageBus the bridge already holds; the wire payload itself
// (internal/cim/diff.Builder's JSON envelope) is unchanged from what the
// assessment specified.
type BusPublisher interface {
	Send(ctx context.Context, destination, contentType string, body []byte) error
}

// telemetryContentType is the STOMP content-type stamped on every
// telemetry-relay publish. internal/cim/diff.Bytes always produces JSON.
const telemetryContentType = "application/json"

// derStatusAttributePrefix names the dot-path family
// MapDERStatusToDifferences emits, paralleling
// derControlAttributePrefix's "DERControl.DERControlBase." on the DOWN
// path: distinct prefixes ("DERStatus." vs "DERControl.DERControlBase.")
// mean the two directions can never be confused with each other even if
// a future revision routes them over the same topic.
const derStatusAttributePrefix = "DERStatus."

// derStatusStateOfChargeAttribute names the stateOfChargeStatus
// difference. Reversed from the GAGO-110 raw-passthrough choice: this
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
// why GAGO-110 rejected conversion outright and published the raw
// hundredths-of-a-percent integer instead, named accordingly. A float64
// division is not lossy the same way: it carries the fractional digits,
// and diff.Difference.Value is already `any`, so no plumbing changes.
// 6500 (hundredths of a percent) becomes 65.0 (percent) here.
const derStatusStateOfChargeAttribute = derStatusAttributePrefix + "stateOfChargeStatus"

// derStatusStateOfChargePerCentScale converts a sep2.PerCent wire value
// (hundredths of a percent, per sep.xsd:5945-5952) to percent.
const derStatusStateOfChargePerCentScale = 100.0

// MapDERStatusToDifferences projects the DERStatus fields this bridge
// relays into diff.Difference entries, Object=mrid for every entry (the
// CIM device the status belongs to; see PublishDERStatus's doc comment
// for how mrid is resolved from the caller's own LFDI).
//
// Coverage: every field core's sep2.DERStatus models is mapped
// (readingTime, genConnectStatus, inverterStatus, operationalModeStatus,
// stateOfChargeStatus, storageModeStatus, alarmStatus). Before GAGO-110
// only the middle three of those existed here, which made a DERStatus
// populating none of them a total silent no-op: PublishDERStatus's
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
// Field order: the three pre-GAGO-110 attributes keep their exact
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
// value. This deliberately differs from the DOWN path's forward/reverse
// pair (which anticipates a genuine future undo): a status report is an
// observation, not a revertible command, and this bridge does not track
// the device's prior status to synthesize a real reverse. Documented
// here rather than silently reusing diff.Builder's undo-oriented
// contract without comment.
//
// Raw passthrough, unbounded (Leon LOW, GAGO-034 PR #9 review): every
// value below is carried through exactly as the device reported it, at
// its full wire type range (genConnectStatus is sep2.HexBinary8,
// alarmStatus is sep2.HexBinary32; operationalModeStatus,
// inverterStatus and storageModeStatus are plain uint8; readingTime is
// int64; IEEECORE-047 moved genConnectStatus and alarmStatus onto the
// hexBinary family, operationalModeStatus was and stays a plain UInt8
// per sep.xsd), with no plausibility or range check against what a real
// device could sanely report. stateOfChargeStatus is the one exception:
// its wire type (PerCent, sep.xsd:5945-5952) fixes its scale by
// definition, so it is published scaled to percent rather than raw; see
// derStatusStateOfChargeAttribute's doc comment. This is a deliberate
// discovery-stage choice (GAGO-046 tracks the enum/bitmap passthrough
// broadly), not an oversight: these are device-SUPPLIED values from an
// already ACL-scoped, mTLS-authenticated caller, so an out-of-range
// value is a malfunctioning-or-malicious device signal worth seeing
// unmodified on the bus rather than silently clamped. Adding a runtime
// bound (a sane alarmStatus bitmask, a known genConnectStatus/
// operationalModeStatus enum range) is a defense-in-depth follow-up,
// not part of this PR.
func MapDERStatusToDifferences(mrid string, status sep2.DERStatus) ([]diff.Difference, error) {
	if mrid == "" {
		return nil, errors.New("sep2embed: MapDERStatusToDifferences: empty mrid")
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

	// Fields added by GAGO-110, appended after the three above so the
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

// PublishDERStatus maps status (via MapDERStatusToDifferences) to a
// diff.Builder envelope and sends it to dest over pub. now is threaded
// explicitly (rather than read from time.Now() internally) so callers
// can assert a deterministic timestamp in tests; the HTTP wiring in
// telemetryMiddleware passes time.Now().
//
// A DERStatus with no mapped fields set (MapDERStatusToDifferences
// returns an empty slice) is a no-op: nothing is published, and nil is
// returned rather than sending an empty envelope. That no-op is LOGGED
// (GAGO-110). It used to be entirely silent, which is what made the
// missing-field class of bug hard to diagnose: a device's PUT succeeded,
// the relay ran, and the absence of a bus frame was indistinguishable
// from success at every layer except the topic itself.
//
// Logged unconditionally rather than rate-limited: now that every field
// core models is mapped, reaching this branch means the device PUT a
// DERStatus carrying no status field at all, which is a malformed-client
// signal and inherently rare, so there is no volume to suppress and no
// per-device state worth keeping to suppress it. Only the mrid is
// logged, matching the other relay log lines in this file; no field
// values are logged.
func PublishDERStatus(ctx context.Context, pub BusPublisher, dest, simID, mrid string, status sep2.DERStatus, now time.Time) error {
	diffs, err := MapDERStatusToDifferences(mrid, status)
	if err != nil {
		return fmt.Errorf("sep2embed: PublishDERStatus: %w", err)
	}
	if len(diffs) == 0 {
		log.Printf("sep2embed: PublishDERStatus: DERStatus for mrid=%s carried no mapped field; nothing published", mrid)
		return nil
	}

	b := diff.NewBuilder(simID)
	for _, d := range diffs {
		if err := b.AddDifference(d.Object, d.Attribute, d.Value, d.Value); err != nil {
			return fmt.Errorf("sep2embed: PublishDERStatus: build envelope: %w", err)
		}
	}

	payload, err := b.Bytes(now.UTC().Unix())
	if err != nil {
		return fmt.Errorf("sep2embed: PublishDERStatus: encode envelope: %w", err)
	}

	if err := pub.Send(ctx, dest, telemetryContentType, payload); err != nil {
		return fmt.Errorf("sep2embed: PublishDERStatus: send: %w", err)
	}
	return nil
}

// endDeviceKeyResolver is the minimal surface telemetryMiddleware needs to
// resolve the URL {id} segment back to the CIM device key (mRID) seeding
// allocated that segment under. Defined at the consumer (this package),
// per the workspace Go standards, rather than depending on
// *memory.EndDeviceIndex directly; that type satisfies this method set
// unchanged, and a test double can supply it without pulling in the real
// allocator (GAGO-109).
//
// The {id} segment is the opaque, server-chosen index seed.go's
// stores.EndDeviceIndexes.Allocate hands out, keyed on the CIM mRID (see
// seed.go's own doc comment): the reverse direction is DeviceKey(index),
// not a certificate-derived-LFDI lookup, because seeding never indexed
// EndDevices by LFDI in the first place.
type endDeviceKeyResolver interface {
	DeviceKey(index string) (string, bool)
}

// telemetryConfig groups the optional UP-path wiring. The zero value
// (enabled() == false) disables the relay entirely: buildHandler
// composes telemetryMiddleware as a pass-through in that case, so a
// bridge that never sets Config.Bus/Config.SimulationID sees no
// behavior change from this card.
type telemetryConfig struct {
	bus       BusPublisher
	edevIndex endDeviceKeyResolver
	dest      string
	simID     string
}

func (c telemetryConfig) enabled() bool {
	return c.bus != nil && c.edevIndex != nil && c.dest != "" && c.simID != ""
}

// derStatusPathEndDeviceID extracts the {id} segment from a
// "/edev/{id}/der/{derId}/ders" path. ok is false for any other shape
// (wrong length, wrong literal segments); ok is deliberately NOT
// conditioned on {id} or {derId} being non-empty, mirroring
// sep2acl.EndDeviceID's boundary-of-the-boundary discipline: a
// malformed path is reported as a shape match with a blank id rather
// than silently falling through as "not a DERStatus path".
func derStatusPathEndDeviceID(path string) (edevID string, ok bool) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "", false
	}
	segs := strings.Split(trimmed, "/")
	if len(segs) != 5 || segs[0] != "edev" || segs[2] != "der" || segs[4] != "ders" {
		return "", false
	}
	return segs[1], true
}

// statusRecorder wraps an http.ResponseWriter to capture the status code
// the wrapped handler wrote, defaulting to 200 (net/http's own default
// when a handler calls Write without ever calling WriteHeader).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// telemetryMiddleware wraps next so that a successful PUT to
// /edev/{id}/der/{derId}/ders also relays the just-written DERStatus to
// the GridAPPS-D bus via cfg. When cfg is not enabled, next is returned
// unwrapped: zero overhead, zero behavior change.
//
// Composed inside aclMiddleware in buildHandler (see auth.go), so this
// middleware only ever observes requests the ACL already confirmed are
// the caller's OWN device: {id} in the path is the caller's own LFDI by
// construction, and no separate ownership check is needed here.
//
// A relay failure (bad XML, unknown mRID, bus send error) is logged and
// does not affect the response already written to the device: the
// device's PUT succeeded and got its 204 regardless of whether the
// platform-side echo landed.
func telemetryMiddleware(cfg telemetryConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !cfg.enabled() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			edevID, ok := derStatusPathEndDeviceID(r.URL.Path)
			if r.Method != http.MethodPut || !ok {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				// Let the real handler see (and fail on) the same read
				// error; nothing to relay.
				r.Body = io.NopCloser(bytes.NewReader(nil))
				next.ServeHTTP(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			if rec.status != http.StatusOK && rec.status != http.StatusNoContent {
				return // PUT was rejected upstream; nothing changed to relay.
			}

			var status sep2.DERStatus
			if err := xml.Unmarshal(body, &status); err != nil {
				log.Printf("sep2embed: telemetry relay: decode DERStatus edev=%s: %v", edevID, err)
				return
			}

			// edevID is the {id} segment of the request path, which is the
			// opaque, server-chosen URL index seed.go's
			// stores.EndDeviceIndexes.Allocate assigned to this device (see
			// seed.go:202-220), NOT its LFDI: seeding never keyed
			// EndDevices by LFDI. Resolve it through cfg.edevIndex, the
			// index-to-mRID reverse lookup that same allocator exposes.
			// This middleware runs inside aclMiddleware, so edevID is
			// already the caller's own confirmed device.
			mrid, ok := cfg.edevIndex.DeviceKey(edevID)
			if !ok {
				log.Printf("sep2embed: telemetry relay: no registered mRID for edev=%s; dropping telemetry", edevID)
				return
			}

			if err := PublishDERStatus(r.Context(), cfg.bus, cfg.dest, cfg.simID, mrid, status, time.Now()); err != nil {
				log.Printf("sep2embed: telemetry relay: publish edev=%s mrid=%s: %v", edevID, mrid, err)
			}
		})
	}
}
