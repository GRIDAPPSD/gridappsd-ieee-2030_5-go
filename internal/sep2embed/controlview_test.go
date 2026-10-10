package sep2embed

import (
	"context"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
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

	if got, err := e.ResponsesFor(ctx, "ctl-1"); err != nil || len(got) != 0 {
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

	got, err := e.ResponsesFor(ctx, "ctl-1")
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
	if got, _ := e.ResponsesFor(ctx, ""); got != nil {
		t.Errorf("empty subject = %v, want nil", got)
	}
}
