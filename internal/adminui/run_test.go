package adminui

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// openStream starts Run on s and opens the plane's dashboard event stream
// through the real listener, returning the reader positioned after the
// first event, Run's result channel and its cancel.
func openStream(t *testing.T, s *Server) (*bufio.Reader, <-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	t.Cleanup(cancel)

	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/dashboard/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /dashboard/events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET /dashboard/events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	r := bufio.NewReader(resp.Body)
	if _, err := nextEvent(r); err != nil {
		t.Fatalf("first event: %v", err)
	}
	return r, runErr, cancel
}

// nextEvent reads up to and including the next "data:" line.
func nextEvent(r *bufio.Reader) (string, error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(line, "data:") {
			return line, nil
		}
	}
}

// TestRunReturnsNilPromptlyWithAStreamOpen: a browser tab holding the
// dashboard stream open must not turn a SIGTERM into a failed shutdown.
func TestRunReturnsNilPromptlyWithAStreamOpen(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	s.timeouts.shutdown = time.Second
	_, runErr, cancel := openStream(t, s)

	start := time.Now()
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v after %v, want nil", err, time.Since(start))
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("Run took %v to return, want well inside the shutdown timeout", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of cancel")
	}
}

// TestStreamOutlivesTheWriteTimeout: the dashboard sends an event every
// five seconds, so a 300ms write timeout must not end the stream before
// the second event. It takes about five seconds.
func TestStreamOutlivesTheWriteTimeout(t *testing.T) {
	t.Parallel()

	s := newServer(t, Config{Key: testKey}, testSources())
	s.timeouts.write = 300 * time.Millisecond
	s.timeouts.read = 300 * time.Millisecond
	r, _, _ := openStream(t, s)

	got := make(chan error, 1)
	go func() {
		_, err := nextEvent(r)
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second event: %v, want the stream still open past the write timeout", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no second event within 8s")
	}
}

// TestOnlyStreamsLoseTheWriteDeadline: an event stream has its write
// deadline lifted, and an ordinary response keeps it.
func TestOnlyStreamsLoseTheWriteDeadline(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		contentType string
		wantCleared bool
	}{
		{"text/event-stream", true},
		{"application/json", false},
	} {
		cleared := false
		h := keepStreamsOpenWith(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", tc.contentType)
			_, _ = w.Write([]byte("x"))
		}), func(*http.ResponseController) error { cleared = true; return nil })
		doRequest(t, h, http.MethodGet, "/", "", "localhost")
		if cleared != tc.wantCleared {
			t.Errorf("%s: write deadline lifted = %v, want %v", tc.contentType, cleared, tc.wantCleared)
		}
	}
}
