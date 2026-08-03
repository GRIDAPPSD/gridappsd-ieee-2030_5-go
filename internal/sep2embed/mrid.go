package sep2embed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
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
// SCOPE. This function is for resources whose identity is the DEVICE's, and
// that a server may legally modify in place: the FSA, the DERProgram, and the
// DefaultDERControl. None of those is an Event, so 2018 rule c) p.90 (no
// editing except status) does not reach them, and CSIP v2.0 section 4.4.1
// lines 274 to 279 explicitly sanctions modifying a DefaultDERControl in
// place. A DERControl IS an Event and must NOT use this function: see
// deriveEventMRID.
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

// deriveEventMRID returns a deterministic, schema-valid mRID for ONE
// DERControl Event: the given control payload, issued to the given device at
// the given creation instant.
//
// WHY IT IS NOT deriveMRID. deriveMRID is a pure function of (kind, LFDI), so
// it yields ONE mRID per device forever. For an Event that is a conformance
// defect and an operational dead end at once:
//
//   - 2018 clause 10.10.4.2 p.120 and 2023 clause 10.10.4.2.1 p.129: "Each
//     DERControl and DefaultDERControl instance SHALL be uniquely identified
//     by an mRID." A control carrying different parameters is a different
//     instance, because rule c) p.90 and rules q)2) p.91 / t)3) p.92 forbid
//     reaching the new parameters by editing the old event.
//   - 2018 clause 10.2.5.6 p.95: clients "SHALL detect duplicate Events by
//     comparing the mRIDs of the Events." A repeated mRID is therefore read
//     as a duplicate of an event the client already holds, and correctly
//     ignored. Observed live: a 5000 W to 7500 W change served under the
//     device's one constant mRID was fetched 22 times and actuated zero
//     times.
//   - Response.subject "is populated with the mRID of the original object",
//     which makes the mRID the sole correlation key between a client's
//     Response POST and the control it reports on. Two different setpoints
//     under one mRID make a status 3 or status 7 POST unattributable, so
//     reusing an mRID destroys the server's own audit trail as well.
//
// WHY IT IS STILL DERIVED, NOT RANDOM OR PERSISTED. Determinism across
// restarts with no persisted state is a property worth keeping: a client
// holding a cached mRID still recognises the resource after a bridge
// restart, and the bridge keeps no database. mRIDType (2018 p.167, 2023
// p.177, normative at sep.xsd:5919) fixes only the PEN in the leading bits
// and requires the remainder to be unique per object; HOW the remaining bits
// are derived is unconstrained. So the fix is to widen the derivation's
// INPUTS rather than to abandon derivation: adding creationTime and the
// control payload makes the mRID vary exactly when the event varies, and
// leaves it reproducible from inputs a replay can reconstruct.
//
// The payload is mixed in as its own XML serialization rather than as a
// hand-rolled field summary. That is the same byte sequence the client
// receives, so two controls that are indistinguishable on the wire get the
// same mRID and two that differ anywhere get different ones, with no separate
// notion of "significant field" to drift out of step with the schema.
//
// Fields are NUL-separated so that no combination of inputs can be
// reinterpreted as a different combination with the same concatenation.
func deriveEventMRID(kind, lfdi string, creationTime int64, base *sep2.DERControlBase) (string, error) {
	var payload []byte
	if base != nil {
		var err error
		payload, err = xml.Marshal(base)
		if err != nil {
			// Refused, never defaulted: an mRID derived from a payload we
			// could not serialize would not be a function of the bytes the
			// client receives, which is the whole property this identity
			// rests on.
			return "", fmt.Errorf("sep2embed: canonicalize DERControlBase for mRID: %w", err)
		}
	}

	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(kind),
		[]byte(lfdi),
		[]byte(strconv.FormatInt(creationTime, 10)),
		payload,
	} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)[:16])), nil
}

// derControlID returns the store key, and therefore the final href segment,
// of one issued DERControl.
//
// It is the creation instant in DESCENDING order, then the event's own mRID.
// Both halves are load-bearing.
//
// The mRID half supplies uniqueness and restart-reproducibility: it is
// already a function of the event's whole content, so no counter or clock
// state has to survive a restart for the same replay to land on the same
// href.
//
// The descending-time half exists because of paging, and dropping it
// reintroduces this card's defect by another route. Core lists a scoped
// collection in ascending id byte order (store.SortByIDAsc) and defaults an
// unpaged GET to ten items (sep2srv/paging.DefaultLimit). Keying on the mRID
// alone would order the DERControlList by what is effectively a random hash,
// so once a device has more than ten live controls the NEWEST one, the whole
// point of issuing it, may fall outside the first page and never be fetched.
// Ordering newest-first puts the control a client most needs on the page it
// gets without asking.
//
// IEEE 2030.5 section 4.6.1 requires each list resource to have a defined
// order but does not name one for DERControlList, and nothing in either CSIP
// guide constrains the id. Newest-first is therefore OUR decision, recorded
// here as ours (Devi, "What the standard leaves to us", item 4), not a
// requirement read out of the standard.
func derControlID(creationTime int64, mrid string) string {
	return fmt.Sprintf("%016X-%s", uint64(math.MaxInt64-creationTime), mrid)
}
