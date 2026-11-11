package portal

import "net/http"

// overviewRoutes declares the pages a person starts from, and the reads that mirror them in the
// API: what the kit is doing now, what waits for a person, the board of changes and their list.
func (s *Server) overviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", pageFrom(s, "dashboard", s.readDashboard))
	mux.HandleFunc("GET /inbox", pageFrom(s, "inbox", s.readInbox))
	mux.HandleFunc("GET /board", pageFrom(s, "board", s.readBoard))
	mux.HandleFunc("GET /changes", pageFrom(s, "changes", s.readChanges))

	mux.HandleFunc("GET /api/dashboard", jsonFrom(s, s.readDashboard))
	mux.HandleFunc("GET /api/inbox", jsonFrom(s, s.readInbox))
	mux.HandleFunc("GET /api/board", jsonFrom(s, s.readBoard))
	mux.HandleFunc("GET /api/changes", jsonFrom(s, s.readChanges))
}
