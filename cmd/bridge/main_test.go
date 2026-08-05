package main

import (
	"context"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// TestBusConfigMapsFields verifies busConfig's field-by-field mapping
// from the bridge's own config onto gridappsd.Config, in particular
// that AllowPlaintext passes through unmodified in both directions
// rather than being silently forced to a fixed value in the wiring
// step. This is what stands in for a live-broker connect test at this
// layer; the end-to-end proof against a real broker is a separate job.
func TestBusConfigMapsFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  config
	}{
		{
			name: "plaintext left at the fail-closed default",
			cfg: config{
				STOMPAddr:      "127.0.0.1:61613",
				STOMPUser:      "system",
				STOMPPassword:  "manager",
				AllowPlaintext: false,
			},
		},
		{
			name: "plaintext explicitly opted in",
			cfg: config{
				STOMPAddr:      "broker.example:61613",
				STOMPUser:      "u",
				STOMPPassword:  "p",
				AllowPlaintext: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := busConfig(tc.cfg)
			if got.Address != tc.cfg.STOMPAddr {
				t.Errorf("Address: got %q, want %q", got.Address, tc.cfg.STOMPAddr)
			}
			if got.User != tc.cfg.STOMPUser {
				t.Errorf("User: got %q, want %q", got.User, tc.cfg.STOMPUser)
			}
			if got.Password != tc.cfg.STOMPPassword {
				t.Errorf("Password: got %q, want %q", got.Password, tc.cfg.STOMPPassword)
			}
			if got.AllowPlaintext != tc.cfg.AllowPlaintext {
				t.Errorf("AllowPlaintext: got %v, want %v", got.AllowPlaintext, tc.cfg.AllowPlaintext)
			}
			if got.TLSConfig != nil {
				t.Errorf("TLSConfig: got %v, want nil (bridge does not yet supply a custom TLS config)", got.TLSConfig)
			}
		})
	}
}

// TestBusConfigDefaultIsFailClosed verifies that a zero-value config
// (as loadConfig produces when no plaintext override is set) maps to
// a gridappsd.Config that dials TLS, not plaintext. This is the
// wiring-layer half of the fail-closed invariant; loadConfig's own
// default-off behavior is covered in config_test.go.
func TestBusConfigDefaultIsFailClosed(t *testing.T) {
	t.Parallel()

	got := busConfig(config{})
	if got.AllowPlaintext {
		t.Errorf("AllowPlaintext: got true for a zero-value config, want false (fail-closed default)")
	}
	if got.TLSConfig != nil {
		t.Errorf("TLSConfig: got %v, want nil", got.TLSConfig)
	}
}

// TestSEP2EmbedConfigMapsFields verifies sep2EmbedConfig's field-by-field
// mapping from the bridge's own config onto sep2embed.Config, mirroring
// TestBusConfigMapsFields for the STOMP side. This is the pure-mapping
// unit test that stands in for exercising newSEP2Embed's cert minting
// and listener bind at this layer; the boot-over-mTLS proof lives in
// sep2embed_wiring_test.go.
func TestSEP2EmbedConfigMapsFields(t *testing.T) {
	t.Parallel()

	cfg := config{
		SEP2ServerAddr:    "127.0.0.1:8443",
		SEP2ServerCertDir: "/var/lib/bridge/sep2-certs",
	}

	policy := sep2config.DefaultPolicy()
	var connHook connobs.Hook
	got := sep2EmbedConfig(cfg, policy, &connHook, sep2embed.DeviceCertModeDevMint)
	if got.Addr != cfg.SEP2ServerAddr {
		t.Errorf("Addr: got %q, want %q", got.Addr, cfg.SEP2ServerAddr)
	}
	if got.Observer != &connHook {
		t.Errorf("Observer: got %p, want the passed-in connHook %p", got.Observer, &connHook)
	}
	if got.CertDir != cfg.SEP2ServerCertDir {
		t.Errorf("CertDir: got %q, want %q", got.CertDir, cfg.SEP2ServerCertDir)
	}

	// DefaultControl passes through from policy verbatim,
	// never hardcoded at the sep2EmbedConfig mapping layer. The shipped
	// policy commands nothing, so verbatim means both mode flags arrive
	// nil; a non-nil value here would mean this layer invented one.
	base := got.DefaultControl.DERControlBase
	if base == nil {
		t.Fatalf("DefaultControl.DERControlBase = nil, want the policy's present but empty base")
	}
	if base.OpModConnect != nil {
		t.Errorf("DefaultControl.DERControlBase.OpModConnect = %v, want nil (the policy commands nothing)", *base.OpModConnect)
	}
	if base.OpModEnergize != nil {
		t.Errorf("DefaultControl.DERControlBase.OpModEnergize = %v, want nil (the policy commands nothing)", *base.OpModEnergize)
	}

	// The issued-control interval policy maps through too. A zero
	// Duration here would mean the mapping layer dropped the field, and
	// ApplyControlDelta would then refuse every delta the bridge received.
	if got.DERControl.Duration != policy.DERControl.Duration {
		t.Errorf("DERControl.Duration: got %d, want %d (the policy value)", got.DERControl.Duration, policy.DERControl.Duration)
	}
	if got.DERControl.Duration == 0 {
		t.Error("DERControl.Duration = 0: every issued DERControl would carry a zero-length interval and expire on arrival")
	}
	if got.DERControl.RandomizeDuration != policy.DERControl.RandomizeDuration {
		t.Errorf("DERControl.RandomizeDuration: got %d, want %d (the policy value)",
			got.DERControl.RandomizeDuration, policy.DERControl.RandomizeDuration)
	}
	// Nil so the bridge reads the real clock. A non-nil clock reaching
	// production would freeze creationTime and every interval start.
	if got.DERControl.Now != nil {
		t.Error("DERControl.Now is non-nil; the bridge must read the real clock, the seam exists only for tests")
	}
}

