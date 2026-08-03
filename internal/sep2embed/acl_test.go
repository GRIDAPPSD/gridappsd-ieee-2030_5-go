package sep2embed

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2cert"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store/memory"
)

// spyResolver records every OwnsEndDevice call it receives and answers
// via the injected owns func, so tests can both control the answer and
// assert whether (and with what arguments) the resolver was consulted
// at all.
type spyResolver struct {
	owns      func(callerLFDI, edevID string) bool
	called    bool
	gotCaller string
	gotEdevID string
}

func (s *spyResolver) OwnsEndDevice(_ context.Context, callerLFDI, edevID string) bool {
	s.called = true
	s.gotCaller = callerLFDI
	s.gotEdevID = edevID
	return s.owns(callerLFDI, edevID)
}

// spyHandler is the next handler in the chain: it records whether it
// was reached and always answers 200, so a test can distinguish "the
// ACL denied before dispatch" (spy never called, status is the ACL's
// own error code) from "the ACL passed the request through" (spy
// called, status 200).
type spyHandler struct {
	called bool
}

func (s *spyHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.called = true
	w.WriteHeader(http.StatusOK)
}

// withIdentity attaches a deviceIdentity to r's context, the same way
// identityMiddleware would after a successful mTLS handshake. Tests
// use this to simulate aclMiddleware running downstream of
// identityMiddleware without standing up real TLS.
func withIdentity(r *http.Request, lfdi, sfdi string) *http.Request {
	ctx := context.WithValue(r.Context(), identityCtxKey{}, deviceIdentity{lfdi: lfdi, sfdi: sfdi})
	return r.WithContext(ctx)
}

func TestACLMiddlewareDeniesWhenNoIdentity(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(string, string) bool { return true }}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := httptest.NewRequest(http.MethodGet, "/edev/DEV-A", nil) // no identity attached
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (no identity)", w.Code, http.StatusForbidden)
	}
	if next.called {
		t.Error("next handler was called; want denied before dispatch")
	}
	if resolver.called {
		t.Error("resolver was consulted with no identity present; want denied before ownership check")
	}
}

func TestACLMiddlewareDeniesMethodOutsideFamily(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(string, string) bool { return true }}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/dcap", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d (read-only family, POST)", w.Code, http.StatusMethodNotAllowed)
	}
	if next.called {
		t.Error("next handler was called; want denied before dispatch")
	}
}

func TestACLMiddlewareAllowsOwnedDeviceResource(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/edev/DEV-A", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (owned device)", w.Code, http.StatusOK)
	}
	if !next.called {
		t.Error("next handler was not called; want the request dispatched")
	}
	if !resolver.called {
		t.Fatal("resolver was never consulted")
	}
	if resolver.gotCaller != "CALLER-LFDI" || resolver.gotEdevID != "DEV-A" {
		t.Errorf("resolver called with (%q, %q), want (%q, %q)", resolver.gotCaller, resolver.gotEdevID, "CALLER-LFDI", "DEV-A")
	}
}

func TestACLMiddlewareDeniesCrossDeviceOwnership(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		// CALLER-LFDI owns only DEV-A, never DEV-B.
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/edev/DEV-B", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (cross-device read)", w.Code, http.StatusForbidden)
	}
	if next.called {
		t.Error("next handler was called; want cross-device access denied before dispatch")
	}
}

// TestACLMiddlewareAllowsOwnedDERInstanceWrite is the GAGO-111
// regression test: a PUT to the DER instance path
// (/edev/{id}/der/{derId}) from the OWNING device must reach the
// handler, not be refused by the method-family table (the defect this
// card fixes) or by ownership.
func TestACLMiddlewareAllowsOwnedDERInstanceWrite(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodPut, "/edev/DEV-A/der/1", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (owning device PUT to its own DER instance)", w.Code, http.StatusOK)
	}
	if !next.called {
		t.Error("next handler was not called; want the owning device's DER instance PUT dispatched")
	}
}

