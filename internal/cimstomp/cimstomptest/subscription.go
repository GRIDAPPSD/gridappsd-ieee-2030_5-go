// Package cimstomptest provides test-only helpers for constructing
// in-memory cimstomp.Subscription values. Other packages in this repo
// (notably internal/cim/sim) use it to build fakes without dialing a
// broker. Mirrors the net/http vs net/http/httptest split: the
// production package stays free of test seams, and consumers that need
// to construct internals do so through this companion package.
//
// Do not import cimstomptest from production code. The package exists
// only to serve _test.go files.
package cimstomptest

import (
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cimstomp"
)

// NewSubscription returns a live *cimstomp.Subscription and the
// caller-driven inbox channel. The returned channel is what
// Subscription.Messages will deliver to its consumer; the test
// goroutine sends frames on it, optionally calls SetErr to record an
// end-cause, then closes the channel to end the subscription.
//
// Ordering contract: SetErr writes synchronously to the Subscription's
// internal err state, mirroring the cimstomp.runSubscription contract.
// A test that calls SetErr BEFORE closing the channel guarantees the
// consumer sees the err the moment it observes the close. A test that
// closes the channel without calling SetErr leaves Err returning nil.
func NewSubscription() (*cimstomp.Subscription, chan<- cimstomp.Message) {
	sub, msgs := cimstomp.NewTestSubscription()
	return sub, msgs
}

// SetErr records the cause that Subscription.Err will return after the
// messages channel is closed. Pair with NewSubscription.
func SetErr(sub *cimstomp.Subscription, err error) {
	cimstomp.SetTestErr(sub, err)
}