// TestAdminUIConfigMapsFields verifies adminUIConfig's field-by-field
// mapping from the bridge's own config onto adminui.Config, mirroring
// TestSEP2EmbedConfigMapsFields for the embedded IEEE 2030.5 side. This
// is the pure-mapping unit test for the admin UI config: no listener
// bound, no admin token required.
func TestAdminUIConfigMapsFields(t *testing.T) {
	t.Parallel()

	cfg := config{
		SEP2AdminUIAddr:             "127.0.0.1:8444",
		SEP2AdminUIAllowNonLoopback: true,
		SEP2AdminUIKey:              "secret-token",
		SEP2AdminUIAllowedHosts:     []string{"admin.internal.example"},
		FeederMRID:                  "feeder-mrid-1",
		SimulationID:                "sim-1",
		SEP2AdminUISORLink:          "https://sor.example/dashboard",
	}

	got := adminUIConfig(cfg)
	if got.Addr != cfg.SEP2AdminUIAddr {
		t.Errorf("Addr: got %q, want %q", got.Addr, cfg.SEP2AdminUIAddr)
	}
	if got.AllowNonLoopback != cfg.SEP2AdminUIAllowNonLoopback {
		t.Errorf("AllowNonLoopback: got %v, want %v", got.AllowNonLoopback, cfg.SEP2AdminUIAllowNonLoopback)
	}
	if got.Key != cfg.SEP2AdminUIKey {
		t.Errorf("Key: got %q, want %q", got.Key, cfg.SEP2AdminUIKey)
	}
	if len(got.AllowedHosts) != 1 || got.AllowedHosts[0] != "admin.internal.example" {
		t.Errorf("AllowedHosts: got %v, want [admin.internal.example]", got.AllowedHosts)
	}
	if got.FeederMRID != cfg.FeederMRID {
		t.Errorf("FeederMRID: got %q, want %q", got.FeederMRID, cfg.FeederMRID)
	}
	if got.SimulationID != cfg.SimulationID {
		t.Errorf("SimulationID: got %q, want %q", got.SimulationID, cfg.SimulationID)
	}
	if got.SORLink != cfg.SEP2AdminUISORLink {
		t.Errorf("SORLink: got %q, want %q", got.SORLink, cfg.SEP2AdminUISORLink)
	}
}

// TestAdminUIConfigZeroValueMapsToDisabledShape confirms a zero-value
// config (the "admin UI disabled" state loadConfig produces when
// SEP2_ADMIN_UI_KEY is unset) projects to an empty Key, matching
// adminui.New's ErrDisabled contract at the next layer down.
func TestAdminUIConfigZeroValueMapsToDisabledShape(t *testing.T) {
	t.Parallel()

	got := adminUIConfig(config{})
	if got.Key != "" {
		t.Errorf("Key: got %q, want empty for a zero-value config", got.Key)
	}
	if got.AllowNonLoopback {
		t.Errorf("AllowNonLoopback: got true, want false for a zero-value config")
	}
	if got.AllowedHosts != nil {
		t.Errorf("AllowedHosts: got %v, want nil for a zero-value config", got.AllowedHosts)
	}
	if got.SORLink != "" {
		t.Errorf("SORLink: got %q, want empty for a zero-value config", got.SORLink)
	}
}

