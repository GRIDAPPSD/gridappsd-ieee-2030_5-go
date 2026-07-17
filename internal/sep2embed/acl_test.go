package sep2embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
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

func TestStoreOwnerResolverOwnsEndDevice(t *testing.T) {
	t.Parallel()

	store := memory.NewEndDeviceStore()
	enabled := true
	if err := store.Create(context.Background(), "DEV-A", sep2.EndDevice{Enabled: &enabled, LFDI: "LFDI-A"}); err != nil {
		t.Fatalf("seed EndDevice: %v", err)
	}

	resolver := newStoreOwnerResolver(store)

	tests := []struct {
		name       string
		callerLFDI string
		edevID     string
		want       bool
	}{
		{"caller's own LFDI matches the device at that id", "LFDI-A", "DEV-A", true},
		{"different caller LFDI is not the owner", "LFDI-WRONG", "DEV-A", false},
		{"unknown device id fails closed", "LFDI-A", "DEV-UNKNOWN", false},
		{"blank caller LFDI fails closed", "", "DEV-A", false},
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
