package adminui

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/activity"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
)

type devicesPayload struct {
	After   float64 `json:"commsOfflineAfterSeconds"`
	Devices []struct {
		LFDI        string  `json:"lfdi"`
		LastRequest *string `json:"lastRequest"`
		Comms       string  `json:"comms"`
	} `json:"devices"`
}

// commsServer is a Server whose recorder saw lfdi just now, over a
// protocol store holding that one EndDevice, and whose connobs snapshot
// holds a stale entry for the same LFDI.
func commsServer(t *testing.T, cfg Config, lfdi string) (*Server, *activity.Recorder) {
	t.Helper()
	rec := activity.New()
	rec.Record(lfdi)
	rec.Record(lfdi)
	rec.Record(lfdi)

	proto := newFakeProtocol()
	if err := proto.stores.EndDevices.Create(context.Background(), "1", sep2.EndDevice{SFDI: "111", LFDI: lfdi}); err != nil {
		t.Fatal(err)
	}
	src := testSources()
	src.Protocol = proto
	src.Activity = rec
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{Clients: []connobs.ClientSnapshot{
		{LFDI: lfdi, LastSeen: time.Unix(1700000000, 0).UTC(), Age: 24 * time.Hour, RequestCount: 1, Paths: []string{"/dcap"}},
	}}}
	cfg.Key = testKey
	return newServer(t, cfg, src), rec
}

func devicesData(t *testing.T, s *Server) devicesPayload {
	t.Helper()
	w := doRequest(t, s.Handler(), "GET", "/dashboard/data", "Bearer "+testKey, "localhost")
	var p devicesPayload
	if w.Code != 200 {
		t.Fatalf("GET /dashboard/data = %d %s", w.Code, w.Body)
	}
	decodeJSON(t, w.Body.Bytes(), &p)
	return p
}

// TestClientsAndDevicesAgreeOnLastRequest: both read the one recorder, so
// the stale connobs entry for the same LFDI cannot make them differ.
func TestClientsAndDevicesAgreeOnLastRequest(t *testing.T) {
	t.Parallel()
	const lfdi = "AABBCCDDEEFF00112233445566778899AABBCCDD"
	s, rec := commsServer(t, Config{}, lfdi)

	p := devicesData(t, s)
	if len(p.Devices) != 1 || p.Devices[0].LastRequest == nil {
		t.Fatalf("devices = %+v, want one with a lastRequest", p.Devices)
	}
	devAt, err := time.Parse(time.RFC3339, *p.Devices[0].LastRequest)
	if err != nil {
		t.Fatal(err)
	}

	w := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	var got clientsResponse
	decodeJSON(t, w.Body.Bytes(), &got)
	if len(got.Clients) != 1 {
		t.Fatalf("clients = %+v, want one", got.Clients)
	}
	c := got.Clients[0]
	cliAt, err := time.Parse(timeFormat, c.LastSeen)
	if err != nil {
		t.Fatal(err)
	}
	if !cliAt.Truncate(time.Second).Equal(devAt) {
		t.Errorf("/api/clients lastSeen %s vs devices lastRequest %s, want the same second", c.LastSeen, *p.Devices[0].LastRequest)
	}
	if _, n, _ := rec.Last(lfdi); n != 3 {
		t.Fatalf("recorder count = %d, want 3", n)
	}
	if c.RequestCount != 1 {
		t.Errorf("requestCount = %d, want connobs's 1: the panel counts every observed request", c.RequestCount)
	}
	if !c.Connected || c.AgeSeconds > 5 {
		t.Errorf("connected=%v ageSeconds=%d, want connected and fresh from the later recorder time", c.Connected, c.AgeSeconds)
	}
	if len(c.Paths) != 1 || c.Paths[0] != "/dcap" {
		t.Errorf("paths = %v, want connobs's [/dcap] kept", c.Paths)
	}
	if p.Devices[0].Comms != "online" {
		t.Errorf("comms = %q, want online", p.Devices[0].Comms)
	}
}