// TestBuildSEP2PolicyNeitherFlagSetMatchesDefaultPolicy verifies the
// no-op contract at the policy layer: a zero-value config (no
// -sep2-registration-pin, no -sep2-registration-pin-file) produces a
// policy whose registration-PIN fields are exactly
// sep2config.DefaultPolicy()'s own unset state, so the fail-closed
// seeding behavior for an unconfigured device is unchanged.
func TestBuildSEP2PolicyNeitherFlagSetMatchesDefaultPolicy(t *testing.T) {
	t.Parallel()

	policy, err := buildSEP2Policy(config{})
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	want := sep2config.DefaultPolicy()
	if policy.DefaultRegistrationPIN != want.DefaultRegistrationPIN {
		t.Errorf("DefaultRegistrationPIN: got %v, want %v (DefaultPolicy's own nil)", policy.DefaultRegistrationPIN, want.DefaultRegistrationPIN)
	}
	if policy.RegistrationPINs != nil {
		t.Errorf("RegistrationPINs: got %v, want nil (DefaultPolicy's own unset state)", policy.RegistrationPINs)
	}
	if _, ok := policy.ResolveRegistrationPIN("ANY-LFDI"); ok {
		t.Errorf("ResolveRegistrationPIN: got ok=true for an unconfigured device, want ok=false (fail closed)")
	}
}

// TestBuildSEP2PolicyPopulatesFleetDefault verifies
// cfg.SEP2RegistrationPIN becomes policy.DefaultRegistrationPIN, and
// that a device with no per-device entry resolves to it.
func TestBuildSEP2PolicyPopulatesFleetDefault(t *testing.T) {
	t.Parallel()

	pin := uint32(123455)
	policy, err := buildSEP2Policy(config{SEP2RegistrationPIN: &pin})
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	if policy.DefaultRegistrationPIN == nil || *policy.DefaultRegistrationPIN != 123455 {
		t.Errorf("DefaultRegistrationPIN: got %v, want 123455", policy.DefaultRegistrationPIN)
	}
	got, ok := policy.ResolveRegistrationPIN("SOME-DEVICE-LFDI")
	if !ok || got != 123455 {
		t.Errorf("ResolveRegistrationPIN: got (%d, %v), want (123455, true)", got, ok)
	}
}

// TestBuildSEP2PolicyPerDeviceWinsOverFleetDefault verifies the
// resolution precedence buildSEP2Policy must not reimplement, only
// populate: a device present in RegistrationPINs resolves to its own
// entry even when a fleet-wide default is also configured, a device
// absent from the map falls back to the default, and a device absent
// from both resolves ok=false rather than a fabricated 0.
func TestBuildSEP2PolicyPerDeviceWinsOverFleetDefault(t *testing.T) {
	t.Parallel()

	fleetDefault := uint32(123455)
	cfg := config{
		SEP2RegistrationPIN:  &fleetDefault,
		SEP2RegistrationPINs: map[string]uint32{"DEVICE-A-LFDI": 200008},
	}
	policy, err := buildSEP2Policy(cfg)
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}

	got, ok := policy.ResolveRegistrationPIN("DEVICE-A-LFDI")
	if !ok || got != 200008 {
		t.Errorf("device with a per-device entry: got (%d, %v), want (200008, true)", got, ok)
	}

	got, ok = policy.ResolveRegistrationPIN("DEVICE-B-LFDI")
	if !ok || got != 123455 {
		t.Errorf("device with no per-device entry: got (%d, %v), want the fleet default (123455, true)", got, ok)
	}
}

// TestBuildSEP2PolicyPopulatesRegistrationPINsMapExactly verifies the
// full map, not just one lookup, passes through cfg to policy
// unmodified.
func TestBuildSEP2PolicyPopulatesRegistrationPINsMapExactly(t *testing.T) {
	t.Parallel()

	want := map[string]uint32{"F0FA1AC6": 123455, "BCD85AA8": 200008}
	policy, err := buildSEP2Policy(config{SEP2RegistrationPINs: want})
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	if len(policy.RegistrationPINs) != len(want) {
		t.Fatalf("RegistrationPINs: got %v, want %v", policy.RegistrationPINs, want)
	}
	for k, v := range want {
		if policy.RegistrationPINs[k] != v {
			t.Errorf("RegistrationPINs[%q]: got %d, want %d", k, policy.RegistrationPINs[k], v)
		}
	}
}

// TestBuildSEP2PolicyRejectsBadCheckDigitFleetDefault verifies a fleet
// default PIN failing the IEEE 2030.5 section 6.3.5 check-digit rule is
// rejected at buildSEP2Policy time (called from run before newSEP2Embed,
// i.e. before the bridge serves anything), and that the offending value
// is never present in the error. 999999 is a good negative fixture:
// digits sum to 54, and 54 mod 10 is 4, not 0.
func TestBuildSEP2PolicyRejectsBadCheckDigitFleetDefault(t *testing.T) {
	t.Parallel()

	pin := uint32(999999)
	_, err := buildSEP2Policy(config{SEP2RegistrationPIN: &pin})
	if err == nil {
		t.Fatal("expected an error for a fleet default PIN failing the check digit, got nil")
	}
	if strings.Contains(err.Error(), "999999") {
		t.Errorf("error must not echo the PIN value: %v", err)
	}
}

