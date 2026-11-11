package portal

import (
	"context"
	"net/http"

	"github.com/mayeco/foreman-kit/internal/store"
)

// systemRoutes declares the pages of System, the reads that mirror them in the API, one for each
// tab (/api/system/{tab}), and what a person does about the installation of the kit. Health and
// Processes are rows, read from one snapshot; the others are what the kit says of the application,
// asked of the kit before the page takes its snapshot.
func (s *Server) systemRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /system/health", pageFrom(s, "health", s.readHealth))
	mux.HandleFunc("GET /system/settings", pageAsking(s, "settings", s.askSettings))
	mux.HandleFunc("GET /system/recommendations", pageAsking(s, "recommendations", s.askRecommendations))
	mux.HandleFunc("GET /system/setup", pageAsking(s, "setup", s.askSetup))
	mux.HandleFunc("GET /system/processes", pageFrom(s, "processes", s.readProcesses))
	mux.HandleFunc("GET /system/prompts", pageAsking(s, "prompts", s.askPrompts))
	mux.HandleFunc("GET /system/prompts/{role}", pageAsking(s, "prompt", s.askPrompt))

	mux.HandleFunc("GET /api/system/health", jsonFrom(s, s.readHealth))
	mux.HandleFunc("GET /api/system/settings", jsonAsking(s, s.askSettings))
	mux.HandleFunc("GET /api/system/recommendations", jsonAsking(s, s.askRecommendations))
	mux.HandleFunc("GET /api/system/setup", jsonAsking(s, s.askSetup))
	mux.HandleFunc("GET /api/system/processes", jsonFrom(s, s.readProcesses))
	mux.HandleFunc("GET /api/system/prompts", jsonAsking(s, s.askPrompts))
	mux.HandleFunc("GET /api/system/prompts/{role}", jsonAsking(s, s.askPrompt))

	mux.HandleFunc("POST /api/system/preflight", s.apiRunPreflight)
	mux.HandleFunc("POST /api/system/settings", s.apiSetSetting)
	mux.HandleFunc("POST /api/system/repos", s.apiSetupRepository)
}

// POST /api/system/settings changes one setting, as `foreman-kit config set` does: key names it the
// way the page of settings lists it, and value is what its file is to hold. The value is checked
// against what the catalog of the kit says the key accepts; with dry_run it is only checked. It
// answers with what the kit printed: what changed, or what would.
func (s *Server) apiSetSetting(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		DryRun bool   `json:"dry_run"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := s.work.SetSetting(in.Key, in.Value, in.DryRun)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, said{Output: out})
}

// POST /api/system/repos adds a child repository to the application, or prepares one that is there:
// url (with name and branch, both optional) adds it under repos/ and prepares it; name alone prepares
// the repository of that name, and force writes its repo.config.yaml again over the one it has. It
// answers with what the kit printed.
func (s *Server) apiSetupRepository(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		URL    string `json:"url"`
		Name   string `json:"name"`
		Branch string `json:"branch"`
		Force  bool   `json:"force"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	// Adding a repository clones it and writes its files: it is carried through whatever becomes of
	// the request, or it would leave the repository half added.
	out, err := s.work.SetupRepository(context.WithoutCancel(r.Context()), in.URL, in.Name, in.Branch, in.Force)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, said{Output: out})
}

// POST /api/system/preflight starts the preflight of the kit: the toolchain, the files of the kit,
// the services its repositories need, a ping of its agents and the canary of the evaluator, or only
// the services when services_only says so. It answers 202 with the execution that runs it, and 409
// while one it started before is still going.
func (s *Server) apiRunPreflight(w http.ResponseWriter, r *http.Request) {
	in, err := decodeInput[struct {
		ServicesOnly bool `json:"services_only"`
	}](r)
	if err != nil {
		fail(w, r, err)
		return
	}
	var last *store.ExecutionDetail
	err = s.store.View(r.Context(), func(db *store.View) (err error) {
		last, err = db.LatestJob(r.Context(), preflightKinds...)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	if last != nil && last.Status == "running" && !last.OwnerGone {
		fail(w, r, errConflict("a preflight is running: its output is at %s", executionHref(last.ExecID)))
		return
	}
	operation := "preflight"
	if in.ServicesOnly {
		operation = "preflight-services"
	}
	// The preflight goes on in the background whatever becomes of the request that started it.
	ex, err := s.exec.StartOperation(context.WithoutCancel(r.Context()), operation, "")
	if err != nil {
		fail(w, r, err)
		return
	}
	writeStarted(w, ex)
}
