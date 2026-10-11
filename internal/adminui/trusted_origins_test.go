package adminui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2adminplane"
)

// TestTrustedOriginsReachTheEmbeddedPlane: SEP2_ADMIN_TRUSTED_ORIGINS parsed by
// SettingsFromEnv must arrive in the plane's Config, or an admin browser on a
// listed origin stays refused as cross-origin.
func TestTrustedOriginsReachTheEmbeddedPlane(t *testing.T) {
	t.Parallel()

	const trusted = "http://console.example.com"
	cases := []struct {
		name    string
		env     map[string]string
		origin  string
		refused bool
	}{
		{"unset refuses a cross-site POST", nil, trusted, true},
		{"a listed origin passes", map[string]string{"SEP2_ADMIN_TRUSTED_ORIGINS": trusted}, trusted, false},
		{"an unlisted origin is refused", map[string]string{"SEP2_ADMIN_TRUSTED_ORIGINS": trusted}, "http://attacker.example", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings, err := sep2adminplane.SettingsFromEnv(func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatalf("SettingsFromEnv: %v", err)
			}
			src := testSources()
			src.Sender = newTestSender(t, &recordingBus{}, false)
			s := newServer(t, Config{InsecureNoKey: true, Settings: settings}, src)

			rec := postNoAuth(s.Handler(), switchPanelID, "publishing", `{"on":true}`,
				map[string]string{"Origin": tc.origin, "Sec-Fetch-Site": "cross-site"})
			if tc.refused {
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin") {
					t.Fatalf("cross-site POST = %d %s, want 403 cross-origin refusal", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Publishing is ON.") {
				t.Fatalf("cross-site POST from a trusted origin = %d %s, want 200 Publishing is ON.", rec.Code, rec.Body.String())
			}
		})
	}
}
