package main

import (
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/measurements"
	"github.com/GRIDAPPSD/gridappsd-2030_5-go/internal/registry"
)

// ResolveStatus is the outcome bucket of a measurement-mRID lookup. The
// bridge handler logs the (mRID, status) pair so an operator can grep
// for unattributed frames at runtime.
type ResolveStatus int

const (
	// ResolveStatusUnknownMeasurement: the side table did not contain
	// the measurement-mRID. Either the SPARQL enumeration missed it at
	// startup, or the platform emitted a frame for a measurement that
	// was added after the bridge bootstrapped.
	ResolveStatusUnknownMeasurement ResolveStatus = iota

	// ResolveStatusUnregisteredDevice: the side table mapped the
	// measurement-mRID to a device-mRID, but the registry has no entry
	// for that device. This usually means the DER enumeration queries
	// (QueryInverter / QuerySolar / QueryBattery) did not include the
	// device class that owns the measurement.
	ResolveStatusUnregisteredDevice

	// ResolveStatusHit: the side table mapped the measurement-mRID and
	// the registry has an entry for the resolved device.
	ResolveStatusHit
)

// String renders ResolveStatus for log readability. It is not part of
// the resolver's wire contract; callers can switch on the iota.
func (s ResolveStatus) String() string {
	switch s {
	case ResolveStatusUnknownMeasurement:
		return "unknown-measurement"
	case ResolveStatusUnregisteredDevice:
		return "unregistered-device"
	case ResolveStatusHit:
		return "hit"
	default:
		return "unknown-status"
	}
}

// ResolveResult is the slim outcome of a measurement-mRID lookup.
// DeviceMRID and LFDI are empty unless populated by the resolution
// chain that produced the corresponding Status.
type ResolveResult struct {
	Status     ResolveStatus
	DeviceMRID string
	LFDI       string
}

// resolveMeasurement walks the measurement-mRID -> device-mRID -> LFDI
// chain. A nil side table is treated as an empty table: every lookup
// surfaces as ResolveStatusUnknownMeasurement. This matches the
// expected behavior when bridge startup ran without a CIM measurement
// query (e.g., the platform was unreachable for that one query but the
// rest of bootstrap completed and the bridge was allowed to keep
// running).
func resolveMeasurement(reg *registry.Registry, tbl *measurements.Table, measMRID string) ResolveResult {
	if tbl == nil {
		return ResolveResult{Status: ResolveStatusUnknownMeasurement}
	}
	deviceMRID, ok := tbl.Lookup(measMRID)
	if !ok {
		return ResolveResult{Status: ResolveStatusUnknownMeasurement}
	}
	lfdi, ok := reg.LFDI(deviceMRID)
	if !ok {
		return ResolveResult{
			Status:     ResolveStatusUnregisteredDevice,
			DeviceMRID: deviceMRID,
		}
	}
	return ResolveResult{
		Status:     ResolveStatusHit,
		DeviceMRID: deviceMRID,
		LFDI:       lfdi,
	}
}
