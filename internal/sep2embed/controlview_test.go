package sep2embed

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/flow_reservation"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

func TestControlSnapshotFindsTheIssuedControlByItsID(t *testing.T) {
	t.Parallel()

	clock := newMovableClock(controlClockUnix)
	_, _, e, reg := newEmbedTestServer(t, func(cfg *Config) {
		cfg.DERControl.Now = clock.now
	}, "DERVIEW1")
	ctx := context.Background()

	send, err := e.ApplyControlFor(ctx, reg, targetWDelta("mrid-DERVIEW1", 0, 1500), 120)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}

	got, ok, err := e.ControlSnapshot(ctx, "mrid-DERVIEW1", send.ControlID)
	if err != nil || !ok {
		t.Fatalf("ControlSnapshot(control) = ok %v, err %v", ok, err)
	}
	if got.ID != send.ControlID || got.CurrentStatus != sep2.EventStatusActive {
		t.Errorf("control = id %q status %d, want id %q status %d", got.ID, got.CurrentStatus, send.ControlID, sep2.EventStatusActive)
	}
	if got.Base == nil || got.Base.OpModTargetW == nil || got.Base.OpModTargetW.Value != 1500 {
		t.Errorf("control base = %+v, want opModTargetW 1500", got.Base)
	}
	if got.MRID == "" {
		t.Error("control MRID is empty")
	}

	fo, ok, err := e.ControlSnapshot(ctx, "mrid-DERVIEW1", send.FollowOnID)
	if err != nil || !ok || fo.CurrentStatus != sep2.EventStatusScheduled {
		t.Errorf("follow-on = %+v ok %v err %v, want Scheduled", fo, ok, err)
	}

	if _, ok, _ := e.ControlSnapshot(ctx, "mrid-DERVIEW1", "no-such-control"); ok {
		t.Error("an unknown control id was found")
	}
	if _, ok, err := e.ControlSnapshot(ctx, "mrid-not-registered", send.ControlID); ok || err != nil {
		t.Errorf("an unknown device = ok %v err %v, want not found and no error", ok, err)
	}
}

func TestResponsesForFiltersBySubjectAndKeepsFieldValues(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	ctx := context.Background()

	if got, err := e.ResponsesFor(ctx, "ctl-1", 0); err != nil || len(got) != 0 {
		t.Fatalf("ResponsesFor on an empty store = %v, %v; want none", got, err)
	}

	received, started := sep2.ResponseStatusEventReceived, sep2.ResponseStatusEventStarted
	put := func(id, subject string, status *uint8, lfdi string, created int64) {
		t.Helper()
		r := sep2.Response{CreatedDateTime: created, EndDeviceLFDI: lfdi, Status: status, Subject: subject}
		if err := e.stores.Responses.Create(ctx, "1", id, r); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	put("a", "ctl-1", &received, "LFDI-A", 1000)
	put("b", "ctl-2", &received, "LFDI-B", 1001)
	put("c", "ctl-1", &started, "LFDI-A", 1002)
	put("d", "ctl-1", nil, "LFDI-A", 1003)

	got, err := e.ResponsesFor(ctx, "ctl-1", 0)
	if err != nil {
		t.Fatalf("ResponsesFor: %v", err)
	}
	byTime := map[int64]ResponseSnapshot{}
	for _, r := range got {
		byTime[r.CreatedDateTime] = r
	}
	if len(got) != 3 {
		t.Fatalf("ResponsesFor(ctl-1) returned %d, want 3: %+v", len(got), got)
	}
	want := map[int64]ResponseSnapshot{
		1000: {Subject: "ctl-1", EndDeviceLFDI: "LFDI-A", Status: 1, CreatedDateTime: 1000},
		1002: {Subject: "ctl-1", EndDeviceLFDI: "LFDI-A", Status: 2, CreatedDateTime: 1002},
		1003: {Subject: "ctl-1", EndDeviceLFDI: "LFDI-A", Status: 0, CreatedDateTime: 1003},
	}
	for ts, w := range want {
		if byTime[ts] != w {
			t.Errorf("response at %d = %+v, want %+v", ts, byTime[ts], w)
		}
	}
	if got, _ := e.ResponsesFor(ctx, "", 0); got != nil {
		t.Errorf("empty subject = %v, want nil", got)
	}
}

// rspKey is the key the protocol listener stores a Response under when it
// receives it at the given Unix nanosecond.
func rspKey(nano int64) string { return fmt.Sprintf("rsp-%d", nano) }

// Responses received before the send are never read, however many there are,
// and every Response after it is, past one list page.
func TestResponsesForReadsOnlyResponsesSinceTheSend(t *testing.T) {
	t.Parallel()

	const since = int64(1_700_000_000)
	received := sep2.ResponseStatusEventReceived
	cases := []struct {
		name         string
		older, newer int
	}{
		{"more older responses than one page", snapshotListLimit, 0},
		{"more newer responses than one page", 0, snapshotListLimit},
		{"one older match is not read", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _ := newTestEmbed(t)
			ctx := context.Background()
			put := func(key, subject string, created int64) {
				t.Helper()
				if err := e.stores.Responses.Create(ctx, "1", key, sep2.Response{Subject: subject, Status: &received, CreatedDateTime: created}); err != nil {
					t.Fatalf("Create %s: %v", key, err)
				}
			}
			for i := 0; i < tc.older; i++ {
				subject := "other"
				if i == tc.older-1 {
					subject = "ctl-new"
				}
				put(rspKey(since*1e9-int64(tc.older-i)), subject, since-1)
			}
			for i := 0; i < tc.newer; i++ {
				put(rspKey(since*1e9+int64(i)), "other", since)
			}
			put(rspKey((since+5)*1e9), "ctl-new", since+5)

			got, err := e.ResponsesFor(ctx, "ctl-new", since)
			if err != nil {
				t.Fatalf("ResponsesFor: %v", err)
			}
			if len(got) != 1 || got[0].CreatedDateTime != since+5 {
				t.Errorf("ResponsesFor = %+v, want only the response at %d", got, since+5)
			}
		})
	}
}

