package portal

import (
	"context"
	"net/http"

	"github.com/mayeco/foreman-kit/internal/store"
)

// agentRoutes declares the page of the engines the kit runs its prompts on, the read that mirrors it
// in the API, and the one thing a person does to an engine from the portal: ping it.
func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /agents", pageFrom(s, "agents", s.readAgents))
	mux.HandleFunc("GET /api/agents", jsonFrom(s, s.readAgents))

	mux.HandleFunc("POST /api/engines/{engine}/ping", s.apiPingEngine)
}

// pinged is the answer of a ping: how it went, and the engine as the registry has it afterwards.
type pinged struct {
	// OK says the engine answered; Summary says in a sentence how the ping went.
	OK      bool       `json:"ok"`
	Summary string     `json:"summary"`
	Ping    PingResult `json:"ping"`
	// Engine is the engine on record after the ping: its health is what the ping found.
	Engine *store.EngineRecord `json:"engine,omitempty"`
}

// POST /api/engines/{engine}/ping runs the ping of an engine and records how it answered as its
// health. It answers 200 whether the engine answered or not: a ping that finds the engine down did
// what it was asked. It is 404 for a name the kit runs no engine under.
func (s *Server) apiPingEngine(w http.ResponseWriter, r *http.Request) {
	if !noInput(w, r) {
		return
	}
	// The ping is recorded whatever becomes of the request that asked for it.
	ctx := context.WithoutCancel(r.Context())
	res, err := s.exec.Ping(ctx, r.PathValue("engine"))
	if err != nil {
		fail(w, r, err)
		return
	}
	out := pinged{OK: res.Status == "ok", Summary: res.Summary(), Ping: res}
	err = s.store.View(ctx, func(db *store.View) error {
		engines, err := db.ListEngines(ctx)
		for i := range engines {
			if engines[i].Engine == res.Engine {
				out.Engine = &engines[i]
			}
		}
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
