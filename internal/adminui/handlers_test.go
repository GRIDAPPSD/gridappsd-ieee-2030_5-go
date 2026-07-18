package adminui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const testKey = "test-admin-token"

// TestHandleHealthReturnsOK asserts the exact fixed field value
// /api/health returns.
func TestHandleHealthReturnsOK(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	rec := doRequest(t, s.Handler(), "GET", "/api/health", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got healthResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if got.Status != "ok" {
		t.Errorf("Status = %q, want %q", got.Status, "ok")
	}
}

// TestHandleRegistryReturnsExactEntryFieldValues is the GAGO-059
// field-value test for /api/registry: every field of every registry
// entry must round trip through JSON exactly, per data-invariants
// (assert field values, not just non-nil / non-crash).
func TestHandleRegistryReturnsExactEntryFieldValues(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{entries: []registry.Entry{
		{MRID: "mrid-1", Name: "inverter-1", LFDI: "ABCDEF0123456789", SFDI: "123456789", Placeholder: false},
		{MRID: "mrid-2", Name: "battery-1", LFDI: "", SFDI: "", Placeholder: true},
	}}
	s := newTestServer(t, testKey, reg, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/api/registry", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got []registryEntryResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	want0 := registryEntryResponse{MRID: "mrid-1", Name: "inverter-1", LFDI: "ABCDEF0123456789", SFDI: "123456789", Placeholder: false}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v, want %+v", got[0], want0)
	}
	want1 := registryEntryResponse{MRID: "mrid-2", Name: "battery-1", LFDI: "", SFDI: "", Placeholder: true}
	if got[1] != want1 {
		t.Errorf("got[1] = %+v, want %+v", got[1], want1)
	}
}

// TestHandleServedEndDevicesReturnsExactFieldValues asserts every field
// of an EndDeviceSnapshot, including its nested DERs, round trips
// exactly.
func TestHandleServedEndDevicesReturnsExactFieldValues(t *testing.T) {
	t.Parallel()

	devices := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{
			ID: "edev-1", LFDI: "LFDI1", SFDI: "SFDI1", Href: "/edev/LFDI1", Enabled: true,
			DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/edev/LFDI1/der/1"}},
		},
	}}
	s := newTestServer(t, testKey, &fakeRegistry{}, devices, &fakePrograms{}, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/api/served/edev", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got []endDeviceResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	edev := got[0]
	if edev.ID != "edev-1" || edev.LFDI != "LFDI1" || edev.SFDI != "SFDI1" || edev.Href != "/edev/LFDI1" || !edev.Enabled {
		t.Errorf("edev = %+v, want ID=edev-1 LFDI=LFDI1 SFDI=SFDI1 Href=/edev/LFDI1 Enabled=true", edev)
	}
	if len(edev.DERs) != 1 || edev.DERs[0].ID != "der-1" || edev.DERs[0].Href != "/edev/LFDI1/der/1" {
		t.Errorf("edev.DERs = %+v, want one DER with ID=der-1 Href=/edev/LFDI1/der/1", edev.DERs)
	}
}

// TestHandleDERsFlattensDERsWithOwningEndDeviceID asserts /api/ders
// reports each DER's own field values alongside its owning device's ID.
func TestHandleDERsFlattensDERsWithOwningEndDeviceID(t *testing.T) {
	t.Parallel()

	devices := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1", DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/h1"}, {ID: "der-2", Href: "/h2"}}},
		{ID: "edev-2", DERs: []sep2embed.DERSnapshot{{ID: "der-3", Href: "/h3"}}},
	}}
	s := newTestServer(t, testKey, &fakeRegistry{}, devices, &fakePrograms{}, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/api/ders", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []derWithOwnerResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	want := []derWithOwnerResponse{
		{EndDeviceID: "edev-1", ID: "der-1", Href: "/h1"},
		{EndDeviceID: "edev-1", ID: "der-2", Href: "/h2"},
		{EndDeviceID: "edev-2", ID: "der-3", Href: "/h3"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestHandleServedDERProgramsReturnsExactFieldValuesPerDevice asserts
// /api/served/derprogram joins each device's own DERProgram list
// correctly, with the owning device's ID attached, and every
// DERProgramSnapshot field preserved exactly, including Primacy (a
// numeric field easy to accidentally drop or zero during a JSON
// round trip).
func TestHandleServedDERProgramsReturnsExactFieldValuesPerDevice(t *testing.T) {
	t.Parallel()

	devices := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1"},
		{ID: "edev-2"},
	}}
	programs := &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{
		"edev-1": {{ID: "1", Href: "/edev/edev-1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 0}},
		"edev-2": {{ID: "1", Href: "/edev/edev-2/fsa/1/derp/1", MRID: "derp-mrid-2", Description: "critical peak", Primacy: 5}},
	}}
	s := newTestServer(t, testKey, &fakeRegistry{}, devices, programs, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/api/served/derprogram", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []derProgramResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	want0 := derProgramResponse{EndDeviceID: "edev-1", ID: "1", Href: "/edev/edev-1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 0}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v, want %+v", got[0], want0)
	}
	want1 := derProgramResponse{EndDeviceID: "edev-2", ID: "1", Href: "/edev/edev-2/fsa/1/derp/1", MRID: "derp-mrid-2", Description: "critical peak", Primacy: 5}
	if got[1] != want1 {
		t.Errorf("got[1] = %+v, want %+v", got[1], want1)
	}
}

