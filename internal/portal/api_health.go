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
}

// healthAddress is the address of the health probe.
const healthAddress = "/healthz"

// healthRefuses reports whether the health probe refuses method: it takes GET, and HEAD, which
// follows GET. The methods it refuses are not listed in routes: any token is a valid method and a
// method-less /healthz pattern would conflict with the GET-only pages, so Server.ServeHTTP asks
// this before the mux, which would answer the rest with its plain-text 405 and an Allow header that
// names every pattern of the address.
func healthRefuses(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

// healthNotAllowed answers a method the health probe refuses: the API's JSON 405 with the methods it takes.
func healthNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, HEAD")
	fail(w, r, errMethod("%s takes GET or HEAD, not %s", r.URL.Path, r.Method))
}
