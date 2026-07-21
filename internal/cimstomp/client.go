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
// lifetime of the connection (not the Client); each Reconnect refetches.
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
// Request, Connect, Reconnect, and Close all serialize via an internal
// mutex so that v0 issues a single in-flight Request at a time and the
// connect/disconnect lifecycle is observed atomically by all callers.
//
// Lifecycle invariants:
//
//	New -> Connect -> { Request | Subscribe | Reconnect }* -> Close (terminal).
//
// After Close, neither Connect nor Reconnect succeed; both return
// ErrClosed. A Client whose Close has been called is single-shot: to
// reuse a lifecycle, construct a new Client via NewClient.
//
// TOCTOU contract: a Close that races a mid-dial Connect or Reconnect
// causes the just-dialed conn to be closed cleanly and the racing call
// to return ErrClosed. The Client never settles into "closed=true with
// a live conn" or "closed=true with a non-empty token". This is enforced
// by re-checking c.closed under c.mu after the dial completes.
//
// Connection-loss detection: v0 is active. Request and Publish wrap
// transport-level errors (go-stomp ErrAlreadyClosed,
// ErrClosedUnexpectedly, io.EOF, net.ErrClosed) as ErrConnectionLost so
// callers can errors.Is and call Reconnect. Passive heartbeat-driven
// reconnection is a future enhancement; see GAGO-012 follow-ups.
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
// guarded against here. To reconnect after a transport failure, use
// Reconnect (GAGO-012).
//
// TOCTOU: Connect's outer closed.Load() is a fast-path early return.
// The authoritative check happens under c.mu after the dial completes:
// if Close ran while we were dialing, the just-dialed conn is closed
// and ErrClosed is returned. The Client never settles into "closed=true
// with a live conn" (Leon GAGO-013 review M-1).
func (c *Client) Connect(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	conn, token, err := c.dialAndBootstrap(ctx)
	if err != nil {
		return err
	}

	// Authoritative TOCTOU re-check under the mutex. If Close ran while
	// we were dialing, abandon the new conn and return ErrClosed. The
	// closed flag is the terminal state; once set, no future Connect or
	// Reconnect may install a live conn or token.
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		logDisconnectErr(conn.Disconnect(), "Connect.afterClose")
		return ErrClosed
	}
	c.conn = conn
	c.token = token
	c.connected.Store(true)
	c.mu.Unlock()

	return nil
}

// dialAndBootstrap performs the work that runs without c.mu held:
// dial the transport, run the STOMP handshake, and fetch the auth
// token. Caller installs the result under c.mu after a final closed
// re-check. Errors are wrapped at the helper boundary; callers should
// return them as-is.
//
// On any failure, all partial state (TCP socket, STOMP conn) is torn
// down before return, so the caller does not need to clean up on the
// error path.
//
// c.cfg is snapshotted (a full struct copy) into a local under c.mu
// before any lock-free work begins, and every subsequent use in this
// function reads the local, not c.cfg. Close zeroes c.cfg.Password
// under c.mu (GAGO-015); without this snapshot, the lock-free dial
// section below would race that write every time a Connect or
// Reconnect overlaps a Close, because a bare `c.cfg` reference (even
// one only used to pass the struct by value to another function)
// touches every field, Password included (GAGO-015 follow-up,
// Leon/Dutch race finding on the polish sweep).
func (c *Client) dialAndBootstrap(ctx context.Context) (*stomp.Conn, string, error) {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	// go-stomp v3.1.5's DialWithContext calls net.Dial (not net.DialContext),
	// so a ctx deadline is ignored at the TCP layer. Dial ourselves with
	// net.DialContext to honor ctx, then hand the live conn to
	// stomp.ConnectWithContext which observes ctx for the STOMP handshake.
	// When cfg.TLS is non-nil, wrap the TCP connection with crypto/tls
	// before handing it to stomp.ConnectWithContext (GAGO-014).
	tcp, err := dialSTOMPTransport(ctx, cfg)
	if err != nil {
		return nil, "", err
	}

	conn, err := stomp.ConnectWithContext(ctx, tcp,
		stomp.ConnOpt.Login(cfg.User, cfg.Password),
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
		return nil, "", fmt.Errorf("cimstomp.Client: stomp connect %s: %w", cfg.Address, err)
	}

	token, err := fetchAuthToken(ctx, conn, cfg.User, cfg.Password)
	if err != nil {
		// STOMP connection is up but token bootstrap failed; tear it down
		// and log any Disconnect error rather than swallowing it (Leon H2).
		logDisconnectErr(conn.Disconnect(), "Connect.fetchAuthToken")
		return nil, "", fmt.Errorf("cimstomp.Client: fetch auth token: %w", err)
	}

	return conn, token, nil
}

