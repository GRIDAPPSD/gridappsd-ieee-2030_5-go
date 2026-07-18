package adminui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// fakeRegistry is a minimal RegistrySource test double: Snapshot
// returns whatever entries the test preloaded, with no locking, since
// tests drive it single threaded.
type fakeRegistry struct {
	entries []registry.Entry
}

func (f *fakeRegistry) Snapshot() []registry.Entry { return f.entries }

// fakeEndDevices is a minimal EndDeviceSource test double.
type fakeEndDevices struct {
	edevs []sep2embed.EndDeviceSnapshot
	err   error
}

func (f *fakeEndDevices) EndDevices(context.Context) ([]sep2embed.EndDeviceSnapshot, error) {
	return f.edevs, f.err
}

// fakePrograms is a minimal DERProgramSource test double, keyed by
// EndDevice ID so a test can assign a distinct program list per device.
type fakePrograms struct {
	byEdevID map[string][]sep2embed.DERProgramSnapshot
	err      error
}

func (f *fakePrograms) DERPrograms(_ context.Context, edevID string) ([]sep2embed.DERProgramSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byEdevID[edevID], nil
}

// fakeFlow is a minimal ControlFlowSource test double.
type fakeFlow struct {
	snap controlobs.Snapshot
}

func (f *fakeFlow) Snapshot() controlobs.Snapshot { return f.snap }

// newTestServer builds a Server wired to the four fakes above, bound to
// an ephemeral loopback port, for tests that only need s.Handler() via
// httptest and never call Run.
func newTestServer(t *testing.T, key string, reg RegistrySource, devices EndDeviceSource, programs DERProgramSource, flow ControlFlowSource) *Server {
	t.Helper()
	s, err := New(Config{Addr: "127.0.0.1:0", Key: key}, reg, devices, programs, flow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.ln.Close() })
	return s
}

// doRequest issues a single in-process request against handler via
// httptest, with the given method, path, Authorization header value
// (empty means omit the header entirely), and Host header (empty means
// leave httptest's own default). It returns the recorded response so
// callers can assert on status code and, where relevant, body.
func doRequest(t *testing.T, handler http.Handler, method, path, authHeader, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// assertGET issues a GET and fails the test if the status code does
// not exactly match want.
func assertGET(t *testing.T, handler http.Handler, path, authHeader, host string, want int) {
	t.Helper()
	rec := doRequest(t, handler, http.MethodGet, path, authHeader, host)
	if rec.Code != want {
		t.Errorf("GET %s (auth=%q) status = %d, want %d; body = %s", path, authHeader, rec.Code, want, rec.Body.String())
	}
}

// assertGETWithHost is assertGET with an explicit, always-set Host
// header, for the host allowlist tests where an empty host is not a
// meaningful "default" case.
func assertGETWithHost(t *testing.T, handler http.Handler, path, authHeader, host string, want int) {
	t.Helper()
	rec := doRequest(t, handler, http.MethodGet, path, authHeader, host)
	if rec.Code != want {
		t.Errorf("GET %s (host=%q) status = %d, want %d; body = %s", path, host, rec.Code, want, rec.Body.String())
	}
}
