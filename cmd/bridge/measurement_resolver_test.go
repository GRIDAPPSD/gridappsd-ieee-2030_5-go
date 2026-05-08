package main

import (
	"testing"

	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/measurements"
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/registry"
)

// TestResolveMeasurement covers the three paths of the measurement-mRID
// resolver: side-table miss, side-table hit but registry miss, and side-
// table hit plus registry hit.
//
// The resolver is the small glue function that turns a measurement-mRID
// into an (LFDI, status) pair by consulting the side table and then the
// registry. Pump-side handler code calls it for every measurement in
// every frame.
func TestResolveMeasurement(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.Add(registry.Entry{
		MRID: "dev-1", Name: "Device One", LFDI: "lfdi-dev-1", Placeholder: true,
	}); err != nil {
		t.Fatalf("registry Add: %v", err)
	}

	tbl := measurements.New()
	if err := tbl.Add("meas-known-mapped", "dev-1"); err != nil {
		t.Fatalf("measurements Add (mapped): %v", err)
	}
	// A side-table mapping whose target is NOT in the registry: this
	// covers the case where the measurement -> equipment query returned
	// a row but the equipment did not show up in the DER enumeration.
	if err := tbl.Add("meas-known-unregistered", "dev-orphan"); err != nil {
		t.Fatalf("measurements Add (orphan): %v", err)
	}

	t.Run("side-table-miss", func(t *testing.T) {
		t.Parallel()
		got := resolveMeasurement(reg, tbl, "meas-unknown")
		if got.Status != ResolveStatusUnknownMeasurement {
			t.Errorf("Status = %v, want ResolveStatusUnknownMeasurement", got.Status)
		}
		if got.LFDI != "" {
			t.Errorf("LFDI = %q, want empty", got.LFDI)
		}
	})

	t.Run("side-table-hit-registry-miss", func(t *testing.T) {
		t.Parallel()
		got := resolveMeasurement(reg, tbl, "meas-known-unregistered")
		if got.Status != ResolveStatusUnregisteredDevice {
			t.Errorf("Status = %v, want ResolveStatusUnregisteredDevice", got.Status)
		}
		if got.DeviceMRID != "dev-orphan" {
			t.Errorf("DeviceMRID = %q, want dev-orphan", got.DeviceMRID)
		}
		if got.LFDI != "" {
			t.Errorf("LFDI = %q, want empty", got.LFDI)
		}
	})

	t.Run("side-table-hit-registry-hit", func(t *testing.T) {
		t.Parallel()
		got := resolveMeasurement(reg, tbl, "meas-known-mapped")
		if got.Status != ResolveStatusHit {
			t.Errorf("Status = %v, want ResolveStatusHit", got.Status)
		}
		if got.DeviceMRID != "dev-1" {
			t.Errorf("DeviceMRID = %q, want dev-1", got.DeviceMRID)
		}
		if got.LFDI != "lfdi-dev-1" {
			t.Errorf("LFDI = %q, want lfdi-dev-1", got.LFDI)
		}
	})
}

// TestResolveMeasurementNilTableTreatsAllAsUnknown ensures the resolver
// degrades safely when the side table is nil (population failed at
// startup but the bridge chose to continue). Every measurement resolves
// as ResolveStatusUnknownMeasurement.
func TestResolveMeasurementNilTable(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	got := resolveMeasurement(reg, nil, "meas-anything")
	if got.Status != ResolveStatusUnknownMeasurement {
		t.Errorf("Status = %v, want ResolveStatusUnknownMeasurement on nil table", got.Status)
	}
}

// TestFormatResolution pins the one-line log shape across the three
// status branches. The placeholder rendering for empty device and lfdi
// fields is intentional: log parsers downstream switch on the status
// key, not on the literal placeholder strings, but the placeholders
// must be stable so a grep for unattributed frames works.
func TestFormatResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		r    ResolveResult
		want string
	}{
		{
			name: "hit",
			r:    ResolveResult{Status: ResolveStatusHit, DeviceMRID: "dev-1", LFDI: "lfdi-1"},
			want: "frame for meas=meas-1 device=dev-1 lfdi=lfdi-1 status=hit",
		},
		{
			name: "unregistered-device",
			r:    ResolveResult{Status: ResolveStatusUnregisteredDevice, DeviceMRID: "dev-orphan"},
			want: "frame for meas=meas-1 device=dev-orphan lfdi=<unregistered> status=unregistered-device",
		},
		{
			name: "unknown-measurement",
			r:    ResolveResult{Status: ResolveStatusUnknownMeasurement},
			want: "frame for meas=meas-1 device=<unknown> lfdi=<unknown> status=unknown-measurement",
		},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatResolution("meas-1", tt.r)
			if got != tt.want {
				t.Errorf("formatResolution = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveStatusString pins the human-readable rendering used in
// pump-handler log lines so future refactors of the iota set cannot
// silently mistranslate a status into "unknown-status".
func TestResolveStatusString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		s    ResolveStatus
		want string
	}{
		{ResolveStatusUnknownMeasurement, "unknown-measurement"},
		{ResolveStatusUnregisteredDevice, "unregistered-device"},
		{ResolveStatusHit, "hit"},
		{ResolveStatus(99), "unknown-status"},
	}
	for _, tt := range cases {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("ResolveStatus(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}
