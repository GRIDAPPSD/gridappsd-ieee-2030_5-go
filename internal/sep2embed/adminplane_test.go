package sep2embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2adminplane"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// TestAdminPlaneServesTheEmbedStores builds the admin plane over the
// store set and notifier an Embed's protocol listener serves, as
// cmd/bridge does, and reads a seeded device back through the plane: the
// admin view and the protocol view are one store set, not two. Read-only, as
// the bridge builds it, the plane mounts no write but the two auth POSTs.
func TestAdminPlaneServesTheEmbedStores(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.AddBatch(fixtureEntries()); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	e, err := New(context.Background(), Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                t.TempDir(),
		ResolveRegistrationPIN: testResolvePIN,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const key = "embed-admin-key-0123"
	plane, err := sep2adminplane.New(sep2adminplane.Config{
		Stores:       e.Stores(),
		Notifier:     e.Notifier(),
		AdminKey:     key,
		AllowedHosts: []string{"localhost"},
		ReadOnly:     true,
	})
	if err != nil {
		t.Fatalf("sep2adminplane.New over the embed stores: %v", err)
	}

	lfdi := fixtureEntries()[0].LFDI
	req := httptest.NewRequest(http.MethodGet, "/api/devices/by-lfdi/"+lfdi, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	plane.Handler().ServeHTTP(rec, req)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `"found":true`) || !strings.Contains(body, lfdi) {
		t.Fatalf("GET the seeded device through the plane: %d %s", rec.Code, rec.Body.String())
	}

	var writes []string
	for _, p := range plane.Patterns() {
		if method, _, _ := strings.Cut(p, " "); method != http.MethodGet {
			writes = append(writes, p)
		}
	}
	if want := []string{"POST /auth/login", "POST /auth/ticket"}; !slices.Equal(writes, want) {
		t.Errorf("non-GET admin routes over the embed stores = %q, want only %q", writes, want)
	}
}

// SEP2_EDITION=2023 sets Edition2023 on the stores the protocol router
// serves, and the plane then starts over them; New refuses an edition that
// disagrees with the stores.
func TestEdition2023StoresLetThePlaneStart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		edition2023 bool
		edition     string
	}{
		{"2023", true, "2023"},
		{"2018", false, "2018"},
		{"unset", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := New(context.Background(), Config{
				Addr:                   "127.0.0.1:0",
				CertDir:                t.TempDir(),
				ResolveRegistrationPIN: testResolvePIN,
				Edition2023:            tc.edition2023,
			}, registry.New())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := e.Stores().Edition2023; got != tc.edition2023 {
				t.Fatalf("Stores().Edition2023 = %v, want %v", got, tc.edition2023)
			}
			if _, err := sep2adminplane.New(sep2adminplane.Config{
				Stores:       e.Stores(),
				AdminKey:     "embed-admin-key-0123",
				AllowedHosts: []string{"localhost"},
				Edition:      tc.edition,
				ReadOnly:     true,
			}); err != nil {
				t.Fatalf("plane over the stores: %v", err)
			}
		})
	}
}