// The key ResponsesFor starts after must sort below what the protocol
// listener stores, so this goes through its real POST handler.
func TestResponsesForFindsAResponseThroughTheProtocolHandler(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	mux := http.NewServeMux()
	allow := func(_ *http.Request, lfdi string) (bool, string, error) { return true, lfdi, nil }
	mux.HandleFunc("POST /rsps/{rspsId}/rsp", flow_reservation.HandlePostResponse(e.stores.Responses, allow))
	status := sep2.ResponseStatusEventReceived
	body, err := xml.Marshal(&sep2.DERControlResponse{Response: sep2.Response{
		EndDeviceLFDI: "ABCDEF0123456789ABCDEF0123456789ABCDEF01", Status: &status, Subject: "0123456789ABCDEF0123456789ABCDEF",
	}})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/rsps/1/rsp", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST response = %d: %s", w.Code, w.Body)
	}

	got, err := e.ResponsesFor(context.Background(), "0123456789ABCDEF0123456789ABCDEF", before)
	if err != nil || len(got) != 1 || got[0].Status != status {
		t.Fatalf("ResponsesFor since the post = %+v, %v; want the one Received response", got, err)
	}
	if later, err := e.ResponsesFor(context.Background(), "0123456789ABCDEF0123456789ABCDEF", before+60); err != nil || len(later) != 0 {
		t.Errorf("ResponsesFor since a minute later = %+v, %v; want none", later, err)
	}
}

// listFailing answers every List with err.
type listFailing struct {
	store.ScopedStore[sep2.Response]
	err error
}

func (l listFailing) List(context.Context, string, store.ListOptions) (store.ListResult[sep2.Response], error) {
	return store.ListResult[sep2.Response]{}, l.err
}

func TestResponsesForNotFoundIsEmptyAndOtherErrorsAreReturned(t *testing.T) {
	t.Parallel()

	e, _ := newTestEmbed(t)
	e.stores.Responses = listFailing{ScopedStore: e.stores.Responses, err: fmt.Errorf("wrapped: %w", store.ErrNotFound)}
	if got, err := e.ResponsesFor(context.Background(), "ctl-1", 0); err != nil || got != nil {
		t.Errorf("not found = %v, %v; want nil and no error", got, err)
	}

	boom := errors.New("store down")
	e.stores.Responses = listFailing{ScopedStore: e.stores.Responses, err: boom}
	if got, err := e.ResponsesFor(context.Background(), "ctl-1", 0); !errors.Is(err, boom) || got != nil {
		t.Errorf("store failure = %v, %v; want nil and the store error", got, err)
	}
}
