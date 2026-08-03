// Package sep2acl implements the IEEE 2030.5 section 6.2.3 resource
// method allow-list: a longest-prefix path-to-family table pairing each
// resource family with the HTTP methods a CSIP-conformant client is
// permitted to use against it.
//
// This package deliberately knows nothing about device ownership,
// stores, or the registry: it is pure path parsing plus a static
// table. That is what makes it the liftable half of GAGO-043's
// authorization design (deferred card IEEECORE-011: a future core lift
// of the method table moves this package unchanged; only the
// per-device ownership half in internal/sep2embed is bridge-owned and
// stays behind).
package sep2acl

import (
	"net/http"
	"strings"
)

// segWild marks a wildcard path segment (an id, such as {id}, {derId},
// {fsaId}, or {derpId}) in a family pattern: it matches any single,
// non-consumed path segment regardless of value.
//
// segWild is compared only against families' own fixed pattern tokens
// (see matchesPrefix: "if p == segWild", where p ranges over pattern,
// never over segs), so a request path segment whose literal value
// happens to be "*" cannot be mistaken for the wildcard marker: this
// package never tests an actual path segment against segWild. Real
// EndDevice ids are also structurally excluded from ever equaling
// "*": they are LFDIs, 40-character uppercase hex strings per IEEE
// 2030.5 spec section 6.3.4, so this is defense in depth, not the
// only reason the check is safe.
const segWild = "*"

// family pairs a path pattern (a sequence of literal or wildcard
// segments) with the HTTP methods allowed against any path this
// pattern matches as a prefix.
type family struct {
	pattern []string
	methods map[string]struct{}
}

func methodSet(ms ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(ms))
	for _, m := range ms {
		out[m] = struct{}{}
	}
	return out
}

var (
	readOnly     = methodSet(http.MethodGet, http.MethodHead)
	readWrite    = methodSet(http.MethodGet, http.MethodHead, http.MethodPut)
	readCreate   = methodSet(http.MethodGet, http.MethodHead, http.MethodPost)
	readWriteAll = methodSet(http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete)
	readSub      = methodSet(http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete)
)

// families is the resource-family table, IEEE 2030.5 section 6.2.3.
// Entries are matched by longest-prefix (most matched segments wins),
// not by declaration order: see match below. This lets a deeper,
// more-specific pattern (e.g. the "dderc" DefaultDERControl singleton,
// which allows PUT) override a shallower, more general one (e.g. the
// "fsa" family, which is read-only) that would otherwise also match as
// a prefix of the same path.
//
// Read-only families per the design (GAGO-043, Noor): /dcap, /tm,
// /sdev, /dc, /rt. Every other family here either allows a create
// (POST) at its own root, per the routes core's assembly package
// actually registers (see assembly.BuildProtocolRouter), or allows a
// PUT/DELETE against a specific writable sub-resource.
var families = []family{
	// Read-only, spec-fixed families: no client, of any device, may
	// write these regardless of ownership.
	{[]string{"dcap"}, readOnly},
	{[]string{"tm"}, readOnly},
	{[]string{"sdev"}, readOnly},
	{[]string{"dc"}, readOnly},
	{[]string{"rt"}, readOnly},

	// Common/global families reachable by any authenticated device.
	// POST is allowed where core registers a create route at the
	// family root (POST /edev self-registration, POST /mup and POST
	// /upt mirror creation, POST /msg/{msgId}/tm and POST
	// /rsps/{rspsId}/rsp operator-facing posts); GET/HEAD cover the
	// family's list and item reads.
	{[]string{"msg"}, readCreate},
	{[]string{"rsps"}, readCreate},
	{[]string{"upt"}, readCreate},
	{[]string{"mup"}, readCreate},
	{[]string{"edev"}, readCreate}, // exactly "/edev": GET (list), POST (self-register)

	// /edev/{id}-scoped families: ownership-gated by
	// internal/sep2embed's aclMiddleware, in addition to the method
	// check this table performs.
	{[]string{"edev", segWild}, readWriteAll}, // EndDevice singleton: GET/PUT/DELETE
	{[]string{"edev", segWild, "rg"}, readOnly},
	{[]string{"edev", segWild, "sub"}, readSub},
	{[]string{"edev", segWild, "der"}, readOnly},
	{[]string{"edev", segWild, "der", segWild}, readWrite}, // DER instance: GET/HEAD/PUT (WADL PUTDER mode O, GAGO-111)
	{[]string{"edev", segWild, "der", segWild, "dercap"}, readWrite},
	{[]string{"edev", segWild, "der", segWild, "derg"}, readWrite},
	{[]string{"edev", segWild, "der", segWild, "ders"}, readWrite},
	{[]string{"edev", segWild, "der", segWild, "dera"}, readWrite},
	{[]string{"edev", segWild, "fsa"}, readOnly}, // covers fsa, fsa/{fsaId}, .../derp, .../derp/{derpId}/derc
	{[]string{"edev", segWild, "fsa", segWild, "derp", segWild, "dderc"}, readWrite},
	{[]string{"edev", segWild, "cfg"}, readWrite},
	{[]string{"edev", segWild, "dstat"}, readWrite},
	// LogEvent list and instance. The address is /lel, not /log: core
	// v0.13.0 retired /edev/{id}/log and /edev/{id}/log/{logId} and mounts
	// the WADL-declared /edev/{id}/lel and /edev/{id}/lel/{lelId} instead
	// (2018 A.3.5.1, A.3.5.2; sep_wadl.xml:1358, 1404). /log was never a
	// WADL address and was never advertised, so no entry for it is kept
	// here: a rule matching a path nothing serves is dead configuration.
	//
	// Ordering rule (GAGO-111, Noor): this entry lands with or before the
	// core change that moves the address, never after. A window where the
	// table names the old address fails CLOSED and silently: /lel then
	// matches only the /edev/{id} singleton entry, which has no POST, so
	// every LogEvent report is refused with a 405 before dispatch.
	{[]string{"edev", segWild, "lel"}, readCreate},
	{[]string{"edev", segWild, "ps"}, readWrite},
	{[]string{"edev", segWild, "frq"}, readCreate},
	{[]string{"edev", segWild, "frp"}, readOnly},
}

