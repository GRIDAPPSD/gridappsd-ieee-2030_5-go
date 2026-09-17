package sep2embed

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

// TestBuildNotifierLogsWarningExactlyOnceWhenLoopbackAllowed proves the
// startup warning fires exactly once, names the switch (both spellings),
// and states the real blast radius: any loopback destination on any port,
// not the admin UI alone, matching server-go's newSubscriptionNotifier
// warning shape.
func TestBuildNotifierLogsWarningExactlyOnceWhenLoopbackAllowed(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	buildNotifier(newStores().Subscriptions, 1, 1, true)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("log output = %q, want exactly one line", buf.String())
	}
	logged := lines[0]
	if !strings.Contains(logged, "SEP2_NOTIFICATION_ALLOW_LOOPBACK") {
		t.Errorf("warning %q does not name the switch SEP2_NOTIFICATION_ALLOW_LOOPBACK", logged)
	}
	if !strings.Contains(logged, "admin UI") {
		t.Errorf("warning %q does not name the admin UI blast radius", logged)
	}
	// The corrected warning must state the permission's real width, not
	// just the admin UI: any loopback destination on any port. The pre-fix
	// wording ("loopback addresses, including this bridge's own admin UI
	// listener") never said "any port"; a revert to it must fail this.
	if !strings.Contains(logged, "any port") {
		t.Errorf("warning %q does not say the permission reaches any port, not just the admin UI", logged)
	}
	// The corrected warning must say a hostname that resolves to loopback
	// (e.g. localhost) counts as loopback too. The pre-fix wording never
	// mentioned a hostname at all.
	if !strings.Contains(logged, "localhost") {
		t.Errorf("warning %q does not say a hostname such as localhost that resolves to loopback counts", logged)
	}
	// The corrected warning must name both spellings the switch can be set
	// with (F7's answer: no plumbing exists to report which one actually
	// set it). The pre-fix wording named only the environment variable.
	if !strings.Contains(logged, "-sep2-notification-allow-loopback") {
		t.Errorf("warning %q does not name the flag spelling -sep2-notification-allow-loopback", logged)
	}
}

// TestBuildNotifierNoWarningWhenLoopbackDisallowed proves the warning is
// not emitted when the switch is off, the default state.
func TestBuildNotifierNoWarningWhenLoopbackDisallowed(t *testing.T) {
	var buf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOutput)

	buildNotifier(newStores().Subscriptions, 1, 1, false)

	if buf.Len() != 0 {
		t.Errorf("log output = %q, want none (switch is off)", buf.String())
	}
}

// TestBuildNotifierValidatesNotificationURIPerPolicy asserts the returned
// Manager's ValidateNotificationURI, the same call creation-time subscription
// handling uses, refuses loopback only when allowLoopback is false and
// accepts a private-range destination in both states. This is the unit-level
// proof behind the production-wiring tests in subscription_wiring_test.go.
func TestBuildNotifierValidatesNotificationURIPerPolicy(t *testing.T) {
	tests := []struct {
		name          string
		allowLoopback bool
		uri           string
		wantErr       bool
	}{
		{"loopback refused when off", false, "http://127.0.0.1:9/notify", true},
		{"loopback accepted when on", true, "http://127.0.0.1:9/notify", false},
		{"private-range accepted when off", false, "http://10.0.0.5/notify", false},
		{"private-range accepted when on", true, "http://10.0.0.5/notify", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := buildNotifier(newStores().Subscriptions, 1, 1, tt.allowLoopback)
			err := m.ValidateNotificationURI(context.Background(), tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateNotificationURI(%q) with allowLoopback=%v: err = %v, wantErr %v",
					tt.uri, tt.allowLoopback, err, tt.wantErr)
			}
		})
	}
}
