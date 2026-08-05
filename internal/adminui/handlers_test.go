package adminui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

const testKey = "test-admin-token"

// TestHandleHealthReturnsAllEnrichedFieldValues is the
// field-value test for /api/health: every enriched field must round
// trip through JSON exactly, sourced from the injected registry,
// identity, and STOMP fakes plus the Server's own Config, per
// data-invariants (assert field values, not just non-crash).
func TestHandleHealthReturnsAllEnrichedFieldValues(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{entries: []registry.Entry{
		{MRID: "mrid-1", Name: "inverter-1", LFDI: "LFDI1", SFDI: "SFDI1", Placeholder: false},
		{MRID: "mrid-2", Name: "battery-1", LFDI: "", SFDI: "", Placeholder: true},
		{MRID: "mrid-3", Name: "solar-1", LFDI: "LFDI3", SFDI: "SFDI3", Placeholder: false},
	}}
	identity := &fakeIdentity{
		addr:     "127.0.0.1:8443",
		identity: sep2srv.Identity{SFDI: "999888777", LFDI: "FEDCBA9876543210"},
	}
	stomp := &fakeStomp{connected: true}

	s, err := New(Config{
		Addr:         "127.0.0.1:0",
		Key:          testKey,
		FeederMRID:   "feeder-mrid-1",
		SimulationID: "sim-1",
		SORLink:      "https://sor.example/dashboard",
	}, reg, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, identity, stomp, &fakeClientObserver{}, &fakeHistory{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.ln.Close() })

	rec := doRequest(t, s.Handler(), "GET", "/api/health", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got healthResponse
	decodeJSON(t, rec.Body.Bytes(), &got)

	if got.Status != "ok" {
		t.Errorf("Status = %q, want %q", got.Status, "ok")
	}
	if !got.StompConnected {
		t.Errorf("StompConnected = %v, want true", got.StompConnected)
	}
	if got.MTLSListener != "127.0.0.1:8443" {
		t.Errorf("MTLSListener = %q, want %q", got.MTLSListener, "127.0.0.1:8443")
	}
	if got.ServerSFDI != "999888777" {
		t.Errorf("ServerSFDI = %q, want %q", got.ServerSFDI, "999888777")
	}
	if got.ServerLFDI != "FEDCBA9876543210" {
		t.Errorf("ServerLFDI = %q, want %q", got.ServerLFDI, "FEDCBA9876543210")
	}
	if got.FeederMRID != "feeder-mrid-1" {
		t.Errorf("FeederMRID = %q, want %q", got.FeederMRID, "feeder-mrid-1")
	}
	if got.SimulationID != "sim-1" {
		t.Errorf("SimulationID = %q, want %q", got.SimulationID, "sim-1")
	}
	if got.RegistryCount != 3 {
		t.Errorf("RegistryCount = %d, want 3", got.RegistryCount)
	}
	if got.PlaceholderCount != 1 {
		t.Errorf("PlaceholderCount = %d, want 1", got.PlaceholderCount)
	}
	if got.CertificateCount != 2 {
		t.Errorf("CertificateCount = %d, want 2", got.CertificateCount)
	}
	if got.PlaceholderCount+got.CertificateCount != got.RegistryCount {
		t.Errorf("PlaceholderCount(%d) + CertificateCount(%d) = %d, want RegistryCount %d",
			got.PlaceholderCount, got.CertificateCount, got.PlaceholderCount+got.CertificateCount, got.RegistryCount)
	}
	if got.SORLink != "https://sor.example/dashboard" {
		t.Errorf("SORLink = %q, want %q", got.SORLink, "https://sor.example/dashboard")
	}
	if got.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %d, want a non-negative value", got.UptimeSeconds)
	}
}

// TestHandleHealthSORLinkEmptyStringWhenUnset locks in the
// serialization contract chosen for SORLink: the field is always
// present in the JSON body, serialized as an empty string, never
// omitted, when Config.SORLink is unset. This mirrors
// TestHandleControlFlowOmitsLastWhenNil's literal string-contains
// pattern for asserting a specific serialization shape.
func TestHandleHealthSORLinkEmptyStringWhenUnset(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	rec := doRequest(t, s.Handler(), "GET", "/api/health", "Bearer "+testKey, "localhost")
	body := rec.Body.String()
	if !strings.Contains(body, `"sorLink":""`) {
		t.Errorf("body = %s, want a literal \"sorLink\":\"\" field present even when unset", body)
	}
}

