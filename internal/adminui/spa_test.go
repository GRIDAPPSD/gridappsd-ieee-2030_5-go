package adminui

import (
	"strings"
	"testing"
)

// TestSPARouteRequiresBearerAuth is the GAGO-060 posture-preservation
// acceptance test: the SPA route sits behind the exact same
// bearerAuth middleware as the /api/... routes (GAGO-058), so a
// missing or wrong token on "/" must still 401, not serve index.html.
func TestSPARouteRequiresBearerAuth(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})

	assertGET(t, s.Handler(), "/", "", "localhost", 401)
	assertGET(t, s.Handler(), "/", "Bearer wrong-token", "localhost", 401)
}

// TestSPARouteServesIndexHTMLWithValidBearer confirms a correctly
// authenticated "/" request returns 200 with the built index.html
// content, proving the SPA handler is reachable through the full
// middleware chain (host allowlist, then bearer auth, then requireGET)
// exactly like every /api/... route.
func TestSPARouteServesIndexHTMLWithValidBearer(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("GET / status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<div id=\"app\">") {
		t.Errorf("GET / body does not look like the built index.html (missing #app mount point); body = %s", body)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want a text/html prefix", ct)
	}
}

// TestSPAFallbackServesIndexHTMLForUnknownClientRoute is the SPA
// fallback acceptance test: an unknown path that is NOT under /api
// (a client side route such as "/registry", not yet its own server
// side handler) must serve the same index.html body as "/", so a hard
// reload on a client side route still works.
func TestSPAFallbackServesIndexHTMLForUnknownClientRoute(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})

	rootRec := doRequest(t, s.Handler(), "GET", "/", "Bearer "+testKey, "localhost")
	fallbackRec := doRequest(t, s.Handler(), "GET", "/registry", "Bearer "+testKey, "localhost")

	if fallbackRec.Code != 200 {
		t.Fatalf("GET /registry status = %d, want 200; body = %s", fallbackRec.Code, fallbackRec.Body.String())
	}
	if fallbackRec.Body.String() != rootRec.Body.String() {
		t.Errorf("GET /registry body does not match GET / body: fallback must serve the same index.html")
	}
}

// TestSPAFallbackDoesNotShadowUnknownAPIPath is the invariant test
// guarding the "/api paths must fall through to the existing API
// handlers (or 404 as they do today), never to index.html" requirement:
// an /api path with no registered handler must 404, and its body must
// NOT be the SPA's index.html.
func TestSPAFallbackDoesNotShadowUnknownAPIPath(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/api/does-not-exist", "Bearer "+testKey, "localhost")
	if rec.Code != 404 {
		t.Fatalf("GET /api/does-not-exist status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Errorf("GET /api/does-not-exist body looks like index.html; the SPA fallback must never shadow /api: body = %s", rec.Body.String())
	}

	var got errorResponse
	decodeJSON(t, rec.Body.Bytes(), &got)
	if got.Error != "not found" {
		t.Errorf("GET /api/does-not-exist error = %q, want %q", got.Error, "not found")
	}
}

// TestSPAStaticAssetIsServedDirectly confirms a real built asset (the
// favicon, which the committed dist/ always carries) is served as
// itself rather than being redirected to index.html: only UNKNOWN
// paths fall back to the SPA shell.
func TestSPAStaticAssetIsServedDirectly(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, testKey, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})

	rec := doRequest(t, s.Handler(), "GET", "/favicon.svg", "Bearer "+testKey, "localhost")
	if rec.Code != 200 {
		t.Fatalf("GET /favicon.svg status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Errorf("GET /favicon.svg body looks like index.html, not the actual favicon asset")
	}
}
