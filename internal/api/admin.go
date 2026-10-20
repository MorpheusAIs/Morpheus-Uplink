package api

import (
	"encoding/json"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/chain"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// recordsForAdminJSON returns key records with secrets forced empty when revoked.
func recordsForAdminJSON(recs []keymaker.Record) []keymaker.Record {
	out := make([]keymaker.Record, len(recs))
	copy(out, recs)
	for i := range out {
		if out[i].RevokedAt != nil {
			out[i].Secret = ""
		}
	}
	return out
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "1" || r.URL.Query().Get("reveal") == "true"
	out := map[string]any{
		"keys":   recordsForAdminJSON(s.store.ListKeys()),
		"masked": !reveal,
	}
	if reveal {
		// Explicit reveal only — master key is never returned by default.
		out["masterKey"] = s.masterKey
		out["promptKey"] = s.promptKey
		s.audit.Record("key_reveal", s.lockout.ClientIP(r), "admin revealed master/prompt keys")
	} else {
		out["masterKey"] = maskSecret(s.masterKey)
		out["promptKey"] = maskSecret(s.promptKey)
		// Mask stored secrets on key records by default.
		if keys, ok := out["keys"].([]keymaker.Record); ok {
			masked := make([]keymaker.Record, len(keys))
			copy(masked, keys)
			for i := range masked {
				if masked[i].Secret != "" {
					masked[i].Secret = maskSecret(masked[i].Secret)
				}
			}
			out["keys"] = masked
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "********"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, `body must be {"name": "..."}`, http.StatusBadRequest)
		return
	}
	full, rec, err := keymaker.NewKey(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.AddKey(rec); err != nil {
		// id/hash collision (incl. tombstones) - same style as import
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.audit.Record("key_create", s.lockout.ClientIP(r), "created key "+rec.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":    full, // shown once; only the hash is stored
		"record": rec,
	})
}

// handleImportKey restores a previously issued sk-… key after a storage wipe.
func (s *Server) handleImportKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Key == "" {
		http.Error(w, `body must be {"name": "...", "key": "sk-..."}`, http.StatusBadRequest)
		return
	}
	if keymaker.Equal(req.Key, s.masterKey) || keymaker.Equal(req.Key, s.promptKey) {
		http.Error(w, "master and prompt keys are derived from the seed — no import needed", http.StatusBadRequest)
		return
	}
	rec, err := keymaker.ImportKey(req.Name, req.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.AddKey(rec); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"record": rec})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == keymaker.MasterKeyID || id == keymaker.PromptKeyID {
		http.Error(w, "cannot revoke "+id+" key", http.StatusBadRequest)
		return
	}
	removed, err := s.store.RevokeKey(id)
	if err != nil {
		// Store also refuses master/prompt; surface as 400.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !removed {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	s.audit.Record("key_revoke", s.lockout.ClientIP(r), "revoked key "+id)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Refresh on-chain open sessions into the reuse pool (and GUI list).
	if err := s.pool.Rehydrate(); err != nil {
		// Non-fatal: still return balance / router health.
		_ = err
	}

	sessions := s.pool.ChainOpen()
	for i := range sessions {
		if name := s.catalog.NameByID(sessions[i].ModelID); name != "" {
			sessions[i].ModelName = name
		}
	}
	status := map[string]any{
		"version":                Version,
		"sessions":               sessions, // on-chain opens (pooled flagged)
		"poolSessions":           s.pool.Snapshot(),
		"sessionDurationSec":     s.cfg.SessionDurationSec,
		"sessionDurationDefault": s.cfg.SessionDurationSec,
	}
	// Read-only router wallet (replaces former /node/wallet passthrough).
	// Falls back to the last router answer when the router is unavailable.
	addr, src, werr := s.walletAddress()
	if addr != "" {
		status["walletAddress"] = addr
		status["walletAddressSource"] = src
	}
	if werr != nil {
		status["walletAddressError"] = werr.Error()
	}
	if msg := s.pool.RehydrateError(); msg != "" {
		status["sessionsError"] = msg
	}
	if health, err := s.router.Healthcheck(); err == nil {
		status["router"] = health
	} else {
		status["routerError"] = err.Error()
		status["routerErrorKind"] = router.KindNotReady
	}

	var liquidWei *big.Int
	if balance, err := s.router.Balance(); err == nil {
		status["balance"] = balance
		liquidWei = morFromBalance(balance)
	} else {
		status["balanceError"] = err.Error()
	}
	status["routerAuth"] = s.routerAuthStatus()
	status["sessionReconcile"] = s.sessionReconcileStatus()

	activeWei := s.pool.ActiveStakeWei()
	mor := map[string]any{
		"liquidWei":          bigStr(liquidWei),
		"activeWei":          activeWei.String(),
		"onHoldClaimableWei": nil,
		"onHoldLockedWei":    nil,
		"nextUnlockUTC":      chain.NextUTCMidnight(time.Now()).Format(time.RFC3339),
		"onHoldConfigured":   true,
	}
	if hold, err := s.router.UserStakesOnHold(1); err != nil {
		mor["onHoldReadFailed"] = true
	} else {
		mor["onHoldClaimableWei"] = hold.Claimable.String()
		mor["onHoldLockedWei"] = hold.Locked.String()
	}
	status["mor"] = mor
	if s.housekeep != nil {
		status["housekeep"] = map[string]any{
			"reclaimReady": s.housekeep.ReclaimReady(),
			"last":         s.housekeep.Last(),
		}
	}

	// Free-space for / and DATA_DIR - never include wallet/cookie/Bearer material.
	status["disk"] = collectDiskStatus("/", s.cfg.DataDir)
	auth := s.store.GetAuthState()
	status["twoFactor"] = map[string]any{
		"enrolled":       auth.Enrolled,
		"required":       s.cfg.Admin2FARequired,
		"breakGlassWarn": s.breakGlassWarn(),
		// MASTER_KEY_ADMIN=true: the master key works as an admin Bearer
		// token without 2FA. The GUI shows a banner while this is on.
		"masterKeyAdmin": s.cfg.MasterKeyAdmin,
	}
	writeJSON(w, http.StatusOK, status)
}

// walletAddress returns the node wallet address and where it came from:
// "router" (live) or "last-known" (earlier live answer). err is the live lookup error, if any.
func (s *Server) walletAddress() (addr, source string, err error) {
	live, err := s.router.WalletAddress()
	s.walletMu.Lock()
	defer s.walletMu.Unlock()
	if err == nil && live != "" {
		s.lastWallet = live
		return live, "router", nil
	}
	if s.lastWallet != "" {
		return s.lastWallet, "last-known", err
	}
	return "", "", err
}

// routerAuthStatus summarises whether the router accepts Uplink's
// credential. Never includes the credential.
func (s *Server) routerAuthStatus() map[string]any {
	since, msg, failing := s.router.AuthFailure()
	out := map[string]any{"ok": !failing}
	if failing {
		out["kind"] = router.KindAuth
		out["since"] = since.UTC().Format(time.RFC3339)
		out["message"] = router.ErrorHint(router.KindAuth)
		if msg != "" {
			out["routerMessage"] = msg
		}
	}
	return out
}

// routerErr writes a router failure with a distinct, operator-facing kind
// (router_auth / router_not_ready / router_error) so the GUI can tell a
// rejected credential from a router that is still starting.
func routerErr(w http.ResponseWriter, what string, err error) {
	kind := router.ErrorKind(err)
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"error": what + ": " + err.Error(),
		"kind":  kind,
		"hint":  router.ErrorHint(kind),
	})
}

