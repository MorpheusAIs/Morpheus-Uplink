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
	writeJSON(w, http.StatusOK, map[string]any{
		// Both derived keys are re-derivable from the seed, so showing them
		// to the authenticated admin does not widen the secret surface.
		"masterKey": s.masterKey, // full access: prompt + admin
		"promptKey": s.promptKey, // inference only
		"keys":      recordsForAdminJSON(s.store.ListKeys()),
	})
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
		// id/hash collision (incl. tombstones) — same style as import
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
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
	if msg := s.pool.RehydrateError(); msg != "" {
		status["sessionsError"] = msg
	}
	if health, err := s.router.Healthcheck(); err == nil {
		status["router"] = health
	} else {
		status["routerError"] = err.Error()
	}

	var liquidWei *big.Int
	if balance, err := s.router.Balance(); err == nil {
		status["balance"] = balance
		liquidWei = morFromBalance(balance)
	} else {
		status["balanceError"] = err.Error()
	}

	activeWei := s.pool.ActiveStakeWei()
	mor := map[string]any{
		"liquidWei":          bigStr(liquidWei),
		"activeWei":          activeWei.String(),
		"onHoldClaimableWei": nil,
		"onHoldLockedWei":    nil,
		"nextUnlockUTC":      chain.NextUTCMidnight(time.Now()).Format(time.RFC3339),
		"onHoldConfigured":   s.cfg.EthNodeAddress != "",
	}
	if s.cfg.EthNodeAddress != "" {
		addr, err := s.router.WalletAddress()
		if err != nil {
			mor["onHoldConfigured"] = false
		} else if hold, err := chain.UserStakesOnHold(s.cfg.EthNodeAddress, s.cfg.DiamondAddress, addr, 1); err != nil {
			// Transient RPC failure — keep configured=true so GUI doesn't imply mis-deploy.
			mor["onHoldReadFailed"] = true
		} else {
			mor["onHoldClaimableWei"] = hold.Claimable.String()
			mor["onHoldLockedWei"] = hold.Locked.String()
		}
	}
	status["mor"] = mor
	if s.housekeep != nil {
		status["housekeep"] = map[string]any{
			"reclaimReady": s.housekeep.ReclaimReady(),
			"last":         s.housekeep.Last(),
		}
	}

	// Free-space for / and DATA_DIR — never include wallet/cookie/Bearer material.
	status["disk"] = collectDiskStatus("/", s.cfg.DataDir)

	writeJSON(w, http.StatusOK, status)
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
		http.Error(w, err.Error(), http.StatusBadGateway)
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
