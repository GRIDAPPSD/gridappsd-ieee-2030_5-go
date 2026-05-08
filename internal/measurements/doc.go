// Package measurements provides an in-memory side table that maps
// CIM measurement-mRIDs to their parent ConductingEquipment mRID. The
// table sits next to internal/registry: incoming GridAPPS-D simulation
// frames are keyed by measurement-mRID, while the bridge's mRID-to-LFDI
// registry is keyed by device-mRID. Resolving a frame to an LFDI is a
// two-step lookup: measurement-mRID -> equipment-mRID via this side
// table, then equipment-mRID -> LFDI via the registry.
//
// The Table type is safe for concurrent use. Reads vastly outnumber
// writes in steady-state bridge operation, so an RWMutex guards the
// internal state. Stats counters use atomic increments so the read
// path stays inside an RLock.
//
// Population is the bridge's responsibility: at startup, run the
// cim.Client.QueryMeasurements wrapper, project each binding row down
// to a Mapping, and call AddBatch. The package itself is unaware of
// CIM or SPARQL; it is a string-to-string lookup with hit-rate metrics.
package measurements
