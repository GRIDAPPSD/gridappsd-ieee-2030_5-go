package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
)

// This file covers the operator surface added for the issued-DERControl
// interval and for the DefaultDERControl, both at the parse layer
// (loadConfig) and at the layer that turns a parsed config into policy
// (buildSEP2Policy). The split mirrors the one already in place for the
// program flags: loadConfig judges representability, buildSEP2Policy applies
// precedence and runs the IEEE 2030.5 domain validators.

// TestLoadConfigControlIntervalUnsetByDefault pins the no-op case. Both
// fields stay nil with no flags, so buildSEP2Policy leaves the compiled-in
// defaults in place. A non-nil zero here would be worse than for the program
// flags: a zero duration is not merely a surprising value, it is the
// zero-length interval that a client expires on arrival.
func TestLoadConfigControlIntervalUnsetByDefault(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ControlDuration != nil {
		t.Errorf("SEP2ControlDuration = %v, want nil", *cfg.SEP2ControlDuration)
	}
	if cfg.SEP2ControlRandomizeDuration != nil {
		t.Errorf("SEP2ControlRandomizeDuration = %v, want nil", *cfg.SEP2ControlRandomizeDuration)
	}
}

// TestLoadConfigControlIntervalFromFlags covers the ordinary configured path,
// including a NEGATIVE randomization: sep.xsd's OneHourRangeType is a signed
// -3600 to 3600 offset, so a parser that rejected or dropped the sign would
// silently halve the operator's usable range.
func TestLoadConfigControlIntervalFromFlags(t *testing.T) {
	cfg, err := loadConfig([]string{
		"-sep2-control-duration=600",
		"-sep2-control-randomize-duration=-120",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ControlDuration == nil || *cfg.SEP2ControlDuration != 600 {
		t.Errorf("SEP2ControlDuration = %v, want 600", cfg.SEP2ControlDuration)
	}
	if cfg.SEP2ControlRandomizeDuration == nil || *cfg.SEP2ControlRandomizeDuration != -120 {
		t.Errorf("SEP2ControlRandomizeDuration = %v, want -120", cfg.SEP2ControlRandomizeDuration)
	}
}

// TestLoadConfigControlRandomizeZeroIsCarried guards the ambiguity the
// empty-string flag sentinel exists to resolve. 0 is the SHIPPED value for
// randomizeDuration, so an explicit -sep2-control-randomize-duration=0 is
// indistinguishable from unset in its effect today; it must still arrive as a
// configured 0, because the day the compiled-in default changes an operator
// who explicitly asked for 0 must keep getting 0.
func TestLoadConfigControlRandomizeZeroIsCarried(t *testing.T) {
	cfg, err := loadConfig([]string{"-sep2-control-randomize-duration=0"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2ControlRandomizeDuration == nil {
		t.Fatal("SEP2ControlRandomizeDuration = nil for an explicit 0; an explicitly requested value must be distinguishable from unset")
	}
	if *cfg.SEP2ControlRandomizeDuration != 0 {
		t.Errorf("SEP2ControlRandomizeDuration = %d, want 0", *cfg.SEP2ControlRandomizeDuration)
	}
}

// TestLoadConfigControlIntervalRejectsUnparseableValues covers the parse
// layer only. Whether a syntactically valid number is USABLE is a domain
// question answered by ValidateDERControl, which is why a bare "0" is absent
// from this table: it parses fine and is rejected later, by the validator
// that owns that rule for every source.
func TestLoadConfigControlIntervalRejectsUnparseableValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
		flag string
	}{
		{"duration is not a number", []string{"-sep2-control-duration=soon"}, "-sep2-control-duration"},
		{"duration is negative", []string{"-sep2-control-duration=-1"}, "-sep2-control-duration"},
		{"duration is fractional", []string{"-sep2-control-duration=1.5"}, "-sep2-control-duration"},
		{"duration overflows uint32", []string{"-sep2-control-duration=4294967296"}, "-sep2-control-duration"},
		{"randomization is not a number", []string{"-sep2-control-randomize-duration=some"}, "-sep2-control-randomize-duration"},
		{"randomization overflows int32", []string{"-sep2-control-randomize-duration=2147483648"}, "-sep2-control-randomize-duration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(tt.args)
			if err == nil {
				t.Fatalf("loadConfig(%v) = nil error, want a load failure", tt.args)
			}
			if !strings.Contains(err.Error(), tt.flag) {
				t.Errorf("error %q does not name %s", err, tt.flag)
			}
		})
	}
}

// TestBuildSEP2PolicyAppliesControlInterval covers the assignment layer: a
// configured value reaches the policy, and an unset one leaves the
// compiled-in default alone rather than zeroing it.
func TestBuildSEP2PolicyAppliesControlInterval(t *testing.T) {
	cfg, err := loadConfig([]string{
		"-sep2-control-duration=600",
		"-sep2-control-randomize-duration=-120",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	policy, err := buildSEP2Policy(cfg)
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	if policy.DERControl.Duration != 600 {
		t.Errorf("DERControl.Duration = %d, want 600", policy.DERControl.Duration)
	}
	if policy.DERControl.RandomizeDuration != -120 {
		t.Errorf("DERControl.RandomizeDuration = %d, want -120", policy.DERControl.RandomizeDuration)
	}

	defaults, err := buildSEP2Policy(mustLoadConfig(t, nil))
	if err != nil {
		t.Fatalf("buildSEP2Policy (no flags): %v", err)
	}
	if defaults.DERControl.Duration != sep2config.DefaultDERControlDuration {
		t.Errorf("DERControl.Duration with no flags = %d, want the compiled-in %d",
			defaults.DERControl.Duration, sep2config.DefaultDERControlDuration)
	}
}

// TestBuildSEP2PolicyRejectsUnusableControlInterval is the boot gate. It must
// fail here rather than at the first control delta, because a control served
// with a zero-length interval fails INVISIBLY: the client still fetches,
// parses and acknowledges it, so nothing downstream reports a problem and the
// only symptom is a device that never moves.
func TestBuildSEP2PolicyRejectsUnusableControlInterval(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "zero duration",
			args: []string{"-sep2-control-duration=0"},
			want: "-sep2-control-duration",
		},
		{
			name: "randomization outside OneHourRangeType",
			args: []string{"-sep2-control-randomize-duration=3601"},
			want: "-sep2-control-randomize-duration",
		},
		{
			name: "randomization wider than the duration",
			args: []string{"-sep2-control-duration=60", "-sep2-control-randomize-duration=600"},
			want: "-sep2-control-randomize-duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustLoadConfig(t, tt.args)
			_, err := buildSEP2Policy(cfg)
			if err == nil {
				t.Fatalf("buildSEP2Policy(%v) = nil error, want the boot gate to refuse", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %s, the flag an operator would set", err, tt.want)
			}
		})
	}
}

// writeDefaultControlFile writes a -sep2-default-control-file document and
// returns its path.
func writeDefaultControlFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "default-control.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestBuildSEP2PolicyDefaultControlCommandsNothingByDefault is the shipped
// case, asserted through the whole config path rather than only against
// sep2config.DefaultPolicy: with no file, the policy the bridge would serve
// commands nothing.
func TestBuildSEP2PolicyDefaultControlCommandsNothingByDefault(t *testing.T) {
	policy, err := buildSEP2Policy(mustLoadConfig(t, nil))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	base := policy.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want a present but empty base (minOccurs=1, sep.xsd:3270)")
	}
	if base.OpModConnect != nil {
		t.Errorf("DefaultControl.OpModConnect = %v, want nil; the shipped fallback must not command a grid connect or disconnect", *base.OpModConnect)
	}
	if base.OpModEnergize != nil {
		t.Errorf("DefaultControl.OpModEnergize = %v, want nil", *base.OpModEnergize)
	}
}

