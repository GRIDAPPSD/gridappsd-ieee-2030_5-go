package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearPINEnv blanks both PIN variables so a test starts from "unset"
// regardless of the host environment.
func clearPINEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SEP2_REGISTRATION_PIN", "")
	t.Setenv("SEP2_REGISTRATION_PIN_FILE", "")
}

// TestRegistrationPINEnvReachesPolicy checks the env value arrives at the
// server policy, which is what the registration handler reads, and not
// only the config struct.
func TestRegistrationPINEnvReachesPolicy(t *testing.T) {
	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN", "123455")

	policy, err := buildSEP2Policy(mustLoadConfig(t, nil))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	if policy.DefaultRegistrationPIN == nil || *policy.DefaultRegistrationPIN != 123455 {
		t.Errorf("DefaultRegistrationPIN: got %v, want 123455", policy.DefaultRegistrationPIN)
	}
	if got, ok := policy.ResolveRegistrationPIN("ANY-LFDI"); !ok || got != 123455 {
		t.Errorf("ResolveRegistrationPIN: got (%d, %v), want (123455, true)", got, ok)
	}
}

// TestRegistrationPINFileEnvReachesPolicy is the same check for the
// per-device file named by SEP2_REGISTRATION_PIN_FILE.
func TestRegistrationPINFileEnvReachesPolicy(t *testing.T) {
	clearPINEnv(t)
	path := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(path, []byte(`{"DEV-A":123455,"DEV-B":234560}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("SEP2_REGISTRATION_PIN_FILE", path)

	policy, err := buildSEP2Policy(mustLoadConfig(t, nil))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	for lfdi, want := range map[string]uint32{"DEV-A": 123455, "DEV-B": 234560} {
		if got, ok := policy.ResolveRegistrationPIN(lfdi); !ok || got != want {
			t.Errorf("ResolveRegistrationPIN(%s): got (%d, %v), want (%d, true)", lfdi, got, ok, want)
		}
	}
}

// TestRegistrationPINFlagBeatsEnv sets both sources to different valid
// PINs and expects the flag's value.
func TestRegistrationPINFlagBeatsEnv(t *testing.T) {
	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN", "234560")

	policy, err := buildSEP2Policy(mustLoadConfig(t, []string{"-sep2-registration-pin=123455"}))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	if policy.DefaultRegistrationPIN == nil || *policy.DefaultRegistrationPIN != 123455 {
		t.Errorf("DefaultRegistrationPIN: got %v, want the flag's 123455", policy.DefaultRegistrationPIN)
	}
}

// TestRegistrationPINFileFlagBeatsEnv checks the same precedence for the
// file: the env names a missing file, so using it would fail the load.
func TestRegistrationPINFileFlagBeatsEnv(t *testing.T) {
	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN_FILE", filepath.Join(t.TempDir(), "missing.json"))
	path := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(path, []byte(`{"DEV-A":123455}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := mustLoadConfig(t, []string{"-sep2-registration-pin-file=" + path})
	if got := cfg.SEP2RegistrationPINs["DEV-A"]; got != 123455 {
		t.Errorf("SEP2RegistrationPINs[DEV-A]: got %d, want 123455 from the flag's file", got)
	}
}

// TestRegistrationPINEnvInvalidNamesVariableNotValue covers a malformed
// env PIN and a bad env file path: the error names the variable and the
// PIN text never appears in it.
func TestRegistrationPINEnvInvalidNamesVariableNotValue(t *testing.T) {
	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN", "s3cret-pin")

	_, err := loadConfig(nil)
	if err == nil {
		t.Fatal("loadConfig accepted a non-numeric SEP2_REGISTRATION_PIN, want an error")
	}
	if !strings.Contains(err.Error(), "SEP2_REGISTRATION_PIN") {
		t.Errorf("error should name the variable: %v", err)
	}
	if strings.Contains(err.Error(), "s3cret-pin") {
		t.Errorf("error must not echo the PIN value: %v", err)
	}

	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN_FILE", filepath.Join(t.TempDir(), "missing.json"))
	_, err = loadConfig(nil)
	if err == nil || !strings.Contains(err.Error(), "SEP2_REGISTRATION_PIN_FILE") {
		t.Errorf("a missing PIN file should fail naming SEP2_REGISTRATION_PIN_FILE: %v", err)
	}
}

// TestRegistrationPINEnvBadCheckDigitRefused confirms the same domain
// validation as the flag applies to the env value and does not echo it.
func TestRegistrationPINEnvBadCheckDigitRefused(t *testing.T) {
	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN", "999999")

	_, err := buildSEP2Policy(mustLoadConfig(t, nil))
	if err == nil {
		t.Fatal("buildSEP2Policy accepted a PIN failing the check digit, want an error")
	}
	if strings.Contains(err.Error(), "999999") {
		t.Errorf("error must not echo the PIN value: %v", err)
	}
}

// TestRegistrationPINEnvUnsetLeavesNil expects no PIN anywhere when both
// variables are empty.
func TestRegistrationPINEnvUnsetLeavesNil(t *testing.T) {
	clearPINEnv(t)

	cfg := mustLoadConfig(t, nil)
	if cfg.SEP2RegistrationPIN != nil {
		t.Errorf("SEP2RegistrationPIN: got %d, want nil", *cfg.SEP2RegistrationPIN)
	}
	if cfg.SEP2RegistrationPINs != nil {
		t.Errorf("SEP2RegistrationPINs: got %v, want nil", cfg.SEP2RegistrationPINs)
	}
}

// TestRegistrationPINEnvScrubbed checks the PIN is removed from the
// process environment after load, like the other credentials.
func TestRegistrationPINEnvScrubbed(t *testing.T) {
	clearPINEnv(t)
	t.Setenv("SEP2_REGISTRATION_PIN", "123455")

	mustLoadConfig(t, nil)
	if v := os.Getenv("SEP2_REGISTRATION_PIN"); v != "" {
		t.Errorf("SEP2_REGISTRATION_PIN still in the environment after load (len %d)", len(v))
	}
}