// TestClientsJSONShapeUnchanged pins the key set the mTLS harness reads.
func TestClientsJSONShapeUnchanged(t *testing.T) {
	t.Parallel()
	s, _ := commsServer(t, Config{}, "AABBCCDDEEFF00112233445566778899AABBCCDD")
	w := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	var top map[string]json.RawMessage
	decodeJSON(t, w.Body.Bytes(), &top)
	keys := func(m map[string]json.RawMessage) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(top); len(got) != 3 || got[0] != "clients" || got[1] != "handshakes" || got[2] != "observationDisabled" {
		t.Errorf("top-level keys = %v", got)
	}
	var cl []map[string]json.RawMessage
	decodeJSON(t, top["clients"], &cl)
	want := []string{"ageSeconds", "connected", "lastSeen", "lfdi", "paths", "requestCount"}
	got := keys(cl[0])
	if len(got) != len(want) {
		t.Fatalf("client keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("client keys = %v, want %v", got, want)
		}
	}
}

// TestIdleSettingReachesCommsOfflineAfter: the bridge's existing idle
// knob is the plane's offline threshold, default and override.
func TestIdleSettingReachesCommsOfflineAfter(t *testing.T) {
	t.Parallel()
	const lfdi = "AABBCCDDEEFF00112233445566778899AABBCCDD"
	for _, tc := range []struct {
		name string
		cfg  Config
		want float64
	}{
		{"default", Config{}, 300},
		{"override", Config{ClientIdleAfter: 90 * time.Second}, 90},
	} {
		s, _ := commsServer(t, tc.cfg, lfdi)
		if got := devicesData(t, s).After; got != tc.want {
			t.Errorf("%s: commsOfflineAfterSeconds = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRefusedRequestsKeepClientConnected: connobs counts requests the ACL
// refuses and the recorder does not, so a device in a refusal loop was seen
// a second ago by connobs and ten minutes ago by the recorder. /api/clients
// reflects all observed requests and reads it connected; the Devices tab
// shows its last accepted request and reads it offline.
func TestRefusedRequestsKeepClientConnected(t *testing.T) {
	t.Parallel()
	const lfdi = "AABBCCDDEEFF00112233445566778899AABBCCDD"
	accepted := time.Now().Add(-10 * time.Minute)
	rec := activity.NewWithClock(func() time.Time { return accepted })
	rec.Record(lfdi)
	seen := time.Now().Add(-time.Second)

	proto := newFakeProtocol()
	if err := proto.stores.EndDevices.Create(context.Background(), "1", sep2.EndDevice{SFDI: "111", LFDI: lfdi}); err != nil {
		t.Fatal(err)
	}
	src := testSources()
	src.Protocol = proto
	src.Activity = rec
	src.Clients = &fakeClientObserver{snap: connobs.Snapshot{Clients: []connobs.ClientSnapshot{
		{LFDI: lfdi, LastSeen: seen, Age: time.Second, RequestCount: 41, Paths: []string{"/edev"}},
	}}}
	s := newServer(t, Config{Key: testKey}, src)

	w := doRequest(t, s.Handler(), "GET", "/api/clients", "Bearer "+testKey, "localhost")
	var got clientsResponse
	decodeJSON(t, w.Body.Bytes(), &got)
	c := got.Clients[0]
	at, err := time.Parse(timeFormat, c.LastSeen)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Connected || c.AgeSeconds > 5 || c.RequestCount != 41 || at.Before(seen.Add(-time.Second)) {
		t.Errorf("client = %+v, want connected, fresh lastSeen, requestCount 41", c)
	}
	if len(c.Paths) != 1 || c.Paths[0] != "/edev" {
		t.Errorf("paths = %v, want [/edev]", c.Paths)
	}

	p := devicesData(t, s)
	if len(p.Devices) != 1 || p.Devices[0].Comms != "offline" || p.Devices[0].LastRequest == nil {
		t.Fatalf("devices = %+v, want offline with the last accepted request", p.Devices)
	}
	devAt, _ := time.Parse(time.RFC3339, *p.Devices[0].LastRequest)
	if devAt.After(time.Now().Add(-9 * time.Minute)) {
		t.Errorf("Devices lastRequest %s, want the accepted request ten minutes ago", devAt)
	}
}