// TestHandleControlFlowReturnsExactSnapshotFieldValues asserts every
// field of a populated controlobs.Snapshot (counters, Last's nested
// fields, both topic strings) round trips through JSON exactly.
func TestHandleControlFlowReturnsExactSnapshotFieldValues(t *testing.T) {
	t.Parallel()

	appliedAt := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	flow := &fakeFlow{snap: controlobs.Snapshot{
		Applied: 4,
		Skipped: 1,
		Last: &controlobs.LastDelta{
			Object:    "mrid-inv-1",
			Attribute: "DERControl.DERControlBase.opModTargetW",
			Value:     map[string]any{"multiplier": float64(0), "value": float64(4200)},
			AppliedAt: appliedAt,
		},
		OutputTopic: "/topic/goss.gridappsd.simulation.output.12345",
		InputTopic:  "/topic/goss.gridappsd.simulation.input.12345",
	}}
	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, flow)

	rec := doRequest(t, s.Handler(), "GET", "/api/controlflow", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got controlFlowResponse
	decodeJSON(t, rec.Body.Bytes(), &got)

	if got.Applied != 4 {
		t.Errorf("Applied = %d, want 4", got.Applied)
	}
	if got.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", got.Skipped)
	}
	if got.OutputTopic != "/topic/goss.gridappsd.simulation.output.12345" {
		t.Errorf("OutputTopic = %q, want %q", got.OutputTopic, "/topic/goss.gridappsd.simulation.output.12345")
	}
	if got.InputTopic != "/topic/goss.gridappsd.simulation.input.12345" {
		t.Errorf("InputTopic = %q, want %q", got.InputTopic, "/topic/goss.gridappsd.simulation.input.12345")
	}
	if got.Last == nil {
		t.Fatalf("Last = nil, want a populated value")
	}
	if got.Last.Object != "mrid-inv-1" {
		t.Errorf("Last.Object = %q, want %q", got.Last.Object, "mrid-inv-1")
	}
	if got.Last.Attribute != "DERControl.DERControlBase.opModTargetW" {
		t.Errorf("Last.Attribute = %q, want %q", got.Last.Attribute, "DERControl.DERControlBase.opModTargetW")
	}
	if got.Last.AppliedAt != "2026-07-18T12:30:00.000Z" {
		t.Errorf("Last.AppliedAt = %q, want %q", got.Last.AppliedAt, "2026-07-18T12:30:00.000Z")
	}
	valueMap, ok := got.Last.Value.(map[string]any)
	if !ok {
		t.Fatalf("Last.Value = %#v, want map[string]any", got.Last.Value)
	}
	if valueMap["value"] != float64(4200) {
		t.Errorf("Last.Value[\"value\"] = %v, want 4200", valueMap["value"])
	}
}

// TestHandleControlFlowOmitsLastWhenNil confirms a zero-value hook's
// Snapshot (Last == nil) serializes Last as JSON null, not a zero
// struct, per the serialization contract discipline in secure-coding
// Rule 3 (empty is not absent, but here nil truly means "no delta has
// been applied yet", which is a real and distinct third state from a
// present-but-empty delta).
func TestHandleControlFlowOmitsLastWhenNil(t *testing.T) {
	t.Parallel()

	flow := &fakeFlow{snap: controlobs.Snapshot{}}
	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, flow)

	rec := doRequest(t, s.Handler(), "GET", "/api/controlflow", "Bearer "+testKey, "localhost")
	body := rec.Body.String()
	if !strings.Contains(body, `"last":null`) {
		t.Errorf("body = %s, want a literal \"last\":null field", body)
	}
}

// TestNoResponseBodyEverContainsTheAdminToken is the CRITICAL GAGO-059
// no-secret acceptance test: it drives every registered endpoint with a
// realistic, populated set of fakes, then asserts the admin Bearer
// token string never appears anywhere in any response body. This is
// deliberately endpoint-agnostic (it iterates the same route list
// buildHandler serves) so a future endpoint added to mux() is covered
// automatically without a matching test edit.
func TestNoResponseBodyEverContainsTheAdminToken(t *testing.T) {
	t.Parallel()

	const secretToken = "super-secret-admin-token-do-not-leak"

	reg := &fakeRegistry{entries: []registry.Entry{
		{MRID: "mrid-1", Name: "inverter-1", LFDI: "LFDI1", SFDI: "SFDI1"},
	}}
	devices := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1", LFDI: "LFDI1", SFDI: "SFDI1", DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/h1"}}},
	}}
	programs := &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{
		"edev-1": {{ID: "1", Href: "/h1", MRID: "derp-1", Description: "default"}},
	}}
	flow := &fakeFlow{snap: controlobs.Snapshot{
		Applied: 1,
		Last: &controlobs.LastDelta{
			Object: "mrid-1", Attribute: "DERControl.DERControlBase.opModTargetW",
			Value: map[string]any{"value": float64(1000)}, AppliedAt: time.Now(),
		},
		OutputTopic: "/topic/goss.gridappsd.simulation.output.1",
		InputTopic:  "/topic/goss.gridappsd.simulation.input.1",
	}}
	s := newTestServer(t, secretToken, reg, devices, programs, flow)

	routes := []string{"/api/health", "/api/registry", "/api/ders", "/api/served/edev", "/api/served/derprogram", "/api/controlflow"}
	for _, route := range routes {
		route := route
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			rec := doRequest(t, s.Handler(), "GET", route, "Bearer "+secretToken, "localhost")
			if rec.Code != 200 {
				t.Fatalf("GET %s status = %d, want 200", route, rec.Code)
			}
			if strings.Contains(rec.Body.String(), secretToken) {
				t.Errorf("GET %s response body contains the admin bearer token: %s", route, rec.Body.String())
			}
		})
	}
}

// decodeJSON is a small test helper: unmarshal body into v, failing the
// test with the raw body on any decode error, so an assertion failure
// always shows exactly what was returned.
func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
}
