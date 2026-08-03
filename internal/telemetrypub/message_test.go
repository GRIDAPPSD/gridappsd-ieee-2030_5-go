package telemetrypub

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// buildTime is a fixed timestamp so envelope assertions are exact.
var buildTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// TestDiffMessageBuilderAggregatesEveryDeviceUnderItsOwnMRID is the
// aggregation invariant: one envelope covers all devices, and every
// difference in it carries the mRID of the device it came from. A
// builder that crossed two devices' values would publish one device's
// telemetry under another device's identity, which no amount of
// "something was published" assertion would catch.
func TestDiffMessageBuilderAggregatesEveryDeviceUnderItsOwnMRID(t *testing.T) {
	t.Parallel()

	soc := sep2.StateOfChargeStatusType{Value: 6500}
	devices := []sep2embed.DERStatusSnapshot{
		snapWithMode("mrid-a", 2),
		{MRID: "mrid-b", EDevID: "9", DERID: "1", Status: sep2.DERStatus{StateOfChargeStatus: &soc, ReadingTime: 1785714218}},
	}

	msg, err := DiffMessageBuilder("sim-1")(devices, buildTime)
	if err != nil {
		t.Fatalf("DiffMessageBuilder: %v", err)
	}
	if msg.ContentType != ContentTypeJSON {
		t.Errorf("ContentType = %q, want %q", msg.ContentType, ContentTypeJSON)
	}

	view := decodeDiffMessage(t, msg.Body)
	if view.Command != "update" {
		t.Errorf("command = %q, want %q", view.Command, "update")
	}
	if view.Input.SimulationID == nil || *view.Input.SimulationID != "sim-1" {
		t.Errorf("simulation_id = %v, want %q", view.Input.SimulationID, "sim-1")
	}
	if view.Input.Message.Timestamp != buildTime.Unix() {
		t.Errorf("timestamp = %d, want %d", view.Input.Message.Timestamp, buildTime.Unix())
	}

	byObject := map[string]map[string]any{}
	for _, fd := range view.Input.Message.ForwardDifferences {
		if byObject[fd.Object] == nil {
			byObject[fd.Object] = map[string]any{}
		}
		byObject[fd.Object][fd.Attribute] = fd.Value
	}
	if len(byObject) != 2 {
		t.Fatalf("forward differences cover %d objects, want 2: %+v", len(byObject), byObject)
	}
	if got := byObject["mrid-a"]["DERStatus.operationalModeStatus"]; got != float64(2) {
		t.Errorf("mrid-a operationalModeStatus = %v, want 2", got)
	}
	if len(byObject["mrid-a"]) != 1 {
		t.Errorf("mrid-a carried %d differences, want 1: %+v", len(byObject["mrid-a"]), byObject["mrid-a"])
	}
	if got := byObject["mrid-b"]["DERStatus.stateOfChargeStatus"]; got != float64(65) {
		t.Errorf("mrid-b stateOfChargeStatus = %v, want 65 (percent)", got)
	}
	if got := byObject["mrid-b"]["DERStatus.readingTime"]; got != float64(1785714218) {
		t.Errorf("mrid-b readingTime = %v, want 1785714218", got)
	}
	if _, crossed := byObject["mrid-b"]["DERStatus.operationalModeStatus"]; crossed {
		t.Error("mrid-b carried mrid-a's operationalModeStatus: devices were crossed")
	}
}

