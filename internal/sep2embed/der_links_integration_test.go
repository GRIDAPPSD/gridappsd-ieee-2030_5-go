package sep2embed

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// TestDERLinksResolveOverRealServer boots a real embedded server (real
// mTLS, real router, no shortcuts) with one seeded device and proves that
// all four DER links seedOne now stamps (DERCapabilityLink,
// DERSettingsLink, DERStatusLink, DERAvailabilityLink) resolve to
// actually-mounted routes: a GET against each advertised href returns
// 200, not 404.
//
// This is the check the delegation prompt calls out explicitly:
// advertising a link to a route that 404s is worse than not advertising
// it at all (the exact failure shape hit earlier with POST /mup/{id}).
// Core's singleton GET/PUT handler (pkg/sep2srv/handlers/singleton)
// returns a spec-valid empty default with 200 when the backing store
// holds nothing yet, so a 200 here is possible without this seeding path
// writing any placeholder content into DERSettings/DERStatus/
// DERAvailability itself; only the DER's own link fields are stamped.
func TestDERLinksResolveOverRealServer(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	_, _, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, caKeyFileName))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}

	certA, keyA, lfdiA := mintDeviceIdentity(t, caCert, caKey, "test-serial-derlinks-a")

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-derlinks-a", Name: "Device DERLinks A", LFDI: lfdiA},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned error after ctx cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not return within 3s of ctx cancel (goroutine leak or unbounded shutdown)")
		}
	})

	baseURL := "https://" + e.Addr()
	clientA := deviceClient(t, certA, keyA, caCertPEM)

	edevA := embedURLIndex(t, e, "mrid-derlinks-a")

	// Store-level exact-href assertion is TestSeedStoresStampsAllFourDERLinksOnDER
	// (seed_test.go); this test proves those same server-assigned-index
	// hrefs resolve against a real mounted route rather than 404ing.
	base := baseURL + "/edev/" + edevA + "/der/1/"

	cases := []struct {
		name string
		path string
	}{
		{"dercap", base + "dercap"},
		{"derg", base + "derg"},
		{"ders", base + "ders"},
		{"dera", base + "dera"},
	}
	for _, c := range cases {
		assertStatus(t, clientA, http.MethodGet, c.path, nil, http.StatusOK,
			"device A GETting its own advertised "+c.name+" link")
	}
}
