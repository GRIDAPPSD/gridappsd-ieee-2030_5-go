package adminui

import (
	"io/fs"
	"log"
	"net/http"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/adminui/healthpage"
)

// healthPageCSP is the page's own policy. The plane keeps it and adds
// frame-ancestors 'self'.
const healthPageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'"

// publishingResponse is the sender's switch as the health data carries it.
// ChangedAt is "" until the switch has changed.
type publishingResponse struct {
	On        bool   `json:"on"`
	ChangedAt string `json:"changedAt"`
	ChangedBy string `json:"changedBy"`
}

// healthData is the health page's data: the /api/health payload plus the
// sender's switch. Publishing is omitted, not null, when the bridge has no
// sender, so the page can tell the two apart.
type healthData struct {
	healthResponse
	Publishing *publishingResponse `json:"publishing,omitempty"`
}

func (s *Server) healthData() healthData {
	d := healthData{healthResponse: s.health()}
	if s.sender != nil {
		st := s.sender.s.Publishing()
		p := publishingResponse{On: st.On, ChangedBy: st.ChangedBy}
		if !st.ChangedAt.IsZero() {
			p.ChangedAt = st.ChangedAt.Format(timeFormat)
		}
		d.Publishing = &p
	}
	return d
}

// healthPage is the handler of the health page panel. The plane mounts it
// under /apps/gridappsd-health/ behind its own authentication and strips
// that prefix, so the page needs no route or credential handling of its own.
// It is read only: the plane lets every method through to a page.
func (s *Server) healthPage() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", healthPageFile("index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /app.js", healthPageFile("app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /style.css", healthPageFile("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /data", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.healthData())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", healthPageCSP)
		mux.ServeHTTP(w, r)
	})
}

func healthPageFile(name, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, err := fs.ReadFile(healthpage.Files, name)
		if err != nil {
			log.Printf("adminui: health page file %s: %v", name, err)
			http.Error(w, "page file unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		// The status is sent; a failed write is the client leaving.
		_, _ = w.Write(b)
	})
}
