package portal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/store"
)

// The portal is a multi-page application: every page below is rendered on the server from
// internal/assets/portal_ui/templates and has its own URL, so links, the browser history, bookmarks
// and reloads work without client-side routing. portal.js only enhances a page: it posts actions to
// the JSON API, refreshes the regions of live pages, and adds find/copy/wrap to file viewers.

var (
	pageTemplatesOnce sync.Once
	pageTemplates     *template.Template
	pageTemplatesErr  error
)

func loadPageTemplates() (*template.Template, error) {
	pageTemplatesOnce.Do(func() {
		pageTemplates, pageTemplatesErr = template.New("portal").Funcs(templateFuncs()).Funcs(pageFuncs()).ParseFS(assets.FS, "portal_ui/templates/*.html")
	})
	return pageTemplates, pageTemplatesErr
}

type crumb struct {
	Label string
	Href  string
}

// pageProblem is what the page of an error says: the status the request was answered with, and why.
type pageProblem struct {
	Code    int
	Message string
}

// pageView is what every page template receives: what is around the page (its title, the menu, the
// pause, the theme) and, in Page, what the page itself shows.
type pageView struct {
	Title       string
	Section     string
	Crumbs      []crumb
	Theme       string
	Themes      []themeInfo
	Nav         []navGroup
	App         string
	Paused      bool
	PauseReason string
	Path        string
	// Address is the address the page was asked for, with its query: where its reload leads.
	Address string
	// Page is what the page shows: a struct of its own, declared next to the read that returns it
	// (page_*.go). ReadAt is the instant of the snapshot it was read from. Problem is set in its
	// place for the page of an error.
	Page    any
	ReadAt  time.Time
	Problem *pageProblem
	Live    int
	Content template.HTML
	// chromed says the menu and the pause were read with the page, from its snapshot.
	chromed bool
}

// IsActive reports whether the sidebar link href is the section of the current page.
func (v *pageView) IsActive(href string) bool {
	if href == "/" {
		return v.Path == "/"
	}
	return v.Section == href || strings.HasPrefix(v.Section, href+"/")
}

type pageFunc func(ctx context.Context, r *http.Request, v *pageView) (string, error)

// validatePageRequest applies the API's host and token rules to pages, which render the same data.
func (s *Server) validatePageRequest(w http.ResponseWriter, r *http.Request) bool {
	if !s.isAllowedHost(normalizeHost(r.Host)) {
		http.Error(w, "The portal does not answer to this host.", http.StatusForbidden)
		return false
	}
	if s.requiresNonLoopbackAuth(r) {
		ip := remoteIP(r)
		if retryAfter, ok := s.tokens.locked(ip); ok {
			w.Header().Set("Retry-After", retryAfterHeader(retryAfter))
			http.Error(w, "Too many wrong tokens from this address; try again later.", http.StatusTooManyRequests)
			return false
		}
		provided := providedPortalToken(r)
		if !matchesPortalToken(provided) {
			if provided != "" {
				s.tokens.fail(ip)
			}
			http.Error(w, "A request from outside this machine needs the token of the portal.", http.StatusForbidden)
			return false
		}
	}
	return true
}

func (s *Server) newView(r *http.Request) *pageView {
	return &pageView{Theme: themeOf(r), Themes: portalThemes, Path: r.URL.Path, Address: r.URL.RequestURI()}
}

func (s *Server) servePage(fn pageFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := s.newView(r)
		name, err := fn(r.Context(), r, v)
		status := http.StatusOK
		if err != nil {
			// The error says what to answer: what does not exist is a 404, and what is the portal's own
			// failure is a 500 that goes to the log as well as to the page.
			answer := answerOf(err)
			status = answer.status
			title := "Error"
			switch {
			case status == http.StatusNotFound:
				title = "Not found"
			case status < http.StatusInternalServerError:
				// The request asks for something the portal cannot give: it is not a failure of the portal.
				title = "Not valid"
			case status >= http.StatusInternalServerError:
				log.Printf("portal: %s %s: %v", r.Method, r.URL.Path, err)
			}
			v.Title, v.Crumbs, v.Live = title, []crumb{{Label: title}}, 0
			v.Problem = &pageProblem{Code: status, Message: answer.message}
			name = "error"
		}
		s.renderPage(w, r, v, name, status)
	}
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, v *pageView, name string, status int) {
	tpl, err := loadPageTemplates()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !v.chromed {
		// A page of before the redesign, or one whose read failed: what is around it is read now.
		ctx := r.Context()
		if err := s.store.View(ctx, func(db *store.View) error { s.chrome(ctx, db, v); return nil }); err != nil {
			log.Printf("portal: reading the menu of %s: %v", r.URL.Path, err)
			v.App, v.Nav = s.app, navOf(waiting{})
		}
	}
	if v.Section == "" {
		v.Section = v.Path
	}

	var body bytes.Buffer
	if err := tpl.ExecuteTemplate(&body, name, v); err != nil {
		log.Printf("portal: render %s: %v", name, err)
		body.Reset()
		status = http.StatusInternalServerError
		v.Title, v.Live = "Error", 0
		v.Problem = &pageProblem{Code: http.StatusInternalServerError, Message: "The page could not be rendered: " + err.Error()}
		if err := tpl.ExecuteTemplate(&body, "error", v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	v.Content = template.HTML(body.String())
	var page bytes.Buffer
	if err := tpl.ExecuteTemplate(&page, "layout", v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Never inside a frame: a page that framed the portal could line a decoy up over an action (Run
	// cycle, Accept base) whose same-origin request passes every CSRF check.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = w.Write(page.Bytes()) // a client that went away is not an error to report
}

// tabItem is one tab of a page with sub-pages.
type tabItem struct {
	Key    string
	Label  string
	Icon   string
	Href   string
	Count  int
	Counts bool
}

// ---- dashboard -------------------------------------------------------------------------------

// ---- changes ---------------------------------------------------------------------------------

// ---- executions and events -------------------------------------------------------------------

// ---- queue -----------------------------------------------------------------------------------

// ---- cycles and visual QA --------------------------------------------------------------------

// ---- agents, runs and jobs -------------------------------------------------------------------

// ---- system ----------------------------------------------------------------------------------

// staticAsset serves an embedded file of portal_ui (the icon sprite and the fonts).
func staticAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	b, err := fs.ReadFile(assets.FS, "portal_ui/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	if strings.HasSuffix(name, ".svg") {
		// The icon sprite changes with the kit: a copy cached for a day left every icon added by an
		// upgrade blank (the page names a symbol the old sprite lacks). Revalidate it, and answer 304
		// while it is unchanged. Fonts never change under the same name: they keep the day.
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b) // a client that went away is not an error to report
}
