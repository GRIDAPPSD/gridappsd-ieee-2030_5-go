package adminui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

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

	src := testSources()
	src.Registry, src.Identity, src.Stomp = reg, identity, stomp
	s := newServer(t, Config{
		Key:          testKey,
		FeederMRID:   "feeder-mrid-1",
		SimulationID: "sim-1",
		SORLink:      "https://sor.example/dashboard",
	}, src)

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
// omitted, when Config.SORLink is unset. It inspects the
// serialized bytes, since a decoded struct cannot tell absent from empty.
func TestHandleHealthSORLinkEmptyStringWhenUnset(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	rec := doRequest(t, s.Handler(), "GET", "/api/health", "Bearer "+testKey, "localhost")
	body := rec.Body.String()
	if !strings.Contains(body, `"sorLink":""`) {
		t.Errorf("body = %s, want a literal \"sorLink\":\"\" field present even when unset", body)
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
	src := testSources()
	src.Clients = clients
	s := newServer(t, Config{Key: testKey}, src)

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
// recorded) serializes as empty JSON arrays, not null: the contract is
// "empty", and a nil slice would put "absent" on the wire.
func TestHandleClientsReturnsEmptyArraysNotNullWhenNoState(t *testing.T) {
	t.Parallel()

	clients := &fakeClientObserver{snap: connobs.Snapshot{}}
	src := testSources()
	src.Clients = clients
	s := newServer(t, Config{Key: testKey}, src)

	rec := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	body := rec.Body.String()
	if !strings.Contains(body, `"clients":[]`) {
		t.Errorf("body = %s, want a literal \"clients\":[] field", body)
	}
	if !strings.Contains(body, `"handshakes":[]`) {
		t.Errorf("body = %s, want a literal \"handshakes\":[] field", body)
	}
	if !strings.Contains(body, `"observationDisabled":false`) {
		t.Errorf("body = %s, want a literal \"observationDisabled\":false field", body)
	}
}

// TestHandleClientsReportsObservationDisabled is the field-value test
// for clientsResponse.ObservationDisabled (PR 108 review HIGH 2 /
// MEDIUM): with Config.ObservationDisabled set, the field must be true
// on the wire even when the snapshot itself is empty, so a consumer can
// tell "the observer is off" apart from "nothing connected yet".
func TestHandleClientsReportsObservationDisabled(t *testing.T) {
	t.Parallel()

	clients := &fakeClientObserver{snap: connobs.Snapshot{}}
	src := testSources()
	src.Clients = clients
	s := newServer(t, Config{Key: testKey, ObservationDisabled: true}, src)

	rec := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got clientsResponse
	decodeJSON(t, rec.Body.Bytes(), &got)

	if !got.ObservationDisabled {
		t.Error("ObservationDisabled = false, want true")
	}
	if len(got.Clients) != 0 || len(got.Handshakes) != 0 {
		t.Errorf("Clients/Handshakes = %v/%v, want both empty", got.Clients, got.Handshakes)
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
