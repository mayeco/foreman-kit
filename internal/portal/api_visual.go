package portal

import (
	"context"
	"net/http"

	"github.com/mayeco/foreman-kit/internal/store"
)

// visualRoutes declares the pages of visual QA, the reads that mirror them in the API, and what a
// person does about it: ask for a check in a browser, say how one ended, and open and close the
// session that works them.
func (s *Server) visualRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /visual", pageFrom(s, "visual", s.readVisual))
	mux.HandleFunc("GET /visual/sessions", pageFrom(s, "visual_sessions", s.readVisualSessions))
	mux.HandleFunc("GET /visual/sessions/{stamp}", pageFrom(s, "visual_session", s.readVisualSession))

	mux.HandleFunc("GET /api/visual", jsonFrom(s, s.readVisual))
	mux.HandleFunc("GET /api/visual/sessions", jsonFrom(s, s.readVisualSessions))
	mux.HandleFunc("GET /api/visual/sessions/{stamp}", jsonFrom(s, s.readVisualSession))

	mux.HandleFunc("POST /api/visual-qa/items", s.apiRequestVisualCheck)
	mux.HandleFunc("POST /api/visual-qa/items/{id}/verdict", s.apiResolveVisualCheck)
	mux.HandleFunc("POST /api/visual-qa/session/begin", s.apiBeginVisualSession)
	mux.HandleFunc("POST /api/visual-qa/session/end", s.apiEndVisualSession)
}

// POST /api/visual-qa/items asks for a check in a browser: what says what to look at, and
// change_name names the change it is about, when it is about one. It answers 201 with the check.
func (s *Server) apiRequestVisualCheck(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		What       string `json:"what"`
		ChangeName string `json:"change_name"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	check, err := s.work.RequestVisualCheck(context.WithoutCancel(r.Context()), in.What, in.ChangeName)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, check)
}

// POST /api/visual-qa/items/{id}/verdict records how a check in a browser ended: verdict is
// "verified", "regression", "not-visual" or "cannot-verify", and note says what was seen. It
// answers with the check as it is afterwards.
func (s *Server) apiResolveVisualCheck(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	check, err := s.work.ResolveVisualCheck(context.WithoutCancel(r.Context()), r.PathValue("id"), in.Verdict, in.Note)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, check)
}

// sessionBegun is the answer of the action that opens a visual QA session.
type sessionBegun struct {
	Session *store.VisualSession `json:"session"`
	Output  string               `json:"output"`
}

// POST /api/visual-qa/session/begin opens a visual QA session. It answers with the session and
// with what the kit printed, and 409 when no session opened.
func (s *Server) apiBeginVisualSession(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	// A session that begins takes the lock of visual QA and the checks that wait: it is carried
	// through whatever becomes of the request, or it would leave a session nobody opened.
	session, output, err := s.work.BeginVisualSession(context.WithoutCancel(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionBegun{Session: session, Output: output})
}

// POST /api/visual-qa/session/end closes the session in progress. It answers with what the kit
// printed, and 409 when no session is open.
func (s *Server) apiEndVisualSession(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	// Ending a session waits for the lock that records, commits, pushes and releases the lock of
	// visual QA: stopped halfway by a browser that goes away, it leaves a session open for nobody.
	output, err := s.work.EndVisualSession(context.WithoutCancel(r.Context()))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, said{Output: output})
}
