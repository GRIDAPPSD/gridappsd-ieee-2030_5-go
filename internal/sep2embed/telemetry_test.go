package sep2embed

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
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

// TestTelemetryMiddlewareRelaysSuccessfulPUT is the UP-path wiring
// centerpiece: a PUT of a device's own DERStatus, through the full
// telemetry middleware, results in exactly one bus Send carrying the
// mapped field values, and the original PUT response is unaffected.
func TestTelemetryMiddlewareRelaysSuccessfulPUT(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-a", LFDI: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	pub := &fakeBusPublisher{}

	cfg := telemetryConfig{bus: pub, reg: reg, dest: "/topic/dest", simID: "sim-1"}
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

	req := httptest.NewRequest(http.MethodPut, "/edev/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/der/1/ders", bytes.NewReader(body))
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
	if fd.Object != "mrid-a" {
		t.Errorf("forward difference Object = %q, want %q (reverse-resolved via registry.MRID)", fd.Object, "mrid-a")
	}
	if fd.Attribute != "DERStatus.operationalModeStatus" {
		t.Errorf("forward difference Attribute = %q, want %q", fd.Attribute, "DERStatus.operationalModeStatus")
	}
	if v, ok := fd.Value.(float64); !ok || v != 2 {
		t.Errorf("forward difference Value = %v (%T), want 2", fd.Value, fd.Value)
	}
}

// TestTelemetryMiddlewareSkipsNonMatchingRequests proves the middleware
// is a pure pass-through for anything that isn't a PUT on the DERStatus
// path shape: no bus Send fires, and the wrapped handler's own response
// is untouched.
func TestTelemetryMiddlewareSkipsNonMatchingRequests(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	pub := &fakeBusPublisher{}
	cfg := telemetryConfig{bus: pub, reg: reg, dest: "/topic/dest", simID: "sim-1"}

	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	handler := telemetryMiddleware(cfg)(inner)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/edev/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/der/1/ders", nil),
		httptest.NewRequest(http.MethodPut, "/edev/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/der/1/dercap", nil),
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

	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-a", LFDI: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	pub := &fakeBusPublisher{}
	cfg := telemetryConfig{bus: pub, reg: reg, dest: "/topic/dest", simID: "sim-1"}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	handler := telemetryMiddleware(cfg)(inner)

	conn := sep2.ConnectStatusType{Value: 1}
	status := sep2.DERStatus{GenConnectStatus: &conn}
	body, _ := xml.Marshal(&status)

	req := httptest.NewRequest(http.MethodPut, "/edev/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/der/1/ders", bytes.NewReader(body))
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
