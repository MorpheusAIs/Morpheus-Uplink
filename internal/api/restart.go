package api

import "net/http"

// Console restart (POST /admin/restart): restarts the Uplink process so
// "Uplink restart" settings take effect. Same guard as the settings API
// (cookie session, CSRF, no Authorization header) plus a fresh step-up,
// like a restart-scope save. The handler only asks for the restart; the
// hook from cmd/uplink performs it (graceful HTTP stop, audit flush, then
// the same binary started again in place). Neither touches the router.

// SetRestartHook sets the function that restarts the process. It is called
// once, after the 202 answer is written.
func (s *Server) SetRestartHook(fn func()) { s.restartHook = fn }

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireStepUp(w, r) {
		return
	}
	if s.restartHook == nil {
		http.Error(w, "restart is not available", http.StatusServiceUnavailable)
		return
	}
	if !s.restarting.CompareAndSwap(false, true) {
		http.Error(w, "restart already in progress", http.StatusConflict)
		return
	}
	s.auditAdmin(r, "uplink_restart", "requested from the console")
	writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go s.restartHook()
}
