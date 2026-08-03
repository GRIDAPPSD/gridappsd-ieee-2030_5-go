package sep2embed

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

func TestMapDERStatusToDifferences(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{Value: 1, DateTime: 100}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 200}
	alarm := sep2.HexBinary32(7)

	status := sep2.DERStatus{
		GenConnectStatus:      &conn,
		OperationalModeStatus: &mode,
		AlarmStatus:           &alarm,
	}

	diffs, err := MapDERStatusToDifferences("mrid-a", status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 3 {
		t.Fatalf("len(diffs) = %d, want 3", len(diffs))
	}

	byAttr := make(map[string]diff.Difference, len(diffs))
	for _, d := range diffs {
		if d.Object != "mrid-a" {
			t.Errorf("difference %q: Object = %q, want %q", d.Attribute, d.Object, "mrid-a")
		}
		byAttr[d.Attribute] = d
	}

	// ConnectStatusType.Value is sep2.HexBinary8 (sep.xsd:4471,
	// IEEECORE-047), unlike its OperationalModeStatusType sibling below,
	// which stays plain UInt8 (sep.xsd:4559) and so stays uint8 here too.
	if v, ok := byAttr["DERStatus.genConnectStatus"]; !ok || v.Value != sep2.HexBinary8(1) {
		t.Errorf("DERStatus.genConnectStatus = %+v, want Value=1", v)
	}
	if v, ok := byAttr["DERStatus.operationalModeStatus"]; !ok || v.Value != uint8(2) {
		t.Errorf("DERStatus.operationalModeStatus = %+v, want Value=2", v)
	}
	if v, ok := byAttr["DERStatus.alarmStatus"]; !ok || v.Value != sep2.HexBinary32(7) {
		t.Errorf("DERStatus.alarmStatus = %+v, want Value=7", v)
	}
}

func TestMapDERStatusToDifferencesEmptyStatusYieldsNoDifferences(t *testing.T) {
	t.Parallel()

	diffs, err := MapDERStatusToDifferences("mrid-a", sep2.DERStatus{})
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("len(diffs) = %d, want 0 for an empty DERStatus", len(diffs))
	}
}

func TestMapDERStatusToDifferencesRejectsEmptyMRID(t *testing.T) {
	t.Parallel()

	if _, err := MapDERStatusToDifferences("", sep2.DERStatus{}); err == nil {
		t.Fatal("MapDERStatusToDifferences(\"\", ...): want error, got nil")
	}
}

// fakeBusPublisher records every Send call for assertion.
type fakeBusPublisher struct {
	mu    sync.Mutex
	sends []fakeSend
	err   error
}

type fakeSend struct {
	dest        string
	contentType string
	body        []byte
}

func (f *fakeBusPublisher) Send(_ context.Context, destination, contentType string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, fakeSend{dest: destination, contentType: contentType, body: append([]byte(nil), body...)})
	return f.err
}

func (f *fakeBusPublisher) snapshot() []fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeSend, len(f.sends))
	copy(out, f.sends)
	return out
}

// decodeDiffMessage round-trips a diff.Builder payload back into a
// generic map so the test can assert on the exact JSON field values
// without depending on diff package internals beyond its own public
// Message/Difference shapes.
func decodeDiffMessage(t *testing.T, body []byte) diffMessageView {
	t.Helper()
	var v diffMessageView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode diff message: %v\nbody=%s", err, body)
	}
	return v
}

type diffMessageView struct {
	Command string `json:"command"`
	Input   struct {
		SimulationID *string `json:"simulation_id,omitempty"`
		Message      struct {
			ForwardDifferences []diff.Difference `json:"forward_differences"`
			ReverseDifferences []diff.Difference `json:"reverse_differences"`
		} `json:"message"`
	} `json:"input"`
}

