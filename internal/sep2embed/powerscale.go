package sep2embed

import "fmt"

// maxPowerOfTenMultiplier is the largest PowerOfTenMultiplierType exponent
// computePowerOfTen will try (IEEE 2030.5 PowerOfTenMultiplierType is an
// Int8 with a documented range of -9..9; this package only ever needs the
// non-negative half, since scaling up rather than down is what lets a
// larger-than-int16 magnitude fit).
const maxPowerOfTenMultiplier = 9

// computePowerOfTen picks the smallest non-negative power-of-ten multiplier
// m (0..maxPowerOfTenMultiplier) such that round(vAr / 10^m) fits in int16,
// and returns the scaled value together with m. This is the GAGO-083 fix
// for the Class-3 defect of hardcoding Multiplier: 0 regardless of
// magnitude: a real fleet's maxQ (CIM PowerElectronicsConnection.maxQ, in
// unscaled base VAr) routinely exceeds int16's +-32767 range, so the
// multiplier must be computed from the magnitude, not assumed.
//
// "Smallest" means the most significant digits of vAr are retained: e.g.
// 125000 scales to Value 12500, Multiplier 1 (not Multiplier 2, which would
// discard a digit unnecessarily). Effective VAr is Value * 10^Multiplier,
// per the IEEE 2030.5 PowerOfTenMultiplierType convention; for inputs that
// are not an exact multiple of the chosen power of ten, this reconstructs
// to a rounded value, not the exact original (see the doc comment on the
// rounding below).
//
// The sign of vAr is preserved: computePowerOfTen scales the magnitude and
// restores the sign afterward, so a negative vAr yields a negative Value
// at the same multiplier a positive vAr of the same magnitude would use.
// int16's range is asymmetric (-32768..32767), but bounding the scaled
// magnitude to <= 32767 keeps both signs representable without special
// casing the negative boundary.
//
// If vAr is so large that even m=maxPowerOfTenMultiplier does not bring
// the scaled magnitude into int16 range (roughly |vAr| > 3.2767e13),
// computePowerOfTen returns an error rather than fabricating a truncated
// or clamped value: per [[data-invariants]] Rule 2, a value that cannot be
// represented validly must be refused, not silently corrupted. A raw VAr
// magnitude in that range is not a plausible physical rating, so refusing
// it surfaces a data problem instead of advertising a wrong capability.
func computePowerOfTen(vAr int64) (value int16, mult int8, err error) {
	sign := int64(1)
	magnitude := vAr
	if magnitude < 0 {
		sign = -1
		magnitude = -magnitude
	}

	for m := int8(0); m <= maxPowerOfTenMultiplier; m++ {
		divisor := pow10Int64(m)
		// Round to nearest rather than truncate, so the retained digits
		// are the closest int16 representation of vAr at this scale
		// (e.g. 32768 at m=1 rounds to 3277, not 3276).
		scaled := (magnitude + divisor/2) / divisor
		if scaled <= 32767 {
			return int16(sign * scaled), m, nil
		}
	}

	return 0, 0, fmt.Errorf("computePowerOfTen: %d does not fit int16 even at multiplier %d (magnitude too large for a plausible VAr rating)", vAr, maxPowerOfTenMultiplier)
}

// pow10Int64 returns 10^exp as an int64. exp is always in
// 0..maxPowerOfTenMultiplier (9) here, so the result never overflows
// int64 (10^9 is far below int64's range).
func pow10Int64(exp int8) int64 {
	r := int64(1)
	for i := int8(0); i < exp; i++ {
		r *= 10
	}
	return r
}

// toInt16Checked narrows an int64 to int16, returning an error instead of
// truncating when v falls outside int16's range. Used by control.go's
// config-sourced power writes, where the multiplier is already decoded
// separately (decodeMultiplierValue) and only the value's width needs
// guarding: per [[data-invariants]] Rule 2, an out-of-range config value
// must be refused, not silently wrapped or clamped.
func toInt16Checked(v int64) (int16, error) {
	if v < -32768 || v > 32767 {
		return 0, fmt.Errorf("value %d does not fit int16 (range -32768..32767)", v)
	}
	return int16(v), nil
}