// TestACLMiddlewareDeniesNonOwnerDERInstanceWrite pins the ownership
// boundary that must NOT regress alongside the GAGO-111 method-table
// widening: a PUT to the DER instance path from a device that does not
// own the target EndDevice is still refused, before the handler ever
// sees it.
func TestACLMiddlewareDeniesNonOwnerDERInstanceWrite(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		// CALLER-LFDI owns only DEV-A, never DEV-B.
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodPut, "/edev/DEV-B/der/1", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (non-owner DER instance write)", w.Code, http.StatusForbidden)
	}
	if next.called {
		t.Error("next handler was called; want non-owner DER instance write denied before dispatch")
	}
}

// TestACLMiddlewareAllowsOwnedLogEventPost is the GAGO-132 regression
// test. Core v0.13.0 retired /edev/{id}/log and mounts the WADL address
// /edev/{id}/lel instead (2018 A.3.5.1; sep_wadl.xml:1358), so a
// LogEvent POST from the OWNING device must reach the handler at the new
// address. This fails closed: with the ACL still keyed on the old
// address, /lel matches only the /edev/{id} singleton entry, which does
// not permit POST, and every LogEvent report is refused with a 405
// before dispatch.
func TestACLMiddlewareAllowsOwnedLogEventPost(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/edev/DEV-A/lel", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (owning device POSTing a LogEvent to its own list)", w.Code, http.StatusOK)
	}
	if !next.called {
		t.Error("next handler was not called; want the owning device's LogEvent POST dispatched")
	}
}

// TestACLMiddlewareDeniesNonOwnerLogEventPost pins the ownership
// boundary that must NOT regress alongside the GAGO-132 address move: a
// LogEvent POST to another device's list is still refused, before the
// handler ever sees it. Asserting next.called separately from the status
// keeps "denied before dispatch" distinguishable from a 403 some later
// layer might produce on its own.
func TestACLMiddlewareDeniesNonOwnerLogEventPost(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		// CALLER-LFDI owns only DEV-A, never DEV-B.
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/edev/DEV-B/lel", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (non-owner LogEvent POST)", w.Code, http.StatusForbidden)
	}
	if next.called {
		t.Error("next handler was called; want non-owner LogEvent POST denied before dispatch")
	}
}

func TestACLMiddlewareDeniesCrossDeviceWrite(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(caller, edevID string) bool {
		return caller == "CALLER-LFDI" && edevID == "DEV-A"
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodPut, "/edev/DEV-B/ps", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (cross-device write)", w.Code, http.StatusForbidden)
	}
	if next.called {
		t.Error("next handler was called; want cross-device write denied before dispatch")
	}
}

func TestACLMiddlewareAllowsCommonResourceWithoutOwnershipCheck(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(string, string) bool {
		t.Fatal("resolver consulted for a common, non-edev-scoped resource")
		return false
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/edev", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (common list resource)", w.Code, http.StatusOK)
	}
	if !next.called {
		t.Error("next handler was not called; want the request dispatched")
	}
	if resolver.called {
		t.Error("resolver was consulted for a non-/edev/{id} resource; want no ownership check")
	}
}

func TestACLMiddlewareUnmatchedNonEdevPathPassesThrough(t *testing.T) {
	t.Parallel()

	resolver := &spyResolver{owns: func(string, string) bool {
		t.Fatal("resolver consulted for a non-edev path")
		return false
	}}
	next := &spyHandler{}
	h := aclMiddleware(resolver)(next)

	// sep2acl has no family for this path at all: matched=false. Deferred
	// to the downstream mux (here, the spy), per the design's "unmatched
	// path with identity -> pass through to the mux" rule.
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/totally/unrecognized/path", nil), "CALLER-LFDI", "CALLER-SFDI")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (unmatched non-edev path passes through)", w.Code, http.StatusOK)
	}
	if !next.called {
		t.Error("next handler was not called; want an unmatched non-edev path dispatched to the mux")
	}
}

