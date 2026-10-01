package sep2embed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
)

// Status 2 on a change notification means "subscription canceled, resource
// moved", so a conformant client drops its subscription. These tests read
// the bytes a subscriber actually receives from each send path.

// subscribeCapture seeds a subscription on device A's DERProgramList whose
// notification URI is a loopback server, and returns the channel the first
// delivered body arrives on.
func subscribeCapture(t *testing.T, ctx context.Context, e string, subs interface {
	Create(context.Context, string, sep2.Subscription) error
}) (<-chan string, string) {
	t.Helper()
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case got <- string(b):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if err := subs.Create(ctx, "sub-a", sep2.Subscription{
		SubscribableResource: sep2.SubscribableResource{Resource: sep2.Resource{Href: "/edev/" + e + "/sub/1"}},
		SubscribedResource:   derProgramListHref(e, controlFSAID),
		NotificationURI:      srv.URL + "/notify",
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return got, srv.URL
}

func awaitBody(t *testing.T, got <-chan string) string {
	t.Helper()
	select {
	case b := <-got:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("no notification delivered within 3s")
		return ""
	}
}

func assertStatusZero(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, "<status>0</status>") {
		t.Errorf("notification does not carry <status>0</status>\nbody=%s", body)
	}
	if strings.Contains(body, "<status>2</status>") {
		t.Errorf("notification carries <status>2</status> (canceled, moved)\nbody=%s", body)
	}
}

func TestApplyControlDeltaNotificationCarriesStatusZero(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10,
		coresub.WithDestinationPolicy(coresub.DestinationPolicy{AllowLoopback: true}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Start(ctx)

	edevA := urlIndexFor(t, st, "mrid-a")
	got, _ := subscribeCapture(t, ctx, edevA, st.Subscriptions)

	delta := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 5000.0},
	}
	if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}
	assertStatusZero(t, awaitBody(t, got))
}

func TestExpireEndedControlsNotificationCarriesStatusZero(t *testing.T) {
	t.Parallel()

	reg, st := twoDeviceFixture(t)
	notifier := coresub.NewManager(st.Subscriptions, 1, 10,
		coresub.WithDestinationPolicy(coresub.DestinationPolicy{AllowLoopback: true}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Start(ctx)

	edevA := urlIndexFor(t, st, "mrid-a")
	delta := diff.Difference{
		Object:    "mrid-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1000.0},
	}
	if err := ApplyControlDelta(ctx, st, notifier, reg, testControlPolicy, delta); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	// Subscribe after the apply so the only delivery is the sweep's.
	got, _ := subscribeCapture(t, ctx, edevA, st.Subscriptions)

	removed, err := expireEndedControls(ctx, st, notifier, newEndedControlLedger(), servedEventEdition, 1<<40)
	if err != nil {
		t.Fatalf("expireEndedControls: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expireEndedControls removed %d controls, want 1", removed)
	}
	assertStatusZero(t, awaitBody(t, got))
}
