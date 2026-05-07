package cimstomp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-stomp/stomp/v3"
)

// Sentinel errors are declared in errors.go so that both Client and
// Publisher reference the same values.

// tokenTopic is the GridAPPS-D auth-token bootstrap destination. The broker
// is expected to reply to the SEND with a single frame whose body is the
// auth token string.
const tokenTopic = "/topic/pnnl.goss.token.topic"

// gossHasSubjectHeader and gossSubjectHeader are the GridAPPS-D-specific
// headers that every request SEND must carry. They are not part of the
// STOMP spec; without them the broker rejects or filters our SENDs.
// See: plans/plan-1-design/research-stomp-cim-catalog.md sections 2 and 7.
const (
	gossHasSubjectHeader = "GOSS_HAS_SUBJECT"
	gossSubjectHeader    = "GOSS_SUBJECT"
	replyToHeader        = "reply-to"
	correlationIDHeader  = "correlation-id"
)

// heartbeat is the STOMP heartbeat interval in both directions. Matches the
// existing Publisher.
const heartbeat = 10 * time.Second

// Client is a STOMP request/response client for the GridAPPS-D message bus.
//
// At Connect, Client dials STOMP and bootstraps a GridAPPS-D auth token by
// sending a base64-encoded `user:password` to /topic/pnnl.goss.token.topic
// and reading the broker's single-frame reply. The token is cached for the
// lifetime of the Client.
//
// Request sends a body to the given destination with two mandatory
// GridAPPS-D headers attached: `GOSS_HAS_SUBJECT: True` and
// `GOSS_SUBJECT: <token>`. These headers are not in the STOMP spec; they
// are required by the GridAPPS-D broker and are documented only in
// gridappsd-python's goss.py. The reply-to is a per-request
// /temp-queue/response.<ts>; the broker correlates by destination, no
// `correlation-id` header is set.
//
// Concurrency: Client is safe for use from multiple goroutines, but
// Request serializes via an internal mutex so that v0 issues a single
// in-flight request at a time. This is sufficient for the bridge's
// startup CIM queries and periodic refresh; lifting the lock is a
// future change once a real second-consumer exists.
//
// Lifecycle: NewClient allocates; Connect opens the TCP/STOMP session and
// fetches the token; Close disconnects. Close is idempotent. There is no
// auto-reconnect in v0; callers needing reconnect must call Close then
// NewClient + Connect again. See GAGO-012.
type Client struct {
	cfg STOMPConfig

	mu        sync.Mutex
	conn      *stomp.Conn
	token     string
	connected atomic.Bool
	closed    atomic.Bool
}

// NewClient allocates a Client. It does not perform any I/O.
func NewClient(cfg STOMPConfig) *Client {
	return &Client{cfg: cfg}
}

