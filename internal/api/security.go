package api

import (
	"encoding/json"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// OutflowAlert is a minimal on-chain outflow watch (read-only RPC).
type OutflowAlert struct {
	mu          sync.Mutex
	lastETH     *big.Int
	lastMOR     *big.Int
	lastChecked time.Time
	alerts      []AuditEvent
}

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	alerts := s.audit.Recent(50)
	// Merge outflow stub alerts
	if s.outflow != nil {
		s.outflow.mu.Lock()
		alerts = append(s.outflow.alerts, alerts...)
		s.outflow.mu.Unlock()
	}
	allow, resync := s.allowanceView()
	if s.cfg.MORAllowanceWarning != "" {
		alerts = append([]AuditEvent{{
			At:      time.Now().UTC(),
			Kind:    "allowance",
			Actor:   "boot",
			Message: s.cfg.MORAllowanceWarning,
		}}, alerts...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts":          alerts,
		"morAllowance":    allow,
		"allowanceResync": resync,
		"note":            "Application audit events. Forward container logs to your SIEM via json-file/journald; do not scrape secrets from env.",
	})
}

// checkOutflow compares router balances (via router API) and records drops.
// Uses no wallet key. Optional ETH_RPC_URL is reserved for a follow-up
// direct eth_getLogs watch; for now balance deltas via router are enough.
func (s *Server) CheckOutflow() {
	if s.outflow == nil || s.router == nil {
		return
	}
	raw, err := s.router.Balance()
	if err != nil {
		return
	}
	var bal struct {
		ETH json.RawMessage `json:"eth"`
		MOR json.RawMessage `json:"mor"`
	}
	if json.Unmarshal(raw, &bal) != nil {
		return
	}
	eth := parseWeiField(bal.ETH)
	mor := parseWeiField(bal.MOR)
	s.outflow.mu.Lock()
	defer s.outflow.mu.Unlock()
	s.outflow.lastChecked = time.Now().UTC()
	if s.outflow.lastETH != nil && eth != nil && eth.Cmp(s.outflow.lastETH) < 0 {
		delta := new(big.Int).Sub(s.outflow.lastETH, eth)
		s.outflow.alerts = append([]AuditEvent{{
			At: time.Now().UTC(), Kind: "outflow", Actor: "router-wallet",
			Message: "ETH balance decreased by " + delta.String() + " wei",
		}}, s.outflow.alerts...)
		if len(s.outflow.alerts) > 32 {
			s.outflow.alerts = s.outflow.alerts[:32]
		}
		s.audit.Record("outflow", "router-wallet", "ETH balance decreased")
	}
	if s.outflow.lastMOR != nil && mor != nil && mor.Cmp(s.outflow.lastMOR) < 0 {
		delta := new(big.Int).Sub(s.outflow.lastMOR, mor)
		// Ignore decreases explained by session stake (best-effort signal only).
		s.outflow.alerts = append([]AuditEvent{{
			At: time.Now().UTC(), Kind: "outflow", Actor: "router-wallet",
			Message: "MOR balance decreased by " + delta.String() + " wei",
		}}, s.outflow.alerts...)
		if len(s.outflow.alerts) > 32 {
			s.outflow.alerts = s.outflow.alerts[:32]
		}
		s.audit.Record("outflow", "router-wallet", "MOR balance decreased")
	}
	if eth != nil {
		s.outflow.lastETH = eth
	}
	if mor != nil {
		s.outflow.lastMOR = mor
	}
}

func parseWeiField(raw json.RawMessage) *big.Int {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil
	}
	return n
}
