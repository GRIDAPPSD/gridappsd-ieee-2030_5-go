package sep2embed

import (
	"context"
	"net/http"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store"

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

// storeOwnerResolver implements OwnerResolver against the same
// EndDeviceStore seed.go populates and the /edev/{id} handlers
// themselves read: there is no separate ownership index to drift out
// of sync with the seeded devices. The device at store key edevID owns
// itself if and only if its own EndDevice.LFDI field equals the
// caller's LFDI (per Noor's design: "authoritative EndDevice.LFDI ==
// the owning device").
type storeOwnerResolver struct {
	endDevices store.EndDeviceStore
}

func newStoreOwnerResolver(endDevices store.EndDeviceStore) *storeOwnerResolver {
	return &storeOwnerResolver{endDevices: endDevices}
}

// OwnsEndDevice implements OwnerResolver.
//
// context.Background() below is deliberate, not an entry-point
// violation of the workspace's context discipline: the OwnerResolver
// interface signature is fixed by the settled design (GAGO-043) with
// no ctx parameter, because the backing lookup is a synchronous
// in-memory map read with nothing to cancel. This mirrors core's own
// EndDeviceStore.GetBySFDI/GetByLFDI (pkg/store/memory/enddevice.go),
// which ignore their ctx parameter and call context.Background()
// internally for the exact same reason.
//
// The comparison below is an exact, uppercase-sensitive string compare
// with no case-folding, and that is correct, not an oversight: both
// operands derive from the same function, sepTLS.LFDI, which always
// returns the canonical uppercase-hex form (spec section 6.3.4).
// callerLFDI reaches here via identityMiddleware's
// sepTLS.LFDI(r.TLS.PeerCertificates[0]) call; the stored
// EndDevice.LFDI reaches here via seed.go's seedOne, which sets it
// from registry.Entry.LFDI, itself sourced (GAGO-033) from
// sepTLS.LFDI on that same device's certificate. Two callers of one
// canonicalizing function agree by construction, so exact-string
// compare is the correct check. Do NOT add runtime case-folding here:
// that would mask a real drift bug (one side no longer deriving from
// sepTLS.LFDI) instead of surfacing it. TestOwnsEndDeviceAgreesWithSepTLSLFDIDerivation
// locks this invariant: it derives both sides from the same
// certificate the way each real caller does, and fails if either
// derivation path's casing or shape ever drifts.
func (r *storeOwnerResolver) OwnsEndDevice(callerLFDI, edevID string) bool {
	if callerLFDI == "" || edevID == "" {
		return false
	}
	dev, err := r.endDevices.Get(context.Background(), edevID)
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
				if !resolver.OwnsEndDevice(callerLFDI, id) {
					http.Error(w, "forbidden: not the owner of this EndDevice", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
