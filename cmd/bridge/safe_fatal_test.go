package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestRedactCredsStripsPassword is the data-invariants required VALUE
// assertion for GAGO-028 Leon L3: a message that contains the
// configured STOMPPassword verbatim must have every occurrence
// replaced with the fixed placeholder, and the placeholder must not
// leak any fragment of the original secret.
func TestRedactCredsStripsPassword(t *testing.T) {
	t.Parallel()

	cfg := config{STOMPPassword: "s3cr3t-manager"}
	msg := "connect 127.0.0.1:61613: dial failed: password s3cr3t-manager rejected"

	got := redactCreds(cfg, msg)

	if strings.Contains(got, "s3cr3t-manager") {
		t.Fatalf("redactCreds left the password in the message: %q", got)
	}
	want := "connect 127.0.0.1:61613: dial failed: password [REDACTED] rejected"
	if got != want {
		t.Errorf("redactCreds: got %q, want %q", got, want)
	}
}

// TestRedactCredsStripsAdminUIKey mirrors the password case for the
// admin UI Bearer token field.
func TestRedactCredsStripsAdminUIKey(t *testing.T) {
	t.Parallel()

	cfg := config{SEP2AdminUIKey: "topsecret-token"}
	msg := "admin ui: bearer topsecret-token rejected by upstream"

	got := redactCreds(cfg, msg)

	if strings.Contains(got, "topsecret-token") {
		t.Fatalf("redactCreds left the admin UI key in the message: %q", got)
	}
	want := "admin ui: bearer [REDACTED] rejected by upstream"
	if got != want {
		t.Errorf("redactCreds: got %q, want %q", got, want)
	}
}

// TestRedactCredsStripsBoth verifies both credential fields are
// redacted independently when a single message happens to carry both.
func TestRedactCredsStripsBoth(t *testing.T) {
	t.Parallel()

	cfg := config{STOMPPassword: "pw123", SEP2AdminUIKey: "key456"}
	msg := "pw123 and key456 both appear here"

	got := redactCreds(cfg, msg)

	if strings.Contains(got, "pw123") || strings.Contains(got, "key456") {
		t.Fatalf("redactCreds left a credential in the message: %q", got)
	}
	want := "[REDACTED] and [REDACTED] both appear here"
	if got != want {
		t.Errorf("redactCreds: got %q, want %q", got, want)
	}
}

// TestRedactCredsEmptyFieldsNoOp verifies the empty-field guard: a
// zero-value config (no password, no admin UI key set) must return
// msg completely unchanged. Without the empty-string guard,
// strings.ReplaceAll(msg, "", "[REDACTED]") would insert the
// placeholder between every rune in msg, corrupting any message when
// neither credential field happens to be set (e.g. the -h / -version
// early-exit paths, and any run where the admin UI is disabled).
func TestRedactCredsEmptyFieldsNoOp(t *testing.T) {
	t.Parallel()

	cfg := config{}
	msg := "flag parse error: unknown flag -bogus"

	got := redactCreds(cfg, msg)

	if got != msg {
		t.Errorf("redactCreds with empty credential fields: got %q, want unchanged %q", got, msg)
	}
}

// TestRedactCredsNoMatchNoOp verifies a message containing neither
// credential substring is returned unchanged, not just "not
// corrupted": this guards against a redaction pass leaving stray
// placeholder text in a message that never carried the secret.
func TestRedactCredsNoMatchNoOp(t *testing.T) {
	t.Parallel()

	cfg := config{STOMPPassword: "pw123", SEP2AdminUIKey: "key456"}
	msg := "connect 127.0.0.1:61613: context deadline exceeded"

	got := redactCreds(cfg, msg)

	if got != msg {
		t.Errorf("redactCreds with no credential match: got %q, want unchanged %q", got, msg)
	}
}

// TestRedactCredsStripsBase64AuthBlob is the GAGO-028 follow-up VALUE
// assertion: the GOSS auth-token bootstrap sends
// base64(STOMPUser:STOMPPassword) over the wire (internal/cimstomp's
// fetchAuthToken), a form that shares no substring with the raw
// password. Before this fix, redactCreds only matched the raw password
// and admin UI key verbatim, so this encoded form passed through
// untouched even though the doc comment claimed coverage of "any
// credential-shaped substring". This asserts the encoded blob is now
// also replaced.
func TestRedactCredsStripsBase64AuthBlob(t *testing.T) {
	t.Parallel()

	cfg := config{STOMPUser: "system", STOMPPassword: "manager"}
	authBlob := base64.StdEncoding.EncodeToString([]byte("system:manager"))
	msg := "connect 127.0.0.1:61613: dial failed: auth " + authBlob + " rejected"

	got := redactCreds(cfg, msg)

	if strings.Contains(got, authBlob) {
		t.Fatalf("redactCreds left the base64 auth blob in the message: %q", got)
	}
	want := "connect 127.0.0.1:61613: dial failed: auth [REDACTED] rejected"
	if got != want {
		t.Errorf("redactCreds: got %q, want %q", got, want)
	}
}

// TestRedactCredsNoUserNoBase64Redaction verifies the STOMPUser-empty
// guard: without a configured user, redactCreds must not attempt the
// base64(user:password) redaction (it has no real user to pair the
// password with), so a message carrying only the raw password is still
// redacted for that raw occurrence, but a base64 blob computed from a
// nonsensical ":password" pairing is left untouched rather than
// spuriously matched.
func TestRedactCredsNoUserNoBase64Redaction(t *testing.T) {
	t.Parallel()

	cfg := config{STOMPPassword: "manager"}
	nonsenseBlob := base64.StdEncoding.EncodeToString([]byte(":manager"))
	msg := "connect 127.0.0.1:61613: dial failed: auth " + nonsenseBlob + " rejected"

	got := redactCreds(cfg, msg)

	if !strings.Contains(got, nonsenseBlob) {
		t.Fatalf("redactCreds unexpectedly redacted a :password blob despite no configured STOMPUser: %q", got)
	}
	if got != msg {
		t.Errorf("redactCreds with no STOMPUser: got %q, want unchanged %q", got, msg)
	}
}