func TestACLMiddlewareUnmatchedEdevScopedPathStillEnforcesOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		edevID     string
		owns       bool
		wantStatus int
		wantNext   bool
	}{
		{"owned device, unmatched family, passes through", "DEV-A", true, http.StatusOK, true},
		{"cross-device, unmatched family, still denied", "DEV-B", false, http.StatusForbidden, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolver := &spyResolver{owns: func(caller, edevID string) bool {
				return caller == "CALLER-LFDI" && edevID == tt.edevID && tt.owns
			}}
			next := &spyHandler{}
			h := aclMiddleware(resolver)(next)

			// "unregistered/deep/leaf" matches no sep2acl family (matched
			// = false), but the path IS /edev/{id}/..., so EndDeviceID
			// still reports it as edev-scoped: ownership must still gate
			// it per the design's ordering rule.
			path := "/edev/" + tt.edevID + "/unregistered/deep/leaf"
			req := withIdentity(httptest.NewRequest(http.MethodGet, path, nil), "CALLER-LFDI", "CALLER-SFDI")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if next.called != tt.wantNext {
				t.Errorf("next.called = %v, want %v", next.called, tt.wantNext)
			}
		})
	}
}

// newTestEndDeviceStore creates a fresh memory-backed EndDeviceStore and
// seeds it with one EndDevice per (id, lfdi) pair, id used both as the
// store key and as EndDevice.LFDI, matching seed.go's real convention
// that the store key and the advertised LFDI are the same canonical
// value. ctx is used only for the seeding Create calls.
func newTestEndDeviceStore(ctx context.Context, t *testing.T, idToLFDI map[string]string) store.EndDeviceStore {
	t.Helper()
	s := memory.NewEndDeviceStore()
	for id, lfdi := range idToLFDI {
		enabled := true
		dev := sep2.EndDevice{Enabled: &enabled, LFDI: lfdi, SFDI: "00000000000"}
		dev.Href = "/edev/" + id
		if err := s.Create(ctx, id, dev); err != nil {
			t.Fatalf("seed EndDeviceStore Create(%q): %v", id, err)
		}
	}
	return s
}

func TestStoreOwnerResolverOwnsEndDevice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A conformant device: advertised under and owned via its canonical
	// LFDI, so its /edev id and its ownership identity coincide.
	s := newTestEndDeviceStore(ctx, t, map[string]string{"LFDI-A": "LFDI-A"})
	resolver := newStoreOwnerResolver(s)

	tests := []struct {
		name       string
		callerLFDI string
		edevID     string
		want       bool
	}{
		{"caller's own LFDI matches the device at that id", "LFDI-A", "LFDI-A", true},
		{"different caller LFDI is not the owner", "LFDI-WRONG", "LFDI-A", false},
		{"unknown device id fails closed", "LFDI-A", "DEV-UNKNOWN", false},
		{"blank caller LFDI fails closed", "", "LFDI-A", false},
		{"blank device id fails closed", "LFDI-A", "", false},
		{"both blank fail closed", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolver.OwnsEndDevice(ctx, tt.callerLFDI, tt.edevID)
			if got != tt.want {
				t.Errorf("OwnsEndDevice(%q, %q) = %v, want %v", tt.callerLFDI, tt.edevID, got, tt.want)
			}
		})
	}
}

// TestStoreOwnerResolverNoCrossDeviceResolution is the SECURITY BOUNDARY
// test (data-invariants Rule 3, boundary-of-the-boundary): with two
// devices each seeded under their own canonical LFDI, device A's caller
// owns ONLY device A, never device B, and B's caller owns only B. It
// proves the store-backed resolver creates no path by which one
// certificate's holder controls another device.
func TestStoreOwnerResolverNoCrossDeviceResolution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		canonA = "A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1"
		canonB = "B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1"
	)
	s := newTestEndDeviceStore(ctx, t, map[string]string{canonA: canonA, canonB: canonB})
	resolver := newStoreOwnerResolver(s)

	if !resolver.OwnsEndDevice(ctx, canonA, canonA) {
		t.Fatal("device A's caller must own device A")
	}
	if !resolver.OwnsEndDevice(ctx, canonB, canonB) {
		t.Fatal("device B's caller must own device B")
	}

	denied := []struct {
		name       string
		callerLFDI string
		edevID     string
	}{
		{"A caller cannot own B", canonA, canonB},
		{"B caller cannot own A", canonB, canonA},
	}
	for _, tt := range denied {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if resolver.OwnsEndDevice(ctx, tt.callerLFDI, tt.edevID) {
				t.Errorf("OwnsEndDevice(%q, %q) = true, want false: no cross-device resolution allowed", tt.callerLFDI, tt.edevID)
			}
		})
	}
}

