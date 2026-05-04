// Package registry provides an in-memory bidirectional mapping between
// CIM mRIDs and IEEE 2030.5 LFDIs, optionally annotated with a human
// friendly name. It mirrors the shape of the Python upstream's
// HouseLookup struct (mRID, name, lfdi) from gridappsd_adapter.py.
//
// The Registry type is safe for concurrent use. Reads vastly outnumber
// writes in normal bridge operation, so an RWMutex guards the indexes.
//
// v0 is deliberately scoped to in-memory state. File-backed loading from
// CIM query results is a downstream concern that lives in the bridge
// command package, not here. Persistence, TTL, and change notifications
// are deferred until a real consumer needs them.
package registry