// Reconnect tears down the current STOMP connection (if any) and
// re-establishes it, including a fresh auth-token bootstrap. Use after
// a transport-level failure (broker drop, heartbeat timeout) detected
// by Request, Subscribe, or Publisher.Publish returning a wrapped
// connection error (errors.Is(err, ErrConnectionLost)).
//
// Reconnect blocks until the new connection is established or ctx is
// canceled. Returns ErrClosed if the Client has been Closed; in that
// case construct a new Client.
//
// Reconnect is mutex-serialized with Connect, Close, and Request, so
// it is safe to call concurrent with in-flight requests. The behavior
// for in-flight callers is asymmetric:
//
//   - Request callers wait on c.mu for the swap and then run against
//     the new conn (their go-stomp handle is replaced before their
//     critical section runs).
//   - Subscribe consumers do NOT see the swap. The underlying
//     *stomp.Conn is torn down; the listener goroutine sees its
//     stomp.Subscription channel close and exits with a wrapped
//     ErrConnectionLost (Subscription.Err returns it). Callers must
//     call Subscribe again on the new connection to keep receiving
//     frames. Pump-level orchestration of resubscribe-on-Reconnect is
//     the caller's responsibility (deferred to a future ticket).
//
// Concurrent Reconnect calls are safe, not merely non-crashing: each
// caller that dials a new conn either installs it or, if a sibling
// Reconnect installed one first, Disconnects its own redundant conn
// under the mutex (see the supersede loop below) so exactly one
// broker session survives and no session leaks (GAGO-012 Dutch C1 /
// Leon H1, reworded for GAGO-024 Dutch L1 once that fix had settled).
//
// Transport-level failures during the dial or token bootstrap are
// wrapped so callers can errors.Is(err, ErrConnectionLost) and drive
// retry policy. ErrClosed remains its own sentinel for the
// Closed-Client case.
//
// SECURITY INVARIANT (per GAGO-012 spec): the cached auth token is
// discarded before reconnect; the new connection re-fetches via the
// /topic/pnnl.goss.token.topic dance. Token reuse across reconnects
// is forbidden.
//
// TOCTOU: identical contract to Connect. closed is re-checked under
// c.mu after the dial; a Close racing with Reconnect causes the new
// conn to be torn down and ErrClosed returned.
func (c *Client) Reconnect(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return mapCtxErr(err)
	}

	// Swap out the existing connection fields under the mutex first, so
	// that concurrent Request callers see ErrNotConnected during the
	// dial rather than a closed go-stomp handle (GAGO-024 Dutch L3: the
	// mutex covers only this field swap, not the Disconnect call below,
	// which runs after c.mu.Unlock so the actual teardown round-trip
	// with the broker does not hold the lock). Holding c.mu across the
	// dial would also work, but the lock-free window during the dial
	// keeps Request fast-fail-able under broker-drop conditions.
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return ErrClosed
	}
	old := c.conn
	c.conn = nil
	c.token = ""
	c.connected.Store(false)
	c.mu.Unlock()

	if old != nil {
		// Disconnect the prior conn before dialing the new one. Errors
		// here are surface-only; the connection is being abandoned.
		logDisconnectErr(old.Disconnect(), "Reconnect.oldConn")
	}

	conn, token, err := c.dialAndBootstrap(ctx)
	if err != nil {
		// Reconnect failures during dial / TLS handshake / STOMP frame
		// layer / token bootstrap are by definition transport-level: we
		// could not establish a session with the broker. Label them as
		// ErrConnectionLost so retry-loop callers can
		// errors.Is(err, ErrConnectionLost) and drive reconnect policy
		// (Dutch H2). Context errors are passed through unlabelled so
		// callers can distinguish "we cancelled" from "broker
		// unreachable"; ErrClosed is its own sentinel handled above.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRequestTimeout) {
			return err
		}
		return fmt.Errorf("cimstomp.Client: reconnect: %w", errors.Join(ErrConnectionLost, err))
	}

	// Authoritative TOCTOU re-check under the mutex. Close can run
	// during the dial; if it has, abandon the new conn.
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		logDisconnectErr(conn.Disconnect(), "Reconnect.afterClose")
		return ErrClosed
	}
	// Plug the concurrent-Reconnect session leak (Dutch C1 / Leon H1).
	// Two goroutines that both entered Reconnect each released c.mu with
	// c.conn == nil before dialing. If a sibling Reconnect installed a
	// fresh conn while we were dialing, we must Disconnect that conn
	// before overwriting it; otherwise its broker-side session leaks.
	// Token assignment does not need this dance because Go strings are
	// values with no resource to release.
	//
	// We loop because Disconnect releases c.mu (Disconnect itself can
	// block on a RECEIPT round-trip with the broker), and during that
	// window yet another sibling Reconnect can install a fresh conn.
	// Each iteration shrinks the in-flight set by one; the loop
	// terminates when we observe c.conn == nil under the lock.
	for c.conn != nil {
		existing := c.conn
		c.conn = nil
		c.mu.Unlock()
		logDisconnectErr(existing.Disconnect(), "Reconnect.superseded")
		c.mu.Lock()
		if c.closed.Load() {
			c.mu.Unlock()
			logDisconnectErr(conn.Disconnect(), "Reconnect.afterClose")
			return ErrClosed
		}
	}
	c.conn = conn
	c.token = token
	c.connected.Store(true)
	c.mu.Unlock()

	return nil
}

