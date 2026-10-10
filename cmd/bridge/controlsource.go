package main

import (
	"context"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// embedControl gives the admin UI's watts control route the embedded server's
// in-process control call, bound to the registry it validates devices against.
type embedControl struct {
	embed *sep2embed.Embed
	reg   *registry.Registry
}

func (c embedControl) ApplyControlFor(ctx context.Context, delta sep2embed.ControlDelta, durationSeconds uint32) (sep2embed.ControlSend, error) {
	return c.embed.ApplyControlFor(ctx, c.reg, delta, durationSeconds)
}

func (c embedControl) ControlSnapshot(ctx context.Context, deviceMRID, controlID string) (sep2embed.DERControlSnapshot, bool, error) {
	return c.embed.ControlSnapshot(ctx, deviceMRID, controlID)
}

func (c embedControl) ResponsesFor(ctx context.Context, subject string, since int64) ([]sep2embed.ResponseSnapshot, error) {
	return c.embed.ResponsesFor(ctx, subject, since)
}
