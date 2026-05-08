package main

import (
	"context"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/measurements"
)

// TestLogStatsUntilDoneExitsOnContextCancel pins the lifecycle contract
// of logStatsUntilDone: when the caller cancels ctx, the goroutine must
// return and close its done channel within a short deadline. This is the
// regression guard for the H1 fix in runPump (Dutch review): runPump
// relies on ctx cancellation to drain this goroutine before pump.Run
// returns. If a future refactor breaks the ctx.Done() path here, the
// runPump leak comes back.
//
// Not t.Parallel: this test mutates the package-level statsLogInterval
// var to drive the ticker arm at a fast cadence. Running in parallel
// with a sibling test that also touches the var would race.
func TestLogStatsUntilDoneExitsOnContextCancel(t *testing.T) {
	// Shrink the ticker so the goroutine has cycled at least once before
	// we cancel; this exercises the select arm under realistic timing
	// rather than racing the cancel against the very first iteration.
	prev := statsLogInterval
	statsLogInterval = 5 * time.Millisecond
	t.Cleanup(func() { statsLogInterval = prev })

	tbl := measurements.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go logStatsUntilDone(ctx, tbl, done)

	// Let the ticker fire at least once so we cover the t.C arm.
	time.Sleep(20 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// expected: ctx-cancel path closes done.
	case <-time.After(time.Second):
		t.Fatal("logStatsUntilDone did not exit within 1s of ctx cancel")
	}
}

// TestLogStatsUntilDoneClosesDoneOnPreCancelledContext covers the edge
// case where ctx is already cancelled before the goroutine starts. The
// function must still close done so the caller's <-done sync point does
// not block forever.
//
// Not t.Parallel: shares the package-level statsLogInterval read path
// with TestLogStatsUntilDoneExitsOnContextCancel, which writes to it.
func TestLogStatsUntilDoneClosesDoneOnPreCancelledContext(t *testing.T) {
	tbl := measurements.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go logStatsUntilDone(ctx, tbl, done)

	select {
	case <-done:
		// expected.
	case <-time.After(time.Second):
		t.Fatal("logStatsUntilDone did not exit within 1s on pre-cancelled ctx")
	}
}
