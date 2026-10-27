package api

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OutflowAlert watches the router wallet's ETH and MOR balances (read-only,
// via the router API) and records one audit event per decrease. A MOR drop
// is usually a session stake lock, which is returned to the wallet; an ETH
// drop is usually gas.
type OutflowAlert struct {
	mu          sync.Mutex
	lastETH     *big.Int
	lastMOR     *big.Int
	lastChecked time.Time
}

const (
	balanceDropKind = "balance_drop"
	morDropNote     = "(session stakes lock and are returned)"
	ethDropNote     = "(usually gas)"
)

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts": s.audit.Recent(50),
		"note":   "Application audit events. Forward container logs to your SIEM via json-file/journald; do not scrape secrets from env.",
	})
}

// CheckOutflow compares router balances (via router API) and records drops.
// Uses no wallet key.
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
		s.auditSystem(balanceDropKind, "router-wallet", "", "ETH balance decreased by "+formatUnits18(delta)+" ETH "+ethDropNote, false)
	}
	if s.outflow.lastMOR != nil && mor != nil && mor.Cmp(s.outflow.lastMOR) < 0 {
		delta := new(big.Int).Sub(s.outflow.lastMOR, mor)
		s.auditSystem(balanceDropKind, "router-wallet", "", "MOR balance decreased by "+formatUnits18(delta)+" MOR "+morDropNote, false)
	}
	if eth != nil {
		s.outflow.lastETH = eth
	}
	if mor != nil {
		s.outflow.lastMOR = mor
	}
}

// formatUnits18 renders an 18-decimal wei amount with up to 6 decimals.
func formatUnits18(wei *big.Int) string {
	r := new(big.Rat).SetFrac(wei, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	out := r.FloatString(6)
	if strings.Contains(out, ".") {
		out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	}
	return out
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
