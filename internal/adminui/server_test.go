package adminui

import (
	"errors"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/controlobs"
)

// TestNewReturnsErrDisabledWhenKeyEmpty is the GAGO-058 fail closed
// acceptance test: an unset SEP2_ADMIN_UI_KEY (an empty Config.Key)
// must produce ErrDisabled, not a listening server, so cmd/bridge never
// starts an unauthenticated admin UI runner by accident.
func TestNewReturnsErrDisabledWhenKeyEmpty(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Addr: "127.0.0.1:0", Key: ""}, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("New(empty Key) error = %v, want ErrDisabled", err)
	}
}

// TestNewBindsLoopbackAddrByDefault confirms a loopback Addr (the
// SEP2_ADMIN_UI_ADDR default shape) is accepted with no opt-in flag.
func TestNewBindsLoopbackAddrByDefault(t *testing.T) {
	t.Parallel()

	s, err := New(Config{Addr: "127.0.0.1:0", Key: "secret"}, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.ln.Close()

	if s.Addr() == "" {
		t.Errorf("Addr() = %q, want a bound loopback address", s.Addr())
	}
}

// TestNewRejectsNonLoopbackWithoutOptIn is the "explicit opt-in for
// non-loopback" acceptance test: binding a non-loopback host without
// AllowNonLoopback must fail closed, before any socket opens.
func TestNewRejectsNonLoopbackWithoutOptIn(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Addr: "0.0.0.0:0", Key: "secret"}, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	if err == nil {
		t.Fatal("New(non-loopback Addr, AllowNonLoopback=false) error = nil, want a rejection")
	}
}

// TestNewAllowsNonLoopbackWithOptIn confirms the explicit opt-in flag
// actually permits binding a non-loopback host.
func TestNewAllowsNonLoopbackWithOptIn(t *testing.T) {
	t.Parallel()

	s, err := New(Config{Addr: "0.0.0.0:0", Key: "secret", AllowNonLoopback: true}, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	if err != nil {
		t.Fatalf("New(non-loopback Addr, AllowNonLoopback=true): %v", err)
	}
	defer s.ln.Close()
}

// TestNewRejectsEmptyAddr confirms Addr is a required field regardless
// of Key.
func TestNewRejectsEmptyAddr(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Addr: "", Key: "secret"}, &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{})
	if err == nil {
		t.Fatal("New(empty Addr) error = nil, want a rejection")
	}
}

// TestIsLoopbackHostFieldValues drives isLoopbackHost across every
// address shape the posture check must distinguish, asserting the exact
// returned boolean per input per the data-invariants discipline: this
// is the one function the whole loopback-or-not decision rests on, so
// each case's exact value matters, not just "no panic."
func TestIsLoopbackHostFieldValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		addr string
		want bool
	}{
		{"ipv4 loopback", "127.0.0.1:8444", true},
		{"ipv6 loopback", "[::1]:8444", true},
		{"localhost literal", "localhost:8444", true},
		{"localhost mixed case", "LocalHost:8444", true},
		{"all interfaces", "0.0.0.0:8444", false},
		{"specific lan ip", "192.168.1.5:8444", false},
		{"empty host, port only", ":8444", false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := isLoopbackHost(c.addr)
			if err != nil {
				t.Fatalf("isLoopbackHost(%q): %v", c.addr, err)
			}
			if got != c.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", c.addr, got, c.want)
			}
		})
	}
}

// TestBearerAuthAcceptsExactToken confirms the correct token is
// accepted and the request reaches the wrapped handler.
func TestBearerAuthAcceptsExactToken(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, "correct-token", &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{controlobs.Snapshot{}})
	assertGETWithHost(t, s.Handler(), "/api/health", "Bearer correct-token", "localhost", 200)
}

// TestBearerAuthRejectsMissingOrWrongToken is the GAGO-058 "401 on bad
// token" acceptance test, covering both a wrong token and a completely
// absent Authorization header.
func TestBearerAuthRejectsMissingOrWrongToken(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, "correct-token", &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{controlobs.Snapshot{}})

	cases := []struct {
		name   string
		header string
	}{
		{"wrong token", "Bearer wrong-token"},
		{"missing header", ""},
		{"missing bearer prefix", "correct-token"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assertGETWithHost(t, s.Handler(), "/api/health", c.header, "localhost", 401)
		})
	}
}

// TestHostAllowlistAcceptsDefaultsAndConfiguredHosts is the GAGO-058
// host allowlist acceptance test: the built in defaults are always
// accepted, and a name added via Config.AllowedHosts is also accepted.
func TestHostAllowlistAcceptsDefaultsAndConfiguredHosts(t *testing.T) {
	t.Parallel()

	s, err := New(Config{Addr: "127.0.0.1:0", Key: "secret", AllowedHosts: []string{"admin.internal.example"}},
		&fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{controlobs.Snapshot{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.ln.Close()

	for _, host := range []string{"localhost", "127.0.0.1", "admin.internal.example", "ADMIN.INTERNAL.EXAMPLE"} {
		host := host
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			assertGETWithHost(t, s.Handler(), "/api/health", "Bearer secret", host, 200)
		})
	}
}

// TestHostAllowlistRejectsUnrecognizedHost confirms a Host header not
// covered by the defaults or Config.AllowedHosts is rejected with 403,
// even when the Bearer token is correct: the allowlist check runs
// before the auth check in buildHandler's chain.
func TestHostAllowlistRejectsUnrecognizedHost(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, "secret", &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{controlobs.Snapshot{}})
	assertGETWithHost(t, s.Handler(), "/api/health", "Bearer secret", "evil.example.com", 403)
}

// TestRequireGETRejectsNonGETMethods is the GAGO-058 "GET only, 405 on
// others" acceptance test.
func TestRequireGETRejectsNonGETMethods(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, "secret", &fakeRegistry{}, &fakeEndDevices{}, &fakePrograms{}, &fakeFlow{controlobs.Snapshot{}})

	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			rec := doRequest(t, s.Handler(), method, "/api/health", "Bearer secret", "localhost")
			if rec.Code != 405 {
				t.Errorf("%s /api/health status = %d, want 405", method, rec.Code)
			}
		})
	}
}
