package adminui

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noKeyServer builds a Server with a sender and the no-key switch set as given.
func noKeyServer(t *testing.T, insecure bool) *Server {
	t.Helper()
	src := testSources()
	src.Sender = newTestSender(t, &recordingBus{}, false)
	return newServer(t, Config{InsecureNoKey: insecure}, src)
}

// postNoAuth posts a sender action with no Authorization header, from the
// loopback address and Host doRequest uses.
func postNoAuth(h http.Handler, panel, action, body string, extra map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/ui/panels/"+panel+"/actions/"+action, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNoKeyAdmitsRequestsWithoutACredential: with the flag on, a request
// with no Authorization reaches the shell, the panel manifest, the bridge
// JSON routes and a same-origin sender action. The control is the flag off,
// where the same requests are refused.
func TestNoKeyAdmitsRequestsWithoutACredential(t *testing.T) {
	t.Parallel()

	paths := []string{"/ui/", "/api/ui/panels", "/api/health", "/api/clients"}
	on := noKeyServer(t, true)
	for _, p := range paths {
		if rec := doRequest(t, on.Handler(), http.MethodGet, p, "", "localhost"); rec.Code != http.StatusOK {
			t.Errorf("no-key on: GET %s = %d, want 200; body %s", p, rec.Code, rec.Body.String())
		}
	}
	rec := postNoAuth(on.Handler(), panelSender, "raw", `{"json":"{}"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "publishing is off") {
		t.Errorf("no-key on: POST raw = %d %s, want 422 publishing is off", rec.Code, rec.Body.String())
	}
	rec = postNoAuth(on.Handler(), switchPanelID, "publishing", `{"on":true}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Publishing is ON.") {
		t.Errorf("no-key on: POST publishing = %d %s, want 200 Publishing is ON.", rec.Code, rec.Body.String())
	}
}

// TestKeyedAdminStillRefusesWithoutACredential is the flag-off control: the
// behavior is the keyed one, so no credential means refused.
func TestKeyedAdminStillRefusesWithoutACredential(t *testing.T) {
	t.Parallel()

	src := testSources()
	src.Sender = newTestSender(t, &recordingBus{}, false)
	off := newServer(t, Config{Key: testKey}, src)
	for _, p := range []string{"/ui/", "/api/ui/panels", "/api/health", "/api/clients"} {
		if rec := doRequest(t, off.Handler(), http.MethodGet, p, "", "localhost"); rec.Code != http.StatusUnauthorized {
			t.Errorf("flag off: GET %s = %d, want 401", p, rec.Code)
		}
	}
	if rec := postNoAuth(off.Handler(), switchPanelID, "publishing", `{"on":true}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("flag off: POST publishing = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
}

// TestNoKeyKeepsHostAndCrossOriginRefusals: the Host allowlist and the
// plane's cross-origin refusal are the remaining defences and stay on.
func TestNoKeyKeepsHostAndCrossOriginRefusals(t *testing.T) {
	t.Parallel()

	s := noKeyServer(t, true)
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/health", "", "evil.example.com"); rec.Code != http.StatusForbidden {
		t.Errorf("unlisted Host on a bridge route = %d, want 403", rec.Code)
	}
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "", "evil.example.com"); rec.Code == http.StatusOK {
		t.Errorf("unlisted Host on a plane route = %d, want a refusal", rec.Code)
	}
	rec := postNoAuth(s.Handler(), switchPanelID, "publishing", `{"on":true}`, map[string]string{"Origin": "http://evil.example.com"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin") {
		t.Errorf("cross-origin POST = %d %s, want 403 cross-origin refusal", rec.Code, rec.Body.String())
	}
	rec = postNoAuth(s.Handler(), switchPanelID, "publishing", `{"on":true}`, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site POST = %d, want 403", rec.Code)
	}
}

// TestNoKeyOverwritesAClientSentCredential: a wrong Bearer from the client
// is replaced, so it admits and logs no failure.
func TestNoKeyOverwritesAClientSentCredential(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := noKeyServer(t, true)
	rec := doRequest(t, s.Handler(), http.MethodGet, "/api/ui/panels", "Bearer not-the-key-at-all", "localhost")
	if rec.Code != http.StatusOK {
		t.Errorf("wrong client Bearer = %d, want 200", rec.Code)
	}
	if strings.Contains(buf.String(), "admin_auth_failure") {
		t.Errorf("a failure was logged for a request the bridge re-authenticated: %s", buf.String())
	}
}

// TestNoKeyNeverExposesTheGeneratedKey: the key appears in no log line and
// no response body.
func TestNoKeyNeverExposesTheGeneratedKey(t *testing.T) {
	var logs bytes.Buffer
	prevSlog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	prevLog, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	t.Cleanup(func() { slog.SetDefault(prevSlog); log.SetOutput(prevLog); log.SetFlags(prevFlags) })

	s := noKeyServer(t, true)
	key := s.cfg.Key
	if len(key) < 32 {
		t.Fatalf("generated key is %d characters, want at least 32", len(key))
	}
	var bodies strings.Builder
	for _, p := range []string{"/ui/", "/api/ui/panels", "/api/health", "/api/clients", "/api/ui/panels/" + panelBusMonitor} {
		rec := doRequest(t, s.Handler(), http.MethodGet, p, "", "localhost")
		bodies.WriteString(rec.Body.String())
		for k, v := range rec.Header() {
			bodies.WriteString(k + strings.Join(v, ","))
		}
	}
	if strings.Contains(bodies.String(), key) {
		t.Error("the generated key appears in a response")
	}
	if strings.Contains(logs.String(), key) {
		t.Errorf("the generated key appears in a log line: %s", logs.String())
	}
	other := noKeyServer(t, true)
	if other.cfg.Key == key {
		t.Error("two servers generated the same key")
	}
}

// TestNoKeyRefusesWhenAKeyIsAlsoSet: both set is a start failure that is not
// ErrDisabled and does not echo the key.
func TestNoKeyRefusesWhenAKeyIsAlsoSet(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Addr: "127.0.0.1:0", Key: testKey, InsecureNoKey: true}, testSources())
	if err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("New(key and no-key) error = %v, want a start failure that is not ErrDisabled", err)
	}
	want := "SEP2_ADMIN_UI_INSECURE_NO_KEY and SEP2_ADMIN_UI_KEY are both set; unset one"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not say %q", err, want)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error %q echoes the key", err)
	}
}

// TestNoKeyStillRequiresTheOtherConfig: the flag removes the key
// requirement only; Addr and the non-loopback opt-in still apply.
func TestNoKeyStillRequiresTheOtherConfig(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{InsecureNoKey: true}, testSources()); err == nil {
		t.Error("New(no Addr) error = nil, want a refusal")
	}
	if _, err := New(Config{Addr: "0.0.0.0:0", InsecureNoKey: true}, testSources()); err == nil {
		t.Error("New(non-loopback without opt-in) error = nil, want a refusal")
	}
}

// TestNoKeyWarnsOnceAfterBind: the warning names the bound address and
// every accepted Host, and a wildcard bind reads as all interfaces.
func TestNoKeyWarnsOnceAfterBind(t *testing.T) {
	var logs bytes.Buffer
	prevLog, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevLog); log.SetFlags(prevFlags) })

	newServer(t, Config{Addr: "0.0.0.0:0", AllowNonLoopback: true, AllowedHosts: []string{"10.213.168.2"}, InsecureNoKey: true}, testSources())
	out := logs.String()
	for _, want := range []string{
		"WITHOUT A KEY (SEP2_ADMIN_UI_INSECURE_NO_KEY=true)",
		"all interfaces",
		"localhost, 127.0.0.1, ::1, 10.213.168.2",
		"anyone who can reach this address can read every panel and, when publishing is switched on, send control messages",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "WITHOUT A KEY"); n != 1 {
		t.Errorf("warning logged %d times, want 1", n)
	}

	logs.Reset()
	newServer(t, Config{Key: testKey}, testSources())
	if strings.Contains(logs.String(), "WITHOUT A KEY") {
		t.Error("keyed mode logged the no-key warning")
	}
}

// TestNoKeyTreatsAWhitespaceKeyAsBlank: a key of spaces is not a key, so
// no-key mode starts with it and the server still holds its own key.
func TestNoKeyTreatsAWhitespaceKeyAsBlank(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: "   ", InsecureNoKey: true}, testSources())
	if strings.TrimSpace(s.cfg.Key) == "" || s.cfg.Key == "   " {
		t.Errorf("server key %q was not replaced by a generated one", s.cfg.Key)
	}
	if rec := doRequest(t, s.Handler(), http.MethodGet, "/api/health", "", "localhost"); rec.Code != http.StatusOK {
		t.Errorf("GET /api/health = %d, want 200", rec.Code)
	}
}
