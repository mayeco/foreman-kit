package portal

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// The pages that are about the portal itself, not about the kit: where the search box leads, what
// this browser opened lately, and the look of the portal. None of them shows a row of the database,
// and none has a mirror in the API: what they show lives in the browser (its history, its theme).

// portalRoutes declares those pages, the address of System, which is its first tab, and the page
// that answers every address the portal does not have.
func (s *Server) portalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /go", s.goTo)
	mux.HandleFunc("GET /recent", pageAsking(s, "recent", s.askRecent))
	mux.HandleFunc("GET /system", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/system/health", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /system/appearance", pageAsking(s, "appearance", s.askAppearance))
	mux.HandleFunc("GET /", s.servePage(func(_ context.Context, r *http.Request, _ *pageView) (string, error) {
		return "", errNotFound("there is no page at %s", r.URL.Path)
	}))
}

// GET /go?q= opens what a person typed in the search box: the page of the change of that name when
// the kit has one on record, else the list of changes searched for it.
func (s *Server) goTo(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	to := "/changes"
	if q != "" {
		to = "/changes?q=" + url.QueryEscape(q)
		if rec, err := s.changes.known(r.Context(), q); err == nil && rec != nil {
			to = changeHref(q)
		}
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// recentPage is the list of the pages this browser opened lately. The list is the browser's: the
// script of the portal keeps it and draws it, and the server sends the page it is drawn in.
type recentPage struct{}

func (p *recentPage) head(v *pageView) {
	v.Title = "Recent"
	v.Crumbs = []crumb{{Label: "Recent"}}
}

func (s *Server) askRecent(context.Context, *http.Request) (*recentPage, error) {
	return &recentPage{}, nil
}

// themeOf returns the theme the browser of a request chose, or the one of the kit when it chose
// none the portal offers.
func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && validTheme(c.Value) {
		return c.Value
	}
	return defaultTheme
}

// appearancePage is the look of the portal: the themes it offers, with the one this browser chose
// set beside another to compare them.
type appearancePage struct {
	// Compare is the theme the chosen one is set beside.
	Compare string
}

func (p *appearancePage) head(v *pageView) {
	v.Title, v.Section = "Appearance", "/system/appearance"
	v.Crumbs = []crumb{{Label: "System", Href: "/system/health"}, {Label: "Appearance"}}
}

// askAppearance reads which theme to compare the chosen one with: the one the address names, else
// the kit's own, or the light one when the kit's own is the one chosen.
func (s *Server) askAppearance(_ context.Context, r *http.Request) (*appearancePage, error) {
	compare := strings.TrimSpace(r.URL.Query().Get("compare"))
	switch {
	case compare == "" && themeOf(r) == defaultTheme:
		compare = "light"
	case compare == "":
		compare = defaultTheme
	case !validTheme(compare):
		return nil, errInvalid("compare names a theme the portal offers, and %q is not one", compare)
	}
	return &appearancePage{Compare: compare}, nil
}
