package sep2embed

import (
	"net/http"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2acl"
)

// OwnerResolver reports whether callerLFDI is the LFDI of the
// EndDevice identified by edevID, the {id} path segment parsed from an
// /edev/{id}... request path.
//
// Implementations MUST fail closed: an edevID with no matching
// EndDevice, a lookup error, or a blank edevID all report false, never
// true. A false positive here lets one device read or write another
// device's resources, so "unknown" and "not owned" are deliberately
// the same answer: this interface has no way to distinguish them, and
// callers must not try to.
type OwnerResolver interface {
	OwnsEndDevice(callerLFDI, edevID string) bool
}

// registryOwnerResolver implements OwnerResolver against the registry,
// the single source of truth for the canonical-vs-advertised identity
// split (dual-index LFDI). The device addressed by the wire path segment
// edevID (which is the device's ADVERTISED id: a file-hash alias when it
// has one, otherwise its canonical LFDI) owns itself if and only if the
// caller's LFDI equals that device's CANONICAL LFDI.
//
// Why the registry and not the EndDeviceStore: the EndDeviceStore's
// EndDevice.LFDI field is now the ADVERTISED identity (the alias for an
// EPRI device), which is a lowercase file-hash the wire caller never
// presents. Matching callerLFDI against that advertised field would
// therefore fail for every aliased device (and, worse, would be the
// wrong identity to match against on principle). The registry carries
// BOTH identities per device (Entry.LFDI canonical, Entry.AliasLFDI
// advertised) and resolves an /edev id back to its Entry via
// GetByEdevID, so the ownership match is always against the canonical
// LFDI regardless of which identity the device is advertised under.
type registryOwnerResolver struct {
	reg *registry.Registry
}

func newRegistryOwnerResolver(reg *registry.Registry) *registryOwnerResolver {
	return &registryOwnerResolver{reg: reg}
}

// OwnsEndDevice implements OwnerResolver.
//
// The comparison below is an exact, uppercase-sensitive string compare
// with no case-folding, and that is correct, not an oversight: both
// operands are CANONICAL LFDIs derived from the same function,
// sepTLS.LFDI, which always returns the uppercase-hex form (spec
// section 6.3.4). callerLFDI reaches here via identityMiddleware's
// sepTLS.LFDI(r.TLS.PeerCertificates[0]) call; the stored canonical
// LFDI reaches here via registry.Entry.LFDI, sourced (GAGO-033) from
// sepTLS.LFDI on that same device's certificate. Two callers of one
// canonicalizing function agree by construction, so exact-string
// compare is the correct check. Do NOT add runtime case-folding here:
// that would mask a real drift bug (one side no longer deriving from
// sepTLS.LFDI) instead of surfacing it.
//
// SECURITY BOUNDARY (dual-index): edevID is resolved to a device via
// GetByEdevID (the advertised-id index), then the caller is matched
// against THAT device's canonical LFDI. The caller's DER-hash LFDI is
// never compared against any device's file-hash ALIAS, so an alias
// value a wire caller could somehow present can never satisfy the
// ownership check for a device it does not canonically own. The match
// resolves device-by-advertised-id then compares-by-canonical-LFDI, so
// device A's caller (canonical LFDI A) owns exactly the device whose
// canonical LFDI is A, whether B is addressed by B's alias or B's
// canonical id. Fail closed: a blank callerLFDI, a blank edevID, or an
// edevID that resolves to no registered device all return false.
func (r *registryOwnerResolver) OwnsEndDevice(callerLFDI, edevID string) bool {
	if callerLFDI == "" || edevID == "" {
		return false
	}
	entry, ok := r.reg.GetByEdevID(edevID)
	if !ok {
		return false
	}
	return entry.LFDI == callerLFDI
}

// aclMiddleware enforces the IEEE 2030.5 section 6.2.3 method
// allow-list (internal/sep2acl) plus per-device ownership scoping, on
// top of the identity identityMiddleware already extracted into the
// request context. It must run AFTER identityMiddleware in the Wrap
// chain (see buildHandler): it reads the identity identityMiddleware
// set, it does not extract one itself.
//
// Every branch below fails closed: a missing identity, a method
// outside the matched family's allow-list, or ownership denied on an
// /edev/{id} path all return before next is ever called. An unmatched
// family (sep2acl.MethodAllowed's matched=false) is the sole pass-through
// case, and only when the path is not itself an /edev/{id}-scoped
// resource with a denied owner: see the ordering comment below.
//
// Load-bearing mounting assumption: aclMiddleware parses r.URL.Path
// directly (sep2acl.EndDeviceID, sep2acl.MethodAllowed) rather than
// using stdlib http.ServeMux's own {id} pattern matching, because it
// runs BEFORE the protocol mux's own leaf pattern match (see
// buildHandler's Wrap composition in auth.go). This is only safe
// because a request reaches this middleware chain by first passing
// through assembly.BuildProtocolRouter's outer "top" http.ServeMux,
// which canonicalizes the path before dispatching: a path containing
// ".." segments, "//" (empty segments), or a percent-encoded slash is
// 301-redirected to its cleaned form by that outer mux, so this
// middleware never actually observes those forms. That
// canonicalization, owned entirely by the outer mux and NOT by this
// package, is what makes this package's segment-based parsing safe
// against dot-dot or slash-encoding traversal. If aclMiddleware is
// ever remounted ahead of a non-canonicalizing router (i.e., no longer
// behind that outer http.ServeMux), that bypass surface reopens and
// this middleware must not be trusted as-is: re-canonicalize the path
// here first.
func aclMiddleware(resolver OwnerResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callerLFDI, _, ok := identityFromContext(r.Context())
			if !ok {
				http.Error(w, "forbidden: no verified device identity", http.StatusForbidden)
				return
			}

			if allowed, matched := sep2acl.MethodAllowed(r.URL.Path, r.Method); matched && !allowed {
				http.Error(w, "method not allowed for this resource family", http.StatusMethodNotAllowed)
				return
			}

			// Ownership check runs regardless of whether the method-family
			// table matched: an /edev/{id} path with a denied owner must
			// not fall through to the mux just because sep2acl doesn't
			// happen to have a family entry for some deeper sub-path (see
			// EndDeviceID's own boundary-of-the-boundary doc comment).
			if id, isEdevScoped := sep2acl.EndDeviceID(r.URL.Path); isEdevScoped {
				if !resolver.OwnsEndDevice(callerLFDI, id) {
					http.Error(w, "forbidden: not the owner of this EndDevice", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
