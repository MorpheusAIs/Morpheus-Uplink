package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// ReconcileStatusFileName is the boot reconcile status file in DATA_DIR.
const ReconcileStatusFileName = "session-reconcile.json"

// AuditSessionReconcile writes the per-pass summary audit line (counts only).
func (s *Server) AuditSessionReconcile(r pool.ReconcileResult) {
	result := "ok"
	if r.Failed > 0 || r.Deferred > 0 {
		result = "partial"
	}
	slog.Info("audit", "event", "session_reconcile", "result", result,
		"open", r.Open, "adopted", r.Adopted, "closed", r.Closed, "failed", r.Failed,
		"skipped", r.Skipped, "deferred", r.Deferred, "attempts", r.Attempts, "late", r.Late, "catalog", r.Catalog)
	s.auditSystem("session_reconcile", "system", result,
		fmt.Sprintf("session_reconcile %s open=%d adopted=%d closed=%d failed=%d skipped=%d deferred=%d",
			result, r.Open, r.Adopted, r.Closed, r.Failed, r.Skipped, r.Deferred), true)
}

// AuditSessionReconcileEvent writes one audit event per adopt, per close and
// for a cap hit. Session ids appear only as a short prefix.
func (s *Server) AuditSessionReconcileEvent(ev pool.ReconcileEvent) {
	attrs := []any{"event", ev.Kind, "result", ev.Result, "reason", ev.Reason}
	if ev.Source != "" {
		attrs = append(attrs, "source", ev.Source)
	}
	msg := ev.Kind + " " + ev.Result + " " + ev.Reason
	if ev.Session != "" {
		attrs = append(attrs, "session", ev.Session)
		msg += " session=" + ev.Session
	}
	if ev.Kind == "session_reconcile_cap" {
		attrs = append(attrs, "deferred", ev.Count)
		msg += fmt.Sprintf(" deferred=%d", ev.Count)
	}
	slog.Info("audit", attrs...)
	s.auditSystem(ev.Kind, "system", ev.Result, msg, true)
}

// AuditSessionReconcileDeferred records that opens were un-gated before the
// session list succeeded (new opens stay refused until it does).
func (s *Server) AuditSessionReconcileDeferred(err error) {
	kind := "router_error"
	if err != nil {
		kind = router.ErrorKind(err)
	}
	slog.Warn("audit", "event", "session_reconcile", "result", "deferred", "reason", kind)
	s.auditSystem("session_reconcile", "system", "deferred", "session_reconcile deferred "+kind, true)
}

// WriteReconcileStatus writes the reconcile state to DATA_DIR atomically
// (0640). Best effort: a write failure is logged, never fatal.
func (s *Server) WriteReconcileStatus(st pool.ReconcileState) {
	if err := writeReconcileStatusFile(s.cfg.DataDir, st); err != nil {
		slog.Warn("reconcile status file", "error", err.Error())
	}
}

func writeReconcileStatusFile(dir string, st pool.ReconcileState) error {
	if dir == "" {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-reconcile-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		if werr = os.Chmod(tmp.Name(), 0o640); werr == nil {
			if werr = os.Rename(tmp.Name(), filepath.Join(dir, ReconcileStatusFileName)); werr == nil {
				return nil
			}
		}
	}
	_ = os.Remove(tmp.Name())
	if werr != nil {
		return werr
	}
	return cerr
}

// sessionReconcileStatus is the /admin/status view.
func (s *Server) sessionReconcileStatus() pool.ReconcileState {
	return s.pool.ReconcileStatus()
}
