package portal

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"github.com/mayeco/foreman-kit/internal/assets"
)

// Every address the portal answers is declared here, on one http.ServeMux: its method and its
// pattern, with the name of what it is about read from the pattern (r.PathValue). A request with a
// method an address does not take is answered 405 by the mux itself.

// router returns the mux with every route of the portal: the assets, the API, the streams and the
// pages.
func (s *Server) router() *http.ServeMux {
	s.routesOnce.Do(func() {
		mux := http.NewServeMux()
		s.assetRoutes(mux)

		s.overviewRoutes(mux)
		s.workspaceRoutes(mux)
		s.executionRoutes(mux)
		s.changeRoutes(mux)
		s.agentRoutes(mux)
		s.visualRoutes(mux)
		s.systemRoutes(mux)
		// An address of the API that no route takes. The page that answers every other unknown address
		// is for GET alone, so these are per method too: one for all methods would conflict with it.
		for _, method := range apiMethods {
			mux.HandleFunc(method+" /api/", s.apiNoRoute)
		}

		s.healthRoutes(mux)
		s.portalRoutes(mux)
		s.mux = mux
	})
	return s.mux
}

// apiMethods are the methods the API has routes for.
var apiMethods = []string{http.MethodGet, http.MethodPost, http.MethodDelete}

// apiNoRoute answers a request of the API that no route takes: 405 with the methods its address
// does take, when it takes some, and 404 when the address is not of the API at all.
func (s *Server) apiNoRoute(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, method := range apiMethods {
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := s.router().Handler(probe); pattern != method+" /api/" {
			allow = append(allow, method)
		}
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		fail(w, r, errMethod("%s takes %s, not %s", r.URL.Path, strings.Join(allow, " or "), r.Method))
		return
	}
	fail(w, r, errNotFound("the API has no %s", r.URL.Path))
}

// UICSS returns the stylesheet of the portal, from the files the binary carries.
func UICSS() (string, error) {
	b, err := assets.FS.ReadFile("portal_ui/portal.css")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UIJS returns the one script of the portal: the files of portal_ui/js, in the order of their names.
func UIJS() (string, error) {
	entries, err := fs.ReadDir(assets.FS, "portal_ui/js")
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		b, err := assets.FS.ReadFile("portal_ui/js/" + name)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("// %s\n%s", name, string(b)))
	}
	return strings.Join(parts, "\n;\n"), nil
}

func (s *Server) assetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /assets/portal.css", func(w http.ResponseWriter, _ *http.Request) {
		css, err := UICSS()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, css) // a client that went away is not an error to report
	})
	mux.HandleFunc("GET /assets/portal.js", func(w http.ResponseWriter, _ *http.Request) {
		js, err := UIJS()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, js) // a client that went away is not an error to report
	})
	mux.HandleFunc("GET /assets/icons.svg", func(w http.ResponseWriter, r *http.Request) {
		staticAsset(w, r, "icons.svg", "image/svg+xml")
	})
	mux.HandleFunc("GET /assets/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		staticAsset(w, r, "favicon.svg", "image/svg+xml")
	})
	mux.HandleFunc("GET /assets/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		ct := agentLogoType(name)
		if ct == "" {
			http.NotFound(w, r)
			return
		}
		// The marks are third-party files: never let one run anything when opened directly.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
		staticAsset(w, r, "agents/"+name, ct)
	})
	mux.HandleFunc("GET /assets/fonts/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !strings.HasSuffix(name, ".woff2") {
			http.NotFound(w, r)
			return
		}
		staticAsset(w, r, "fonts/"+name, "font/woff2")
	})
	// An asset the portal does not have is not a page.
	mux.HandleFunc("GET /assets/", http.NotFound)
}
