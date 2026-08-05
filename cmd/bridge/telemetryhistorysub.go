package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/sim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/telemetryhistory"
)

// runHistorySubscriber subscribes independently, read-only, to every
// destination in destinations, decodes each delivered frame as a
// diff.Message via telemetryhistory.DecodeMessage, and appends every
// decoded sample to store. One goroutine per destination.
//
// A Subscribe failure on one destination, or a subscription ending for
// any reason, is logged and does not affect the others: this function
// never returns a non-nil error for that reason. That is deliberate.
// This runner backs an operator-diagnostic view (the admin UI's
// telemetry history), not the control or measurement path, so a
// subscribe failure here must never be treated as a bridge-startup
// failure the way a pump or control-subscriber failure is. See run()'s
// wiring: this function is called directly against ctx and its error is
// discarded, rather than threaded through runBridgeRunners' cancel-on-
// any-exit/join-errors chain.
//
// destinations is presumed already validated (capped, wildcard-free) by
// config.go's loadConfig; this function does not re-validate it. An
// empty destinations subscribes nothing and starts no per-destination
// goroutine: run() only calls this at all when
// cfg.SEP2TelemetryHistoryTopics is non-empty, but the nil-safe,
// zero-goroutine behavior holds regardless of caller.
//
// Returns once every per-destination goroutine has exited (each ends
// when ctx is canceled, or immediately if its own initial Subscribe
// call failed), always as ctx.Err(). A test drives this with a fake
// sim.SubscribeClient and asserts no goroutine is left running past
// that point (see TestRunHistorySubscriberExitsCleanlyOnCtxCancel).
func runHistorySubscriber(ctx context.Context, subs sim.SubscribeClient, store *telemetryhistory.Store, destinations []string) error {
	var wg sync.WaitGroup
	for _, dest := range destinations {
		wg.Add(1)
		go func(dest string) {
			defer wg.Done()
			subscribeHistoryDestination(ctx, subs, store, dest)
		}(dest)
	}
	wg.Wait()
	return ctx.Err()
}

// subscribeHistoryDestination runs one destination's read-only
// subscribe-decode-append loop until its subscription ends. Every
// failure path here is logged, never returned or panicked on: see
// runHistorySubscriber's doc comment for why a per-destination failure
// must never propagate as a bridge-fatal error.
func subscribeHistoryDestination(ctx context.Context, subs sim.SubscribeClient, store *telemetryhistory.Store, dest string) {
	log.Printf("bridge: subscribing to %s for telemetry history (read-only)", dest)

	sub, err := subs.Subscribe(ctx, dest)
	if err != nil {
		log.Printf("telemetry history: subscribe %s failed: %v", dest, err)
		return
	}

	for msg := range sub.Messages() {
		var envelope diff.Message
		if derr := json.Unmarshal(msg.Body, &envelope); derr != nil {
			log.Printf("telemetry history: skip malformed frame on %s: %v", dest, derr)
			continue
		}
		for _, d := range telemetryhistory.DecodeMessage(envelope, func(format string, args ...any) {
			log.Printf("telemetry history: "+format, args...)
		}) {
			store.Append(d.Key, d.Sample)
		}
	}

	if endErr := sub.Err(); endErr != nil && !errors.Is(endErr, context.Canceled) && !errors.Is(endErr, context.DeadlineExceeded) {
		log.Printf("telemetry history: subscription on %s ended: %v", dest, endErr)
	}
}
