package sep2embed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
)

func TestIdentityFromContextRoundTrips(t *testing.T) {
	t.Parallel()

	id := deviceIdentity{lfdi: "AABBCC", sfdi: "112233445566"}
	ctx := context.WithValue(context.Background(), identityCtxKey{}, id)

	lfdi, sfdi, ok := identityFromContext(ctx)
	if !ok {
		t.Fatal("identityFromContext: ok = false, want true")
	}
	if lfdi != id.lfdi {
		t.Errorf("lfdi = %q, want %q", lfdi, id.lfdi)
	}
	if sfdi != id.sfdi {
		t.Errorf("sfdi = %q, want %q", sfdi, id.sfdi)
	}
}

func TestIdentityFromContextMissingReturnsNotOK(t *testing.T) {
	t.Parallel()

	lfdi, sfdi, ok := identityFromContext(context.Background())
	if ok {
		t.Fatalf("identityFromContext on bare context: ok = true, want false (lfdi=%q sfdi=%q)", lfdi, sfdi)
	}
	if lfdi != "" || sfdi != "" {
		t.Errorf("identityFromContext on bare context returned non-empty values: lfdi=%q sfdi=%q", lfdi, sfdi)
	}
}

// TestConnObserveMiddlewareRecordsRequestThroughIdentityMiddleware
// drives one real HTTP request through the exact composition auth.go's
// buildHandler wires (identityMiddleware -> connObserveMiddleware ->
// next), with a real, non-nil *connobs.Hook, and asserts the LFDI, path,
// and request count connObserveMiddleware records match the caller's
// actual certificate-derived identity and the actual request path: this
// is the unit-level lock-in for the wiring seam
// auth.go's identityMiddleware/connObserveMiddleware composition covers,
// previously only exercised with hook == nil.
func TestConnObserveMiddlewareRecordsRequestThroughIdentityMiddleware(t *testing.T) {
	t.Parallel()

	caCertPEM, caKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{
		Organization: "sep2embed auth test CA",
		CommonName:   "sep2embed auth test CA",
	})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}
	devCertPEM, _, err := sep2cert.GenerateDeviceCert(caCert, caKey, sep2cert.DeviceCertOptions{
		DeviceType:  sep2cert.DeviceTypeGeneric,
		HWSerialNum: "test-serial-observe-middleware",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	leaf, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM: %v", err)
	}
	wantLFDI := sepTLS.LFDI(leaf)

	var hook connobs.Hook
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	chain := identityMiddleware(connObserveMiddleware(&hook)(next))

	const wantPath = "/edev/0/der/1"
	req := httptest.NewRequest(http.MethodGet, wantPath, nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	w := httptest.NewRecorder()
	chain.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	snap := hook.Snapshot()
	if len(snap.Clients) != 1 {
		t.Fatalf("Snapshot().Clients len = %d, want 1", len(snap.Clients))
	}
	c := snap.Clients[0]
	if c.LFDI != wantLFDI {
		t.Errorf("Clients[0].LFDI = %q, want %q (the caller's certificate-derived LFDI)", c.LFDI, wantLFDI)
	}
	if c.RequestCount != 1 {
		t.Errorf("Clients[0].RequestCount = %d, want 1", c.RequestCount)
	}
	if len(c.Paths) != 1 || c.Paths[0] != wantPath {
		t.Errorf("Clients[0].Paths = %v, want [%q]", c.Paths, wantPath)
	}
}

func TestSFDIPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sfdi    string
		want    string
		wantErr bool
	}{
		{name: "exact length", sfdi: "12345678", want: "12345678"},
		{name: "longer truncates", sfdi: "123456789012", want: "12345678"},
		{name: "too short errors", sfdi: "1234567", wantErr: true},
		{name: "empty errors", sfdi: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sfdiPrefix(tt.sfdi)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sfdiPrefix(%q): want error, got nil (result %q)", tt.sfdi, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sfdiPrefix(%q): unexpected error: %v", tt.sfdi, err)
			}
			if got != tt.want {
				t.Errorf("sfdiPrefix(%q) = %q, want %q", tt.sfdi, got, tt.want)
			}
		})
	}
}