// TestBuildSEP2PolicyDefaultControlFromFile covers the operator override,
// with an explicit FALSE. False is the value a fill-or-override bug swallows
// silently, because once the pointer is lost it is indistinguishable from the
// bool zero value, and on this particular field it means a commanded
// disconnect rather than "no opinion".
func TestBuildSEP2PolicyDefaultControlFromFile(t *testing.T) {
	path := writeDefaultControlFile(t, `{"opModConnect": false, "opModEnergize": true}`)

	policy, err := buildSEP2Policy(mustLoadConfig(t, []string{"-sep2-default-control-file=" + path}))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	base := policy.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want the configured base")
	}
	if base.OpModConnect == nil || *base.OpModConnect {
		t.Errorf("DefaultControl.OpModConnect = %v, want a configured false", base.OpModConnect)
	}
	if base.OpModEnergize == nil || !*base.OpModEnergize {
		t.Errorf("DefaultControl.OpModEnergize = %v, want a configured true", base.OpModEnergize)
	}
}

// TestBuildSEP2PolicyDefaultControlFilePartialLeavesOtherFieldUnset is the
// invariant that forces the pointer returns out of loadDefaultControlFile: a
// file naming one member must not reset the other. Here the other member's
// compiled-in value is "unset", and unset is precisely what must survive,
// because materializing it would turn a fallback that commands nothing into
// one that commands a switch operation.
func TestBuildSEP2PolicyDefaultControlFilePartialLeavesOtherFieldUnset(t *testing.T) {
	path := writeDefaultControlFile(t, `{"opModConnect": true}`)

	policy, err := buildSEP2Policy(mustLoadConfig(t, []string{"-sep2-default-control-file=" + path}))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	base := policy.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want the configured base")
	}
	if base.OpModConnect == nil || !*base.OpModConnect {
		t.Errorf("DefaultControl.OpModConnect = %v, want the configured true", base.OpModConnect)
	}
	if base.OpModEnergize != nil {
		t.Errorf("DefaultControl.OpModEnergize = %v, want nil; a file that omits a member must leave it unset rather than materializing a command", *base.OpModEnergize)
	}
}

