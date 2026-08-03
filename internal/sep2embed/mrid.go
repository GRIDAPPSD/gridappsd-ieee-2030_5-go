package sep2embed

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// mRID kind tags. Each names one resource class the bridge mints an mRID for.
// The tag is mixed into the digest below so two resources belonging to the
// same device get different mRIDs, which is what the previous
// "<LFDI>-<suffix>" scheme was reaching for.
const (
	mridKindFSA               = "fsa"
	mridKindDERProgram        = "derp"
	mridKindDERControl        = "derc"
	mridKindDefaultDERControl = "dderc"
)

// deriveMRID returns a deterministic, schema-valid IEEE 2030.5 mRID for the
// given resource kind on the given device.
//
// WHY THIS EXISTS. sep.xsd types mRID as mRIDType, which is HexBinary128:
// exactly 16 bytes, so exactly 32 hexadecimal characters and nothing else.
// The bridge previously minted mRIDs as "<LFDI>-<suffix>" (for example
// "<40 hex chars>-active"). That value is invalid twice over: it is 40-plus
// characters rather than 32, and "-active" is not hexadecimal at all.
//
// This is not a cosmetic schema nit. A conformant client parses mRID as
// hexBinary and fails the whole document when it is not. The EPRI reference
// client (se_list.c, xs_type(XS_HEX_BINARY,16)) aborts parsing the enclosing
// list with "parse error in message body" and never reads any sibling field.
// For a DERControl that means the client never sees responseRequired or
// replyTo, so it never posts a Response: the served bytes look plausible in a
// log while being unusable on the wire. Refusing to emit an invalid identifier
// is required here rather than optional (see .claude/rules/data-invariants.md
// Rule 2: do not paper over an identity that cannot be represented validly).
//
// PROPERTIES. The digest is SHA-256 over "<kind>:<lfdi>", truncated to the
// first 16 bytes and hex encoded uppercase. That gives:
//
//   - Validity: the output is 32 hex characters by construction, for every
//     input, including an empty one.
//   - Determinism: the same device and kind always yield the same mRID across
//     restarts, so a client that has cached an mRID still recognises the
//     resource. This is why a random UUID is NOT used here.
//   - Separation: distinct kinds on one device yield distinct mRIDs, which is
//     the property the old "-active"/"-dderc" suffixes provided.
//   - Device binding: the mRID remains a pure function of the device identity
//     the rest of this package keys on (the certificate-derived LFDI), so no
//     second, unrelated numbering scheme is introduced.
//
// Truncating a SHA-256 to 16 bytes is sound for an identifier: mRID is an
// identity, not a security token, and 128 bits leaves collision probability
// negligible across any fleet this bridge will address.
func deriveMRID(kind, lfdi string) string {
	sum := sha256.Sum256([]byte(kind + ":" + lfdi))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}
