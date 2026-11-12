package portal

import (
	"net/http"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/store"
)

// health is the answer of GET /healthz: that the portal is up, which build of the kit serves it,
// and which store schema this binary reads and writes.
type health struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	SchemaVersion int    `json:"schema_version"`
}

// healthRoutes declares the machine-readable liveness and build probe of the portal at /healthz.
func (s *Server) healthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, health{
			Status:        "ok",
			Version:       assets.Version,
			Commit:        assets.SourceCommit,
			SchemaVersion: store.SchemaVersion(),
		})
	})
	notAllowed := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, r, errMethod("%s takes GET or HEAD, not %s", r.URL.Path, r.Method))
	}
	mux.HandleFunc("POST /healthz", notAllowed)
	mux.HandleFunc("DELETE /healthz", notAllowed)
}