// TestBuildSEP2PolicyDefaultControlFromFileOpModMaxLimW covers the third
// member end to end: parsed, carried through buildSEP2Policy, and cast to
// sep2.PerCent on the policy's DERControlBase. The combined body also pins
// that configuring opModMaxLimW does not disturb an opModConnect set in the
// same file, the same fill-not-replace invariant the two-bool test above
// covers for that pair.
func TestBuildSEP2PolicyDefaultControlFromFileOpModMaxLimW(t *testing.T) {
	path := writeDefaultControlFile(t, `{"opModConnect": true, "opModMaxLimW": 5000}`)

	policy, err := buildSEP2Policy(mustLoadConfig(t, []string{"-sep2-default-control-file=" + path}))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	base := policy.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want the configured base")
	}
	if base.OpModMaxLimW == nil || *base.OpModMaxLimW != sep2.PerCent(5000) {
		t.Errorf("DefaultControl.OpModMaxLimW = %v, want a configured 5000", base.OpModMaxLimW)
	}
	if base.OpModConnect == nil || !*base.OpModConnect {
		t.Errorf("DefaultControl.OpModConnect = %v, want the configured true", base.OpModConnect)
	}
	if base.OpModEnergize != nil {
		t.Errorf("DefaultControl.OpModEnergize = %v, want nil; opModMaxLimW must not materialize an unconfigured member", *base.OpModEnergize)
	}
}

// TestBuildSEP2PolicyDefaultControlFileOpModMaxLimWOmittedLeavesUnset is the
// mirror of TestBuildSEP2PolicyDefaultControlFilePartialLeavesOtherFieldUnset
// for the third member: a file that never names opModMaxLimW must leave the
// compiled-in nil (no active limit) in place rather than materializing a
// zero-percent (full curtailment) command.
func TestBuildSEP2PolicyDefaultControlFileOpModMaxLimWOmittedLeavesUnset(t *testing.T) {
	path := writeDefaultControlFile(t, `{"opModConnect": true}`)

	policy, err := buildSEP2Policy(mustLoadConfig(t, []string{"-sep2-default-control-file=" + path}))
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}
	base := policy.DefaultControl.DERControlBase
	if base == nil {
		t.Fatal("DefaultControl.DERControlBase = nil, want the configured base")
	}
	if base.OpModMaxLimW != nil {
		t.Errorf("DefaultControl.OpModMaxLimW = %v, want nil; a file that omits the member must not command full curtailment", *base.OpModMaxLimW)
	}
}

