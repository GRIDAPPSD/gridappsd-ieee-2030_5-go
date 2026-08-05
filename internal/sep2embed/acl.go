package sep2embed

import (
	"context"
	"net/http"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2acl"
)

// OwnerResolver reports whether callerLFDI is the LFDI of the
// EndDevice identified by edevID, the {id} path segment parsed from an
// /edev/{id}... request path. ctx is the request's own context (from
// aclMiddleware's r.Context()), passed through so a store-backed
// implementation can propagate request cancellation rather than
// creating its own background context mid-stack.
//
// Implementations MUST fail closed: an edevID with no matching
// EndDevice, a lookup error, or a blank edevID all report false, never
// true. A false positive here lets one device read or write another
// device's resources, so "unknown" and "not owned" are deliberately
// the same answer: this interface has no way to distinguish them, and
// callers must not try to.
type OwnerResolver interface {
	OwnsEndDevice(ctx context.Context, callerLFDI, edevID string) bool
}

// storeOwnerResolver implements OwnerResolver directly against the
// EndDeviceStore, the single source of truth for a device's stored
// identity. The device addressed by the wire path segment edevID (the
// EndDeviceStore key, which is the device's canonical LFDI: see
// seed.go) owns itself if and only if the caller's LFDI equals that
// stored EndDevice's LFDI field. There is no separate advertised
// identity to reconcile: the store key, the EndDevice.LFDI field, and
// the ownership identity are all the same canonical value.
type storeOwnerResolver struct {
	endDevices store.EndDeviceStore
}

func newStoreOwnerResolver(endDevices store.EndDeviceStore) *storeOwnerResolver {
	return &storeOwnerResolver{endDevices: endDevices}
}

// OwnsEndDevice implements OwnerResolver.
//
// The comparison below is an exact, uppercase-sensitive string compare
// with no case-folding, and that is correct, not an oversight: both
// operands are CANONICAL LFDIs derived from the same function,
// sepTLS.LFDI, which always returns the uppercase-hex form (spec
// section 6.3.4). callerLFDI reaches here via identityMiddleware's
// sepTLS.LFDI(r.TLS.PeerCertificates[0]) call; the stored LFDI reaches
// here via the EndDevice record's LFDI field, sourced (seed.go) from
// registry.Entry.LFDI, which is itself certificate-derived via
// sepTLS.LFDI. Two callers of one canonicalizing function
// agree by construction, so exact-string compare is the correct check.
// Do NOT add runtime case-folding here: that would mask a real drift
// bug (one side no longer deriving from sepTLS.LFDI) instead of
// surfacing it.
//
// Fail closed: a blank callerLFDI, a blank edevID, or a store lookup
// that returns any non-nil error all return false. The error is not
// inspected by type; every lookup failure is denied, never treated as
// an implicit allow.
func (r *storeOwnerResolver) OwnsEndDevice(ctx context.Context, callerLFDI, edevID string) bool {
	if callerLFDI == "" || edevID == "" {
		return false
	}
	dev, err := r.endDevices.Get(ctx, edevID)
	if err != nil {
		return false
	}
	return dev.LFDI == callerLFDI
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
				if !resolver.OwnsEndDevice(r.Context(), callerLFDI, id) {
					http.Error(w, "forbidden: not the owner of this EndDevice", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