func (s *Server) handleHousekeep(w http.ResponseWriter, r *http.Request) {
	if s.housekeep == nil || !s.housekeep.ReclaimReady() {
		http.Error(w, "reclaim not available", http.StatusServiceUnavailable)
		return
	}
	res := s.housekeep.RunOnce(r.Context())
	writeJSON(w, http.StatusOK, res)
}

func morFromBalance(raw json.RawMessage) *big.Int {
	var bal struct {
		MOR json.RawMessage `json:"mor"`
		Mor json.RawMessage `json:"MOR"`
	}
	if json.Unmarshal(raw, &bal) != nil {
		return nil
	}
	for _, field := range []json.RawMessage{bal.MOR, bal.Mor} {
		if len(field) == 0 {
			continue
		}
		s := string(field)
		if len(s) >= 2 && s[0] == '"' {
			s = s[1 : len(s)-1]
		}
		if n, ok := new(big.Int).SetString(s, 10); ok {
			return n
		}
	}
	return nil
}

func bigStr(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"usage": s.store.Usage()})
}

func (s *Server) handlePoolCloseAll(w http.ResponseWriter, r *http.Request) {
	res := s.pool.CloseAll()
	status := http.StatusOK
	if res.Attempted > 0 && res.Closed == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{
		"closed":    res.Closed > 0,
		"attempted": res.Attempted,
		"ok":        res.Closed,
		"failed":    res.Failed,
		"errors":    res.Errors,
	})
}