// TestLoadConfigDefaultControlFileParsesOpModMaxLimWRange covers the parse
// layer's accepted boundary values: 0 (full curtailment) and 10000 (100%,
// the shipped no-op equivalent an operator might still write explicitly) are
// both legal PerCent values (IEEE 2030.5-2018 Annex B.2.3.4), not just the
// interior 5000 the other tests use.
func TestLoadConfigDefaultControlFileParsesOpModMaxLimWRange(t *testing.T) {
	tests := []struct {
		name string
		body string
		want uint16
	}{
		{"zero (full curtailment)", `{"opModMaxLimW": 0}`, 0},
		{"upper bound (100%)", `{"opModMaxLimW": 10000}`, 10000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeDefaultControlFile(t, tt.body)
			cfg, err := loadConfig([]string{"-sep2-default-control-file=" + path})
			if err != nil {
				t.Fatalf("loadConfig(%s): %v", tt.body, err)
			}
			if cfg.SEP2DefaultControlOpModMaxLimW == nil || *cfg.SEP2DefaultControlOpModMaxLimW != tt.want {
				t.Errorf("SEP2DefaultControlOpModMaxLimW = %v, want %d", cfg.SEP2DefaultControlOpModMaxLimW, tt.want)
			}
		})
	}
}

// TestLoadConfigDefaultControlFileRejectsBadInput: every unusable file is a
// load error naming the flag, never a silently ignored setting. The
// misspelled-member and unsupported-member cases matter most here, because an
// operator who writes opModTargetW and sees the bridge come up would
// otherwise believe a power target was configured.
func TestLoadConfigDefaultControlFileRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON at all", `not json`},
		{"top-level array rather than an object", `[true]`},
		{"misspelled member", `{"opModConnected": true}`},
		{"deliberately unsupported power target", `{"opModTargetW": 5000}`},
		{"deliberately unsupported ramp setting", `{"setGradW": 100}`},
		{"boolean as a string", `{"opModConnect": "true"}`},
		{"boolean as a number", `{"opModEnergize": 1}`},
		{"opModMaxLimW negative", `{"opModMaxLimW": -1}`},
		{"opModMaxLimW above range", `{"opModMaxLimW": 10001}`},
		{"opModMaxLimW fractional", `{"opModMaxLimW": 50.5}`},
		{"opModMaxLimW as a string", `{"opModMaxLimW": "5000"}`},
		{"opModMaxLimW as a boolean", `{"opModMaxLimW": true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeDefaultControlFile(t, tt.body)
			_, err := loadConfig([]string{"-sep2-default-control-file=" + path})
			if err == nil {
				t.Fatalf("loadConfig(%s) = nil error, want a load failure", tt.body)
			}
			if !strings.Contains(err.Error(), "-sep2-default-control-file") {
				t.Errorf("error %q does not name -sep2-default-control-file", err)
			}
		})
	}
}

// TestLoadConfigDefaultControlFileMissingIsAnError: a path the operator named
// but that does not exist is a mistake, not a reason to fall back silently to
// a different fallback control than the one they asked for.
func TestLoadConfigDefaultControlFileMissingIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := loadConfig([]string{"-sep2-default-control-file=" + path})
	if err == nil {
		t.Fatal("loadConfig with a nonexistent -sep2-default-control-file = nil error, want a load failure")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q does not say the file is missing", err)
	}
}

// mustLoadConfig is loadConfig with the error folded into a t.Fatalf, for the
// cases above whose subject is buildSEP2Policy rather than parsing.
func mustLoadConfig(t *testing.T, args []string) config {
	t.Helper()
	cfg, err := loadConfig(args)
	if err != nil {
		t.Fatalf("loadConfig(%v): %v", args, err)
	}
	return cfg
}
