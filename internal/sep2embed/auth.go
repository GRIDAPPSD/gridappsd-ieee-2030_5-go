package sep2embed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/connobs"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// buildHandler assembles the protocol router for one Embed instance,
// once the server's mTLS Identity is known (sep2srv.New guarantees this
// ordering: Identity before build runs).
//
// AuthPolicy scope: Wrap below composes identityMiddleware
// (extracts the caller's LFDI/SFDI from the verified TLS peer
// certificate) with aclMiddleware (enforces the section 6.2.3 method
// allow-list from internal/sep2acl, plus per-device ownership scoping
// backed by stores.EndDevices). identityMiddleware runs first so
// aclMiddleware's identityFromContext read has something to find;
// see aclMiddleware's own doc comment for the ordering requirement.
// This closes the gap the previous doc comment on this function
// described: every device that clears the mTLS handshake used to be
// able to read or write any other device's resources. It now cannot.
//
// acl (aclMiddleware) is composed inside identityMiddleware here, and
// this whole chain is dispatched to only after
// assembly.BuildProtocolRouter's outer "top" http.ServeMux has already
// matched and, where needed, path-cleaned the request: that outer mux
// owns the canonicalizing redirect (".." / "//" / percent-encoded-slash
// forms are 301-redirected to their cleaned form before top ever
// dispatches into this Wrap chain). aclMiddleware therefore never
// observes an uncleaned path. See aclMiddleware's own doc comment for
// why that ordering is load-bearing for its path parsing.
//
// There is deliberately no GridAPPS-D telemetry relay in this chain.
// A prior implementation had a successful DERStatus PUT also publish to
// the platform bus from inside this request path, which coupled the
// protocol layer to the platform layer: receiving a 2030.5 request
// caused a bus send. The 2030.5 server's responsibility now ends at
// storing the resource, and internal/telemetrypub reads that store on
// its own timer. Do not reintroduce a publishing middleware here; add
// to the publisher instead.
//
// observe is composed OUTSIDE acl, immediately after
// identityMiddleware: it records every request from a cert-verified
// caller into hook, regardless of whether the ACL goes on to permit or
// deny that specific request. This is deliberate: the "served !=
// connected" question this hook answers is "did this LFDI actually
// reach the listener and authenticate", not "was this specific request
// authorized". A nil hook (the zero value of Config.Observer) makes
// connObserveMiddleware a pass-through, so this composition is a no-op
// wherever no observer is wired, e.g. every existing test that calls
// buildHandler with hook == nil.
func buildHandler(routerCfg assembly.RouterConfig, stores *assembly.Stores, reg *registry.Registry, identity sep2srv.Identity, notifier assembly.ResourceNotifier, hook *connobs.Hook) http.Handler {
	resolver := newStoreOwnerResolver(stores.EndDevices)
	acl := aclMiddleware(resolver)
	observe := connObserveMiddleware(hook)

	authPolicy := assembly.AuthPolicy{
		Wrap: func(next http.Handler) http.Handler {
			return identityMiddleware(observe(acl(next)))
		},
		Identity:   identityFromContext,
		SFDIPrefix: sfdiPrefix,
	}

	handler, _ := assembly.BuildProtocolRouter(routerCfg, stores, authPolicy, identity.SFDI, identity.LFDI, notifier)
	return handler
}

// identityCtxKey is the unexported context-key type for the extracted
// device identity. An unexported type avoids collisions with keys other
// packages might set on the same context.
type identityCtxKey struct{}

// deviceIdentity is the LFDI/SFDI pair extracted from a request's
// verified TLS peer certificate.
type deviceIdentity struct {
	lfdi string
	sfdi string
}

// identityMiddleware extracts the caller's LFDI/SFDI from the verified
// TLS peer certificate (r.TLS.PeerCertificates[0]) and stores it on the
// request context. Works uniformly for both the GCM (stdlib crypto/tls)
// and CCM (core's gotls fork) listener modes: sep2srv.New wraps the
// handler in sepTLS.CCMIdentityMiddleware when EnableCCM is set, which
// bridges gotls connection state into r.TLS before this middleware runs.
//
// A request with no verified peer certificate passes through with no
// identity attached; identityFromContext then reports ok=false and the
// downstream handler denies (see assembly.BuildProtocolRouter's F1
// deny-all stub). This should not occur in practice: the mTLS listener's
// tls.Config sets ClientAuth: RequireAnyClientCert, so an uncertificated
// client fails the handshake before any request reaches this
// middleware. The no-identity path exists as a fail-closed defense in
// depth, not an expected route.
func identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		id := deviceIdentity{
			lfdi: sepTLS.LFDI(leaf),
			sfdi: sepTLS.SFDI(leaf),
		}
		ctx := context.WithValue(r.Context(), identityCtxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// identityFromContext implements assembly.AuthPolicy.Identity.
func identityFromContext(ctx context.Context) (lfdi, sfdi string, ok bool) {
	id, ok := ctx.Value(identityCtxKey{}).(deviceIdentity)
	if !ok {
		return "", "", false
	}
	return id.lfdi, id.sfdi, true
}

// connObserveMiddleware records every authenticated request's caller
// LFDI and request path into hook (see internal/connobs), feeding the
// admin /api/clients endpoint. Must run AFTER identityMiddleware
// in the Wrap chain, since it reads the identity identityMiddleware
// already extracted into the request context; a request with no
// extracted identity (the fail-closed no-cert path identityMiddleware's
// own doc comment describes) is not recorded, since there is no LFDI to
// key by.
//
// hook == nil makes this a pass-through: every call site that does not
// wire an observer (every existing test, and any future caller of
// buildHandler that leaves Config.Observer unset) is unaffected.
func connObserveMiddleware(hook *connobs.Hook) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if hook == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if lfdi, _, ok := identityFromContext(r.Context()); ok {
				hook.RecordRequest(lfdi, r.URL.Path)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sfdiPrefixLen is the id-prefix length HandleCreateEndDevice derives
// from an SFDI (the short-SFDI guard). Mirrors the truncation the
// core assembly test suite uses (assembly_test.go testAuthPolicy) since
// core's own auth.ExtractSFDIPrefix is server-internal and not exported.
const sfdiPrefixLen = 8

// sfdiPrefix implements assembly.AuthPolicy.SFDIPrefix: a minimal,
// non-authoritative truncation used only by POST /edev (self-registration),
// which this embed does not exercise at seed time (seeding writes
// directly to the stores; see seed.go). Kept non-nil so
// BuildProtocolRouter does not fall back to the always-error deny stub,
// in case a real device self-registers against the embedded server.
func sfdiPrefix(sfdi string) (string, error) {
	if len(sfdi) < sfdiPrefixLen {
		return "", fmt.Errorf("sep2embed: SFDI too short: %q", sfdi)
	}
	return sfdi[:sfdiPrefixLen], nil
}
