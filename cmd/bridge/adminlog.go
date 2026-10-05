package main

import (
	"context"
	"log"
	"log/slog"
)

// authSuccessEvent is the event the admin plane logs at INFO on every
// admitted Bearer request; in no-key mode that is every request.
const authSuccessEvent = "admin_auth_success"

// dropAuthSuccess drops records whose event attribute is authSuccessEvent
// and passes every other record, including admin_auth_failure.
type dropAuthSuccess struct {
	inner slog.Handler
	// drop is set when a WithAttrs call already carried the event.
	drop bool
}

func (h dropAuthSuccess) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h dropAuthSuccess) Handle(ctx context.Context, r slog.Record) error {
	drop := h.drop
	r.Attrs(func(a slog.Attr) bool {
		if isAuthSuccess(a) {
			drop = true
			return false
		}
		return true
	})
	if drop {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h dropAuthSuccess) WithAttrs(attrs []slog.Attr) slog.Handler {
	drop := h.drop
	for _, a := range attrs {
		drop = drop || isAuthSuccess(a)
	}
	return dropAuthSuccess{inner: h.inner.WithAttrs(attrs), drop: drop}
}

func (h dropAuthSuccess) WithGroup(name string) slog.Handler {
	return dropAuthSuccess{inner: h.inner.WithGroup(name), drop: h.drop}
}

func isAuthSuccess(a slog.Attr) bool {
	return a.Key == "event" && a.Value.Kind() == slog.KindString && a.Value.String() == authSuccessEvent
}

// installNoKeyLogFilter wraps the default slog handler so the per-request
// admin_auth_success line is dropped, and returns the undo. slog.SetDefault
// also reroutes the log package, so its writer and flags are put back to
// keep every log.Printf line as it was.
func installNoKeyLogFilter() (restore func()) {
	prevSlog := slog.Default()
	prevWriter, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(dropAuthSuccess{inner: prevSlog.Handler()}))
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)
	return func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}
}