func TestPublishDERStatusSendsMappedDifferences(t *testing.T) {
	t.Parallel()

	pub := &fakeBusPublisher{}
	conn := sep2.ConnectStatusType{Value: 1}
	mode := sep2.OperationalModeStatusType{Value: 2}
	alarm := sep2.HexBinary32(9)
	status := sep2.DERStatus{GenConnectStatus: &conn, OperationalModeStatus: &mode, AlarmStatus: &alarm}

	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	if err := PublishDERStatus(context.Background(), pub, "/topic/dest", "sim-1", "mrid-a", status, now); err != nil {
		t.Fatalf("PublishDERStatus: %v", err)
	}

	sends := pub.snapshot()
	if len(sends) != 1 {
		t.Fatalf("len(sends) = %d, want 1", len(sends))
	}
	if sends[0].dest != "/topic/dest" {
		t.Errorf("dest = %q, want %q", sends[0].dest, "/topic/dest")
	}
	if sends[0].contentType != "application/json" {
		t.Errorf("contentType = %q, want application/json", sends[0].contentType)
	}

	msg := decodeDiffMessage(t, sends[0].body)
	if msg.Input.SimulationID == nil || *msg.Input.SimulationID != "sim-1" {
		t.Errorf("simulation_id = %v, want \"sim-1\"", msg.Input.SimulationID)
	}
	if len(msg.Input.Message.ForwardDifferences) != 3 {
		t.Fatalf("forward_differences len = %d, want 3", len(msg.Input.Message.ForwardDifferences))
	}

	// Value fidelity AND exact attribute names, asserted after a real
	// JSON encode/decode round trip (not just against the intermediate
	// Go struct): a SEP2 operationalModeStatus of 2 must arrive on the
	// bus as exactly 2, under exactly "DERStatus.operationalModeStatus",
	// so any later remap of these names or a value-mangling bug shows
	// up as a failing assertion here, per Vance/GAGO-034 PR #9 review.
	byAttr := make(map[string]float64, len(msg.Input.Message.ForwardDifferences))
	for _, fd := range msg.Input.Message.ForwardDifferences {
		if fd.Object != "mrid-a" {
			t.Errorf("forward difference %q: Object = %v, want %q", fd.Attribute, fd.Object, "mrid-a")
		}
		v, ok := fd.Value.(float64)
		if !ok {
			t.Fatalf("forward difference %q: Value = %v (%T), want a JSON number", fd.Attribute, fd.Value, fd.Value)
		}
		byAttr[fd.Attribute] = v
	}

	wantByAttr := map[string]float64{
		"DERStatus.genConnectStatus":      1,
		"DERStatus.operationalModeStatus": 2,
		"DERStatus.alarmStatus":           9,
	}
	for attr, want := range wantByAttr {
		got, ok := byAttr[attr]
		if !ok {
			t.Errorf("missing forward difference for attribute %q", attr)
			continue
		}
		if got != want {
			t.Errorf("forward difference %q: Value = %v, want %v", attr, got, want)
		}
	}
}

func TestPublishDERStatusNoMappedFieldsIsNoop(t *testing.T) {
	t.Parallel()

	pub := &fakeBusPublisher{}
	if err := PublishDERStatus(context.Background(), pub, "/topic/dest", "sim-1", "mrid-a", sep2.DERStatus{}, time.Now()); err != nil {
		t.Fatalf("PublishDERStatus: %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Fatalf("PublishDERStatus with an empty DERStatus sent %d messages, want 0", len(pub.snapshot()))
	}
}

func TestPublishDERStatusPropagatesSendError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("bus unreachable")
	pub := &fakeBusPublisher{err: wantErr}
	conn := sep2.ConnectStatusType{Value: 1}
	status := sep2.DERStatus{GenConnectStatus: &conn}

	err := PublishDERStatus(context.Background(), pub, "/topic/dest", "sim-1", "mrid-a", status, time.Now())
	if !errors.Is(err, wantErr) {
		t.Fatalf("PublishDERStatus error = %v, want wrapping %v", err, wantErr)
	}
}

func TestDerStatusPathEndDeviceID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path   string
		wantID string
		wantOK bool
	}{
		{"/edev/LFDI123/der/1/ders", "LFDI123", true},
		{"edev/LFDI123/der/1/ders", "LFDI123", true},
		{"/edev/LFDI123/der/1/ders/", "LFDI123", true},
		{"/edev/LFDI123/der/1/dercap", "", false},
		{"/edev/LFDI123/ders", "", false},
		{"/edev", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		id, ok := derStatusPathEndDeviceID(tt.path)
		if ok != tt.wantOK || id != tt.wantID {
			t.Errorf("derStatusPathEndDeviceID(%q) = (%q, %v), want (%q, %v)", tt.path, id, ok, tt.wantID, tt.wantOK)
		}
	}
}

