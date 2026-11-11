package portal

import (
	"context"
	"net/http"

	"github.com/mayeco/foreman-kit/internal/store"
)

// A read is what an address shows, taken from one snapshot of the database: the counts and the rows
// of a page are of the same instant. The page draws what the read returns and the API returns it as
// JSON, so an address has one read and not one for each.
type read[T any] func(ctx context.Context, db *store.View, r *http.Request) (T, error)

// headed is what a read returns for a page: it gives the page its title and its place in the portal.
type headed interface {
	head(v *pageView)
}

// pageFrom serves a read as the page the template tmpl draws.
func pageFrom[T headed](s *Server, tmpl string, rd read[T]) http.HandlerFunc {
	return s.servePage(func(ctx context.Context, r *http.Request, v *pageView) (string, error) {
		err := s.store.View(ctx, func(db *store.View) error {
			data, err := rd(ctx, db, r)
			if err != nil {
				return err
			}
			v.Page, v.ReadAt = data, db.At()
			data.head(v)
			// What is around the page comes from the snapshot the page was read from.
			s.chrome(ctx, db, v)
			return nil
		})
		return tmpl, err
	})
}

// jsonFrom serves a read as JSON: the mirror of its page in the API.
func jsonFrom[T any](s *Server, rd read[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var data T
		err := s.store.View(r.Context(), func(db *store.View) (err error) {
			data, err = rd(r.Context(), db, r)
			return err
		})
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, data)
	}
}

// An ask is what an address shows when it is not rows of the database: what the kit says of the
// application (its configuration, its setup, its prompts, what it would change of its settings),
// which the kit works out from files, from git or through reads of its own. It runs before the page
// takes its snapshot. A snapshot holds a connection of the store for as long as it is open, and a
// read of the kit that asks the store for another one while the page holds the first waits for the
// pages that are doing the same: asked for together, they would all wait for one another.
type ask[T any] func(ctx context.Context, r *http.Request) (T, error)

// pageAsking serves an ask as the page the template tmpl draws: the snapshot is taken after the kit
// answered, for what is around the page.
func pageAsking[T headed](s *Server, tmpl string, a ask[T]) http.HandlerFunc {
	return s.servePage(func(ctx context.Context, r *http.Request, v *pageView) (string, error) {
		data, err := a(ctx, r)
		if err != nil {
			return tmpl, err
		}
		err = s.store.View(ctx, func(db *store.View) error {
			v.Page, v.ReadAt = data, db.At()
			data.head(v)
			s.chrome(ctx, db, v)
			return nil
		})
		return tmpl, err
	})
}

// jsonAsking serves an ask as JSON: the mirror of its page in the API.
func jsonAsking[T any](s *Server, a ask[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := a(r.Context(), r)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, data)
	}
}

// liveRefresh reports whether a page is being asked for again by the script that keeps it up to
// date. Such a request wants the regions that change while the page is open; what the page gets
// through a stream of its own, it does not read again.
func liveRefresh(r *http.Request) bool {
	return r.Header.Get("X-Portal-Refresh") == "regions"
}