// Close disconnects the STOMP session and clears both the cached auth
// token and the configured password from the Client so further Request
// calls fail with ErrNotConnected and the local Client struct no longer
// holds a live credential reference. It is idempotent: calling Close on
// a never-connected or already-closed Client returns nil. Connect cannot
// be called after Close (returns ErrClosed); construct a new Client to
// reuse. Close is terminal, so zeroing c.cfg.Password here is safe: no
// later Reconnect can need it.
//
// Credential clearing is best-effort. Go strings are immutable, so the
// heap allocation that backed c.token or c.cfg.Password is unreachable
// from this Client but the underlying bytes are not overwritten until
// garbage collected. Earlier copies in the fetchAuthToken read path and
// in c.cfg.Password's original caller-supplied string also live on. A
// memory dump of a long-lived process can still surface credentials
// (Leon L1, L3).
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
	c.cfg.Password = ""
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
		return nil, wrapTransportErr(fmt.Sprintf("cimstomp.Client: subscribe %s", replyTo), err)
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
		return nil, wrapTransportErr(fmt.Sprintf("cimstomp.Client: send to %s", dest), err)
	}

	// Wait for either the response frame or context cancellation. The
	// subscription channel is the goroutine-safe exit path; closing the
	// subscription via Unsubscribe in the deferred call drains it.
	select {
	case <-ctx.Done():
		return nil, mapCtxErr(ctx.Err())

	case msg, ok := <-sub.C:
		if !ok || msg == nil {
			// Channel closed without a frame: the broker tore down our
			// subscription, which we report as a connection loss so the
			// caller can errors.Is(err, ErrConnectionLost) and Reconnect.
			return nil, wrapTransportErr("cimstomp.Client: subscription closed before response", stomp.ErrClosedUnexpectedly)
		}
		if msg.Err != nil {
			return nil, wrapTransportErr("cimstomp.Client: response error", msg.Err)
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
	// On exit, drain any pending frames before Unsubscribe. The token
	// bootstrap uses a regular /queue/ destination (not /temp-queue/)
	// to match the Python upstream's bootstrap convention. The
	// Unsubscribe drops the consumer but ActiveMQ keeps the empty
	// queue. Long-running reconnect cycles accumulate empty
	// temp.token_resp.<user>.* queues on the broker; draining is good
	// hygiene but does not delete the queue. Operational mitigation
	// (broker-side TTL on temp.token_resp.* pattern) lives in
	// CLAUDE.md (GAGO-012).
	defer func() {
		drainStompChan(sub.C)
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
			// Document this case: an all-whitespace or zero-length
			// token from the broker is not a wire-protocol error but
			// makes the auth header useless on every later Request.
			// Surface as a Connect/Reconnect failure so the caller
			// sees it immediately rather than at first Request time.
			// GAGO-022 L2 / Pike note: reconnect-loop on this is a
			// caller decision; cimstomp does not retry internally.
			return "", fmt.Errorf("empty token in broker response")
		}
		return token, nil
	}
}

