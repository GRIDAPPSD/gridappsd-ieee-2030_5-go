package main

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"
)

// TestLoadConfigAdminUINoKey pins the five rows of the key and no-key
// table, the flag over env precedence, and a non-boolean refused by name.
func TestLoadConfigAdminUINoKey(t *testing.T) {
	const both = "SEP2_ADMIN_UI_INSECURE_NO_KEY and SEP2_ADMIN_UI_KEY are both set; unset one"
	tests := []struct {
		name    string
		env     string
		key     string
		args    []string
		want    bool
		wantErr string
	}{
		{"off, blank key: disabled state", "", "", nil, false, ""},
		{"off, key set: keyed", "false", "a-sixteen-char-key", nil, false, ""},
		{"on, blank key: no-key mode", "true", "", nil, true, ""},
		{"on, key set: refused", "true", "a-sixteen-char-key", nil, false, both},
		{"on by flag, key set: refused", "", "a-sixteen-char-key", []string{"-admin-ui-insecure-no-key=true"}, false, both},
		{"flag on over env false", "false", "", []string{"-admin-ui-insecure-no-key=true"}, true, ""},
		{"flag off over env true", "true", "a-sixteen-char-key", []string{"-admin-ui-insecure-no-key=false"}, false, ""},
		{"not a boolean", "yesplease", "", nil, false, "SEP2_ADMIN_UI_INSECURE_NO_KEY"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEP2_ADMIN_UI_INSECURE_NO_KEY", tc.env)
			t.Setenv("SEP2_ADMIN_UI_KEY", tc.key)
			cfg, err := loadConfig(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadConfig error = %v, want it to contain %q", err, tc.wantErr)
				}
				if tc.key != "" && strings.Contains(err.Error(), tc.key) {
					t.Errorf("error %q echoes the key", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.SEP2AdminUIInsecureNoKey != tc.want {
				t.Errorf("SEP2AdminUIInsecureNoKey = %v, want %v", cfg.SEP2AdminUIInsecureNoKey, tc.want)
			}
			if cfg.SEP2AdminUIKey != tc.key {
				t.Errorf("SEP2AdminUIKey = %q, want %q", cfg.SEP2AdminUIKey, tc.key)
			}
		})
	}
}

// TestLoadConfigAdminUINoKeyHelp pins the flag's help text and its default.
func TestLoadConfigAdminUINoKeyHelp(t *testing.T) {
	t.Setenv("SEP2_ADMIN_UI_INSECURE_NO_KEY", "")
	usage := loadConfigUsage(t)
	for _, want := range []string{
		"-admin-ui-insecure-no-key",
		"INSECURE: run the admin UI with no key for the operator; anyone who can reach the address gets full access (default false; env: SEP2_ADMIN_UI_INSECURE_NO_KEY)",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage lacks %q:\n%s", want, usage)
		}
	}
}

// TestAdminUIConfigMapsInsecureNoKey: the field reaches adminui.Config.
func TestAdminUIConfigMapsInsecureNoKey(t *testing.T) {
	t.Parallel()
	if !adminUIConfig(config{SEP2AdminUIInsecureNoKey: true}).InsecureNoKey {
		t.Error("InsecureNoKey: got false, want true")
	}
	if adminUIConfig(config{}).InsecureNoKey {
		t.Error("InsecureNoKey: got true for a zero config, want false")
	}
}

// TestNoKeyLogFilterDropsOnlyAuthSuccess: with the filter installed an
// admin_auth_success record is dropped and admin_auth_failure and others
// are kept; the control is the filter off, where the success line appears.
// log.Printf keeps its own format and destination.
func TestNoKeyLogFilterDropsOnlyAuthSuccess(t *testing.T) {
	var buf bytes.Buffer
	prevSlog := slog.Default()
	prevW, prevF := log.Writer(), log.Flags()
	t.Cleanup(func() { slog.SetDefault(prevSlog); log.SetOutput(prevW); log.SetFlags(prevF) })

	emit := func() {
		slog.Info("admin auth", "event", "admin_auth_success", "path", "/x")
		slog.Warn("admin auth", "event", "admin_auth_failure", "path", "/y")
		slog.Info("other", "event", "something_else")
		slog.Info("no event attr")
		slog.With("event", "admin_auth_success").Info("via With")
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	emit()
	if got := strings.Count(buf.String(), "admin_auth_success"); got != 2 {
		t.Fatalf("control: unfiltered log has %d success lines, want 2", got)
	}

	buf.Reset()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	log.SetOutput(&buf)
	log.SetFlags(0)
	restore := installNoKeyLogFilter()
	t.Cleanup(restore)
	emit()
	log.Printf("plain line")
	out := buf.String()
	if strings.Contains(out, "admin_auth_success") {
		t.Errorf("filtered log still holds a success record:\n%s", out)
	}
	for _, want := range []string{"admin_auth_failure", "something_else", "no event attr"} {
		if !strings.Contains(out, want) {
			t.Errorf("filtered log lost %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\nplain line\n") {
		t.Errorf("log.Printf format changed:\n%s", out)
	}
}
