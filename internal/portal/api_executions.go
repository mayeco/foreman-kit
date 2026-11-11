package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/store"
)

// executionRoutes declares the addresses of the executions: their pages, the reads that mirror them
// in the API, the output of one and its stream, and the actions that start, continue, repeat and stop
// one.
func (s *Server) executionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /executions", pageFrom(s, "executions", s.readExecutions))
	mux.HandleFunc("GET /executions/new", pageFrom(s, "execution_new", s.readNewExecution))
	mux.HandleFunc("GET /executions/{id}", pageFrom(s, "execution", s.readExecution))

	mux.HandleFunc("GET /api/executions", jsonFrom(s, s.readExecutions))
	mux.HandleFunc("GET /api/executions/{id}", jsonFrom(s, s.readExecution))
	mux.HandleFunc("GET /api/executions/{id}/log", s.apiExecutionLog)
	mux.HandleFunc("GET /api/executions/{id}/log/stream", s.apiExecutionLogStream)

	mux.HandleFunc("POST /api/executions", s.apiStartExecution)
	mux.HandleFunc("POST /api/executions/{id}/continue", s.apiContinueExecution)
	mux.HandleFunc("POST /api/executions/{id}/rerun", s.apiRerunExecution)
	mux.HandleFunc("POST /api/executions/{id}/stop", s.apiStopExecution)
}

// ---- the output -------------------------------------------------------------------------------------

// readExecutionLog returns chunks of the output of an execution in the order they were written,
// after the one the cursor "after" names.
func (s *Server) readExecutionLog(ctx context.Context, db *store.View, r *http.Request) (store.LogPage, error) {
	id := r.PathValue("id")
	if _, found, err := db.ExecutionStatus(ctx, id); err != nil {
		return store.LogPage{}, err
	} else if !found {
		return store.LogPage{}, errNotFound("there is no execution %q", id)
	}
	limit, err := limitOf(r.URL.Query())
	if err != nil {
		return store.LogPage{}, err
	}
	page, err := db.ReadLogChunks(ctx, id, r.URL.Query().Get("after"), limit)
	if page.Items == nil {
		page.Items = []store.LogChunk{}
	}
	return page, err
}

// apiExecutionLog answers with the output of an execution: a page of its chunks as JSON, or with
// format=text the whole of it as plain text, for a person to open or to save.
func (s *Server) apiExecutionLog(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") != "text" {
		jsonFrom(s, s.readExecutionLog)(w, r)
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	// A page of chunks at a time, each from a snapshot of its own: an output can be larger than what
	// is wise to hold in memory, and a snapshot is not kept open while a slow client takes what it is
	// sent. Chunks are only ever added, so the pages still add up to the output.
	page := func(after string) (chunks store.LogPage, found bool, err error) {
		err = s.store.View(ctx, func(db *store.View) error {
			if _, found, err = db.ExecutionStatus(ctx, id); err != nil || !found {
				return err
			}
			chunks, err = db.ReadLogChunks(ctx, id, after, store.MaxPageSize)
			return err
		})
		return chunks, found, err
	}
	chunks, found, err := page("")
	switch {
	case err != nil:
		fail(w, r, err)
		return
	case !found:
		fail(w, r, errNotFound("there is no execution %q", id))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+url.PathEscape(id)+`.log"`)
	}
	for {
		for _, c := range chunks.Items {
			if _, err := io.WriteString(w, c.Content); err != nil {
				return // the client went away: there is nobody to tell
			}
		}
		if chunks.Next == "" {
			return
		}
		if chunks, _, err = page(chunks.Next); err != nil {
			return // the answer has started: it ends here, short
		}
	}
}

// How often the stream of an output looks for new chunks, how long it stays silent before it writes
// a comment that keeps the connection open, and how long it waits for the last output of an
// execution that was stopped while its work is still ending.
const (
	logStreamPoll      = 200 * time.Millisecond
	logStreamKeepAlive = 15 * time.Second
	logStreamDrain     = 5 * time.Second
)