// drainStompChan empties any frames currently buffered on a
// stomp.Subscription channel without blocking. Returns the number of
// frames consumed. Used before Unsubscribe on the token-bootstrap
// path: the channel is drained, then Unsubscribe drops the consumer.
//
// This is good client-side hygiene only (GAGO-024 Dutch L2): it does
// NOT reduce broker-side queue accumulation. Unsubscribe drops the
// consumer but ActiveMQ keeps the (now empty) temp.token_resp.<user>.*
// queue; draining just avoids leaving unread frames orphaned in the
// local channel. See fetchAuthToken's doc comment for the accurate
// broker-side story and the operational mitigation (GAGO-012).
//
// drainStompChan does not close the channel and does not block. It is
// safe to call on an empty channel (returns 0) and on a channel still
// owned by an active subscription (returns whatever is buffered at
// the moment of the call).
func drainStompChan(ch <-chan *stomp.Message) int {
	n := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return n
			}
			n++
		default:
			return n
		}
	}
}

// dialSTOMPTransport opens the underlying transport for a STOMP session.
// When cfg.TLS is nil, the result is a plain *net.TCPConn (returned through
// the net.Conn interface). When cfg.TLS is non-nil, the result is a
// *tls.Conn whose handshake has completed before return; ServerName,
// RootCAs, and Certificates on the supplied *tls.Config are honored. ctx
// bounds both the TCP dial and the TLS handshake.
//
// The cloned *tls.Config has MinVersion floored to tls.VersionTLS12 if the
// caller leaves it as zero. Callers who explicitly set MinVersion (for
// example, to TLS 1.3) keep their setting; the floor only applies when the
// field has its zero value, which would otherwise allow the deprecated
// SSL 3.0 / TLS 1.0 / TLS 1.1 negotiation paths.
//
// Errors from this helper are already wrapped with the cimstomp prefix
// and the dial address, so callers should return them as-is rather than
// re-wrapping. The helper uses the shorter "cimstomp:" prefix instead of
// "cimstomp.Client:" or "cimstomp.Publisher:" because both Client.Connect
// and Publisher.Connect share this code path; tagging it with one
// caller's name would be misleading when read in a stack trace from the
// other (GAGO-022 L3).
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
	// Dial does not race against caller mutations of the same config, then
	// enforce a TLS 1.2 floor on the clone if the caller did not set
	// MinVersion explicitly (Leon M1).
	tlsCfg := cfg.TLS.Clone()
	if tlsCfg.MinVersion == 0 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	tlsDialer := &tls.Dialer{NetDialer: &dialer, Config: tlsCfg}
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
