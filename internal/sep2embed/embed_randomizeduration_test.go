package sep2embed

import (
	"context"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// TestNewRefusesOutOfRangeRandomizeDuration proves New enforces the
// sep.xsd OneHourRangeType bound on Config.DERControl.RandomizeDuration
// itself, rather than trusting a caller to have already run
// SEP2Policy.ValidateDERControl (cmd/bridge's boot path): the two bound
// values (+/-3600) are accepted, one step past either is refused with an
// error naming the field.
//
// Duration is 7200, well above the +/-3600 range bound under test, so a
// magnitude at or past the range boundary never also trips the separate
// magnitude-versus-duration rule TestNewRefusesRandomizeDurationAtOrAboveDurationMagnitude
// covers below: this table stays isolated to the range dimension.
func TestNewRefusesOutOfRangeRandomizeDuration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		randomize  int32
		wantRefuse bool
	}{
		{"upper bound accepted", 3600, false},
		{"lower bound accepted", -3600, false},
		{"one past the upper bound refused", 3601, true},
		{"one past the lower bound refused", -3601, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := registry.New()
			cfg := Config{
				Addr:                   "127.0.0.1:0",
				CertDir:                t.TempDir(),
				ResolveRegistrationPIN: testResolvePIN,
				DERControl:             DERControlSeed{Duration: 7200, RandomizeDuration: tc.randomize},
			}

			e, err := New(context.Background(), cfg, reg)
			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("New(RandomizeDuration=%d): %v, want nil", tc.randomize, err)
				}
				if e == nil {
					t.Fatal("New returned a nil *Embed with a nil error")
				}
				return
			}
			if err == nil {
				t.Fatalf("New(RandomizeDuration=%d): want an error, got nil", tc.randomize)
			}
			if !strings.Contains(err.Error(), "RandomizeDuration") {
				t.Errorf("New(RandomizeDuration=%d) error = %q, want it to name the RandomizeDuration field", tc.randomize, err.Error())
			}
		})
	}
}

// TestNewRefusesRandomizeDurationAtOrAboveDurationMagnitude is issue 90:
// New must also apply the magnitude-versus-duration rule
// SEP2Policy.ValidateDERControl applies, refusing a (Duration,
// RandomizeDuration) pair the boot validator would refuse. 100/100 is the
// pair issue 90's own reproduction used against ValidateDERControl.
func TestNewRefusesRandomizeDurationAtOrAboveDurationMagnitude(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		duration   uint32
		randomize  int32
		wantRefuse bool
	}{
		{"magnitude below duration accepted", 100, 99, false},
		{"magnitude equals duration refused", 100, 100, true},
		{"negative magnitude equal in size refused", 100, -100, true},
		{"magnitude above duration refused", 100, 150, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := registry.New()
			cfg := Config{
				Addr:                   "127.0.0.1:0",
				CertDir:                t.TempDir(),
				ResolveRegistrationPIN: testResolvePIN,
				DERControl:             DERControlSeed{Duration: tc.duration, RandomizeDuration: tc.randomize},
			}

			e, err := New(context.Background(), cfg, reg)
			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("New(Duration=%d, RandomizeDuration=%d): %v, want nil", tc.duration, tc.randomize, err)
				}
				if e == nil {
					t.Fatal("New returned a nil *Embed with a nil error")
				}
				return
			}
			if err == nil {
				t.Fatalf("New(Duration=%d, RandomizeDuration=%d): want an error, got nil", tc.duration, tc.randomize)
			}
			if !strings.Contains(err.Error(), "RandomizeDuration") || !strings.Contains(err.Error(), "Duration") {
				t.Errorf("New(Duration=%d, RandomizeDuration=%d) error = %q, want it to name both fields", tc.duration, tc.randomize, err.Error())
			}
		})
	}
}