// apiExecutionLogStream sends the output of an execution as it is written, as server-sent events.
// Each "log" message is one chunk and carries the id of that chunk, so a connection that drops
// continues where it was: the browser sends the last id it got as Last-Event-ID. The first
// connection says where to start with "after". When the execution has ended and its output is sent,
// a "done" message says how it ended and the stream closes.
func (s *Server) apiExecutionLogStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	position := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if position == "" {
		position = strings.TrimSpace(r.URL.Query().Get("after"))
	}
	if position != "" {
		if n, err := strconv.ParseInt(position, 10, 64); err != nil || n < 0 {
			fail(w, r, errInvalid("the position of the stream is the id of a chunk of the output"))
			return
		}
	}

	// read returns the chunks written after position and whether the execution still runs, from one
	// snapshot: once it says the execution has ended, every chunk of its output is in what it returned.
	read := func() (chunks []store.LogChunk, status string, found bool, err error) {
		err = s.store.View(ctx, func(db *store.View) error {
			if status, found, err = db.ExecutionStatus(ctx, id); err != nil || !found {
				return err
			}
			after := position
			for {
				page, err := db.ReadLogChunks(ctx, id, after, store.MaxPageSize)
				if err != nil {
					return err
				}
				chunks = append(chunks, page.Items...)
				if page.Next == "" {
					return nil
				}
				after = page.Next
			}
		})
		return chunks, status, found, err
	}

	chunks, status, found, err := read()
	switch {
	case err != nil:
		fail(w, r, err)
		return
	case !found:
		fail(w, r, errNotFound("there is no execution %q", id))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, r, fmt.Errorf("the connection cannot stream"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	silentSince := time.Now()
	var endedAt time.Time
	for {
		for _, c := range chunks {
			data, err := json.Marshal(c)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", c.ID, data); err != nil {
				return // the client went away
			}
			position = strconv.FormatInt(c.ID, 10)
			silentSince = time.Now()
		}
		// An execution this process runs is recorded as stopped at once, while its work may still be
		// ending and writing its last output: the stream goes on until that work is over.
		ended := status != "running"
		if ended && endedAt.IsZero() {
			endedAt = time.Now()
		}
		if ended && s.exec.owns(id) && time.Since(endedAt) < logStreamDrain {
			ended = false
		}
		if ended {
			data, _ := json.Marshal(map[string]string{"exec_id": id, "status": status}) // a map of strings always marshals
			if _, err := fmt.Fprintf(w, "event: done\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
			return
		}
		if time.Since(silentSince) >= logStreamKeepAlive {
			if _, err := io.WriteString(w, ": waiting for output\n\n"); err != nil {
				return
			}
			silentSince = time.Now()
		}
		flusher.Flush()

		select {
		case <-ctx.Done():
			return
		case <-s.exec.done():
			// The portal is shutting down. The browser connects again by itself when it is back.
			return
		case <-time.After(logStreamPoll):
		}
		if chunks, status, found, err = read(); err != nil || !found {
			return
		}
	}
}

// ---- the actions ------------------------------------------------------------------------------------

// POST /api/executions runs an agent on a prompt, at the root of the workspace.
func (s *Server) apiStartExecution(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Engine string `json:"engine"`
		Prompt string `json:"prompt"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	ex, err := s.exec.StartAgent(r.Context(), AgentRun{Engine: in.Engine, Prompt: in.Prompt})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeStarted(w, ex)
}

// POST /api/executions/{id}/continue sends a follow-up to an agent run.
func (s *Server) apiContinueExecution(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Prompt      string `json:"prompt"`
		Engine      string `json:"engine"`
		WithContext bool   `json:"with_context"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	ex, err := s.exec.Continue(r.Context(), r.PathValue("id"), Continuation{Prompt: in.Prompt, Engine: in.Engine, WithContext: in.WithContext})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeStarted(w, ex)
}

// POST /api/executions/{id}/rerun runs an agent run again, on the same prompt.
func (s *Server) apiRerunExecution(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Engine string `json:"engine"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	ex, err := s.exec.Rerun(r.Context(), r.PathValue("id"), in.Engine)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeStarted(w, ex)
}

// POST /api/executions/{id}/stop stops an execution the portal started, and answers with it as it
// is recorded afterwards.
func (s *Server) apiStopExecution(w http.ResponseWriter, r *http.Request) {
	if _, err := decodeInput[struct{}](r); err != nil {
		fail(w, r, err)
		return
	}
	// The request may go away while the work stops: the execution is recorded as stopped all the same.
	ex, err := s.exec.Stop(context.WithoutCancel(r.Context()), r.PathValue("id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}
