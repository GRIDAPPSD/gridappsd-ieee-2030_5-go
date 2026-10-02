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
