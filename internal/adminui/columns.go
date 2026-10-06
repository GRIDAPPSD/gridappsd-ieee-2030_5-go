package adminui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// The Devices tab column ids the bridge adds. They match the server's
// ^[a-z0-9_-]{1,64}$ rule; the server drops a column that does not.
const (
	colName     = "name"
	colIdentity = "identity"
	colDERs     = "ders"
)

// deviceColumns adds the registry and discovered-DER columns to the
// server's Devices tab, one cell per EndDevice, keyed by LFDI.
type deviceColumns struct {
	registry RegistrySource
	devices  EndDeviceSource
}

func (c *deviceColumns) Columns() []sep2admin.DeviceColumn {
	return []sep2admin.DeviceColumn{
		{ID: colName, Label: "Name / mRID"},
		{ID: colIdentity, Label: "Identity"},
		{ID: colDERs, Label: "DERs"},
	}
}

// Cells answers from one registry snapshot and one roster read. A roster
// failure returns the error rather than partial cells, so the server shows
// "-" and the reason instead of a DERs count that reads as zero.
func (c *deviceColumns) Cells(ctx context.Context, lfdis []string) (map[string]map[string]string, error) {
	edevs, err := c.devices.EndDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading end devices: %w", err)
	}
	// Cells are keyed by LFDI, so two roster or registry entries sharing one
	// would merge or flip with map order. They are named instead, with ids
	// sorted so the text is the same on every call.
	ders := make(map[string][]string, len(edevs))
	edevIDs := make(map[string][]string, len(edevs))
	for _, e := range edevs {
		edevIDs[e.LFDI] = append(edevIDs[e.LFDI], e.ID)
		for _, d := range e.DERs {
			ders[e.LFDI] = append(ders[e.LFDI], d.ID)
		}
	}
	snap := c.registry.Snapshot()
	entries := make(map[string][]registry.Entry, len(snap))
	for _, e := range snap {
		entries[e.LFDI] = append(entries[e.LFDI], e)
	}

	out := make(map[string]map[string]string, len(lfdis))
	for _, lfdi := range lfdis {
		cells := map[string]string{}
		switch ids := edevIDs[lfdi]; {
		case len(ids) > 1:
			cells[colDERs] = conflict("EndDevices", ids)
		case len(ids) == 1:
			cells[colDERs] = derCell(ders[lfdi])
		}
		// An LFDI the roster did not return has no known DER count: "0"
		// would be a real value, so the cell is left to show "-".
		switch es := entries[lfdi]; {
		case len(es) > 1:
			mrids := make([]string, len(es))
			for i, e := range es {
				mrids[i] = e.MRID
			}
			cells[colName] = conflict("registry entries", mrids)
			cells[colIdentity] = cells[colName]
		case len(es) == 1:
			cells[colName] = nameCell(es[0].Name, es[0].MRID)
			cells[colIdentity] = "certificate"
			if es[0].Placeholder {
				// A placeholder LFDI is a stand-in, not a device identity.
				cells[colIdentity] = "placeholder"
			}
		}
		out[lfdi] = cells
	}
	return out, nil
}

// conflict names ids that share one LFDI, sorted.
func conflict(what string, ids []string) string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return fmt.Sprintf("conflict: %d %s share this LFDI (%s)", len(sorted), what, strings.Join(sorted, ", "))
}

// nameCell is "name / mRID", or the mRID alone for an unnamed entry.
func nameCell(name, mrid string) string {
	if name == "" {
		return mrid
	}
	return name + " / " + mrid
}

// derCell is the count, with the IDs after it when there are any.
func derCell(ids []string) string {
	if len(ids) == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (%s)", len(ids), strings.Join(ids, ", "))
}
