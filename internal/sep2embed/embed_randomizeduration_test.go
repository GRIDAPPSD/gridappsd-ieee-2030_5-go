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
				DERControl:             DERControlSeed{Duration: 1800, RandomizeDuration: tc.randomize},
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
