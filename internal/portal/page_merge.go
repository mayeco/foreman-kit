package portal

import (
	"context"
	"net/http"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Merge queue: what waits to be integrated and what the merger does. /merge draws it and /api/merge
// returns it: the attempt in progress, the manifests in the queue with the capabilities each
// touches and the evaluation of its risk, the base branches that fail on their own, and the latest
// attempts with their result and its reason.

// mergeAttemptsPerPage is how many of the attempts that ended a page of the merge queue holds.
const mergeAttemptsPerPage = 20

// mergePage is the merge queue.
type mergePage struct {
	store.MergeQueue

	Older  string `json:"-"`
	Newest string `json:"-"`
	// First says the page shows the latest attempts: only then does it show what waits and what runs.
	First bool `json:"-"`
}

func (p *mergePage) head(v *pageView) {
	v.Title, v.Section = "Merge queue", "/merge"
	v.Crumbs = []crumb{{Label: "Merge queue"}}
	if p.First {
		v.Live = 10
	}
}

func (s *Server) readMerge(ctx context.Context, db *store.View, r *http.Request) (*mergePage, error) {
	q := r.URL.Query()
	limit, err := limitOf(q)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = mergeAttemptsPerPage
	}
	before := strings.TrimSpace(q.Get("before"))
	mq, err := db.MergeQueue(ctx, before, limit)
	if err != nil {
		return nil, err
	}
	p := &mergePage{MergeQueue: mq, First: before == ""}
	if mq.Next != "" {
		p.Older = hrefWith(r, "before", mq.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	return p, nil
}
