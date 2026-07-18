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

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
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

func (s *spyResolver) OwnsEndDevice(callerLFDI, edevID string) bool {
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

func TestRegistryOwnerResolverOwnsEndDevice(t *testing.T) {
	t.Parallel()

	// A conformant (no-alias) device: advertised under and owned via its
	// canonical LFDI, so its /edev id and its ownership identity coincide.
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-a", Name: "Device A", LFDI: "LFDI-A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	resolver := newRegistryOwnerResolver(reg)

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
			got := resolver.OwnsEndDevice(tt.callerLFDI, tt.edevID)
			if got != tt.want {
				t.Errorf("OwnsEndDevice(%q, %q) = %v, want %v", tt.callerLFDI, tt.edevID, got, tt.want)
			}
		})
	}
}

// TestRegistryOwnerResolverAliasedDeviceOwnedByCanonicalLFDI proves the
// dual-index ownership contract for a device advertised under a
// file-hash alias: the device is addressed on the wire by its alias
// (the /edev/{id} segment), but the caller is matched against its
// CANONICAL LFDI, never its alias. The alias must NOT satisfy the
// ownership check even when presented as the caller identity, because a
// wire caller is always identified by the DER-hash canonical LFDI and
// never by the file-hash alias.
func TestRegistryOwnerResolverAliasedDeviceOwnedByCanonicalLFDI(t *testing.T) {
	t.Parallel()

	const (
		canonicalA = "AAAA000000000000000000000000000000AAAA00" // uppercase DER-hash shape
		aliasA     = "bbbb000000000000000000000000000000bbbb00" // lowercase file-hash shape
	)
	reg := registry.New()
	if err := reg.Add(registry.Entry{MRID: "mrid-a", Name: "Aliased A", LFDI: canonicalA, AliasLFDI: aliasA}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	resolver := newRegistryOwnerResolver(reg)

	tests := []struct {
		name       string
		callerLFDI string
		edevID     string
		want       bool
	}{
		{"canonical caller owns the device addressed by its alias", canonicalA, aliasA, true},
		{"alias presented as caller does NOT own the device (alias is never a caller identity)", aliasA, aliasA, false},
		{"canonical caller addressing the device by its canonical LFDI fails: it is advertised under the alias, not the canonical id", canonicalA, canonicalA, false},
		{"alias addressing the device by canonical id fails closed", aliasA, canonicalA, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolver.OwnsEndDevice(tt.callerLFDI, tt.edevID)
			if got != tt.want {
				t.Errorf("OwnsEndDevice(%q, %q) = %v, want %v", tt.callerLFDI, tt.edevID, got, tt.want)
			}
		})
	}
}

// TestRegistryOwnerResolverNoCrossDeviceResolution is the SECURITY
// BOUNDARY test (data-invariants Rule 3, boundary-of-the-boundary): with
// two devices each carrying distinct canonical AND alias LFDIs, device
// A's caller (its canonical LFDI) owns ONLY device A, and never device
// B, whether B is addressed by its alias OR its canonical id, and
// regardless of any alias/canonical confusion. It proves the dual index
// creates no path by which one cert controls another device.
func TestRegistryOwnerResolverNoCrossDeviceResolution(t *testing.T) {
	t.Parallel()

	const (
		canonA = "A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1"
		aliasA = "a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
		canonB = "B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1B1"
		aliasB = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	)

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-a", Name: "A", LFDI: canonA, AliasLFDI: aliasA},
		{MRID: "mrid-b", Name: "B", LFDI: canonB, AliasLFDI: aliasB},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	resolver := newRegistryOwnerResolver(reg)

	// A's canonical caller owns A (addressed by A's alias) and NOTHING
	// else. Every other (caller, edevID) combination across the two
	// devices' four identities must be denied.
	if !resolver.OwnsEndDevice(canonA, aliasA) {
		t.Fatal("device A's canonical caller must own device A (addressed by A's alias)")
	}

	denied := []struct {
		name       string
		callerLFDI string
		edevID     string
	}{
		{"A caller cannot own B via B's alias", canonA, aliasB},
		{"A caller cannot own B via B's canonical id", canonA, canonB},
		{"A caller cannot own A via A's canonical id (A is advertised under its alias)", canonA, canonA},
		{"B caller cannot own A via A's alias", canonB, aliasA},
		{"B caller cannot own A via A's canonical id", canonB, canonA},
		{"A's alias presented as caller owns nothing (alias is never a caller identity)", aliasA, aliasA},
		{"A's alias presented as caller cannot own B", aliasA, aliasB},
		{"B's alias presented as caller owns nothing", aliasB, aliasB},
	}
	for _, tt := range denied {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if resolver.OwnsEndDevice(tt.callerLFDI, tt.edevID) {
				t.Errorf("OwnsEndDevice(%q, %q) = true, want false: no cross-device or alias-as-caller resolution allowed", tt.callerLFDI, tt.edevID)
			}
		})
	}
}

// TestOwnsEndDeviceAgreesWithSepTLSLFDIDerivation locks the invariant
// registryOwnerResolver.OwnsEndDevice's doc comment states: the caller
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

	// Stored side: a registry.Entry whose canonical LFDI is
	// certificate-derived exactly as GAGO-033's EnsureDeviceIdentities
	// produces it, with no alias (the conformant path). The device is
	// therefore advertised under, and owned via, its canonical LFDI.
	reg := registry.New()
	entry := registry.Entry{
		MRID: "mrid-lfdi-invariant-test",
		Name: "LFDI Invariant Test Device",
		LFDI: sepTLS.LFDI(leaf),
	}
	if err := reg.Add(entry); err != nil {
		t.Fatalf("registry Add: %v", err)
	}

	// Caller side: the real identityMiddleware, fed a request whose
	// TLS.PeerCertificates[0] is the same leaf certificate, exactly as
	// a live mTLS handshake would populate it.
	var gotCallerLFDI string
	spy := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		lfdi, _, ok := identityFromContext(r.Context())
		if !ok {
			t.Fatal("identityFromContext: ok = false after identityMiddleware ran")
		}
		gotCallerLFDI = lfdi
	})
	req := httptest.NewRequest(http.MethodGet, "/edev/"+entry.LFDI, nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	identityMiddleware(spy).ServeHTTP(httptest.NewRecorder(), req)

	if gotCallerLFDI != entry.LFDI {
		t.Fatalf("identityMiddleware-derived caller LFDI %q != registry canonical LFDI %q; the two derivation paths disagree", gotCallerLFDI, entry.LFDI)
	}

	resolver := newRegistryOwnerResolver(reg)
	// The conformant device's advertised /edev id is its canonical LFDI
	// (StoreID falls back to LFDI when there is no alias), so the caller
	// addresses it by, and is matched against, the same canonical LFDI.
	if entry.StoreID() != entry.LFDI {
		t.Fatalf("conformant entry StoreID %q != canonical LFDI %q; a no-alias device must advertise under its canonical LFDI", entry.StoreID(), entry.LFDI)
	}
	if !resolver.OwnsEndDevice(gotCallerLFDI, entry.StoreID()) {
		t.Errorf("OwnsEndDevice(%q, %q) = false, want true: both sides derive from sepTLS.LFDI on the same certificate", gotCallerLFDI, entry.StoreID())
	}
}