// TestHandleRegistryReturnsExactEntryFieldValues is the
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
// reports each DER's own field values alongside its owning device's ID
// and the bridge's own configured FeederMRID, stamped onto
// every entry. FeederMRID is set to a real, non-empty value here
// (rather than the zero-value Config a plain newTestServer would give)
// so this test actually proves the field passes through from
// s.cfg.FeederMRID, not just that both sides default to the same empty
// string.
func TestHandleDERsFlattensDERsWithOwningEndDeviceID(t *testing.T) {
	t.Parallel()

	devices := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1", DERs: []sep2embed.DERSnapshot{{ID: "der-1", Href: "/h1"}, {ID: "der-2", Href: "/h2"}}},
		{ID: "edev-2", DERs: []sep2embed.DERSnapshot{{ID: "der-3", Href: "/h3"}}},
	}}
	s, err := New(Config{Addr: "127.0.0.1:0", Key: testKey, FeederMRID: "feeder-mrid-1"},
		&fakeRegistry{}, devices, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, &fakeClientObserver{}, &fakeHistory{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.ln.Close() })

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
		{EndDeviceID: "edev-1", ID: "der-1", Href: "/h1", FeederMRID: "feeder-mrid-1"},
		{EndDeviceID: "edev-1", ID: "der-2", Href: "/h2", FeederMRID: "feeder-mrid-1"},
		{EndDeviceID: "edev-2", ID: "der-3", Href: "/h3", FeederMRID: "feeder-mrid-1"},
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
// round trip) and DefaultDERControlLink (the CSIP-critical
// addition), seeded here to a distinct, non-empty value per device so
// this test actually proves the field round trips rather than both
// sides vacuously defaulting to empty.
func TestHandleServedDERProgramsReturnsExactFieldValuesPerDevice(t *testing.T) {
	t.Parallel()

	devices := &fakeEndDevices{edevs: []sep2embed.EndDeviceSnapshot{
		{ID: "edev-1"},
		{ID: "edev-2"},
	}}
	programs := &fakePrograms{byEdevID: map[string][]sep2embed.DERProgramSnapshot{
		"edev-1": {{ID: "1", Href: "/edev/edev-1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 0, DefaultDERControlLink: "/edev/edev-1/fsa/1/derp/1/dderc"}},
		"edev-2": {{ID: "1", Href: "/edev/edev-2/fsa/1/derp/1", MRID: "derp-mrid-2", Description: "critical peak", Primacy: 5, DefaultDERControlLink: "/edev/edev-2/fsa/1/derp/1/dderc"}},
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
	want0 := derProgramResponse{EndDeviceID: "edev-1", ID: "1", Href: "/edev/edev-1/fsa/1/derp/1", MRID: "derp-mrid-1", Description: "default", Primacy: 0, DefaultDERControlLink: "/edev/edev-1/fsa/1/derp/1/dderc"}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v, want %+v", got[0], want0)
	}
	want1 := derProgramResponse{EndDeviceID: "edev-2", ID: "1", Href: "/edev/edev-2/fsa/1/derp/1", MRID: "derp-mrid-2", Description: "critical peak", Primacy: 5, DefaultDERControlLink: "/edev/edev-2/fsa/1/derp/1/dderc"}
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

// TestHandleClientsReturnsExactSnapshotFieldValues is the
// field-value test for /api/clients: every field on both the clients
// array and the handshakes array must round trip through JSON exactly,
// sourced from the injected connobs.Snapshot, per data-invariants
// (assert field values, not just non-crash).
func TestHandleClientsReturnsExactSnapshotFieldValues(t *testing.T) {
	t.Parallel()

	lastSeen := time.Date(2026, 7, 27, 9, 15, 30, 0, time.UTC)
	handshakeAt := time.Date(2026, 7, 27, 9, 14, 0, 0, time.UTC)
	clients := &fakeClientObserver{snap: connobs.Snapshot{
		Clients: []connobs.ClientSnapshot{
			{
				LFDI:         "AAAABBBBCCCCDDDDEEEEFFFFAAAABBBBCCCCDDDD",
				LastSeen:     lastSeen,
				RequestCount: 7,
				Paths:        []string{"/dcap", "/edev"},
			},
		},
		Handshakes: []connobs.HandshakeAttempt{
			{
				LFDI:       "1111222233334444555566667777888899990000",
				RemoteAddr: "10.0.0.5:54321",
				Accepted:   false,
				Reason:     "x509: certificate signed by unknown authority",
				Known:      false,
				At:         handshakeAt,
			},
		},
	}}
	s := newTestServerWithSources(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, clients)

	rec := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got clientsResponse
	decodeJSON(t, rec.Body.Bytes(), &got)

	if len(got.Clients) != 1 {
		t.Fatalf("Clients has %d entries, want 1", len(got.Clients))
	}
	gotClient := got.Clients[0]
	if gotClient.LFDI != "AAAABBBBCCCCDDDDEEEEFFFFAAAABBBBCCCCDDDD" {
		t.Errorf("Clients[0].LFDI = %q, want %q", gotClient.LFDI, "AAAABBBBCCCCDDDDEEEEFFFFAAAABBBBCCCCDDDD")
	}
	if gotClient.LastSeen != "2026-07-27T09:15:30.000Z" {
		t.Errorf("Clients[0].LastSeen = %q, want %q", gotClient.LastSeen, "2026-07-27T09:15:30.000Z")
	}
	if gotClient.RequestCount != 7 {
		t.Errorf("Clients[0].RequestCount = %d, want 7", gotClient.RequestCount)
	}
	if len(gotClient.Paths) != 2 || gotClient.Paths[0] != "/dcap" || gotClient.Paths[1] != "/edev" {
		t.Errorf("Clients[0].Paths = %v, want [/dcap /edev]", gotClient.Paths)
	}

	if len(got.Handshakes) != 1 {
		t.Fatalf("Handshakes has %d entries, want 1", len(got.Handshakes))
	}
	gotHandshake := got.Handshakes[0]
	if gotHandshake.LFDI != "1111222233334444555566667777888899990000" {
		t.Errorf("Handshakes[0].LFDI = %q, want %q", gotHandshake.LFDI, "1111222233334444555566667777888899990000")
	}
	if gotHandshake.RemoteAddr != "10.0.0.5:54321" {
		t.Errorf("Handshakes[0].RemoteAddr = %q, want %q", gotHandshake.RemoteAddr, "10.0.0.5:54321")
	}
	if gotHandshake.Accepted {
		t.Errorf("Handshakes[0].Accepted = %v, want false", gotHandshake.Accepted)
	}
	if gotHandshake.Reason != "x509: certificate signed by unknown authority" {
		t.Errorf("Handshakes[0].Reason = %q, want %q", gotHandshake.Reason, "x509: certificate signed by unknown authority")
	}
	if gotHandshake.Known {
		t.Errorf("Handshakes[0].Known = %v, want false", gotHandshake.Known)
	}
	if gotHandshake.At != "2026-07-27T09:14:00.000Z" {
		t.Errorf("Handshakes[0].At = %q, want %q", gotHandshake.At, "2026-07-27T09:14:00.000Z")
	}
}

// TestHandleClientsReturnsEmptyArraysNotNullWhenNoState confirms a
// zero-value observer's Snapshot (no clients, no handshakes ever
// recorded) serializes as empty JSON arrays, not null, matching this
// handler's writeJSON contract for other list-shaped fields (see
// registryEntryResponse and derProgramResponse's own empty-slice
// handling) rather than letting a nil Go slice leak through as a wire
// "absent" signal where the contract is "empty".
func TestHandleClientsReturnsEmptyArraysNotNullWhenNoState(t *testing.T) {
	t.Parallel()

	clients := &fakeClientObserver{snap: connobs.Snapshot{}}
	s := newTestServerWithSources(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{}, &fakeIdentity{}, &fakeStomp{}, clients)

	rec := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	body := rec.Body.String()
	if !strings.Contains(body, `"clients":[]`) {
		t.Errorf("body = %s, want a literal \"clients\":[] field", body)
	}
	if !strings.Contains(body, `"handshakes":[]`) {
		t.Errorf("body = %s, want a literal \"handshakes\":[] field", body)
	}
}

// TestNoResponseBodyEverContainsTheAdminToken is the CRITICAL
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

	routes := []string{"/api/health", "/api/registry", "/api/ders", "/api/served/edev", "/api/served/derprogram", "/api/controlflow", "/api/clients"}
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
