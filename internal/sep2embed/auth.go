package sep2embed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	sepTLS "github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2tls"
)

// buildHandler assembles the protocol router for one Embed instance,
// once the server's mTLS Identity is known (sep2srv.New guarantees this
// ordering: Identity before build runs).
//
// AuthPolicy scope note: Wrap below extracts the caller's LFDI/SFDI from
// the verified TLS peer certificate so Identity() is meaningful, but it
// does NOT enforce per-device ACL rules (auth.DefaultACLRules is
// server-of-record-internal and not part of core's exported surface).
// Every device that clears the mTLS handshake (a cert chaining to the
// trusted CA, carrying a valid HardwareModuleName SAN per
// sep2tls.VerifyPeerCertWithHardwareModuleSAN) can read any resource.
// That is sufficient for GAGO-030's discovery exit criteria; per-device
// read/write scoping is a follow-up, not a regression from the
// server-of-record's own nil-Wrap test posture, and stricter than it
// (identity IS extracted here, just not gated further).
func buildHandler(routerCfg assembly.RouterConfig, stores *assembly.Stores, identity sep2srv.Identity, notifier assembly.ResourceNotifier) http.Handler {
	authPolicy := assembly.AuthPolicy{
		Wrap:       identityMiddleware,
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

// sfdiPrefixLen is the id-prefix length HandleCreateEndDevice derives
// from an SFDI (IEEE-014 short-SFDI guard). Mirrors the truncation the
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
