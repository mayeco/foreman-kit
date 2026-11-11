package portal

import (
	"context"
	"net/http"

	"github.com/mayeco/foreman-kit/internal/store"
)

// changeRoutes declares the addresses of one change: its pages, the reads that mirror them in the
// API, and the actions a person takes about it.
func (s *Server) changeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /changes/{name}", pageFrom(s, "change_story", s.readChangeStory))
	mux.HandleFunc("GET /changes/{name}/files", pageFrom(s, "change_files", s.readChangeFiles))
	mux.HandleFunc("GET /changes/{name}/files/{file...}", pageFrom(s, "change_files", s.readChangeFiles))
	mux.HandleFunc("GET /changes/{name}/events", pageFrom(s, "change_events", s.readChangeEvents))
	mux.HandleFunc("GET /changes/{name}/intervene", pageFrom(s, "change_intervene", s.readChangeIntervene))
	mux.HandleFunc("GET /gates/{id}", pageFrom(s, "gate_round", s.readGateRound))

	mux.HandleFunc("GET /api/changes/{name}", jsonFrom(s, s.readChangeStory))
	mux.HandleFunc("GET /api/changes/{name}/events", jsonFrom(s, s.readChangeEvents))
	mux.HandleFunc("GET /api/changes/{name}/files/{file...}", jsonFrom(s, s.readChangeFile))
	mux.HandleFunc("GET /api/gates/{id}", jsonFrom(s, s.readGateRound))

	mux.HandleFunc("POST /api/changes/{name}/unblock", s.apiUnblockChange)
	mux.HandleFunc("POST /api/changes/{name}/risk-approval", s.apiApproveRisk)
	mux.HandleFunc("POST /api/changes/{name}/reclaim", s.apiReclaimChange)
	mux.HandleFunc("POST /api/changes/{name}/abandon", s.apiAbandonChange)
	mux.HandleFunc("POST /api/changes/{name}/validate", s.apiValidateChange)
	mux.HandleFunc("POST /api/changes/{name}/gates", s.apiRunChangeGates)
	mux.HandleFunc("POST /api/changes/{name}/reset", s.apiResetChange)
	mux.HandleFunc("POST /api/changes/{name}/prompts", s.apiPromptChange)
	mux.HandleFunc("POST /api/bases/accept", s.apiAcceptBase)
}

// noInput reads the body of an action that takes no field, and answers the problem of one that
// sends some. It reports whether the action goes on.
func noInput(w http.ResponseWriter, r *http.Request) bool {
	if _, err := decodeInput[struct{}](r); err != nil {
		fail(w, r, err)
		return false
	}
	return true
}

// changed answers an action about a change: with its problem when it failed, and otherwise with the
// change as its pages show it afterwards.
func (s *Server) changed(w http.ResponseWriter, r *http.Request, name string, err error) {
	if err != nil {
		fail(w, r, err)
		return
	}
	// The action is done: the answer is read whatever became of the request meanwhile.
	ctx := context.WithoutCancel(r.Context())
	var head *changeHead
	err = s.store.View(ctx, func(db *store.View) (err error) {
		head, err = s.readChangeHead(ctx, db, name, tabStory)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, head)
}

// POST /api/changes/{name}/unblock lifts the block of a change and the question an agent left for a
// person. reset_retries starts its counters over; left out, they are kept.
func (s *Server) apiUnblockChange(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		ResetRetries bool `json:"reset_retries"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	// What a person decided is carried out whatever becomes of the request.
	name := r.PathValue("name")
	_, err = s.changes.Unblock(context.WithoutCancel(r.Context()), name, in.ResetRetries)
	s.changed(w, r, name, err)
}

// POST /api/changes/{name}/risk-approval records that a person accepts the risk of a change.
func (s *Server) apiApproveRisk(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	name := r.PathValue("name")
	_, err := s.changes.ApproveRisk(context.WithoutCancel(r.Context()), name)
	s.changed(w, r, name, err)
}

// POST /api/changes/{name}/reclaim gives back the lease of a change whose worker is gone.
func (s *Server) apiReclaimChange(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	name := r.PathValue("name")
	_, err := s.changes.Reclaim(context.WithoutCancel(r.Context()), name)
	s.changed(w, r, name, err)
}

// POST /api/changes/{name}/abandon takes a change out of the backlog and of the merge queue, lifts
// its block and discards it: what `foreman-kit abandon` does. It answers 409 for a change a worker
// is building or the merger is integrating.
func (s *Server) apiAbandonChange(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	name := r.PathValue("name")
	err := s.work.Abandon(context.WithoutCancel(r.Context()), name)
	s.changed(w, r, name, err)
}

// POST /api/changes/{name}/validate checks the specs of a change in its worktree and answers with
// what it found. It changes nothing.
func (s *Server) apiValidateChange(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	found, err := s.changes.Validate(r.Context(), r.PathValue("name"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

// POST /api/changes/{name}/gates runs the gates in the worktree of a change.
func (s *Server) apiRunChangeGates(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	name := r.PathValue("name")
	if _, err := s.changes.known(r.Context(), name); err != nil {
		fail(w, r, err)
		return
	}
	ex, err := s.exec.StartOperation(r.Context(), "gates", name)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeStarted(w, ex)
}

// POST /api/changes/{name}/reset removes what a worker built for a change, so that it starts again
// from the backlog.
func (s *Server) apiResetChange(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	name := r.PathValue("name")
	rec, err := s.changes.Reset(context.WithoutCancel(r.Context()), name)
	if err == nil && rec == nil {
		// A worktree the kit had no record of the change of: it is gone, and there is no change to show.
		writeJSON(w, http.StatusOK, map[string]any{"change_name": name, "has_worktree": false})
		return
	}
	s.changed(w, r, name, err)
}

// POST /api/changes/{name}/prompts sends a prompt to an agent in the worktree of a change.
func (s *Server) apiPromptChange(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Engine string `json:"engine"`
		Prompt string `json:"prompt"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	name := r.PathValue("name")
	if _, err := s.changes.known(r.Context(), name); err != nil {
		fail(w, r, err)
		return
	}
	ex, err := s.exec.StartAgent(r.Context(), AgentRun{Engine: in.Engine, Prompt: in.Prompt, Change: name})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeStarted(w, ex)
}

// POST /api/bases/accept accepts the base of a repository that fails on its own, as it is.
func (s *Server) apiAcceptBase(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Repo string `json:"repo"`
		SHA  string `json:"sha"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	base, err := s.changes.AcceptBase(context.WithoutCancel(r.Context()), in.Repo, in.SHA)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, base)
}
