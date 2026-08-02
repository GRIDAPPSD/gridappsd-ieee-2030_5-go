package main

import "github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2config"

// testPolicyWithPIN is sep2config.DefaultPolicy plus a fleet-wide
// registration PIN, which the compiled-in defaults deliberately omit: a
// PIN is operator-supplied (IEEE 2030.5 section 6.3.5), so an
// unconfigured bridge fails closed at boot rather than inventing one.
// Tests that seed devices must therefore supply one.
//
// 123455 is the standard's own worked example (PIN 12345, digits summing
// to 15, check digit 5), so it satisfies the 6.3.5 checksum rule and is
// an obvious documentation value rather than a plausible operator secret.
func testPolicyWithPIN() sep2config.SEP2Policy {
	p := sep2config.DefaultPolicy()
	pin := uint32(123455)
	p.DefaultRegistrationPIN = &pin
	return p
}
