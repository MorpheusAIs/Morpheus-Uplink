package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
)

// allowanceStatusFile is what router-allowance-manager writes (0444, no
// secrets). Uplink only reads it.
type allowanceStatusFile struct {
	Mode         string `json:"mode"`
	Source       string `json:"source"`
	Wei          string `json:"wei"`
	BalanceWei   string `json:"balanceWei"`
	LastResyncAt string `json:"lastResyncAt"`
	LastResult   string `json:"lastResult"`
	LastReason   string `json:"lastReason"`
	HeartbeatAt  string `json:"heartbeatAt"`
}

const (
	resyncFlagName     = "resync.request"
	resyncRequestEvery = 60 * time.Second
	maxStatusFileBytes = 64 << 10
)

func (s *Server) readAllowanceStatus() (*allowanceStatusFile, error) {
	if s.cfg.AllowanceStatusFile == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(s.cfg.AllowanceStatusFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStatusFileBytes))
	if err != nil {
		return nil, err
	}
	var st allowanceStatusFile
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// allowanceResyncStatus describes the auto-resync manager for /admin/status.
// available is true only in balance mode with a fresh heartbeat; the GUI
// shows the resync button only then.
func (s *Server) allowanceResyncStatus() map[string]any {
	out := map[string]any{"available": false, "configured": s.cfg.AllowanceStatusFile != "" && s.cfg.AllowanceCtlDir != ""}
	st, err := s.readAllowanceStatus()
	if err != nil {
		out["reason"] = "manager status unavailable"
		return out
	}
	hb, herr := time.Parse(time.RFC3339, st.HeartbeatAt)
	fresh := herr == nil && time.Since(hb) < 3*time.Duration(s.cfg.AllowancePollSec)*time.Second
	out["mode"] = st.Mode
	out["wei"] = st.Wei
	out["balanceWei"] = st.BalanceWei
	out["lastResyncAt"] = st.LastResyncAt
	out["lastResult"] = st.LastResult
	out["lastReason"] = st.LastReason
	out["heartbeatAt"] = st.HeartbeatAt
	out["fresh"] = fresh
	switch {
	case !fresh:
		out["reason"] = "manager not running"
	case st.Mode != "balance":
		out["reason"] = "fixed cap (no auto-resync)"
	case s.cfg.AllowanceCtlDir == "":
		out["reason"] = "resync request dir not configured"
	default:
		out["available"] = true
	}
	return out
}

// handleAllowanceResync drops the presence-only resync flag for the manager.
// It never writes content and never follows a symlink; an existing flag
// means a request is already pending. At most one request per minute.
func (s *Server) handleAllowanceResync(w http.ResponseWriter, r *http.Request) {
	ip := s.lockout.ClientIP(r)
	who := "user=admin"
	if sess, ok := s.sessions.Lookup(adminauth.ReadSessionCookie(r, s.cfg.DevHTTP), false); ok && len(sess.Hash) >= 8 {
		who += " session=" + sess.Hash[:8]
	}
	st := s.allowanceResyncStatus()
	if avail, _ := st["available"].(bool); !avail {
		reason, _ := st["reason"].(string)
		s.auditEvent("allowance_resync_request", ip, "unavailable", who)
		writeJSON(w, http.StatusConflict, map[string]any{"error": "auto-resync unavailable", "reason": reason})
		return
	}
	s.resyncMu.Lock()
	now := time.Now()
	if !s.lastResyncReq.IsZero() && now.Sub(s.lastResyncReq) < resyncRequestEvery {
		wait := resyncRequestEvery - now.Sub(s.lastResyncReq)
		s.resyncMu.Unlock()
		s.auditEvent("allowance_resync_request", ip, "rate_limited", who)
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "resync already requested recently", "retryAfterSeconds": int(wait.Seconds()) + 1})
		return
	}
	s.lastResyncReq = now
	s.resyncMu.Unlock()

	err := createResyncFlag(s.cfg.AllowanceCtlDir)
	switch {
	case err == nil:
		s.auditEvent("allowance_resync_request", ip, "ok", who)
		writeJSON(w, http.StatusAccepted, map[string]any{"requested": true})
	case errors.Is(err, os.ErrExist):
		s.auditEvent("allowance_resync_request", ip, "already_requested", who)
		writeJSON(w, http.StatusAccepted, map[string]any{"requested": true, "alreadyPending": true})
	default:
		s.auditEvent("allowance_resync_request", ip, "error", who)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not record resync request"})
	}
}

// createResyncFlag creates an empty flag with O_CREAT|O_EXCL|O_NOFOLLOW so a
// planted file or symlink is never opened or followed.
func createResyncFlag(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, resyncFlagName), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// allowanceView is the effective allowance for status/Security plus the
// manager status. The manager's read-back value is current; the boot file
// may be stale once funding changed.
func (s *Server) allowanceView() (map[string]any, map[string]any) {
	allowance := map[string]any{
		"wei":     s.cfg.MORAllowanceWei,
		"source":  s.cfg.MORAllowanceSource,
		"warning": s.cfg.MORAllowanceWarning,
	}
	resync := s.allowanceResyncStatus()
	if fresh, _ := resync["fresh"].(bool); fresh && resync["mode"] == "balance" {
		if wei, _ := resync["wei"].(string); wei != "" {
			allowance["wei"] = wei
			allowance["bootWei"] = s.cfg.MORAllowanceWei
		}
	}
	return allowance, resync
}
