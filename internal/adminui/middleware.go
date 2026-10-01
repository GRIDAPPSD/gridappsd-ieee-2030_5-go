package adminui

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// defaultAllowedHosts are the loopback names always accepted, by the
// plane and by the bridge JSON routes, beside Config.AllowedHosts.
var defaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

// bridgeJSONRoutes are the bridge's own JSON routes kept beside the plane.
// The plane mounts neither path, and TestPlaneMountsNoBridgeJSONRoute
// pins that, so the exact patterns below never shadow a plane route.
var bridgeJSONRoutes = []string{"/api/health", "/api/clients"}

// buildHandler puts the bridge JSON routes in front of the plane, which
// takes every other path. The JSON routes keep the bridge's own chain, in
// an order that is load bearing: the Host check runs before the Bearer
// check, so a spoofed Host never probes the token, and the Bearer check
// runs before the method check, so a 401 says nothing about methods.
func (s *Server) buildHandler(plane http.Handler) http.Handler {
	jsonMux := http.NewServeMux()
	jsonMux.HandleFunc("/api/health", s.handleHealth)
	jsonMux.HandleFunc("/api/clients", s.handleClients)
	gated := s.hostAllowlist(s.bearerAuth(requireGET(jsonMux)))

	mux := http.NewServeMux()
	for _, p := range bridgeJSONRoutes {
		mux.Handle(p, gated)
	}
	mux.Handle("/", plane)
	return keepStreamsOpen(mux)
}

// bearerAuth rejects any request whose Authorization header is not
// exactly "Bearer <Config.Key>" with 401 Unauthorized. It never logs
// the presented or expected token: only that a request was rejected.
// The token equality test uses subtle.ConstantTimeCompare rather than
// a plain byte or string compare, so a wrong token takes the same time
// to reject regardless of how many leading bytes match: a plain
// compare short-circuits on the first mismatched byte and leaks a
// timing signal an attacker could use to recover the token one byte
// at a time.
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		presented, hasPrefix := strings.CutPrefix(got, prefix)
		if !hasPrefix || subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.Key)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowlist rejects any request whose Host header (with any port
// suffix stripped, compared case insensitively) is not one of
// defaultAllowedHosts or Config.AllowedHosts, with 403 Forbidden.
func (s *Server) hostAllowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := splitHostPortLenient(host); err == nil {
			host = h
		}
		if !s.hostAllowed(host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether host case insensitively matches one of
// defaultAllowedHosts or s.cfg.AllowedHosts.
func (s *Server) hostAllowed(host string) bool {
	for _, allowed := range defaultAllowedHosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}
	for _, allowed := range s.cfg.AllowedHosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}
	return false
}

// splitHostPortLenient strips a trailing ":<port>" from a Host header
// value when present, and otherwise returns host unchanged. Unlike
// net.SplitHostPort, it does not error on a bare host with no port,
// since an http.Request.Host value legitimately omits the port for the
// default scheme port.
func splitHostPortLenient(host string) (string, string, error) {
	if idx := strings.LastIndex(host, ":"); idx >= 0 && !strings.Contains(host[idx+1:], "]") {
		// A bracketed IPv6 literal with no port ("[::1]") has no colon
		// after the closing bracket, so idx here would point at a colon
		// INSIDE the brackets; guard by checking the closing bracket is
		// not still ahead of idx.
		if strings.HasPrefix(host, "[") && strings.LastIndex(host, "]") > idx {
			return host, "", nil
		}
		return host[:idx], host[idx+1:], nil
	}
	return host, "", nil
}

// requireGET rejects any non-GET request with 405 Method Not Allowed.
// This is the narrowest, innermost gate in the chain: every registered
// route in this package is a read only GET endpoint, so a single shared
// check here means no individual handler needs its own method guard.
func requireGET(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}
