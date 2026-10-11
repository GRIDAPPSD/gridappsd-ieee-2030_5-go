package main

import (
	"context"
	"crypto/sha1"
	"fmt"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// The wrapper hands the admin UI the embed's own answers: the controls a
// device holds, and whether the bus path issued one of them.
func TestEmbedControlWrapsControlsForAndBusIssued(t *testing.T) {
	const mrid = "bat-src-1"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := registry.New()
	lfdi := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum([]byte(mrid))))
	if err := reg.Add(registry.Entry{MRID: mrid, LFDI: lfdi}); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	emb, err := newSEP2Embed(ctx, config{SEP2ServerAddr: "127.0.0.1:0", SEP2ServerCertDir: t.TempDir()},
		reg, testPolicyWithPIN(), nil, sep2embed.DeviceCertModeDevMint)
	if err != nil {
		t.Fatalf("newSEP2Embed: %v", err)
	}
	c := embedControl{embed: emb, reg: reg}

	delta := diff.Difference{
		Object:    mrid,
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 1200.0},
	}
	if out, err := emb.ApplyControlDeltaOutcome(ctx, reg, delta); err != nil || out != sep2embed.ControlIssued {
		t.Fatalf("bus delta = %d, %v; want issued", out, err)
	}
	send, err := c.ApplyControlFor(ctx, diff.Difference{
		Object:    mrid,
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 900.0},
	}, 120)
	if err != nil {
		t.Fatalf("ApplyControlFor: %v", err)
	}

	snaps, err := c.ControlsFor(ctx, mrid)
	if err != nil || len(snaps) != 3 {
		t.Fatalf("ControlsFor = %d controls, err %v; want 3 (bus, direct, follow-on)", len(snaps), err)
	}
	bus := 0
	for _, sn := range snaps {
		direct := sn.ID == send.ControlID || sn.ID == send.FollowOnID
		if got := c.BusIssued(sn.ID); got == direct {
			t.Errorf("BusIssued(%s) = %v for a direct send %v", sn.ID, got, direct)
		}
		if !direct {
			bus++
		}
	}
	if bus != 1 {
		t.Errorf("bus-issued controls = %d, want 1", bus)
	}
}
