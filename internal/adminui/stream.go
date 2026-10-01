package adminui

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// keepStreamsOpen lifts the listener's write deadline from a response
// that turns out to be an event stream (the plane's dashboard), which
// would otherwise be cut every write timeout. Every other response keeps
// the deadline. It decides by the response's Content-Type, not by path,
// so it follows the plane's routes rather than copying them.
func keepStreamsOpen(next http.Handler) http.Handler {
	return keepStreamsOpenWith(next, clearWriteDeadline)
}

func keepStreamsOpenWith(next http.Handler, clear func(*http.ResponseController) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&streamWriter{ResponseWriter: w, rc: http.NewResponseController(w), clear: clear}, r)
	})
}

func clearWriteDeadline(rc *http.ResponseController) error {
	return rc.SetWriteDeadline(time.Time{})
}

// streamWriter checks the Content-Type once, when the header is sent.
type streamWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	clear   func(*http.ResponseController) error
	checked bool
}

func (w *streamWriter) check() {
	if w.checked {
		return
	}
	w.checked = true
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		return
	}
	// A recorder in tests supports no deadlines; on a real connection a
	// failure leaves the stream to be cut, so it is logged.
	if err := w.clear(w.rc); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("adminui: lift the write deadline on an event stream: %v", err)
	}
}

func (w *streamWriter) WriteHeader(code int) {
	w.check()
	w.ResponseWriter.WriteHeader(code)
}

func (w *streamWriter) Write(b []byte) (int, error) {
	w.check()
	return w.ResponseWriter.Write(b)
}

// Flush keeps the http.Flusher the stream handler asserts on.
func (w *streamWriter) Flush() {
	w.check()
	if err := w.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("adminui: flush: %v", err)
	}
}

// Unwrap lets http.ResponseController reach the connection.
func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
