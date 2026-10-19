package api

import (
	"fmt"
	"log/slog"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// AuditSessionReconcile writes the single boot session-reconcile audit line
// (counts only; no session ids).
func (s *Server) AuditSessionReconcile(r pool.ReconcileResult) {
	result := "ok"
	if r.Failed > 0 {
		result = "partial"
	}
	slog.Info("audit", "event", "session_reconcile", "result", result,
		"open", r.Open, "adopted", r.Adopted, "closed", r.Closed, "failed", r.Failed,
		"skipped", r.Skipped, "attempts", r.Attempts)
	s.audit.RecordFull("session_reconcile", "system", "", result,
		fmt.Sprintf("session_reconcile %s open=%d adopted=%d closed=%d failed=%d", result, r.Open, r.Adopted, r.Closed, r.Failed))
}

// AuditSessionReconcileDeferred records that opens were un-gated before the
// session list succeeded (new opens stay refused until it does).
func (s *Server) AuditSessionReconcileDeferred(err error) {
	kind := "router_error"
	if err != nil {
		kind = router.ErrorKind(err)
	}
	slog.Warn("audit", "event", "session_reconcile", "result", "deferred", "reason", kind)
	s.audit.RecordFull("session_reconcile", "system", "", "deferred", "session_reconcile deferred "+kind)
}
