//go:build integration

// Integration tests for cimstomp.Client. These tests require a live STOMP
// broker (ActiveMQ classic). Bring it up with:
//
//	docker compose up -d
//
// then run:
//
//	go test -tags=integration ./internal/cimstomp/
//
// The tests do not assume a live GridAPPS-D platform. They stand up an
// in-process STOMP client that plays the role of GridAPPS-D server: it
// subscribes to the token topic and the request queue, and replies on the
// per-request /temp-queue/... destination.

package cimstomp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3"
)

const (
	testBrokerAddr = "127.0.0.1:61613"
	testUser       = "admin"
	testPassword   = "admin"
)

// requireBroker dials the broker as a smoke test; if it is not running, the
// integration tests are skipped rather than failed.
func requireBroker(t *testing.T) {
	t.Helper()
	conn, err := stomp.Dial("tcp", testBrokerAddr,
		stomp.ConnOpt.Login(testUser, testPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		t.Skipf("STOMP broker not reachable at %s: %v (run `docker compose up -d` to enable integration tests)", testBrokerAddr, err)
	}
	_ = conn.Disconnect()
}

// fakeServer plays the role of GridAPPS-D for tests. It opens its own STOMP
// connection, subscribes to the token topic and the given request queue, and
// dispatches replies to the reply-to header.
//
// tokenProvider, if non-nil, is invoked on each token request to compute
// the token value to send. When nil, fakeServer falls back to the static
// tokenSent string. The hook is used by reconnect tests to assert that
// Reconnect refetches a fresh token rather than reusing the cached one.
type fakeServer struct {
	t             *testing.T
	conn          *stomp.Conn
	tokenSub      *stomp.Subscription
	reqSub        *stomp.Subscription
	stop          chan struct{}
	wg            sync.WaitGroup
	tokenSent     string
	tokenProvider func() string
	requests      chan recordedRequest
}

type recordedRequest struct {
	headers map[string]string
	body    []byte
}

// startFakeServer connects, subscribes to /topic/pnnl.goss.token.topic plus
// the given request queue, and serves until Stop is called. Each token
// request is replied with `tokenValue`. Each request on `requestQueue` is
// replied with `responseBody` on the request's reply-to destination.
func startFakeServer(t *testing.T, tokenValue, requestQueue string, responseBody []byte) *fakeServer {
	t.Helper()
	conn, err := stomp.Dial("tcp", testBrokerAddr,
		stomp.ConnOpt.Login(testUser, testPassword),
		stomp.ConnOpt.HeartBeat(5*time.Second, 5*time.Second),
	)
	if err != nil {
		t.Fatalf("fakeServer dial: %v", err)
	}

	tokenSub, err := conn.Subscribe("/topic/pnnl.goss.token.topic", stomp.AckAuto)
	if err != nil {
		t.Fatalf("fakeServer token subscribe: %v", err)
	}
	reqSub, err := conn.Subscribe(requestQueue, stomp.AckAuto)
	if err != nil {
		t.Fatalf("fakeServer request subscribe to %s: %v", requestQueue, err)
	}

	fs := &fakeServer{
		t:         t,
		conn:      conn,
		tokenSub:  tokenSub,
		reqSub:    reqSub,
		stop:      make(chan struct{}),
		tokenSent: tokenValue,
		requests:  make(chan recordedRequest, 16),
	}

	fs.wg.Add(2)
	go fs.serveTokens()
	go fs.serveRequests(responseBody)
	return fs
}

func (fs *fakeServer) serveTokens() {
	defer fs.wg.Done()
	for {
		select {
		case <-fs.stop:
			return
		case msg, ok := <-fs.tokenSub.C:
			if !ok || msg == nil {
				return
			}
			if msg.Err != nil {
				return
			}
			replyTo := msg.Header.Get("reply-to")
			if replyTo == "" {
				continue
			}
			// stomp.py uses /queue/<replyTo> when the destination is bare.
			dest := replyTo
			if !strings.HasPrefix(dest, "/queue/") &&
				!strings.HasPrefix(dest, "/topic/") &&
				!strings.HasPrefix(dest, "/temp-queue/") {
				dest = "/queue/" + dest
			}
			tokenValue := fs.tokenSent
			if fs.tokenProvider != nil {
				tokenValue = fs.tokenProvider()
			}
			if err := fs.conn.Send(dest, "text/plain", []byte(tokenValue)); err != nil {
				fs.t.Logf("fakeServer token send: %v", err)
				return
			}
		}
	}
}

func (fs *fakeServer) serveRequests(responseBody []byte) {
	defer fs.wg.Done()
	for {
		select {
		case <-fs.stop:
			return
		case msg, ok := <-fs.reqSub.C:
			if !ok || msg == nil {
				return
			}
			if msg.Err != nil {
				return
			}
			rec := recordedRequest{
				headers: map[string]string{},
				body:    append([]byte(nil), msg.Body...),
			}
			for i := 0; i < msg.Header.Len(); i++ {
				k, v := msg.Header.GetAt(i)
				rec.headers[k] = v
			}
			select {
			case fs.requests <- rec:
			default:
			}
			replyTo := msg.Header.Get("reply-to")
			if replyTo == "" {
				continue
			}
			if err := fs.conn.Send(replyTo, "application/json", responseBody); err != nil {
				fs.t.Logf("fakeServer reply send: %v", err)
				return
			}
		}
	}
}

func (fs *fakeServer) Stop() {
	close(fs.stop)
	_ = fs.tokenSub.Unsubscribe()
	_ = fs.reqSub.Unsubscribe()
	_ = fs.conn.Disconnect()
	fs.wg.Wait()
}

func TestIntegration_ConnectFetchesAuthToken(t *testing.T) {
	requireBroker(t)

	const expectedToken = "tok-abc-123"
	fs := startFakeServer(t, expectedToken, "/queue/goss.gridappsd.process.request.data.powergridmodel", []byte(`{"data":"ok"}`))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	if got := c.tokenForTest(); got != expectedToken {
		t.Errorf("cached token = %q, want %q", got, expectedToken)
	}
}

func TestIntegration_ConnectBrokerUnreachable(t *testing.T) {
	c := NewClient(STOMPConfig{Address: "127.0.0.1:1", User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatalf("Connect to unreachable broker: expected error, got nil")
	}
}

func TestIntegration_RequestHappyPath(t *testing.T) {
	requireBroker(t)

	const token = "happy-token"
	const reqQueue = "/queue/goss.gridappsd.process.request.data.powergridmodel"
	expectedResponse := []byte(`{"data":{"models":[]}}`)

	fs := startFakeServer(t, token, reqQueue, expectedResponse)
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	body := []byte(`{"requestType":"QUERY_MODEL_INFO","resultFormat":"JSON"}`)
	got, err := c.Request(ctx, "goss.gridappsd.process.request.data.powergridmodel", body)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(got) != string(expectedResponse) {
		t.Errorf("response body = %q, want %q", got, expectedResponse)
	}

	// Verify GOSS headers and reply-to on the recorded request.
	select {
	case rec := <-fs.requests:
		if rec.headers["GOSS_HAS_SUBJECT"] != "True" {
			t.Errorf("GOSS_HAS_SUBJECT = %q, want True", rec.headers["GOSS_HAS_SUBJECT"])
		}
		if rec.headers["GOSS_SUBJECT"] != token {
			t.Errorf("GOSS_SUBJECT = %q, want %q", rec.headers["GOSS_SUBJECT"], token)
		}
		// ActiveMQ rewrites /temp-queue/X to /remote-temp-queue/... for
		// cross-connection receivers; either form is acceptable evidence
		// that the bridge sent on the temp-queue path.
		replyTo := rec.headers["reply-to"]
		if !strings.HasPrefix(replyTo, "/temp-queue/response.") &&
			!strings.HasPrefix(replyTo, "/remote-temp-queue/") {
			t.Errorf("reply-to = %q, want /temp-queue/response.* or /remote-temp-queue/* prefix", replyTo)
		}
		// correlation-id is sent as defense-in-depth (Leon M2 / GAGO-013).
		// The broker still does per-queue correlation via /temp-queue/...; the
		// header is future-proofing if a shared reply queue ever lands.
		if got := rec.headers["correlation-id"]; got == "" {
			t.Errorf("correlation-id header missing; want non-empty value (Leon M2 defense-in-depth)")
		}
		if string(rec.body) != string(body) {
			t.Errorf("recorded body = %q, want %q", rec.body, body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fakeServer did not record any request within timeout")
	}
}

func TestIntegration_RequestPrependsQueuePrefix(t *testing.T) {
	requireBroker(t)
	// The fakeServer is subscribed to the prefixed form. The Client receives
	// the bare form and must add the prefix on send.
	fs := startFakeServer(t, "tok", "/queue/goss.gridappsd.process.request.config", []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	if _, err := c.Request(ctx, "goss.gridappsd.process.request.config", []byte("{}")); err != nil {
		t.Fatalf("Request with bare destination: %v", err)
	}
}

func TestIntegration_RequestContextDeadlineNoResponder(t *testing.T) {
	requireBroker(t)
	// No fake server: the request will go to the broker but no one replies.
	// The deadline must surface as ErrRequestTimeout. Subscribe to the token
	// topic only so Connect does not stall.
	tokenSrv := startFakeServer(t, "tok", "/queue/never.replied", []byte("{}"))
	defer tokenSrv.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer connectCancel()
	if err := c.Connect(connectCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := c.Request(ctx, "/queue/dest.with.no.responder."+timeStamp(), []byte("{}"))
	if !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("Request with no responder: got err = %v, want ErrRequestTimeout", err)
	}
}

func TestIntegration_RequestContextCancelMidFlight(t *testing.T) {
	requireBroker(t)
	tokenSrv := startFakeServer(t, "tok", "/queue/never.replied", []byte("{}"))
	defer tokenSrv.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer connectCancel()
	if err := c.Connect(connectCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	_, err := c.Request(ctx, "/queue/dest.with.no.responder.cancel."+timeStamp(), []byte("{}"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Request with mid-flight cancel: got err = %v, want context.Canceled", err)
	}
}

func TestIntegration_ConcurrentRequestsSerialize(t *testing.T) {
	requireBroker(t)

	const reqQueue = "/queue/goss.gridappsd.process.request.data.serialize"
	fs := startFakeServer(t, "tok", reqQueue, []byte("{}"))
	defer fs.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	const n = 4
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := c.Request(ctx, "goss.gridappsd.process.request.data.serialize", []byte("{}"))
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("concurrent Request[%d]: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("concurrent Request[%d]: deadline exceeded", i)
		}
	}
}

func TestIntegration_CloseIdempotent(t *testing.T) {
	requireBroker(t)
	tokenSrv := startFakeServer(t, "tok", "/queue/never.replied", []byte("{}"))
	defer tokenSrv.Stop()

	c := NewClient(STOMPConfig{Address: testBrokerAddr, User: testUser, Password: testPassword})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// timeStamp returns a unique-enough suffix for ad-hoc destination names.
func timeStamp() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
