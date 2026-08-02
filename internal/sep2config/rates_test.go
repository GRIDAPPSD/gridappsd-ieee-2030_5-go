package sep2config_test

import (
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"
)

func u32(v uint32) *uint32 { return &v }

// TestResolveRatesUnsetReportsFalse pins the fail-open-to-nothing contract:
// with no policy configured at all, both resolvers report false rather than
// returning a zero rate. A zero would be stamped onto the wire as a
// continuous interval, so "no opinion" must stay distinguishable from
// "0 seconds" all the way to the consumer.
func TestResolveRatesUnsetReportsFalse(t *testing.T) {
	t.Parallel()

	p := sep2config.DefaultPolicy()

	if rate, ok := p.ResolvePollRate("ANY-LFDI"); ok {
		t.Errorf("ResolvePollRate = (%d, true) with nothing configured, want ok=false", rate)
	}
	if rate, ok := p.ResolvePostRate("ANY-LFDI"); ok {
		t.Errorf("ResolvePostRate = (%d, true) with nothing configured, want ok=false", rate)
	}
}

// TestResolveRatesFleetDefaultAppliesToEveryDevice asserts the fleet-wide
// value answers for a device that has no per-device entry, which is the only
// configuration an operator can express today.
func TestResolveRatesFleetDefaultAppliesToEveryDevice(t *testing.T) {
	t.Parallel()

	p := sep2config.DefaultPolicy()
	p.DefaultPollRate = u32(900)
	p.DefaultPostRate = u32(30)

	for _, lfdi := range []string{"DEVICE-A", "DEVICE-B"} {
		rate, ok := p.ResolvePollRate(lfdi)
		if !ok || rate != 900 {
			t.Errorf("ResolvePollRate(%q) = (%d, %v), want (900, true)", lfdi, rate, ok)
		}
		rate, ok = p.ResolvePostRate(lfdi)
		if !ok || rate != 30 {
			t.Errorf("ResolvePostRate(%q) = (%d, %v), want (30, true)", lfdi, rate, ok)
		}
	}
}

// TestResolveRatesPerDeviceWinsOverFleetDefault asserts the precedence the
// resolver seam exists to hide. No operator surface populates these maps
// yet, so this test is what keeps the seam honest until one does: it proves
// a later per-device feature needs no change outside this package.
func TestResolveRatesPerDeviceWinsOverFleetDefault(t *testing.T) {
	t.Parallel()

	p := sep2config.DefaultPolicy()
	p.DefaultPollRate = u32(900)
	p.DefaultPostRate = u32(30)
	p.PollRates = map[string]uint32{"DEVICE-A": 60}
	p.PostRates = map[string]uint32{"DEVICE-A": 15}

	if rate, ok := p.ResolvePollRate("DEVICE-A"); !ok || rate != 60 {
		t.Errorf("ResolvePollRate(DEVICE-A) = (%d, %v), want (60, true) from the per-device map", rate, ok)
	}
	if rate, ok := p.ResolvePostRate("DEVICE-A"); !ok || rate != 15 {
		t.Errorf("ResolvePostRate(DEVICE-A) = (%d, %v), want (15, true) from the per-device map", rate, ok)
	}
	// A device with no entry still falls back to the fleet value.
	if rate, ok := p.ResolvePollRate("DEVICE-B"); !ok || rate != 900 {
		t.Errorf("ResolvePollRate(DEVICE-B) = (%d, %v), want (900, true) from the fleet default", rate, ok)
	}
	if rate, ok := p.ResolvePostRate("DEVICE-B"); !ok || rate != 30 {
		t.Errorf("ResolvePostRate(DEVICE-B) = (%d, %v), want (30, true) from the fleet default", rate, ok)
	}
}

// TestResolveRatesPerDeviceKeysAreCaseInsensitive matches the LFDI matching
// rule ResolveRegistrationPIN already uses: the canonical LFDI is uppercase
// hex, and an operator-written config that differs only in casing must still
// match the identity derived from the certificate.
func TestResolveRatesPerDeviceKeysAreCaseInsensitive(t *testing.T) {
	t.Parallel()

	p := sep2config.DefaultPolicy()
	p.PollRates = map[string]uint32{"abcdef0123": 60}
	p.PostRates = map[string]uint32{"  abcdef0123  ": 15}

	if rate, ok := p.ResolvePollRate("ABCDEF0123"); !ok || rate != 60 {
		t.Errorf("ResolvePollRate(ABCDEF0123) = (%d, %v), want (60, true) matching a lowercase key", rate, ok)
	}
	if rate, ok := p.ResolvePostRate("ABCDEF0123"); !ok || rate != 15 {
		t.Errorf("ResolvePostRate(ABCDEF0123) = (%d, %v), want (15, true) matching a padded key", rate, ok)
	}
}

