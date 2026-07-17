package gridappsdclient

import (
	"context"
	"errors"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-go/fieldbus"
)

// fakeMessageBus implements fieldbus.MessageBus for tests. Only
// GetResponse is exercised by Requester; the other methods record
// nothing and are present solely to satisfy the interface.
type fakeMessageBus struct {
	// captured inputs from the last GetResponse call
	gotCtx         context.Context
	gotDestination string
	gotContentType string
	gotBody        []byte

	// configured outputs
	resp []byte
	err  error
}

func (f *fakeMessageBus) Connect(ctx context.Context) error { return nil }
func (f *fakeMessageBus) Disconnect() error                 { return nil }
func (f *fakeMessageBus) IsConnected() bool                 { return true }

func (f *fakeMessageBus) Subscribe(ctx context.Context, destination string, h fieldbus.Handler) (fieldbus.Token, error) {
	return 0, errors.New("fakeMessageBus: Subscribe not used by Requester tests")
}

func (f *fakeMessageBus) Unsubscribe(ctx context.Context, destination string, tok fieldbus.Token) error {
	return nil
}

func (f *fakeMessageBus) Send(ctx context.Context, destination, contentType string, body []byte) error {
	return errors.New("fakeMessageBus: Send not used by Requester tests")
}

func (f *fakeMessageBus) GetResponse(ctx context.Context, destination, contentType string, body []byte) ([]byte, error) {
	f.gotCtx = ctx
	f.gotDestination = destination
	f.gotContentType = contentType
	// Copy so the test can mutate the source slice after the call without
	// corrupting what was captured (mirrors internal/cim's mockRequester).
	f.gotBody = append([]byte(nil), body...)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// compile-time assertion: fakeMessageBus must satisfy fieldbus.MessageBus.
var _ fieldbus.MessageBus = (*fakeMessageBus)(nil)

func TestRequester_RequestForwardsDestinationContentTypeAndBody(t *testing.T) {
	t.Parallel()

	bus := &fakeMessageBus{resp: []byte(`{"data":{"ok":true}}`)}
	r := NewRequester(bus)

	const dest = "goss.gridappsd.process.request.data.powergridmodel"
	body := []byte(`{"requestType":"QUERY"}`)

	ctx := context.Background()
	got, err := r.Request(ctx, dest, body)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if string(got) != string(bus.resp) {
		t.Errorf("Request returned %q, want %q", got, bus.resp)
	}
	if bus.gotDestination != dest {
		t.Errorf("GetResponse destination = %q, want %q", bus.gotDestination, dest)
	}
	if bus.gotContentType != requestContentType {
		t.Errorf("GetResponse contentType = %q, want %q", bus.gotContentType, requestContentType)
	}
	if string(bus.gotBody) != string(body) {
		t.Errorf("GetResponse body = %q, want %q", bus.gotBody, body)
	}
	if bus.gotCtx != ctx {
		t.Errorf("GetResponse ctx not propagated: got %v, want %v", bus.gotCtx, ctx)
	}
}

func TestRequester_RequestMutatingSourceBodyAfterCallDoesNotAffectCapturedBytes(t *testing.T) {
	t.Parallel()

	bus := &fakeMessageBus{resp: []byte(`{"data":{}}`)}
	r := NewRequester(bus)

	body := []byte(`{"a":1}`)
	if _, err := r.Request(context.Background(), "dest", body); err != nil {
		t.Fatalf("Request: %v", err)
	}
	body[0] = 'X'
	if string(bus.gotBody) == string(body) {
		t.Errorf("captured body aliases the caller's slice; want an independent copy on the wire path")
	}
}

func TestRequester_RequestWrapsUnderlyingError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("broker said no")
	bus := &fakeMessageBus{err: wantErr}
	r := NewRequester(bus)

	got, err := r.Request(context.Background(), "dest", []byte(`{}`))
	if got != nil {
		t.Errorf("Request returned non-nil bytes %q on error, want nil", got)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Request error = %v, want wrapping %v", err, wantErr)
	}
}

func TestRequester_RequestReturnsEmptyReplyVerbatim(t *testing.T) {
	t.Parallel()

	// An empty (but non-nil vs nil) reply is a distinct wire signal per
	// data-invariants Rule "empty is not absent, null is not empty";
	// Requester must not paper over the distinction.
	bus := &fakeMessageBus{resp: []byte{}}
	r := NewRequester(bus)

	got, err := r.Request(context.Background(), "dest", []byte(`{}`))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got == nil {
		t.Errorf("Request returned nil for an empty (non-nil) reply; want the empty slice preserved")
	}
	if len(got) != 0 {
		t.Errorf("Request returned %q, want empty", got)
	}
}