// segments splits an URL path into its non-empty slash-delimited
// segments. "/edev/123" and "/edev/123/" both yield ["edev", "123"];
// "/" and "" both yield nil.
func segments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// matchesPrefix reports whether pattern matches the leading
// len(pattern) segments of segs: a literal pattern segment must equal
// the corresponding path segment exactly, and a wildcard pattern
// segment matches any path segment (segs is never long enough to
// index past its own length, since the length check below returns
// false first).
func matchesPrefix(pattern, segs []string) bool {
	if len(segs) < len(pattern) {
		return false
	}
	for i, p := range pattern {
		if p == segWild {
			continue
		}
		if segs[i] != p {
			return false
		}
	}
	return true
}

// MethodAllowed reports whether method is permitted against the
// resource family that longest-prefix-matches path.
//
// matched is false when no family pattern matches path at all: an
// unrecognized resource. Callers must NOT treat matched=false as a
// denial by itself; per the design, an unmatched path is deferred to
// the downstream mux (which 404s any path with no registered route).
// A denial is matched=true with allowed=false: the path IS a known
// resource family, and method is outside what that family permits.
func MethodAllowed(path, method string) (allowed, matched bool) {
	segs := segments(path)

	var best *family
	for i := range families {
		f := &families[i]
		if !matchesPrefix(f.pattern, segs) {
			continue
		}
		if best == nil || len(f.pattern) > len(best.pattern) {
			best = f
		}
	}
	if best == nil {
		return false, false
	}
	_, ok := best.methods[method]
	return ok, true
}

// EndDeviceID extracts the {id} path segment from an /edev/{id}...
// request path. ok is false when path is not an /edev resource path
// carrying an id segment at all: the top-level "/edev" list/create
// endpoint, or any path whose first segment is not literally "edev".
//
// When ok is true, id may still be the empty string (a malformed path
// such as "/edev//x", which produces a blank second segment). This is
// deliberate, per the boundary-of-the-boundary discipline
// (data-invariants Rule 3): EndDeviceID does not silently classify a
// blank id as "not edev-scoped" and let the caller skip the ownership
// check. It reports ok=true with an empty id, so the caller's
// ownership check runs against that empty id and a fail-closed
// OwnerResolver denies it (no device owns the empty id), rather than
// the request bypassing the check entirely.
func EndDeviceID(path string) (id string, ok bool) {
	segs := segments(path)
	if len(segs) < 2 || segs[0] != "edev" {
		return "", false
	}
	return segs[1], true
}