// fakeEndDeviceIndex is a minimal test double for endDeviceKeyResolver: the
// same index-to-device-key shape memory.EndDeviceIndex.DeviceKey exposes,
// without pulling in the real allocator.
type fakeEndDeviceIndex struct {
	byIndex map[string]string
}

func (f *fakeEndDeviceIndex) DeviceKey(index string) (string, bool) {
	key, ok := f.byIndex[index]
	return key, ok
}

// TestTelemetryMiddlewareRelaysSuccessfulPUT is the UP-path wiring
// centerpiece: a PUT of a device's own DERStatus, through the full
// telemetry middleware, results in exactly one bus Send carrying the
// mapped field values, and the original PUT response is unaffected.
//
// The path segment is the opaque, server-chosen URL index seed.go's
// EndDeviceIndexes.Allocate hands out (GAGO-109), not the device's LFDI:
// two devices are registered here (8 and 9) so a lookup that resolved
// through the wrong table, or the wrong entry, would surface as a wrong
// Object on the published message rather than merely an empty one.
func TestTelemetryMiddlewareRelaysSuccessfulPUT(t *testing.T) {
	t.Parallel()

	edevIndex := &fakeEndDeviceIndex{byIndex: map[string]string{
		"8": "mrid-a",
		"9": "mrid-b",
	}}
	pub := &fakeBusPublisher{}

	cfg := telemetryConfig{bus: pub, edevIndex: edevIndex, dest: "/topic/dest", simID: "sim-1"}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := telemetryMiddleware(cfg)(inner)

	mode := sep2.OperationalModeStatusType{Value: 2}
	status := sep2.DERStatus{OperationalModeStatus: &mode}
	body, err := xml.Marshal(&status)
	if err != nil {
		t.Fatalf("marshal DERStatus: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/edev/8/der/1/ders", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)

	if rw.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want 204 (relay must not alter the device's own response)", rw.Code)
	}

	sends := pub.snapshot()
	if len(sends) != 1 {
		t.Fatalf("len(sends) = %d, want 1", len(sends))
	}
	msg := decodeDiffMessage(t, sends[0].body)
	if len(msg.Input.Message.ForwardDifferences) != 1 {
		t.Fatalf("forward_differences len = %d, want 1", len(msg.Input.Message.ForwardDifferences))
	}
	fd := msg.Input.Message.ForwardDifferences[0]
	// Value fidelity AND exact attribute name, over the real JSON wire
	// bytes actually sent to the bus: a SEP2 operationalModeStatus of 2
	// must arrive as exactly 2 under exactly
	// "DERStatus.operationalModeStatus".
	//
	// Object must be device 8's own mRID ("mrid-a"), not device 9's
	// ("mrid-b"): a lookup that silently resolved to the wrong index or
	// the wrong table would still pass a bare "did it publish something"
	// check, so this asserts the specific resolved value per
	// data-invariants Rule 1.
	if fd.Object != "mrid-a" {
		t.Errorf("forward difference Object = %q, want %q (reverse-resolved via EndDeviceIndexes.DeviceKey, GAGO-109)", fd.Object, "mrid-a")
	}
	if fd.Attribute != "DERStatus.operationalModeStatus" {
		t.Errorf("forward difference Attribute = %q, want %q", fd.Attribute, "DERStatus.operationalModeStatus")
	}
	if v, ok := fd.Value.(float64); !ok || v != 2 {
		t.Errorf("forward difference Value = %v (%T), want 2", fd.Value, fd.Value)
	}
}

// TestTelemetryMiddlewareDropsUnknownEndDeviceID proves an EndDevice id
// with no entry in the resolver is dropped and logged, never published
// under an empty or fabricated mRID (GAGO-109 invariant: a miss must stay
// a miss, not become a wrong or empty publish).
func TestTelemetryMiddlewareDropsUnknownEndDeviceID(t *testing.T) {
	t.Parallel()

	edevIndex := &fakeEndDeviceIndex{byIndex: map[string]string{"8": "mrid-a"}}
	pub := &fakeBusPublisher{}
	cfg := telemetryConfig{bus: pub, edevIndex: edevIndex, dest: "/topic/dest", simID: "sim-1"}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := telemetryMiddleware(cfg)(inner)

	mode := sep2.OperationalModeStatusType{Value: 2}
	status := sep2.DERStatus{OperationalModeStatus: &mode}
	body, err := xml.Marshal(&status)
	if err != nil {
		t.Fatalf("marshal DERStatus: %v", err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	req := httptest.NewRequest(http.MethodPut, "/edev/999/der/1/ders", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)

	if rw.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want 204 (device's own PUT must still succeed)", rw.Code)
	}
	if sends := pub.snapshot(); len(sends) != 0 {
		t.Fatalf("bus Send called %d times, want 0 for an unregistered EndDevice id", len(sends))
	}
	if !strings.Contains(logBuf.String(), "edev=999") {
		t.Errorf("log output = %q, want it to mention the dropped edev id 999", logBuf.String())
	}
}

// TestTelemetryMiddlewareSkipsNonMatchingRequests proves the middleware
// is a pure pass-through for anything that isn't a PUT on the DERStatus
// path shape: no bus Send fires, and the wrapped handler's own response
// is untouched.
func TestTelemetryMiddlewareSkipsNonMatchingRequests(t *testing.T) {
	t.Parallel()

	edevIndex := &fakeEndDeviceIndex{}
	pub := &fakeBusPublisher{}
	cfg := telemetryConfig{bus: pub, edevIndex: edevIndex, dest: "/topic/dest", simID: "sim-1"}

	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	handler := telemetryMiddleware(cfg)(inner)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/edev/8/der/1/ders", nil),
		httptest.NewRequest(http.MethodPut, "/edev/8/der/1/dercap", nil),
	} {
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
	}

	if calls != 2 {
		t.Fatalf("inner handler called %d times, want 2 (both requests must still reach it)", calls)
	}
	if len(pub.snapshot()) != 0 {
		t.Fatalf("bus Send called %d times, want 0 for non-matching requests", len(pub.snapshot()))
	}
}

// TestTelemetryMiddlewareSkipsRejectedPUT proves a PUT the downstream
// handler rejects (non-2xx) is not relayed: the relay only echoes
// changes that actually landed.
func TestTelemetryMiddlewareSkipsRejectedPUT(t *testing.T) {
	t.Parallel()

	edevIndex := &fakeEndDeviceIndex{byIndex: map[string]string{"8": "mrid-a"}}
	pub := &fakeBusPublisher{}
	cfg := telemetryConfig{bus: pub, edevIndex: edevIndex, dest: "/topic/dest", simID: "sim-1"}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	handler := telemetryMiddleware(cfg)(inner)

	conn := sep2.ConnectStatusType{Value: 1}
	status := sep2.DERStatus{GenConnectStatus: &conn}
	body, _ := xml.Marshal(&status)

	req := httptest.NewRequest(http.MethodPut, "/edev/8/der/1/ders", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want 400", rw.Code)
	}
	if len(pub.snapshot()) != 0 {
		t.Fatalf("bus Send called %d times, want 0 for a rejected PUT", len(pub.snapshot()))
	}
}

// TestTelemetryMiddlewareDisabledIsPassthrough proves the zero-value
// telemetryConfig (bus/registry/dest/simID all unset) returns next
// completely unwrapped: no interception, no behavior change.
func TestTelemetryMiddlewareDisabledIsPassthrough(t *testing.T) {
	t.Parallel()

	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	handler := telemetryMiddleware(telemetryConfig{})(inner)

	conn := sep2.ConnectStatusType{Value: 1}
	status := sep2.DERStatus{GenConnectStatus: &conn}
	body, _ := xml.Marshal(&status)

	req := httptest.NewRequest(http.MethodPut, "/edev/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/der/1/ders", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)

	if calls != 1 {
		t.Fatalf("inner handler called %d times, want 1", calls)
	}
	if rw.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want 204", rw.Code)
	}
}

// --- GAGO full DERStatus field coverage (GAGO-110) --------------------
//
// Before this card MapDERStatusToDifferences mapped three of the seven
// fields core's sep2.DERStatus models, so a DERStatus populating none of
// those three produced an empty slice and PublishDERStatus returned nil
// without publishing or logging anything: a total silent no-op. The
// tests below pin the full mapping, the unit semantics of the scaled
// stateOfChargeStatus field, the deliberate dropping of the per-field
// dateTime, and the fact that the remaining empty case is no longer
// silent.

// wantExistingThreeJSON is the exact JSON encoding of
// MapDERStatusToDifferences' output for a DERStatus populating only the
// three fields the pre-GAGO-110 implementation mapped. It is a golden
// literal on purpose: attribute names, values, AND slice order are all
// part of the contract existing bus consumers already depend on, so any
// of the three drifting fails here rather than silently on the wire.
const wantExistingThreeJSON = `[` +
	`{"object":"mrid-a","attribute":"DERStatus.genConnectStatus","value":1},` +
	`{"object":"mrid-a","attribute":"DERStatus.operationalModeStatus","value":2},` +
	`{"object":"mrid-a","attribute":"DERStatus.alarmStatus","value":7}` +
	`]`

func TestMapDERStatusToDifferencesExistingThreeAreByteIdentical(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{Value: 1, DateTime: 100}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 200}
	alarm := sep2.HexBinary32(7)
	status := sep2.DERStatus{
		GenConnectStatus:      &conn,
		OperationalModeStatus: &mode,
		AlarmStatus:           &alarm,
	}

	diffs, err := MapDERStatusToDifferences("mrid-a", status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	got, err := json.Marshal(diffs)
	if err != nil {
		t.Fatalf("marshal diffs: %v", err)
	}
	if string(got) != wantExistingThreeJSON {
		t.Errorf("existing three-field output changed.\n got = %s\nwant = %s", got, wantExistingThreeJSON)
	}
}

// socEqualEpsilon bounds the tolerance socEqual allows. A sep2.PerCent
// value divided by 100 is not always exactly representable in binary
// floating point (6501/100 = 65.01, whose nearest float64 carries a
// residual on the order of 1e-14). 6500/100 = 65.0 happens to be exact
// (the division of two exactly representable integers whose true
// quotient is itself an integer is exactly representable), so the tests
// below that use 6500 could use `==` and still pass; socEqual is used
// anyway so the assertions do not become spuriously fragile if a test
// value ever changes to a non-round hundredths-of-a-percent input.
const socEqualEpsilon = 1e-9

// socEqual reports whether got and want are equal to within
// socEqualEpsilon. See socEqualEpsilon's doc comment for why an exact
// `==` is not the right tool for a value derived from float64 division.
func socEqual(got, want float64) bool {
	return math.Abs(got-want) < socEqualEpsilon
}

// fullDERStatus returns a DERStatus with every field core models set to a
// distinct, non-zero value, so a mapping that reads the wrong source
// field cannot pass by coincidence. The dateTime sentinels are large and
// mutually non-overlapping so
// TestMapDERStatusToDifferencesDropsPerFieldDateTime can assert on them
// as substrings without colliding with any published value.
func fullDERStatus() sep2.DERStatus {
	conn := sep2.ConnectStatusType{Value: 1, DateTime: 987654001}
	inv := sep2.InverterStatusType{Value: 3, DateTime: 987654003}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 987654002}
	soc := sep2.StateOfChargeStatusType{Value: 6500, DateTime: -5838048000}
	storage := sep2.StorageModeStatusType{Value: 1, DateTime: 987654005}
	alarm := sep2.HexBinary32(7)
	return sep2.DERStatus{
		AlarmStatus:           &alarm,
		GenConnectStatus:      &conn,
		InverterStatus:        &inv,
		OperationalModeStatus: &mode,
		ReadingTime:           1785714218,
		StateOfChargeStatus:   &soc,
		StorageModeStatus:     &storage,
	}
}

