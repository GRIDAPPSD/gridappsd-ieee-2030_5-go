package adminui

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2adminplane"
)

// testKey is exactly sep2adminplane.MinAdminKeyLength characters, the
// shortest key the plane accepts.
const testKey = "test-admin-token"

// TestNewReturnsErrDisabledForAnUnsetOrBlankKey: an unset or blank key is
// the "admin UI is off" state, so cmd/bridge opens no listener and keeps
// running.
func TestNewReturnsErrDisabledForAnUnsetOrBlankKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", "   "} {
		_, err := New(Config{Addr: "127.0.0.1:0", Key: key}, testSources())
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("New(Key=%q) error = %v, want ErrDisabled", key, err)
		}
	}
}

// TestNewRefusesAShortKey: a key that is set but under the plane's
// minimum is a configuration error that fails start, not "disabled". The
// error names the length rule and never the key.
func TestNewRefusesAShortKey(t *testing.T) {
	t.Parallel()

	short := testKey[:len(testKey)-1]
	_, err := New(Config{Addr: "127.0.0.1:0", Key: short}, testSources())
	if err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("New(short key) error = %v, want a start failure that is not ErrDisabled", err)
	}
	if !strings.Contains(err.Error(), "16 characters") {
		t.Errorf("error %q does not name the length rule", err)
	}
	if strings.Contains(err.Error(), short) {
		t.Errorf("error %q echoes the key", err)
	}

	if len(testKey) != sep2adminplane.MinAdminKeyLength {
		t.Fatalf("testKey is %d characters; the boundary case needs %d", len(testKey), sep2adminplane.MinAdminKeyLength)
	}
	s, err := New(Config{Addr: "127.0.0.1:0", Key: testKey}, testSources())
	if err != nil {
		t.Fatalf("New(Key of exactly the minimum length): %v", err)
	}
	_ = s.ln.Close()
}

// TestNewRequiresEverySource confirms a missing reader is refused rather
// than left to fail on the first request.
func TestNewRequiresEverySource(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Protocol = nil
	if _, err := New(Config{Addr: "127.0.0.1:0", Key: testKey}, src); err == nil {
		t.Fatal("New with no Protocol source: error = nil, want a refusal")
	}
}

// TestNewBindsLoopbackAddrByDefault confirms a loopback Addr is accepted
// with no opt-in flag.
func TestNewBindsLoopbackAddrByDefault(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	if s.Addr() == "" {
		t.Errorf("Addr() = %q, want a bound loopback address", s.Addr())
	}
}

// TestNewRejectsNonLoopbackWithoutOptIn: binding a non-loopback host
// without AllowNonLoopback must fail closed, before any socket opens.
func TestNewRejectsNonLoopbackWithoutOptIn(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Addr: "0.0.0.0:0", Key: testKey}, testSources())
	if err == nil {
		t.Fatal("New(non-loopback Addr, AllowNonLoopback=false) error = nil, want a rejection")
	}
}

// TestNewAllowsNonLoopbackWithOptIn confirms the opt-in permits it.
func TestNewAllowsNonLoopbackWithOptIn(t *testing.T) {
	t.Parallel()

	newServer(t, Config{Addr: "0.0.0.0:0", Key: testKey, AllowNonLoopback: true}, testSources())
}

// TestNewRejectsEmptyAddr confirms Addr is required.
func TestNewRejectsEmptyAddr(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Addr: "", Key: testKey}, testSources())
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

// TestBridgeJSONRoutesAcceptTheExactToken confirms the correct Bearer
// reaches the bridge JSON routes.
func TestBridgeJSONRoutesAcceptTheExactToken(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, route := range bridgeJSONRoutes {
		assertGETWithHost(t, s.Handler(), route, "Bearer "+testKey, "localhost", http.StatusOK)
	}
}

// TestBridgeJSONRoutesRejectAMissingOrWrongToken covers a wrong token,
// no header and a token without the Bearer prefix.
func TestBridgeJSONRoutesRejectAMissingOrWrongToken(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, header := range []string{"Bearer wrong-token-0123456789", "", testKey} {
		for _, route := range bridgeJSONRoutes {
			assertGETWithHost(t, s.Handler(), route, header, "localhost", http.StatusUnauthorized)
		}
	}
}

// TestHostAllowlistAcceptsDefaultsAndConfiguredHosts: the loopback names
// are always accepted, and so is a name in Config.AllowedHosts, on the
// bridge routes and on the plane alike.
func TestHostAllowlistAcceptsDefaultsAndConfiguredHosts(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey, AllowedHosts: []string{"admin.internal.example", " "}}, testSources())
	for _, host := range []string{"localhost", "127.0.0.1", "admin.internal.example", "ADMIN.INTERNAL.EXAMPLE"} {
		assertGETWithHost(t, s.Handler(), "/api/health", "Bearer "+testKey, host, http.StatusOK)
		assertGETWithHost(t, s.Handler(), "/api/ui/panels", "Bearer "+testKey, host, http.StatusOK)
	}
}

// TestHostAllowlistRejectsUnrecognizedHost: an unknown Host is refused
// even with the right token, by the bridge routes (403) and the plane.
func TestHostAllowlistRejectsUnrecognizedHost(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	assertGETWithHost(t, s.Handler(), "/api/health", "Bearer "+testKey, "evil.example.com", http.StatusForbidden)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer "+testKey, "evil.example.com")
	if rec.Code == http.StatusOK {
		t.Errorf("plane answered 200 to an unlisted Host; body = %s", rec.Body.String())
	}
}

// TestRequireGETRejectsNonGETMethods: the bridge JSON routes stay GET
// only.
func TestRequireGETRejectsNonGETMethods(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rec := doRequest(t, s.Handler(), method, "/api/health", "Bearer "+testKey, "localhost")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/health status = %d, want 405", method, rec.Code)
		}
	}
}

// Config.Settings reaches the plane: each value the plane validates makes
// New fail when it is out of range, so a New that dropped Settings would
// start instead.
func TestNewPassesSettingsToThePlane(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		settings sep2adminplane.Settings
	}{
		{"deadline over an hour", sep2adminplane.Settings{FlowReservationDeadline: 2 * time.Hour}},
		{"grace under 15 minutes", sep2adminplane.Settings{RetentionGrace: time.Minute}},
	} {
		_, err := New(Config{Addr: "127.0.0.1:0", Key: testKey, Settings: tc.settings}, testSources())
		if err == nil {
			t.Errorf("%s: New started, want a refusal", tc.name)
		}
	}
}

func TestNewStartsWithInRangeSettings(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey, Settings: sep2adminplane.Settings{
		FlowReservationDeadline: 90 * time.Second,
		RetentionGrace:          time.Hour,
	}}, testSources())
	assertGET(t, s.Handler(), "/api/health", "Bearer "+testKey, "localhost", http.StatusOK)
}
