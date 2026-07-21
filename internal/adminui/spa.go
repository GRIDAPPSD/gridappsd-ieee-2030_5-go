package adminui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/web"
)

// distFS is the SPA's built static assets (internal/adminui/web/dist),
// rooted so its paths start at index.html rather than at dist/index.html.
// fs.Sub can only fail here if the embedded tree does not contain a
// "dist" directory, which cannot happen: web.DistFS's own
// "//go:embed all:dist" directive requires that directory to exist at
// compile time. A panic at package init on that impossible case is a
// build defect, not a runtime condition to handle gracefully.
var distFS = mustSubFS(web.DistFS, "dist")

func mustSubFS(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("adminui: web.DistFS is missing its \"" + dir + "\" root: " + err.Error())
	}
	return sub
}

// spaHandler serves the admin UI's built SPA. It is registered on "/"
// in mux, so http.ServeMux routes every request that does not match one
// of the exact /api/... patterns here (including a request that is
// itself under /api but does not match a registered API route: this
// handler explicitly 404s that case rather than falling through to
// index.html, per the "never shadow /api" requirement).
//
// For a path that exists in distFS (an actual built asset, such as
// /assets/index-XXXX.js or /favicon.svg), the file server serves it
// directly. For any other non-/api path, index.html is served instead,
// so client side routing (internal/adminui/web/frontend/src/lib/router.ts)
// still works on a hard reload of a client side route.
func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServerFS(distFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		if isStaticAsset(r.URL.Path) {
			fileServer.ServeHTTP(w, r)
			return
		}

		indexReq := r.Clone(r.Context())
		indexReq.URL.Path = "/"
		fileServer.ServeHTTP(w, indexReq)
	})
}

// isStaticAsset reports whether reqPath names a real, non-directory
// file in distFS. path.Clean normalizes the incoming request path
// before the lookup; an unrooted or traversal-shaped path (containing
// "..") fails fs.ValidPath inside distFS's Stat and is treated the same
// as "not a static asset", which routes it to the index.html fallback
// below rather than a filesystem error.
func isStaticAsset(reqPath string) bool {
	name := strings.TrimPrefix(path.Clean(reqPath), "/")
	if name == "" || name == "." {
		return false
	}
	info, err := fs.Stat(distFS, name)
	return err == nil && !info.IsDir()
}