func TestMapDERStatusToDifferencesMapsEveryModelledField(t *testing.T) {
	t.Parallel()

	diffs, err := MapDERStatusToDifferences("mrid-a", fullDERStatus())
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 7 {
		t.Fatalf("len(diffs) = %d, want 7 (one per field core's sep2.DERStatus models)", len(diffs))
	}

	byAttr := make(map[string]diff.Difference, len(diffs))
	for _, d := range diffs {
		if d.Object != "mrid-a" {
			t.Errorf("difference %q: Object = %q, want %q", d.Attribute, d.Object, "mrid-a")
		}
		if _, dup := byAttr[d.Attribute]; dup {
			t.Errorf("duplicate difference for attribute %q", d.Attribute)
		}
		byAttr[d.Attribute] = d
	}

	// Values are asserted at their exact wire type as well as their
	// magnitude: a HexBinary8 silently widened to uint16, or a
	// stateOfChargeStatus published raw instead of scaled to percent, is
	// exactly the invisible data change [[data-invariants]] exists to
	// catch. stateOfChargeStatus is compared with tolerance, not `!=`:
	// see soatEqual's doc comment for why.
	want := map[string]any{
		"DERStatus.genConnectStatus":      sep2.HexBinary8(1),
		"DERStatus.operationalModeStatus": uint8(2),
		"DERStatus.alarmStatus":           sep2.HexBinary32(7),
		"DERStatus.readingTime":           int64(1785714218),
		"DERStatus.inverterStatus":        uint8(3),
		"DERStatus.stateOfChargeStatus":   float64(65),
		"DERStatus.storageModeStatus":     uint8(1),
	}
	for attr, wantVal := range want {
		got, ok := byAttr[attr]
		if !ok {
			t.Errorf("missing difference for attribute %q", attr)
			continue
		}
		if attr == "DERStatus.stateOfChargeStatus" {
			gotFloat, ok := got.Value.(float64)
			if !ok || !socEqual(gotFloat, wantVal.(float64)) {
				t.Errorf("difference %q: Value = %v (%T), want %v (float64)", attr, got.Value, got.Value, wantVal)
			}
			continue
		}
		if got.Value != wantVal {
			t.Errorf("difference %q: Value = %v (%T), want %v (%T)", attr, got.Value, got.Value, wantVal, wantVal)
		}
	}
	for attr := range byAttr {
		if _, ok := want[attr]; !ok {
			t.Errorf("unexpected difference for attribute %q", attr)
		}
	}
}