// TestValidateRatesAcceptsUnsetAndPositive asserts validation is not a
// barrier for the two configurations that matter: nothing set at all (the
// pre-existing behavior) and a real positive rate.
func TestValidateRatesAcceptsUnsetAndPositive(t *testing.T) {
	t.Parallel()

	if err := sep2config.DefaultPolicy().ValidateRates(); err != nil {
		t.Errorf("ValidateRates on the default policy = %v, want nil", err)
	}

	p := sep2config.DefaultPolicy()
	p.DefaultPollRate = u32(sep2config.RecommendedPollRate)
	p.DefaultPostRate = u32(sep2config.RecommendedPostRate)
	if err := p.ValidateRates(); err != nil {
		t.Errorf("ValidateRates on the recommended rates = %v, want nil", err)
	}
}

// TestValidateRatesRejectsZero asserts a 0-second rate is a hard boot
// failure and that the message names the FLAG an operator can act on, not
// the struct field they cannot see. 0 is schema-valid (sep.xsd applies no
// facets to either rate), so nothing downstream would catch it: this check
// is the only thing standing between a typo and a client posting with no
// delay at all.
func TestValidateRatesRejectsZero(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		mutate   func(*sep2config.SEP2Policy)
		wantFlag string
	}{
		{
			name:     "fleet poll rate zero",
			mutate:   func(p *sep2config.SEP2Policy) { p.DefaultPollRate = u32(0) },
			wantFlag: "-sep2-poll-rate",
		},
		{
			name:     "fleet post rate zero",
			mutate:   func(p *sep2config.SEP2Policy) { p.DefaultPostRate = u32(0) },
			wantFlag: "-sep2-post-rate",
		},
		{
			name:     "per-device poll rate zero",
			mutate:   func(p *sep2config.SEP2Policy) { p.PollRates = map[string]uint32{"DEVICE-A": 0} },
			wantFlag: "-sep2-poll-rate",
		},
		{
			name:     "per-device post rate zero",
			mutate:   func(p *sep2config.SEP2Policy) { p.PostRates = map[string]uint32{"DEVICE-A": 0} },
			wantFlag: "-sep2-post-rate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := sep2config.DefaultPolicy()
			tc.mutate(&p)

			err := p.ValidateRates()
			if err == nil {
				t.Fatal("ValidateRates = nil, want an error for a 0-second rate")
			}
			if !strings.Contains(err.Error(), tc.wantFlag) {
				t.Errorf("error %q does not name the flag %q an operator would fix", err, tc.wantFlag)
			}
		})
	}
}

// TestRecommendedPollRateMatchesSchemaDefault pins RecommendedPollRate to
// sep.xsd:190's own documented default: "If not specified, a default of 900
// seconds (15 minutes) is used." Advertising the schema default explicitly
// is a deliberate no-op in meaning, so drifting off 900 would silently
// change what every client does.
func TestRecommendedPollRateMatchesSchemaDefault(t *testing.T) {
	t.Parallel()

	if sep2config.RecommendedPollRate != 900 {
		t.Errorf("RecommendedPollRate = %d, want 900 (the sep.xsd:190 documented default)",
			sep2config.RecommendedPollRate)
	}
}

// TestDefaultPolicyLeavesRatesUnset guards the no-op invariant at its
// source: DefaultPolicy must not acquire a compiled-in rate. If it ever
// did, every existing deployment would start advertising a rate on upgrade
// without an operator asking for one.
func TestDefaultPolicyLeavesRatesUnset(t *testing.T) {
	t.Parallel()

	p := sep2config.DefaultPolicy()
	if p.DefaultPollRate != nil {
		t.Errorf("DefaultPolicy().DefaultPollRate = %d, want nil (unset)", *p.DefaultPollRate)
	}
	if p.DefaultPostRate != nil {
		t.Errorf("DefaultPolicy().DefaultPostRate = %d, want nil (unset)", *p.DefaultPostRate)
	}
	if p.PollRates != nil || p.PostRates != nil {
		t.Error("DefaultPolicy() populated a per-device rate map, want both nil")
	}
}