// TestDiffMessageBuilderPerDeviceContentMatchesThePerPUTPath is the
// content invariant for GAGO-121: batching changed, content did not. It
// reconstructs the exact envelope the removed per-PUT relay
// (sep2embed.PublishDERStatus) built for a single device, and requires
// the aggregate builder to produce the same command, simulation_id,
// timestamp, and forward/reverse difference arrays for that same device.
//
// difference_mrid is deliberately excluded from the comparison: it is a
// freshly minted UUID per envelope in both the old and new paths, and
// asserting it would pin randomness rather than content.
func TestDiffMessageBuilderPerDeviceContentMatchesThePerPUTPath(t *testing.T) {
	t.Parallel()

	device := sep2embed.DERStatusSnapshot{
		MRID:   "mrid-a",
		EDevID: "8",
		DERID:  "1",
		Status: fullDERStatus(),
	}

	// The removed per-PUT path, reproduced verbatim: map the device's
	// DERStatus, add every difference with reverse == forward, and encode
	// with the same simulation id and epoch.
	want := diff.NewBuilder("sim-1")
	diffs, err := MapDERStatusToDifferences(device.MRID, device.Status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	for _, d := range diffs {
		if err := want.AddDifference(d.Object, d.Attribute, d.Value, d.Value); err != nil {
			t.Fatalf("AddDifference: %v", err)
		}
	}
	wantBody, err := want.Bytes(buildTime.UTC().Unix())
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	wantView := decodeDiffMessage(t, wantBody)

	msg, err := DiffMessageBuilder("sim-1")([]sep2embed.DERStatusSnapshot{device}, buildTime)
	if err != nil {
		t.Fatalf("DiffMessageBuilder: %v", err)
	}
	gotView := decodeDiffMessage(t, msg.Body)

	if gotView.Command != wantView.Command {
		t.Errorf("command = %q, want %q", gotView.Command, wantView.Command)
	}
	if gotView.Input.SimulationID == nil || wantView.Input.SimulationID == nil ||
		*gotView.Input.SimulationID != *wantView.Input.SimulationID {
		t.Errorf("simulation_id = %v, want %v", gotView.Input.SimulationID, wantView.Input.SimulationID)
	}
	if gotView.Input.Message.Timestamp != wantView.Input.Message.Timestamp {
		t.Errorf("timestamp = %d, want %d", gotView.Input.Message.Timestamp, wantView.Input.Message.Timestamp)
	}
	assertDifferencesEqual(t, "forward", gotView.Input.Message.ForwardDifferences, wantView.Input.Message.ForwardDifferences)
	assertDifferencesEqual(t, "reverse", gotView.Input.Message.ReverseDifferences, wantView.Input.Message.ReverseDifferences)
}

func assertDifferencesEqual(t *testing.T, label string, got, want []diff.Difference) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s differences: got %d, want %d\n got = %+v\nwant = %+v", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Object != want[i].Object || got[i].Attribute != want[i].Attribute || got[i].Value != want[i].Value {
			t.Errorf("%s difference %d: got %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

// epriClientDERStatusBody is the exact DERStatus body the EPRI 2030.5 C
// client PUT in all nine sessions of e2e run 10. Ported unchanged from
// the removed per-PUT relay's test: the aggregate path must publish the
// same values from the same bytes.
const epriClientDERStatusBody = `<DERStatus xmlns="urn:ieee:std:2030.5:ns">` +
	`<readingTime>1785714218</readingTime>` +
	`<stateOfChargeStatus><dateTime>-5838048000</dateTime><value>6500</value></stateOfChargeStatus>` +
	`</DERStatus>`

func TestDiffMessageBuilderPublishesEPRIClientBody(t *testing.T) {
	t.Parallel()

	var status sep2.DERStatus
	if err := xml.Unmarshal([]byte(epriClientDERStatusBody), &status); err != nil {
		t.Fatalf("unmarshal EPRI client DERStatus: %v", err)
	}

	device := sep2embed.DERStatusSnapshot{MRID: "mrid-a", EDevID: "8", DERID: "1", Status: status}
	msg, err := DiffMessageBuilder("sim-1")([]sep2embed.DERStatusSnapshot{device}, buildTime)
	if err != nil {
		t.Fatalf("DiffMessageBuilder: %v", err)
	}

	if strings.Contains(string(msg.Body), "-5838048000") {
		t.Errorf("published payload carries the client's bogus dateTime.\nbody = %s", msg.Body)
	}

	view := decodeDiffMessage(t, msg.Body)
	got := make(map[string]float64, 2)
	for _, fd := range view.Input.Message.ForwardDifferences {
		v, ok := fd.Value.(float64)
		if !ok {
			t.Fatalf("forward difference %q: Value = %v (%T), want a JSON number", fd.Attribute, fd.Value, fd.Value)
		}
		got[fd.Attribute] = v
	}
	want := map[string]float64{
		"DERStatus.readingTime":         1785714218,
		"DERStatus.stateOfChargeStatus": 65,
	}
	if len(got) != len(want) {
		t.Fatalf("forward differences = %v, want exactly %v", got, want)
	}
	for attr, wantVal := range want {
		if got[attr] != wantVal {
			t.Errorf("forward difference %q: Value = %v, want %v", attr, got[attr], wantVal)
		}
	}
	if !strings.Contains(string(msg.Body), `"attribute":"DERStatus.stateOfChargeStatus","value":65}`) {
		t.Errorf("published payload does not carry the expected bare-integer JSON encoding of the scaled value.\nbody = %s", msg.Body)
	}
}

// TestDiffMessageBuilderNoMappedFieldIsNoContent: a device whose stored
// DERStatus carries no mapped field contributes nothing, and a batch of
// only such devices yields ErrNoContent rather than an empty envelope on
// the bus.
func TestDiffMessageBuilderNoMappedFieldIsNoContent(t *testing.T) {
	t.Parallel()

	devices := []sep2embed.DERStatusSnapshot{
		{MRID: "mrid-a", EDevID: "8", DERID: "1", Status: sep2.DERStatus{}},
	}
	_, err := DiffMessageBuilder("sim-1")(devices, buildTime)
	if !errors.Is(err, ErrNoContent) {
		t.Fatalf("DiffMessageBuilder error = %v, want ErrNoContent", err)
	}
}

// TestDiffMessageBuilderRejectsEmptyMRID: a snapshot with no mRID must
// fail the build rather than publish a difference with a blank object.
func TestDiffMessageBuilderRejectsEmptyMRID(t *testing.T) {
	t.Parallel()

	devices := []sep2embed.DERStatusSnapshot{snapWithMode("", 2)}
	if _, err := DiffMessageBuilder("sim-1")(devices, buildTime); err == nil {
		t.Fatal("DiffMessageBuilder with an empty mRID: want error, got nil")
	}
}

// TestDiffFingerprintTracksMappedValuesOnly pins what "unchanged" means:
// the fingerprint moves when a published value moves, and does NOT move
// when only a per-field dateTime moves, because the mapping drops
// dateTime and a dateTime-only change publishes literally the same
// bytes.
func TestDiffFingerprintTracksMappedValuesOnly(t *testing.T) {
	t.Parallel()

	socA := sep2.StateOfChargeStatusType{Value: 6500, DateTime: 100}
	socSameValueLaterTime := sep2.StateOfChargeStatusType{Value: 6500, DateTime: 999}
	socDifferentValue := sep2.StateOfChargeStatusType{Value: 6501, DateTime: 100}

	base := sep2embed.DERStatusSnapshot{MRID: "mrid-a", Status: sep2.DERStatus{StateOfChargeStatus: &socA}}
	sameValue := sep2embed.DERStatusSnapshot{MRID: "mrid-a", Status: sep2.DERStatus{StateOfChargeStatus: &socSameValueLaterTime}}
	changed := sep2embed.DERStatusSnapshot{MRID: "mrid-a", Status: sep2.DERStatus{StateOfChargeStatus: &socDifferentValue}}

	fpBase, err := DiffFingerprint(base)
	if err != nil {
		t.Fatalf("DiffFingerprint: %v", err)
	}
	fpSame, err := DiffFingerprint(sameValue)
	if err != nil {
		t.Fatalf("DiffFingerprint: %v", err)
	}
	fpChanged, err := DiffFingerprint(changed)
	if err != nil {
		t.Fatalf("DiffFingerprint: %v", err)
	}

	if fpBase != fpSame {
		t.Errorf("fingerprint changed on a dateTime-only edit: %q vs %q", fpBase, fpSame)
	}
	if fpBase == fpChanged {
		t.Errorf("fingerprint did not change when the published value changed: both %q", fpBase)
	}
}

// TestDiffFingerprintDistinguishesDevices guards the obvious identity
// slip: two devices reporting the same values must not share a
// fingerprint, or clearing one would suppress the other.
func TestDiffFingerprintDistinguishesDevices(t *testing.T) {
	t.Parallel()

	a, err := DiffFingerprint(snapWithMode("mrid-a", 2))
	if err != nil {
		t.Fatalf("DiffFingerprint: %v", err)
	}
	b, err := DiffFingerprint(snapWithMode("mrid-b", 2))
	if err != nil {
		t.Fatalf("DiffFingerprint: %v", err)
	}
	if a == b {
		t.Errorf("two devices with identical values share fingerprint %q", a)
	}
}