// TestMapDERStatusToDifferencesPerField sets exactly one field at a time.
// It proves both halves of the nil-guard contract: a present field always
// publishes, and every ABSENT field publishes nothing (a synthesized zero
// would be indistinguishable from a device genuinely reporting zero).
func TestMapDERStatusToDifferencesPerField(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{Value: 1, DateTime: 100}
	inv := sep2.InverterStatusType{Value: 3, DateTime: 300}
	mode := sep2.OperationalModeStatusType{Value: 2, DateTime: 200}
	soc := sep2.StateOfChargeStatusType{Value: 6500, DateTime: -5838048000}
	// socFractional is deliberately NOT a round percent: 6501/100 = 65.01
	// is not exactly representable in binary floating point, so this case
	// exercises the residual-precision path 6500 (an exact case) cannot.
	socFractional := sep2.StateOfChargeStatusType{Value: 6501, DateTime: -5838048000}
	storage := sep2.StorageModeStatusType{Value: 1, DateTime: 500}
	alarm := sep2.HexBinary32(7)

	tests := []struct {
		name     string
		status   sep2.DERStatus
		wantAttr string
		wantVal  any
	}{
		{"genConnectStatus", sep2.DERStatus{GenConnectStatus: &conn}, "DERStatus.genConnectStatus", sep2.HexBinary8(1)},
		{"operationalModeStatus", sep2.DERStatus{OperationalModeStatus: &mode}, "DERStatus.operationalModeStatus", uint8(2)},
		{"alarmStatus", sep2.DERStatus{AlarmStatus: &alarm}, "DERStatus.alarmStatus", sep2.HexBinary32(7)},
		{"readingTime", sep2.DERStatus{ReadingTime: 1785714218}, "DERStatus.readingTime", int64(1785714218)},
		{"inverterStatus", sep2.DERStatus{InverterStatus: &inv}, "DERStatus.inverterStatus", uint8(3)},
		{"stateOfChargeStatus", sep2.DERStatus{StateOfChargeStatus: &soc}, "DERStatus.stateOfChargeStatus", float64(65)},
		{"stateOfChargeStatus fractional", sep2.DERStatus{StateOfChargeStatus: &socFractional}, "DERStatus.stateOfChargeStatus", float64(65.01)},
		{"storageModeStatus", sep2.DERStatus{StorageModeStatus: &storage}, "DERStatus.storageModeStatus", uint8(1)},
	}

	for _, tc := range tests {
		t.Run(tc.name+" alone publishes exactly one difference", func(t *testing.T) {
			t.Parallel()

			diffs, err := MapDERStatusToDifferences("mrid-a", tc.status)
			if err != nil {
				t.Fatalf("MapDERStatusToDifferences: %v", err)
			}
			if len(diffs) != 1 {
				t.Fatalf("len(diffs) = %d, want exactly 1 (only %s is set)", len(diffs), tc.name)
			}
			if diffs[0].Attribute != tc.wantAttr {
				t.Errorf("Attribute = %q, want %q", diffs[0].Attribute, tc.wantAttr)
			}
			// stateOfChargeStatus is a float64 division result and is
			// compared with tolerance; see socEqual's doc comment.
			if wantFloat, isFloat := tc.wantVal.(float64); isFloat {
				gotFloat, ok := diffs[0].Value.(float64)
				if !ok || !socEqual(gotFloat, wantFloat) {
					t.Errorf("Value = %v (%T), want %v (float64)", diffs[0].Value, diffs[0].Value, wantFloat)
				}
			} else if diffs[0].Value != tc.wantVal {
				t.Errorf("Value = %v (%T), want %v (%T)", diffs[0].Value, diffs[0].Value, tc.wantVal, tc.wantVal)
			}
			if diffs[0].Object != "mrid-a" {
				t.Errorf("Object = %q, want %q", diffs[0].Object, "mrid-a")
			}
		})
	}
}

