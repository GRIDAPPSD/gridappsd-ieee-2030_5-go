package adminui

import (
	"context"
	"fmt"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"
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
	ders := make(map[string][]string, len(edevs))
	for _, e := range edevs {
		for _, d := range e.DERs {
			ders[e.LFDI] = append(ders[e.LFDI], d.ID)
		}
	}
	entries := make(map[string]int)
	snap := c.registry.Snapshot()
	for i, e := range snap {
		entries[e.LFDI] = i
	}

	out := make(map[string]map[string]string, len(lfdis))
	for _, lfdi := range lfdis {
		cells := map[string]string{colDERs: derCell(ders[lfdi])}
		if i, ok := entries[lfdi]; ok {
			e := snap[i]
			cells[colName] = nameCell(e.Name, e.MRID)
			cells[colIdentity] = "certificate"
			if e.Placeholder {
				// A placeholder LFDI is a stand-in, not a device identity.
				cells[colIdentity] = "placeholder"
			}
		}
		out[lfdi] = cells
	}
	return out, nil
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
