package sep2embed

import (
	"math"
	"testing"
)

// TestComputePowerOfTen is a table-driven test on computePowerOfTen,
// asserting the returned Value/Multiplier fields directly and the
// reconstructed effective VAr (Value * 10^Multiplier), per
// [[data-invariants]] Rule 1: a test that only checks "no error" proves
// nothing about whether the scaling is correct or minimal.
//
// The reconstructed-value assertion pins the documented rounding
// behavior for inputs that are not a clean multiple of the chosen
// power of ten (e.g. 32768 -> Value 3277, Multiplier 1, reconstructed
// 32770, not the original 32768), rather than leaving the rounding
// unverified.
func TestComputePowerOfTen(t *testing.T) {
	t.Parallel()

	pow10 := func(exp int8) int64 {
		r := int64(1)
		for i := int8(0); i < exp; i++ {
			r *= 10
		}
		return r
	}

	tests := []struct {
		name      string
		vAr       int64
		wantValue int16
		wantMult  int8
		wantRecon int64 // wantValue * 10^wantMult, asserted explicitly
		wantErr   bool
	}{
		{
			name:      "zero",
			vAr:       0,
			wantValue: 0,
			wantMult:  0,
			wantRecon: 0,
		},
		{
			name:      "fits at multiplier 0 without scaling",
			vAr:       5000,
			wantValue: 5000,
			wantMult:  0,
			wantRecon: 5000,
		},
		{
			name:      "125000 picks smallest multiplier 1, not 2",
			vAr:       125000,
			wantValue: 12500,
			wantMult:  1,
			wantRecon: 125000,
		},
		{
			name:      "3000000 needs multiplier 2",
			vAr:       3000000,
			wantValue: 30000,
			wantMult:  2,
			wantRecon: 3000000,
		},
		{
			name:      "int16 max boundary fits at multiplier 0",
			vAr:       32767,
			wantValue: 32767,
			wantMult:  0,
			wantRecon: 32767,
		},
		{
			name:      "one over int16 max rounds at multiplier 1",
			vAr:       32768,
			wantValue: 3277,
			wantMult:  1,
			wantRecon: 32770,
		},
		{
			name:      "negative sign is preserved",
			vAr:       -125000,
			wantValue: -12500,
			wantMult:  1,
			wantRecon: -125000,
		},
		{
			name:      "negative rounding preserves sign and magnitude rounding",
			vAr:       -32768,
			wantValue: -3277,
			wantMult:  1,
			wantRecon: -32770,
		},
		{
			name:      "large realistic fleet value (125 kVAr inverter, GAGO-083 example)",
			vAr:       125000,
			wantValue: 12500,
			wantMult:  1,
			wantRecon: 125000,
		},
		{
			name:    "over range even at multiplier 9 is refused, not truncated",
			vAr:     40000000000000, // 4e13 > 32767 * 10^9 (~3.2767e13)
			wantErr: true,
		},
		{
			name:    "negative over range is also refused",
			vAr:     -40000000000000,
			wantErr: true,
		},
		{
			name:    "int64 MinInt64 is refused, not silently corrupted (negation overflow)",
			vAr:     math.MinInt64,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotValue, gotMult, err := computePowerOfTen(tt.vAr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("computePowerOfTen(%d) = (%d, %d, nil), want error", tt.vAr, gotValue, gotMult)
				}
				return
			}
			if err != nil {
				t.Fatalf("computePowerOfTen(%d): unexpected error: %v", tt.vAr, err)
			}
			if gotValue != tt.wantValue {
				t.Errorf("computePowerOfTen(%d).value = %d, want %d", tt.vAr, gotValue, tt.wantValue)
			}
			if gotMult != tt.wantMult {
				t.Errorf("computePowerOfTen(%d).mult = %d, want %d", tt.vAr, gotMult, tt.wantMult)
			}
			recon := int64(gotValue) * pow10(gotMult)
			if recon != tt.wantRecon {
				t.Errorf("computePowerOfTen(%d) reconstructed %d * 10^%d = %d, want %d", tt.vAr, gotValue, gotMult, recon, tt.wantRecon)
			}
		})
	}
}

// TestToInt16Checked asserts the width-guard helper used by control.go's
// config-sourced power writes: a value that fits int16 passes through
// unchanged, and a value that does not fit is refused with an error
// rather than silently truncated (data-invariants Rule 2).
func TestToInt16Checked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      int64
		want    int16
		wantErr bool
	}{
		{name: "zero", in: 0, want: 0},
		{name: "positive in range", in: 1234, want: 1234},
		{name: "negative in range", in: -1234, want: -1234},
		{name: "int16 max boundary", in: 32767, want: 32767},
		{name: "int16 min boundary", in: -32768, want: -32768},
		{name: "one over max is refused", in: 32768, wantErr: true},
		{name: "one under min is refused", in: -32769, wantErr: true},
		{name: "far over range is refused", in: 125000, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toInt16Checked(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("toInt16Checked(%d) = (%d, nil), want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toInt16Checked(%d): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("toInt16Checked(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