// TestMapDERStatusToDifferencesPresentZeroValuesPublish is the other side
// of the nil guard: a device that genuinely reports zero must reach the
// bus as zero. Only ABSENCE suppresses a difference.
func TestMapDERStatusToDifferencesPresentZeroValuesPublish(t *testing.T) {
	t.Parallel()

	conn := sep2.ConnectStatusType{}
	soc := sep2.StateOfChargeStatusType{}
	alarm := sep2.HexBinary32(0)
	status := sep2.DERStatus{GenConnectStatus: &conn, StateOfChargeStatus: &soc, AlarmStatus: &alarm}

	diffs, err := MapDERStatusToDifferences("mrid-a", status)
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	if len(diffs) != 3 {
		t.Fatalf("len(diffs) = %d, want 3: a present zero is a real reading, not an absent field", len(diffs))
	}
	for _, d := range diffs {
		switch d.Attribute {
		case "DERStatus.genConnectStatus":
			if d.Value != sep2.HexBinary8(0) {
				t.Errorf("%s: Value = %v, want HexBinary8(0)", d.Attribute, d.Value)
			}
		case "DERStatus.stateOfChargeStatus":
			// 0/100 = 0.0 exactly, so a direct comparison is safe here;
			// see socEqual's doc comment for the general case.
			if d.Value != float64(0) {
				t.Errorf("%s: Value = %v, want float64(0)", d.Attribute, d.Value)
			}
		case "DERStatus.alarmStatus":
			if d.Value != sep2.HexBinary32(0) {
				t.Errorf("%s: Value = %v, want HexBinary32(0)", d.Attribute, d.Value)
			}
		default:
			t.Errorf("unexpected attribute %q", d.Attribute)
		}
	}
}