// handleEstimateStake returns the real on-chain MOR lock for a model + duration:
// stake ≈ (supply × pricePerSecond × duration) ÷ today's emissions budget.
func (s *Server) handleEstimateStake(w http.ResponseWriter, r *http.Request) {
	modelName := r.URL.Query().Get("model")
	if modelName == "" {
		http.Error(w, `query "model" required`, http.StatusBadRequest)
		return
	}
	durationSec := s.cfg.SessionDurationSec
	if durationSec < 600 {
		durationSec = 600
	}
	if v := r.URL.Query().Get("duration"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 600 {
			http.Error(w, `duration must be an integer >= 600`, http.StatusBadRequest)
			return
		}
		durationSec = n
	}

	modelID, err := s.catalog.Resolve(modelName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	models, err := s.catalog.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var pps *big.Int
	var rate float64
	for _, m := range models {
		if m.ID == modelID || m.Name == modelName {
			pps = m.LowestPricePerSecondWei()
			rate = m.LowestMorPerHour()
			break
		}
	}
	if pps == nil {
		http.Error(w, "no bid price for model", http.StatusNotFound)
		return
	}

	supply, err := s.router.TokenSupply()
	if err != nil {
		http.Error(w, "token supply: "+err.Error(), http.StatusBadGateway)
		return
	}
	budget, err := s.router.TodaysBudget()
	if err != nil {
		http.Error(w, "todays budget: "+err.Error(), http.StatusBadGateway)
		return
	}
	stake, err := router.SessionStakeWei(supply, budget, pps, int64(durationSec))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"model":             modelName,
		"modelId":           modelID,
		"durationSec":       durationSec,
		"priceMorPerHour":   rate,
		"pricePerSecondWei": pps.String(),
		"stakeWei":          stake.String(),
		"sessionCostWei":    new(big.Int).Mul(pps, big.NewInt(int64(durationSec))).String(),
		"supplyWei":         supply.String(),
		"budgetWei":         budget.String(),
		"minDurationSec":    600,
		"explanation":       "On-chain lock ≈ (MOR supply × bid price/sec × duration) ÷ today's emissions budget — not MOR/h × hours.",
	})
}

// handleEstimateStakes returns on-chain lock estimates for every catalog model
// at the given duration, plus liquid wallet MOR for affordability checks.
func (s *Server) handleEstimateStakes(w http.ResponseWriter, r *http.Request) {
	durationSec := s.cfg.SessionDurationSec
	if durationSec < 600 {
		durationSec = 600
	}
	if v := r.URL.Query().Get("duration"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 600 {
			http.Error(w, `duration must be an integer >= 600`, http.StatusBadRequest)
			return
		}
		durationSec = n
	}

	models, err := s.catalog.List()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": err.Error(),
			"kind":  "catalog_fetch",
			"hint":  "model catalog fetch failed (ACTIVE_MODELS_URL)",
		})
		return
	}
	supply, err := s.router.TokenSupply()
	if err != nil {
		routerErr(w, "token supply", err)
		return
	}
	budget, err := s.router.TodaysBudget()
	if err != nil {
		routerErr(w, "todays budget", err)
		return
	}

	var liquidWei *big.Int
	if bal, err := s.router.Balance(); err == nil {
		liquidWei = morFromBalance(bal)
	}

	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		pps := m.LowestPricePerSecondWei()
		if pps == nil {
			continue
		}
		stake, err := router.SessionStakeWei(supply, budget, pps, int64(durationSec))
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"model":             m.Name,
			"modelId":           m.ID,
			"priceMorPerHour":   m.LowestMorPerHour(),
			"pricePerSecondWei": pps.String(),
			"stakeWei":          stake.String(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"durationSec":    durationSec,
		"minDurationSec": 600,
		"liquidWei":      bigStr(liquidWei),
		"supplyWei":      supply.String(),
		"budgetWei":      budget.String(),
		"models":         out,
		"explanation":    "On-chain lock ≈ (MOR supply × bid price/sec × duration) ÷ today's emissions budget.",
	})
}

func (s *Server) handlePoolCloseOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	if err := s.pool.CloseSession(id); err != nil {
		http.Error(w, "close session: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": true, "sessionId": id})
}

// handlePruneUsage removes old usage-history rows under DATA_DIR.
// Body must be {"confirm":"PRUNE_USAGE"}. Keys and open sessions are kept.
func (s *Server) handlePruneUsage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Confirm != "PRUNE_USAGE" {
		http.Error(w, `body must be {"confirm":"PRUNE_USAGE"}`, http.StatusBadRequest)
		return
	}
	days := s.cfg.UsagePruneDays
	if days <= 0 {
		days = 30
	}
	path := s.store.Path()
	bytesBefore := fileSizeBestEffort(path)
	removed, err := s.store.PruneUsageOlderThan(days)
	if err != nil {
		http.Error(w, "prune usage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bytesAfter := fileSizeBestEffort(path)
	writeJSON(w, http.StatusOK, map[string]any{
		"removedRows": removed,
		"bytesBefore": bytesBefore,
		"bytesAfter":  bytesAfter,
	})
}

func fileSizeBestEffort(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
