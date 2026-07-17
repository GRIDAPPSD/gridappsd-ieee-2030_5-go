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
