package main

import (
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
)

// clearRateEnv isolates the rate flags from the ambient environment, the
// same way TestLoadConfigDefaults does for the other env-backed settings:
// loadConfig branches on `== ""`, so setting empty is equivalent to unset.
func clearRateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SEP2_POLL_RATE", "")
	t.Setenv("SEP2_POST_RATE", "")
}

// TestLoadConfigRateFlagsUnsetLeaveNil is the no-op guard at the config
// layer: absent flags must leave both fields nil, because nil is what makes
// the bridge advertise nothing at all. A compiled-in numeric default here
// would start advertising a rate to every existing deployment on upgrade.
func TestLoadConfigRateFlagsUnsetLeaveNil(t *testing.T) {
	clearRateEnv(t)

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2PollRate != nil {
		t.Errorf("SEP2PollRate = %d with no flag, want nil", *cfg.SEP2PollRate)
	}
	if cfg.SEP2PostRate != nil {
		t.Errorf("SEP2PostRate = %d with no flag, want nil", *cfg.SEP2PostRate)
	}
}

// TestLoadConfigRateFlagsParse asserts both flags reach config with the
// value the operator typed.
func TestLoadConfigRateFlagsParse(t *testing.T) {
	clearRateEnv(t)

	cfg, err := loadConfig([]string{"-sep2-poll-rate=900", "-sep2-post-rate=30"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2PollRate == nil || *cfg.SEP2PollRate != 900 {
		t.Errorf("SEP2PollRate = %v, want 900", cfg.SEP2PollRate)
	}
	if cfg.SEP2PostRate == nil || *cfg.SEP2PostRate != 30 {
		t.Errorf("SEP2PostRate = %v, want 30", cfg.SEP2PostRate)
	}
}

// TestLoadConfigRateFlagsFromEnv and the flag-shadows-env case below pin the
// same precedence every other setting in this loader follows.
func TestLoadConfigRateFlagsFromEnv(t *testing.T) {
	t.Setenv("SEP2_POLL_RATE", "600")
	t.Setenv("SEP2_POST_RATE", "45")

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2PollRate == nil || *cfg.SEP2PollRate != 600 {
		t.Errorf("SEP2PollRate = %v, want 600 from env", cfg.SEP2PollRate)
	}
	if cfg.SEP2PostRate == nil || *cfg.SEP2PostRate != 45 {
		t.Errorf("SEP2PostRate = %v, want 45 from env", cfg.SEP2PostRate)
	}
}

func TestLoadConfigRateFlagsShadowEnv(t *testing.T) {
	t.Setenv("SEP2_POLL_RATE", "600")
	t.Setenv("SEP2_POST_RATE", "45")

	cfg, err := loadConfig([]string{"-sep2-poll-rate=900", "-sep2-post-rate=30"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SEP2PollRate == nil || *cfg.SEP2PollRate != 900 {
		t.Errorf("SEP2PollRate = %v, want the flag value 900 to shadow the env", cfg.SEP2PollRate)
	}
	if cfg.SEP2PostRate == nil || *cfg.SEP2PostRate != 30 {
		t.Errorf("SEP2PostRate = %v, want the flag value 30 to shadow the env", cfg.SEP2PostRate)
	}
}

// TestLoadConfigRateFlagsRejectMalformed asserts a syntactically bad value
// fails at config load, before any network I/O, and that the error names the
// flag. An operator handed a bare strconv error has to guess which of the
// two knobs they mistyped.
func TestLoadConfigRateFlagsRejectMalformed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{"poll not a number", []string{"-sep2-poll-rate=abc"}, "-sep2-poll-rate"},
		{"poll negative", []string{"-sep2-poll-rate=-5"}, "-sep2-poll-rate"},
		{"poll overflows uint32", []string{"-sep2-poll-rate=4294967296"}, "-sep2-poll-rate"},
		{"post not a number", []string{"-sep2-post-rate=30s"}, "-sep2-post-rate"},
		{"post negative", []string{"-sep2-post-rate=-1"}, "-sep2-post-rate"},
		{"post fractional", []string{"-sep2-post-rate=1.5"}, "-sep2-post-rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRateEnv(t)

			_, err := loadConfig(tc.args)
			if err == nil {
				t.Fatalf("loadConfig(%v) = nil error, want a parse failure", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantFlag) {
				t.Errorf("error %q does not name the flag %q", err, tc.wantFlag)
			}
		})
	}
}

// TestBuildSEP2PolicyRejectsZeroRates asserts the domain rule fires at
// STARTUP, through the same buildSEP2Policy call the bridge makes before it
// dials anything, rather than lazily at first seed or first POST. 0 parses
// cleanly, so this is the only gate that catches it.
func TestBuildSEP2PolicyRejectsZeroRates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{"zero poll rate", []string{"-sep2-poll-rate=0"}, "-sep2-poll-rate"},
		{"zero post rate", []string{"-sep2-post-rate=0"}, "-sep2-post-rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRateEnv(t)

			cfg, err := loadConfig(tc.args)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if _, err := buildSEP2Policy(cfg); err == nil {
				t.Fatal("buildSEP2Policy = nil error for a 0-second rate, want a startup failure")
			} else if !strings.Contains(err.Error(), tc.wantFlag) {
				t.Errorf("error %q does not name the flag %q", err, tc.wantFlag)
			}
		})
	}
}