// TestMapDERStatusToDifferencesDropsPerFieldDateTime pins the deliberate
// decision to publish only the value half of each complex-typed field.
// The EPRI client observed in e2e run 10 reports
// stateOfChargeStatus/dateTime = -5838048000, a negative epoch landing
// around the year 1785; republishing it would put a bogus timestamp on
// the GridAPPS-D bus where a consumer could reasonably read it as real.
func TestMapDERStatusToDifferencesDropsPerFieldDateTime(t *testing.T) {
	t.Parallel()

	diffs, err := MapDERStatusToDifferences("mrid-a", fullDERStatus())
	if err != nil {
		t.Fatalf("MapDERStatusToDifferences: %v", err)
	}
	encoded, err := json.Marshal(diffs)
	if err != nil {
		t.Fatalf("marshal diffs: %v", err)
	}
	bannedDateTimeMaterial := []string{
		"dateTime",
		"-5838048000",
		"987654001",
		"987654002",
		"987654003",
		"987654005",
	}
	for _, banned := range bannedDateTimeMaterial {
		if strings.Contains(string(encoded), banned) {
			t.Errorf("mapped output contains per-field dateTime material %q; dateTime must be dropped.\ngot = %s", banned, encoded)
		}
	}
}

// epriClientDERStatusBody is the exact DERStatus body the EPRI 2030.5 C
// client PUT in all nine sessions of e2e run 10. Before GAGO-110 it
// mapped to zero differences, so the PUT returned 204 and nothing ever
// reached the bus.
const epriClientDERStatusBody = `<DERStatus xmlns="urn:ieee:std:2030.5:ns">` +
	`<readingTime>1785714218</readingTime>` +
	`<stateOfChargeStatus><dateTime>-5838048000</dateTime><value>6500</value></stateOfChargeStatus>` +
	`</DERStatus>`

