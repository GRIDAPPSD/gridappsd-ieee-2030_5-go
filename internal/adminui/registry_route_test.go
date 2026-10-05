package adminui

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// registryRouteSources returns sources over a real registry holding one
// certificate-backed device and one placeholder.
func registryRouteSources(t *testing.T) Sources {
	t.Helper()
	reg := registry.New()
	entries := []registry.Entry{
		{MRID: "_AAAA-0001", Name: "pv-1", LFDI: strings.Repeat("A", 40), SFDI: "111111111"},
		{MRID: "_BBBB-0002", Name: "bat-2", LFDI: strings.Repeat("B", 40), SFDI: "222222222", Placeholder: true},
	}
	if err := reg.AddBatch(entries); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}
	src := testSources()
	src.Registry = reg
	return src
}

// TestRegistryRouteShape pins the exact keys and value types the client
// config generator reads from GET /api/registry: a JSON array of objects
// with string mrid, name, lfdi, sfdi and a bool placeholder.
func TestRegistryRouteShape(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, registryRouteSources(t))
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/registry", "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/registry = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not a JSON array of objects: %v; body %s", err, rec.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %s", len(got), rec.Body.String())
	}
	byMRID := map[string]map[string]any{}
	for _, e := range got {
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if want := "lfdi,mrid,name,placeholder,sfdi"; strings.Join(keys, ",") != want {
			t.Errorf("keys = %v, want %s", keys, want)
		}
		for _, k := range []string{"mrid", "name", "lfdi", "sfdi"} {
			if _, ok := e[k].(string); !ok {
				t.Errorf("%s = %#v, want a string", k, e[k])
			}
		}
		if _, ok := e["placeholder"].(bool); !ok {
			t.Errorf("placeholder = %#v, want a bool", e["placeholder"])
		}
		byMRID[e["mrid"].(string)] = e
	}
	pv := byMRID["_AAAA-0001"]
	if pv["name"] != "pv-1" || pv["lfdi"] != strings.Repeat("A", 40) || pv["sfdi"] != "111111111" || pv["placeholder"] != false {
		t.Errorf("pv entry = %v", pv)
	}
	if bat := byMRID["_BBBB-0002"]; bat["name"] != "bat-2" || bat["placeholder"] != true {
		t.Errorf("placeholder entry = %v", bat)
	}
}

// TestRegistryRouteEmptyIsAnArray: an empty registry is [] and never null,
// since the generator refuses anything that is not an array.
func TestRegistryRouteEmptyIsAnArray(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/registry", "Bearer "+testKey, "localhost")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty registry = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
}

// TestRegistryRouteAuth: 401 without a key in keyed mode, 200 in no-key mode.
func TestRegistryRouteAuth(t *testing.T) {
	t.Parallel()

	keyed := newServer(t, Config{Key: testKey}, registryRouteSources(t))
	for _, header := range []string{"", "Bearer wrong-token-0123456789"} {
		if rec := doRequest(t, keyed.Handler(), http.MethodGet, "/api/registry", header, "localhost"); rec.Code != http.StatusUnauthorized {
			t.Errorf("keyed, Authorization %q: %d, want 401", header, rec.Code)
		}
	}
	src := registryRouteSources(t)
	src.Sender = newTestSender(t, &recordingBus{}, false)
	open := newServer(t, Config{InsecureNoKey: true}, src)
	rec := doRequest(t, open.Handler(), http.MethodGet, "/api/registry", "", "localhost")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "_AAAA-0001") {
		t.Errorf("no-key: %d %s, want 200 with the entries", rec.Code, rec.Body.String())
	}
}

// TestRegistryRouteHostAndMethod: an unlisted Host is 403 and a write
// method is 405.
func TestRegistryRouteHostAndMethod(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, registryRouteSources(t))
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/registry", "Bearer "+testKey, "evil.example.com"); rec.Code != http.StatusForbidden {
		t.Errorf("unlisted Host: %d, want 403", rec.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if rec := doRequest(t, s.Handler(), m, "/api/registry", "Bearer "+testKey, "localhost"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, rec.Code)
		}
	}
}
