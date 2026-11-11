package portal

import (
	"context"
	"encoding/json"
	"net/http"
)

// workspaceRoutes declares the pages of the queue and of what the kit recorded (the backlog, the
// merge queue, the cycles, the events, and what the gates and the evaluator found), the reads that
// mirror them in the API, and what a person does about the kit as a whole: pause it and resume it,
// start its cycle, its dispatcher, its merger and its gates, ask the dispatcher what it would start,
// bring the repositories up to date, edit the backlog and decide on the proposals.
func (s *Server) workspaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /backlog", pageFrom(s, "backlog", s.readBacklog("queue")))
	mux.HandleFunc("GET /backlog/proposals", pageFrom(s, "backlog", s.readBacklog("proposals")))
	mux.HandleFunc("GET /merge", pageFrom(s, "merge", s.readMerge))
	mux.HandleFunc("GET /cycles", pageFrom(s, "cycles", s.readCycles))
	mux.HandleFunc("GET /cycles/{stamp}", pageFrom(s, "cycle", s.readCycle))
	mux.HandleFunc("GET /events", pageFrom(s, "events", s.readEvents))
	mux.HandleFunc("GET /events/{id}", pageFrom(s, "event", s.readEvent))
	mux.HandleFunc("GET /quality", pageFrom(s, "quality", s.readQuality))

	mux.HandleFunc("GET /api/backlog", jsonFrom(s, s.readBacklog("queue")))
	mux.HandleFunc("GET /api/merge", jsonFrom(s, s.readMerge))
	mux.HandleFunc("GET /api/cycles", jsonFrom(s, s.readCycles))
	mux.HandleFunc("GET /api/cycles/{stamp}", jsonFrom(s, s.readCycle))
	mux.HandleFunc("GET /api/events", jsonFrom(s, s.readEvents))
	mux.HandleFunc("GET /api/events/{id}", jsonFrom(s, s.readEvent))
	mux.HandleFunc("GET /api/quality", jsonFrom(s, s.readQuality))

	mux.HandleFunc("POST /api/workspace/pause", s.apiPause)
	mux.HandleFunc("POST /api/workspace/resume", s.apiResume)
	mux.HandleFunc("POST /api/dispatcher/runs", s.apiStart("dispatch"))
	mux.HandleFunc("POST /api/dispatcher/simulations", s.apiSimulateDispatch)
	mux.HandleFunc("POST /api/merger/runs", s.apiStart("merge"))
	mux.HandleFunc("POST /api/cycles", s.apiStartCycle)
	mux.HandleFunc("POST /api/gates/runs", s.apiStart("gates"))
	mux.HandleFunc("POST /api/repos/sync", s.apiSyncRepos)

	mux.HandleFunc("POST /api/backlog", s.apiAddToBacklog)
	mux.HandleFunc("DELETE /api/backlog/{name}", s.apiRemoveFromBacklog)
	mux.HandleFunc("POST /api/proposals/{id}/approve", s.apiDecideProposal(true))
	mux.HandleFunc("POST /api/proposals/{id}/reject", s.apiDecideProposal(false))
	mux.HandleFunc("POST /api/proposals/review", s.apiReviewProposals)
}

// said is the answer of an action whose result is what the kit printed.
type said struct {
	Output string `json:"output"`
}

// POST /api/workspace/pause opens a pause of the kit. reason says why; left out, the pause says it
// was put from the portal.
func (s *Server) apiPause(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Reason string `json:"reason"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	// What a person decided is carried out whatever becomes of the request.
	pause, err := s.work.Pause(context.WithoutCancel(r.Context()), in.Reason)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pause)
}

// POST /api/workspace/resume closes the pause in force.
func (s *Server) apiResume(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	if err := s.work.Resume(context.WithoutCancel(r.Context())); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paused": false})
}

// apiStart answers POST /api/dispatcher/runs, /api/merger/runs and /api/gates/runs: it starts that
// operation of the kit and answers with the execution it created.
func (s *Server) apiStart(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !noInput(w, r) {
			return
		}
		ex, err := s.exec.StartOperation(r.Context(), operation, "")
		if err != nil {
			fail(w, r, err)
			return
		}
		writeStarted(w, ex)
	}
}

// POST /api/dispatcher/simulations asks the dispatcher what it would start now and why each change
// of the backlog waits. Nothing starts and nothing is kept.
func (s *Server) apiSimulateDispatch(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	sim, err := s.work.SimulateDispatch(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sim)
}

// POST /api/cycles starts a cycle. The cycle goes on by itself: Dashboard and Cycles show it.
func (s *Server) apiStartCycle(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	out, err := s.work.StartCycle(context.WithoutCancel(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, said{out})
}

// POST /api/repos/sync brings the main checkout and the child repositories up to date. publish
// also pushes what the kit committed.
func (s *Server) apiSyncRepos(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Publish bool `json:"publish"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := s.work.SyncRepos(context.WithoutCancel(r.Context()), in.Publish)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, said{out})
}

// names is a list of names an action takes as a JSON list, or as one text with commas, which is how
// a form sends it.
type names []string

func (n *names) UnmarshalJSON(raw []byte) error {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		*n = list
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	*n = nil
	if text != "" {
		*n = names{text}
	}
	return nil
}

// POST /api/backlog queues a change: its name, its priority, what to build and what it depends on.
func (s *Server) apiAddToBacklog(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		ChangeName  string `json:"change_name"`
		Priority    string `json:"priority"`
		Description string `json:"description"`
		DependsOn   names  `json:"depends_on"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	item, err := s.work.AddToBacklog(context.WithoutCancel(r.Context()), in.ChangeName, in.Priority, in.Description, in.DependsOn)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// DELETE /api/backlog/{name} takes a change that waits out of the backlog.
func (s *Server) apiRemoveFromBacklog(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := s.work.RemoveFromBacklog(context.WithoutCancel(r.Context()), name); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"change_name": name, "in_backlog": false})
}

// apiDecideProposal answers POST /api/proposals/{id}/approve and /reject: the decision of a person
// about a proposal.
func (s *Server) apiDecideProposal(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !noInput(w, r) {
			return
		}
		id := r.PathValue("id")
		if err := s.work.DecideProposal(context.WithoutCancel(r.Context()), id, approve); err != nil {
			fail(w, r, err)
			return
		}
		status := "rejected"
		if approve {
			status = "approved"
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
	}
}

// POST /api/proposals/review asks the judge about the proposals nobody decided, and answers with
// what the review said.
func (s *Server) apiReviewProposals(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	out, err := s.work.ReviewProposals(context.WithoutCancel(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, said{out})
}