func TestPublishDERStatusPublishesEPRIClientBody(t *testing.T) {
	t.Parallel()

	var status sep2.DERStatus
	if err := xml.Unmarshal([]byte(epriClientDERStatusBody), &status); err != nil {
		t.Fatalf("unmarshal EPRI client DERStatus: %v", err)
	}

	pub := &fakeBusPublisher{}
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	if err := PublishDERStatus(context.Background(), pub, "/topic/dest", "sim-1", "mrid-a", status, now); err != nil {
		t.Fatalf("PublishDERStatus: %v", err)
	}

	sends := pub.snapshot()
	if len(sends) != 1 {
		t.Fatalf("len(sends) = %d, want 1: the EPRI client's own body must reach the bus", len(sends))
	}
	if strings.Contains(string(sends[0].body), "-5838048000") {
		t.Errorf("published payload carries the client's bogus dateTime.\nbody = %s", sends[0].body)
	}

	msg := decodeDiffMessage(t, sends[0].body)
	got := make(map[string]float64, 2)
	for _, fd := range msg.Input.Message.ForwardDifferences {
		v, ok := fd.Value.(float64)
		if !ok {
			t.Fatalf("forward difference %q: Value = %v (%T), want a JSON number", fd.Attribute, fd.Value, fd.Value)
		}
		got[fd.Attribute] = v
	}
	// 6500 hundredths of a percent is 65 percent: the wire value is
	// scaled and published under the plain sep.xsd attribute name.
	want := map[string]float64{
		"DERStatus.readingTime":         1785714218,
		"DERStatus.stateOfChargeStatus": 65,
	}
	if len(got) != len(want) {
		t.Fatalf("forward differences = %v, want exactly %v", got, want)
	}
	for attr, wantVal := range want {
		// This test round-trips through json.Marshal/Unmarshal (via
		// decodeDiffMessage), and 6500/100 = 65.0 is exactly
		// representable, so `!=` is safe here; see socEqual's doc
		// comment in the non-round-trip tests for the general case.
		if got[attr] != wantVal {
			t.Errorf("forward difference %q: Value = %v, want %v", attr, got[attr], wantVal)
		}
	}

	// Confirm what the marshalled JSON actually looks like for the
	// scaled value: encoding/json emits float64(65) as the bare number
	// "65", not "65.0". A JSON number carries no int-vs-float marker, so
	// a consumer must not infer the type from the presence or absence of
	// a decimal point; only the schema (PerCent, now in percent) says
	// so.
	if !strings.Contains(string(sends[0].body), `"attribute":"DERStatus.stateOfChargeStatus","value":65}`) {
		t.Errorf("published payload does not carry the expected bare-integer JSON encoding of the scaled value.\nbody = %s", sends[0].body)
	}
}

// TestPublishDERStatusNoMappedFieldsLogs pins the "also do this" half of
// GAGO-110: the empty-slice short-circuit is preserved, but it is no
// longer silent. Not parallel: it swaps the global log output.
func TestPublishDERStatusNoMappedFieldsLogs(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	pub := &fakeBusPublisher{}
	if err := PublishDERStatus(context.Background(), pub, "/topic/dest", "sim-1", "mrid-a", sep2.DERStatus{}, time.Now()); err != nil {
		t.Fatalf("PublishDERStatus: %v", err)
	}
	if sends := pub.snapshot(); len(sends) != 0 {
		t.Fatalf("bus Send called %d times, want 0 for a DERStatus with no mapped field", len(sends))
	}
	if !strings.Contains(logBuf.String(), "mrid=mrid-a") {
		t.Errorf("log output = %q, want it to name the mrid whose DERStatus mapped to nothing", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "no mapped field") {
		t.Errorf("log output = %q, want it to state that nothing was published", logBuf.String())
	}
}
