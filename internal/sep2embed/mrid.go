package sep2embed

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// mridHexChars is the number of hexadecimal characters in every mRID this
// package emits: 32, encoding 16 bytes.
//
// IEEE 2030.5 types mRIDType as hexBinary with a 16-octet maximum length
// (section 10.2.2 / sep.xsd's mRIDType), so 32 hex characters is the
// largest legal mRID and any value that is not an even-length run of hex
// digits is not an mRID at all. This is not a soft style preference: a
// schema-driven client rejects the WHOLE document on a single malformed
// simple value. The EPRI reference client is the concrete case measured
// for GAGO-094: its schema table types mRID as
// xs_type(XS_HEX_BINARY,16) (se_schema.c, e.g. the DERProgram and
// FunctionSetAssignments blocks), its parse_hex helper accepts only hex
// digit pairs (xml_parse.c), and parse_doc wraps every simple-value parse
// in ok(), which is "return NULL on failure" (util.c). So one non-hex
// mRID does not degrade one field, it makes the entire resource
// unparseable and the client silently discards it.
const mridHexChars = 32

// deriveResourceMRID returns the mRID for one 2030.5 resource belonging to
// the device identified by lfdi: 32 uppercase hex characters, derived
// deterministically from the device's own canonical LFDI plus a kind
// discriminator naming the resource within that device.
//
// Why derived rather than composed: the obvious composition, lfdi + "-" +
// kind, is what this bridge used to emit (control.go's DERControl and
// DefaultDERControl mRIDs before GAGO-094), and it is not a legal mRID on
// two counts at once: it is 40+ characters where 32 is the ceiling, and
// the "-" and the "kind" text are not hex digits. Measured against the
// EPRI client's own parser, a DERControl or DefaultDERControl carrying
// such an mRID is rejected outright, so the control this bridge worked to
// produce never reaches the device's scheduler.
//
// Why the LFDI is the input: mRIDs must be stable across restarts (a
// client that has already scheduled an event keys it by mRID: EPRI hashes
// event blocks by mRID, schedule.c's mrid_key) and distinct per device (a
// shared mRID would collide in exactly that hash, so one device's control
// would displace another's). Deriving from the device's canonical
// LFDI, the identity every other seeded resource is already keyed by
// (see seed.go's doc comment), satisfies both without introducing a
// second identity scheme or a counter whose value depends on iteration
// order. kind keeps the per-device resources distinct from one another,
// so a device's FSA, DERProgram, DefaultDERControl, and DERControl do not
// collide with each other either.
//
// The truncation to 16 bytes is a length constraint imposed by the wire
// type, not a security boundary: mRIDs are public identifiers, and
// nothing authenticates or authorizes on them (ownership is enforced by
// the per-device ACL against the TLS-presented LFDI; see acl.go). It is
// therefore safe to truncate here in a way it would not be for a
// credential.
func deriveResourceMRID(lfdi, kind string) string {
	sum := sha256.Sum256([]byte(lfdi + "|" + kind))
	return strings.ToUpper(hex.EncodeToString(sum[:mridHexChars/2]))
}

// deriveControlMRID returns the mRID for one GENERATION of a device's
// DERControl: 32 uppercase hex characters, derived from the device's LFDI,
// the control kind, and that generation's creationTime.
//
// Why a DERControl needs a varying mRID where every other resource wants a
// stable one. mRID is an event's IDENTITY to a client scheduler, and a
// client that has already accepted and actuated an event does not re-act on
// the same identity. Measured on the EPRI reference client: schedule_event
// short-circuits on hash_get(s->blocks, ev->mRID), so a document carrying an
// already-known mRID creates no new EventBlock, and activate_block fires
// EVENT_START (the callback that actually pushes the setpoint to the
// inverter) only when the block's status is not already Active. Worse,
// update_existing's equal-mRID branch copies ONLY the incoming EventStatus
// and frees the rest, so a changed opModTargetW is discarded at parse time.
// A new command must therefore arrive as a new event identity.
//
// Why creationTime is the discriminator. It is already the value a client
// compares to resolve equal-primacy supersession
// (block_supersede: x->creationTime > y->creationTime), and
// nextControlCreationTime already guarantees it advances STRICTLY on every
// replacement, including two deltas inside one wall-clock second and across a
// backward clock step. Deriving the identity from it makes the identity and
// the supersession discriminator advance together by construction: there is
// no way to ship a fresh mRID with a stale creationTime (silently rejected as
// Superseded) or a stale mRID with a fresh creationTime (silently ignored),
// because both come from the same number.
//
// It also replaces a separate generation counter that used to be encoded in
// the served href. That coupling is gone: the href is now stable so an
// activated event stays fetchable (see controlHref), which left creationTime
// as the one strictly-advancing value already stored on the record. Reading
// the discriminator back off the record keeps the stored control
// self-describing, with no process-memory counter that could drift from the
// store and re-issue an identity a client has already scheduled.
//
// The truncation and the hex-only output are the same wire constraints
// deriveResourceMRID documents: mRIDType is hexBinary capped at 16 octets,
// and a schema-driven client rejects the entire resource on a malformed
// value. Hashing rather than formatting the discriminator into the string is
// what keeps that guarantee: the output is 32 hex characters for a
// creationTime of 0 and for one far past 2038 alike, with no length or
// character-class change as the number grows.
func deriveControlMRID(lfdi, kind string, creationTime int64) string {
	return deriveResourceMRID(lfdi, kind+"|ct/"+strconv.FormatInt(creationTime, 10))
}

// The kind discriminators passed to deriveResourceMRID. Each names one
// resource within a device's own 2030.5 subtree, and they are declared
// together here so a new resource cannot accidentally reuse an existing
// discriminator (which would collide the two resources' mRIDs). The
// values mirror the corresponding href path segments, which is convention
// only: nothing parses them.
const (
	fsaMRIDKind   = "fsa/1"
	derpMRIDKind  = "derp/1"
	ddercMRIDKind = "dderc"

	// dercMRIDKind is passed to deriveControlMRID, not deriveResourceMRID:
	// a DERControl's mRID varies per generation, so this is only the
	// stable prefix and deriveControlMRID appends the generation
	// discriminator. Every generation's mRID therefore differs from the
	// single value the previous stable scheme emitted, which is the
	// intended behavior change and not a compatibility concern: mRIDs are
	// not persisted across bridge restarts (the stores are in-memory) and
	// nothing outside a live client's own scheduler holds one.
	dercMRIDKind = "derc/active"
)

// maxDescriptionChars is the effective ceiling, in characters, on any
// 2030.5 description element this package emits.
//
// The type is String32, and a schema-driven client rejects the whole
// containing resource when the value is too long rather than truncating
// the one field. The EPRI reference client's XS_STRING check is
// "reject when strlen(data) > n-1" with n = 32, so 31 characters is the
// largest value that parses. Choosing 31 rather than 32 is therefore not
// excess caution, it is the measured boundary.
const maxDescriptionChars = 31