// Connect dials the STOMP broker and fetches the GridAPPS-D auth token.
//
// Connect is single-shot: it cannot be called after Close. A Client whose
// Close has been called returns ErrClosed from Connect. To reuse the
// lifecycle, construct a new Client via NewClient.
//
// Connect should be called at most once per Client; calling it twice on
// a Client that has not been Closed is a programmer error and is not
// guarded against here. v0 has no auto-reconnect; see GAGO-012.
func (c *Client) Connect(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// go-stomp v3.1.5's DialWithContext calls net.Dial (not net.DialContext),
	// so a ctx deadline is ignored at the TCP layer. Dial ourselves with
	// net.DialContext to honor ctx, then hand the live conn to
	// stomp.ConnectWithContext which observes ctx for the STOMP handshake.
	// When cfg.TLS is non-nil, wrap the TCP connection with crypto/tls
	// before handing it to stomp.ConnectWithContext (GAGO-014).
	tcp, err := dialSTOMPTransport(ctx, c.cfg)
	if err != nil {
		return err
	}

	conn, err := stomp.ConnectWithContext(ctx, tcp,
		stomp.ConnOpt.Login(c.cfg.User, c.cfg.Password),
		stomp.ConnOpt.HeartBeat(heartbeat, heartbeat),
	)
	if err != nil {
		// stomp.ConnectWithContext failed before the STOMP frame layer was
		// up; only the raw TCP socket needs closing. Any tcp.Close error is
		// logged inline because a leaked half-open TCP socket masks broker
		// state leaks (Leon H2).
		if cerr := tcp.Close(); cerr != nil {
			log.Printf("cimstomp: tcp close after failed STOMP connect: %v", cerr)
		}
		return fmt.Errorf("cimstomp.Client: stomp connect %s: %w", c.cfg.Address, err)
	}

	token, err := fetchAuthToken(ctx, conn, c.cfg.User, c.cfg.Password)
	if err != nil {
		// STOMP connection is up but token bootstrap failed; tear it down
		// and log any Disconnect error rather than swallowing it (Leon H2).
		logDisconnectErr(conn.Disconnect(), "Connect.fetchAuthToken")
		return fmt.Errorf("cimstomp.Client: fetch auth token: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.token = token
	c.mu.Unlock()
	c.connected.Store(true)

	return nil
}

// Close disconnects the STOMP session and clears the cached auth token
// field on the Client so further Request calls fail with ErrNotConnected
// and the local Client struct no longer holds a live token reference.
// It is idempotent: calling Close on a never-connected or already-closed
// Client returns nil. Connect cannot be called after Close (returns
// ErrClosed); construct a new Client to reuse.
//
// Token clearing is best-effort. Go strings are immutable, so the heap
// allocation that backed c.token is unreachable from this Client but the
// underlying bytes are not overwritten until garbage collected. Earlier
// copies in the fetchAuthToken read path and the c.cfg.Password field
// also live on. A memory dump of a long-lived process can still surface
// credentials (Leon L1, L3).
//
// The Disconnect error, if any, is logged via logDisconnectErr and also
// wrapped into the return value; the connection is being torn down
// regardless.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.connected.Store(false)

	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.token = ""
	c.mu.Unlock()

	if conn == nil {
		return nil
	}
	err := conn.Disconnect()
	logDisconnectErr(err, "Client.Close")
	if err != nil {
		return fmt.Errorf("cimstomp.Client: disconnect: %w", err)
	}
	return nil
}

// Request sends body to destination and waits for a single response frame
// on a per-request /temp-queue/response.<ts> reply-to. The destination may
// be the bare GridAPPS-D form (`goss.gridappsd...`); Request prepends
// `/queue/` if no /queue/, /topic/, or /temp-queue/ prefix is present.
//
// The returned bytes are the raw response body; callers parse JSON.
//
// Errors:
//   - ErrNotConnected if called before Connect or after Close.
//   - ErrRequestTimeout if ctx deadline expires before a response arrives.
//   - context.Canceled if ctx is cancelled mid-flight.
//   - wrapped broker errors otherwise.
func (c *Client) Request(ctx context.Context, destination string, body []byte) ([]byte, error) {
	// Lock-free fast path: connected.Load() short-circuits before-Connect and
	// after-Close callers without touching the mutex.
	if !c.connected.Load() {
		return nil, ErrNotConnected
	}
	if err := ctx.Err(); err != nil {
		return nil, mapCtxErr(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Authoritative check under the mutex (Dutch H2 / item C, option 1).
	// Two cases land here:
	//   1. A concurrent Close raced between the connected.Load() above and
	//      the lock acquisition.
	//   2. A test that flipped the connected flag via markConnectedForTest
	//      without a real STOMP session. The previous implementation kept
	//      connected and a zero-value *stomp.Conn placeholder in lockstep;
	//      we now treat nil c.conn as ErrNotConnected so the placeholder
	//      can stay nil, and the test hook is no longer compiled into the
	//      production binary.
	if c.conn == nil {
		return nil, ErrNotConnected
	}

	replyTo := newTempReplyDest()
	dest := normalizeDestination(destination)

	sub, err := c.conn.Subscribe(replyTo, stomp.AckAuto)
	if err != nil {
		return nil, fmt.Errorf("cimstomp.Client: subscribe %s: %w", replyTo, err)
	}
	// Always tear down the subscription before returning. The broker will
	// drop the corresponding /temp-queue/... destination once unsubscribed.
	defer func() {
		_ = sub.Unsubscribe()
	}()

	// Send the request. The broker still correlates the response via the
	// per-request /temp-queue/... reply-to; the correlation-id header is
	// defense-in-depth (Leon M2 / GAGO-013). If a future ticket
	// consolidates onto a shared reply queue, the demux code on the
	// receive side can then key on this id without a wire-format change.
	corrID, err := newCorrelationID()
	if err != nil {
		return nil, fmt.Errorf("cimstomp.Client: generate correlation id: %w", err)
	}
	err = c.conn.Send(dest, "application/json", body,
		stomp.SendOpt.Header(replyToHeader, replyTo),
		stomp.SendOpt.Header(gossHasSubjectHeader, "True"),
		stomp.SendOpt.Header(gossSubjectHeader, c.token),
		stomp.SendOpt.Header(correlationIDHeader, corrID),
	)
	if err != nil {
		return nil, fmt.Errorf("cimstomp.Client: send to %s: %w", dest, err)
	}

	// Wait for either the response frame or context cancellation. The
	// subscription channel is the goroutine-safe exit path; closing the
	// subscription via Unsubscribe in the deferred call drains it.
	select {
	case <-ctx.Done():
		return nil, mapCtxErr(ctx.Err())

	case msg, ok := <-sub.C:
		if !ok || msg == nil {
			return nil, fmt.Errorf("cimstomp.Client: subscription closed before response")
		}
		if msg.Err != nil {
			return nil, fmt.Errorf("cimstomp.Client: response error: %w", msg.Err)
		}
		// Copy the body; the underlying frame may be reused.
		out := make([]byte, len(msg.Body))
		copy(out, msg.Body)
		return out, nil
	}
}

// fetchAuthToken implements the GridAPPS-D auth-token bootstrap. It
// subscribes to a fresh /queue/temp.token_resp.<user>.<ts>, sends
// base64(user:password) to /topic/pnnl.goss.token.topic with a reply-to
// header, waits for the single reply frame, and returns its body as the
// token. ctx bounds the wait.
//
// A regular /queue/ destination is used (not /temp-queue/) to match the
// Python upstream's bootstrap convention (gridappsd-python goss.py
// _make_connection). The GridAPPS-D platform's token responder sends the
// token back to the exact destination string carried in reply-to. With
// /temp-queue/ the broker's header-rewriting can vary across versions
// and connections; a regular queue with a unique name is unambiguous.
func fetchAuthToken(ctx context.Context, conn *stomp.Conn, user, password string) (string, error) {
	replyTo := newTokenReplyDest(user)
	sub, err := conn.Subscribe(replyTo, stomp.AckAuto)
	if err != nil {
		return "", fmt.Errorf("subscribe %s: %w", replyTo, err)
	}
	defer func() {
		_ = sub.Unsubscribe()
	}()

	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	err = conn.Send(tokenTopic, "text/plain", []byte(auth),
		stomp.SendOpt.Header(replyToHeader, replyTo),
	)
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", tokenTopic, err)
	}

	select {
	case <-ctx.Done():
		return "", mapCtxErr(ctx.Err())

	case msg, ok := <-sub.C:
		if !ok || msg == nil {
			return "", fmt.Errorf("token subscription closed before response")
		}
		if msg.Err != nil {
			return "", fmt.Errorf("token response error: %w", msg.Err)
		}
		token := strings.TrimSpace(string(msg.Body))
		if token == "" {
			return "", fmt.Errorf("empty token in broker response")
		}
		return token, nil
	}
}

// dialSTOMPTransport opens the underlying transport for a STOMP session.
// When cfg.TLS is nil, the result is a plain *net.TCPConn (returned through
// the net.Conn interface). When cfg.TLS is non-nil, the result is a
// *tls.Conn whose handshake has completed before return; ServerName,
// RootCAs, and Certificates on the supplied *tls.Config are honored. ctx
// bounds both the TCP dial and the TLS handshake.
//
// Errors from this helper are already wrapped with the cimstomp prefix
// and the dial address, so callers should return them as-is rather than
// re-wrapping.
func dialSTOMPTransport(ctx context.Context, cfg STOMPConfig) (net.Conn, error) {
	var dialer net.Dialer
	if cfg.TLS == nil {
		conn, err := dialer.DialContext(ctx, "tcp", cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("cimstomp: tcp dial %s: %w", cfg.Address, err)
		}
		return conn, nil
	}
	// crypto/tls Dialer honors NetDialer's context for both TCP dial and
	// TLS handshake (Go 1.15+). Clone the caller's *tls.Config so a future
	// Dial does not race against caller mutations of the same config.
	tlsDialer := &tls.Dialer{NetDialer: &dialer, Config: cfg.TLS.Clone()}
	conn, err := tlsDialer.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("cimstomp: tls dial %s: %w", cfg.Address, err)
	}
	return conn, nil
}

// normalizeDestination prepends "/queue/" to a bare GridAPPS-D destination.
// Destinations already prefixed with /queue/, /topic/, or /temp-queue/ are
// returned unchanged. go-stomp is stricter than stomp.py about explicit
// prefixes on send (see catalog open question 5).
func normalizeDestination(dest string) string {
	switch {
	case strings.HasPrefix(dest, "/queue/"),
		strings.HasPrefix(dest, "/topic/"),
		strings.HasPrefix(dest, "/temp-queue/"):
		return dest
	default:
		return "/queue/" + dest
	}
}

// tempDestCounter ensures that two calls to newTempReplyDest within the
// same nanosecond still produce distinct destinations.
var tempDestCounter atomic.Uint64

// newTempReplyDest returns a fresh /temp-queue/response.<ts> destination.
// The broker rewrites SUBSCRIBE on /temp-queue/X into a per-connection
// real temporary destination and rewrites reply-to headers to match. The
// suffix is timestamp-based to ease debugging when frames are tcpdumped.
func newTempReplyDest() string {
	n := tempDestCounter.Add(1)
	return fmt.Sprintf("/temp-queue/response.%d.%d", time.Now().UnixNano(), n)
}

// newTokenReplyDest returns a fresh regular queue for the token-bootstrap
// reply path, scoped to the given user. Mirrors the Python upstream's
// `temp.token_resp.<user>-<datetime>` convention but with a strictly
// monotonic counter to guarantee uniqueness across rapid reconnects.
func newTokenReplyDest(user string) string {
	n := tempDestCounter.Add(1)
	return fmt.Sprintf("/queue/temp.token_resp.%s.%d.%d", user, time.Now().UnixNano(), n)
}

// newCorrelationID returns a hex-encoded 16-byte random identifier for use
// in the correlation-id STOMP header. crypto/rand is used so that ids do
// not collide across processes or restarts even if a future implementation
// shares a reply queue.
func newCorrelationID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// mapCtxErr converts a context error into the package's sentinel error so
// callers can use errors.Is(err, ErrRequestTimeout). Cancellation is
// surfaced verbatim because callers may want to distinguish it.
func mapCtxErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrRequestTimeout
	}
	return err
}
