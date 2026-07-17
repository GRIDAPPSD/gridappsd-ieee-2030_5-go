package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// waitForCancelThenReturn returns a func(context.Context) error that
// blocks until ctx.Done(), records that it observed the cancellation in
// observedCancel, and then returns retErr. This models the
// graceful-shutdown side of either embed.Run or the stompRun closure in
// run(): both, in the real bridge, block on their ctx until asked to
// stop.
func waitForCancelThenReturn(observedCancel *atomic.Bool, retErr error) func(context.Context) error {
	return func(ctx context.Context) error {
		<-ctx.Done()
		observedCancel.Store(true)
		return retErr
	}
}

// failImmediately returns a func(context.Context) error that returns
// failErr without waiting on ctx at all. This models an independent
// failure, a Serve error in the embed or a broker drop reaching the
// pump, that happens on its own rather than because the other side
// asked it to stop.
func failImmediately(failErr error) func(context.Context) error {
	return func(context.Context) error {
		return failErr
	}
}

// TestRunEmbedAndStompEmbedFailureCancelsStompAndSurfaces covers case
// (a): the embed side fails independently. runEmbedAndStomp must cancel
// the stomp side's ctx, return within a bounded time, and surface the
// embed's error.
func TestRunEmbedAndStompEmbedFailureCancelsStompAndSurfaces(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("embed: fake serve failure")
	var stompObservedCancel atomic.Bool
	embedRun := failImmediately(wantErr)
	stompRun := waitForCancelThenReturn(&stompObservedCancel, context.Canceled)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- runEmbedAndStomp(ctx, embedRun, stompRun) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, wantErr) {
			t.Fatalf("runEmbedAndStomp error = %v, want it to wrap %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runEmbedAndStomp did not return within 2s (embed failure did not cancel the stomp side)")
	}
	if !stompObservedCancel.Load() {
		t.Error("stomp-side runner never observed cancellation after the embed side failed independently")
	}
}

// TestRunEmbedAndStompStompExitCancelsEmbed covers case (b): the
// stomp-side runner exits on its own. runEmbedAndStomp must cancel the
// embed side's ctx, return within a bounded time, and surface the
// stomp-side error (the embed side returns nil, its own contract for a
// graceful shutdown, so it does not override the stomp error).
func TestRunEmbedAndStompStompExitCancelsEmbed(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("stomp: pump exited on its own")
	var embedObservedCancel atomic.Bool
	stompRun := failImmediately(wantErr)
	embedRun := waitForCancelThenReturn(&embedObservedCancel, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- runEmbedAndStomp(ctx, embedRun, stompRun) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, wantErr) {
			t.Fatalf("runEmbedAndStomp error = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runEmbedAndStomp did not return within 2s (stomp exit did not cancel the embed side)")
	}
	if !embedObservedCancel.Load() {
		t.Error("embed-side runner never observed cancellation after the stomp side exited on its own")
	}
}

// TestRunEmbedAndStompParentCancelTearsDownBoth covers case (c): the
// parent ctx is cancelled (the SIGINT/SIGTERM analog) while both sides
// are still running. runEmbedAndStomp must tear both down and return
// within a bounded time with a graceful (context.Canceled) result.
func TestRunEmbedAndStompParentCancelTearsDownBoth(t *testing.T) {
	t.Parallel()

	var embedObservedCancel, stompObservedCancel atomic.Bool
	embedRun := waitForCancelThenReturn(&embedObservedCancel, nil)
	stompRun := waitForCancelThenReturn(&stompObservedCancel, context.Canceled)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- runEmbedAndStomp(ctx, embedRun, stompRun) }()
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runEmbedAndStomp error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runEmbedAndStomp did not return within 2s of the parent ctx cancel")
	}
	if !embedObservedCancel.Load() {
		t.Error("embed-side runner never observed the parent ctx cancellation")
	}
	if !stompObservedCancel.Load() {
		t.Error("stomp-side runner never observed the parent ctx cancellation")
	}
}

// TestRunEmbedAndStompBothFailJoinsErrors is the LOW-fold-in regression
// test: when both sides fail independently (neither error is a
// graceful context.Canceled shutdown), runEmbedAndStomp must not
// silently drop one of the two errors. It joins them via errors.Join,
// so errors.Is against either original failure still matches.
func TestRunEmbedAndStompBothFailJoinsErrors(t *testing.T) {
	t.Parallel()

	embedFailErr := errors.New("embed: fake serve failure")
	stompFailErr := errors.New("stomp: fake pump failure")
	embedRun := failImmediately(embedFailErr)
	stompRun := failImmediately(stompFailErr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- runEmbedAndStomp(ctx, embedRun, stompRun) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, embedFailErr) {
			t.Errorf("runEmbedAndStomp error = %v, want it to wrap the embed failure %v", err, embedFailErr)
		}
		if !errors.Is(err, stompFailErr) {
			t.Errorf("runEmbedAndStomp error = %v, want it to wrap the stomp failure %v", err, stompFailErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runEmbedAndStomp did not return within 2s when both sides failed independently")
	}
}