// TestBuildSEP2PolicyRejectsBadCheckDigitPerDevice mirrors
// TestBuildSEP2PolicyRejectsBadCheckDigitFleetDefault for the
// per-device source: a per-device PIN failing the check digit is
// rejected at the same startup point, names the offending LFDI (public,
// safe to log), and never echoes the value.
func TestBuildSEP2PolicyRejectsBadCheckDigitPerDevice(t *testing.T) {
	t.Parallel()

	cfg := config{SEP2RegistrationPINs: map[string]uint32{"BAD-DEVICE-LFDI": 999999}}
	_, err := buildSEP2Policy(cfg)
	if err == nil {
		t.Fatal("expected an error for a per-device PIN failing the check digit, got nil")
	}
	if strings.Contains(err.Error(), "999999") {
		t.Errorf("error must not echo the PIN value: %v", err)
	}
	if !strings.Contains(err.Error(), "BAD-DEVICE-LFDI") {
		t.Errorf("error should name the offending device LFDI: %v", err)
	}
}

// fakeBusPublisherForTest satisfies telemetrypub.BusPublisher without
// pulling a real fieldbus.MessageBus into this test.
type fakeBusPublisherForTest struct{}

func (fakeBusPublisherForTest) Send(_ context.Context, _, _ string, _ []byte) error { return nil }

// TestBuildSEP2PolicyDefaultProgramFlowsThroughAndValidates covers the
// end-to-end config path for the seeded default DERProgram: an unset config
// keeps the compiled-in default, a configured value replaces it, and a value
// the standard forbids stops the bridge at boot rather than reaching a
// client.
func TestBuildSEP2PolicyDefaultProgramFlowsThroughAndValidates(t *testing.T) {
	t.Parallel()

	// Unconfigured: the compiled-in default survives untouched.
	policy, err := buildSEP2Policy(config{})
	if err != nil {
		t.Fatalf("buildSEP2Policy(empty): %v", err)
	}
	if policy.DefaultProgram.Primacy != sep2config.PrimacyContractedServiceProvider {
		t.Errorf("unconfigured DefaultProgram.Primacy = %d, want %d",
			policy.DefaultProgram.Primacy, sep2config.PrimacyContractedServiceProvider)
	}
	if policy.DefaultProgram.Description == "" {
		t.Error("unconfigured DefaultProgram.Description is empty, want the compiled-in default")
	}

	// Configured: both fields are replaced by the operator's values.
	primacy := uint8(89)
	description := "Feeder DER program"
	policy, err = buildSEP2Policy(config{
		SEP2ProgramPrimacy:     &primacy,
		SEP2ProgramDescription: &description,
	})
	if err != nil {
		t.Fatalf("buildSEP2Policy(configured): %v", err)
	}
	if policy.DefaultProgram.Primacy != 89 {
		t.Errorf("configured DefaultProgram.Primacy = %d, want 89", policy.DefaultProgram.Primacy)
	}
	if policy.DefaultProgram.Description != description {
		t.Errorf("configured DefaultProgram.Description = %q, want %q", policy.DefaultProgram.Description, description)
	}

	// An explicit primacy 0 must survive as a configured 0. It is a legal
	// value (the highest priority), so it must not be mistaken for unset
	// and quietly replaced by the compiled-in 1.
	zero := uint8(0)
	policy, err = buildSEP2Policy(config{SEP2ProgramPrimacy: &zero})
	if err != nil {
		t.Fatalf("buildSEP2Policy(primacy 0): %v", err)
	}
	if policy.DefaultProgram.Primacy != 0 {
		t.Errorf("explicit primacy 0 became %d; 0 is a legal primacy and must not be read as unset", policy.DefaultProgram.Primacy)
	}

	// Rejected at boot: a reserved primacy and an over-length description
	// each stop the bridge before it serves anything.
	reserved := uint8(200)
	if _, err := buildSEP2Policy(config{SEP2ProgramPrimacy: &reserved}); err == nil {
		t.Error("buildSEP2Policy accepted a reserved primacy 200, want a boot-time refusal")
	}
	tooLong := strings.Repeat("x", 33)
	if _, err := buildSEP2Policy(config{SEP2ProgramDescription: &tooLong}); err == nil {
		t.Error("buildSEP2Policy accepted a 33-character description, want a boot-time refusal (sep.xsd String32)")
	}
}