// TestBuildSEP2PolicyResolvesConfiguredRates asserts the flags reach the
// policy RESOLVERS, which is the only surface the rest of the bridge reads.
func TestBuildSEP2PolicyResolvesConfiguredRates(t *testing.T) {
	clearRateEnv(t)

	cfg, err := loadConfig([]string{"-sep2-poll-rate=900", "-sep2-post-rate=30"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	policy, err := buildSEP2Policy(cfg)
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}

	if rate, ok := policy.ResolvePollRate("ANY-DEVICE"); !ok || rate != 900 {
		t.Errorf("ResolvePollRate = (%d, %v), want (900, true)", rate, ok)
	}
	if rate, ok := policy.ResolvePostRate("ANY-DEVICE"); !ok || rate != 30 {
		t.Errorf("ResolvePostRate = (%d, %v), want (30, true)", rate, ok)
	}
}

// TestBuildSEP2PolicyUnsetRatesResolveFalse is the end-to-end no-op guard:
// no flags means the resolvers report "no opinion", which is what makes the
// seeded Registration omit pollRate and POST /mup leave a client's postRate
// untouched.
func TestBuildSEP2PolicyUnsetRatesResolveFalse(t *testing.T) {
	clearRateEnv(t)

	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	policy, err := buildSEP2Policy(cfg)
	if err != nil {
		t.Fatalf("buildSEP2Policy: %v", err)
	}

	if rate, ok := policy.ResolvePollRate("ANY-DEVICE"); ok {
		t.Errorf("ResolvePollRate = (%d, true) with no flag, want ok=false", rate)
	}
	if rate, ok := policy.ResolvePostRate("ANY-DEVICE"); ok {
		t.Errorf("ResolvePostRate = (%d, true) with no flag, want ok=false", rate)
	}
}

// TestSEP2EmbedConfigWiresRateResolvers asserts the projection onto
// sep2embed.Config carries RESOLVERS rather than scalars, and that they
// answer with the configured policy. This is the seam that keeps a future
// per-device rate change confined to sep2config: if this ever regressed to
// copying a bare default value, per-device rates would silently never apply.
func TestSEP2EmbedConfigWiresRateResolvers(t *testing.T) {
	t.Parallel()

	policy := sep2config.DefaultPolicy()
	pollRate := uint32(900)
	postRate := uint32(30)
	policy.DefaultPollRate = &pollRate
	policy.DefaultPostRate = &postRate
	// A per-device override the fleet default would mask if the wiring
	// copied a scalar instead of threading the resolver.
	policy.PollRates = map[string]uint32{"DEVICE-A": 60}
	policy.PostRates = map[string]uint32{"DEVICE-A": 15}

	got := sep2EmbedConfig(config{}, nil, policy, nil)

	if got.ResolveRegistrationPollRate == nil {
		t.Fatal("ResolveRegistrationPollRate is nil, want the policy resolver")
	}
	if got.ResolvePostRate == nil {
		t.Fatal("ResolvePostRate is nil, want the policy resolver")
	}

	if rate, ok := got.ResolveRegistrationPollRate("DEVICE-A"); !ok || rate != 60 {
		t.Errorf("ResolveRegistrationPollRate(DEVICE-A) = (%d, %v), want (60, true) from the per-device map", rate, ok)
	}
	if rate, ok := got.ResolvePostRate("DEVICE-A"); !ok || rate != 15 {
		t.Errorf("ResolvePostRate(DEVICE-A) = (%d, %v), want (15, true) from the per-device map", rate, ok)
	}
	if rate, ok := got.ResolveRegistrationPollRate("DEVICE-B"); !ok || rate != 900 {
		t.Errorf("ResolveRegistrationPollRate(DEVICE-B) = (%d, %v), want (900, true) from the fleet default", rate, ok)
	}
	if rate, ok := got.ResolvePostRate("DEVICE-B"); !ok || rate != 30 {
		t.Errorf("ResolvePostRate(DEVICE-B) = (%d, %v), want (30, true) from the fleet default", rate, ok)
	}
}

// TestSEP2EmbedConfigRateResolversNilSafeWhenUnconfigured asserts the
// resolvers are still non-nil under DefaultPolicy but report no opinion, so
// sep2embed and core both take their untouched-behavior paths.
func TestSEP2EmbedConfigRateResolversNilSafeWhenUnconfigured(t *testing.T) {
	t.Parallel()

	got := sep2EmbedConfig(config{}, nil, sep2config.DefaultPolicy(), nil)

	if got.ResolveRegistrationPollRate == nil || got.ResolvePostRate == nil {
		t.Fatal("rate resolvers must be non-nil even when unconfigured")
	}
	if _, ok := got.ResolveRegistrationPollRate("ANY"); ok {
		t.Error("ResolveRegistrationPollRate reported a rate with none configured")
	}
	if _, ok := got.ResolvePostRate("ANY"); ok {
		t.Error("ResolvePostRate reported a rate with none configured")
	}
}