// TestOwnsEndDeviceAgreesWithSepTLSLFDIDerivation locks the invariant
// storeOwnerResolver.OwnsEndDevice's doc comment states: the caller
// LFDI and the device's canonical LFDI both derive from sepTLS.LFDI and
// are therefore always the same canonical uppercase-hex form, which is
// why an exact, non-case-folded string compare is correct.
//
// It does not merely call sepTLS.LFDI(cert) twice (that would be
// tautological). It drives each side through the real code path its
// real caller uses:
//
//   - the "caller" side runs the request through the actual
//     identityMiddleware, exactly as a live mTLS handshake would, and
//     reads the LFDI back out via identityFromContext;
//   - the "stored" side runs the actual production seedOne function
//     (seed.go), exactly as Embed.New seeds real devices, from a
//     registry.Entry carrying the same certificate-derived LFDI.
//
// If either identityMiddleware's derivation or seedOne's field
// assignment ever drifts (a different casing function, a swapped
// field, a normalization step added on only one side), this test
// fails: it is the regression guard for the comment, not just a
// restatement of it.
func TestOwnsEndDeviceAgreesWithSepTLSLFDIDerivation(t *testing.T) {
	t.Parallel()

	caCertPEM, caKeyPEM, err := sep2cert.GenerateCA(sep2cert.CAOptions{
		Organization: "sep2embed acl test CA",
		CommonName:   "sep2embed acl test CA",
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
		HWSerialNum: "test-serial-lfdi-invariant",
		IsTestCert:  true,
	})
	if err != nil {
		t.Fatalf("GenerateDeviceCert: %v", err)
	}
	leaf, err := sep2cert.ParseCertificatePEM(devCertPEM)
	if err != nil {
		t.Fatalf("ParseCertificatePEM: %v", err)
	}

	// Stored side: an EndDevice keyed by, and carrying, its
	// certificate-derived canonical LFDI, exactly as seed.go seeds a real
	// device: the store key and the advertised LFDI are the same
	// canonical value.
	lfdi := sepTLS.LFDI(leaf)
	ctx := context.Background()
	s := newTestEndDeviceStore(ctx, t, map[string]string{lfdi: lfdi})

	// Caller side: the real identityMiddleware, fed a request whose
	// TLS.PeerCertificates[0] is the same leaf certificate, exactly as
	// a live mTLS handshake would populate it.
	var gotCallerLFDI string
	spy := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _, ok := identityFromContext(r.Context())
		if !ok {
			t.Fatal("identityFromContext: ok = false after identityMiddleware ran")
		}
		gotCallerLFDI = got
	})
	req := httptest.NewRequest(http.MethodGet, "/edev/"+lfdi, nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	identityMiddleware(spy).ServeHTTP(httptest.NewRecorder(), req)

	if gotCallerLFDI != lfdi {
		t.Fatalf("identityMiddleware-derived caller LFDI %q != seeded canonical LFDI %q; the two derivation paths disagree", gotCallerLFDI, lfdi)
	}

	resolver := newStoreOwnerResolver(s)
	if !resolver.OwnsEndDevice(ctx, gotCallerLFDI, lfdi) {
		t.Errorf("OwnsEndDevice(%q, %q) = false, want true: both sides derive from sepTLS.LFDI on the same certificate", gotCallerLFDI, lfdi)
	}
}
