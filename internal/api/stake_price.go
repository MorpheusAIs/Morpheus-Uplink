package api

// Max stMOR/hr refusals (spec §3) and the stMOR/hr shown on the API and GUI
// (spec §4).

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

const stakeLimitExceededMsg = "No provider for this model could be opened within the Uplink Max stMOR/hr limit"
const stakeLimitInvalidMsg = "The Uplink Max stMOR/hr setting is invalid; fix the value and restart"

// writeStakeRefusal audits and writes the appropriate error for stake limit refusals.
// Returns true if the error was a recognized stake limit error, false otherwise.
func (s *Server) writeStakeRefusal(w http.ResponseWriter, modelID string, err error) bool {
	if errors.Is(err, pool.ErrStakeLimitExceeded) {
		s.auditProviderUnavailable(modelID, "stake_limit_exceeded")
		writeTypedError(w, http.StatusServiceUnavailable, "stake_limit_exceeded", stakeLimitExceededMsg)
		return true
	}
	if errors.Is(err, pool.ErrStakeLimitInvalid) {
		s.auditProviderUnavailable(modelID, "stake_limit_invalid")
		writeTypedError(w, http.StatusServiceUnavailable, "stake_limit_invalid", stakeLimitInvalidMsg)
		return true
	}
	return false
}

// stakePrice holds the computed stake price for a model.
type stakePrice struct {
	Wei       *big.Int // nil: unknown
	PPS       *big.Int
	Source    string // "firstTry" | "est." | ""
	OverLimit bool
}

// heldStakePrice computes the stake price from the held pool state.
// Never reads the router directly.
func (s *Server) heldStakePrice(m catalog.Model) stakePrice {
	// Attempt to use cached FirstTry with non-nil StMorPerHourWei
	if ft, ok := s.pool.CachedFirstTry(m.ID); ok && ft.StMorPerHourWei != nil {
		return stakePrice{
			Wei:       ft.StMorPerHourWei,
			PPS:       ft.Bid.PricePerSecond,
			Source:    "firstTry",
			OverLimit: ft.OverLimit,
		}
	}

	// Fallback to estimation using lowest bid
	bid, ok := m.LowestBid()
	if !ok {
		return stakePrice{PPS: nil, Source: ""}
	}

	// Parse PricePerSecond as decimal
	pps, ok := new(big.Int).SetString(bid.PricePerSecond, 10)
	if !ok || pps.Sign() <= 0 {
		return stakePrice{PPS: nil, Source: ""}
	}

	// Get held stake inputs pair (Supply, Budget)
	inputs, hasPair := s.pool.HeldStakeInputs()
	if !hasPair {
		// No held pair; return Wei as nil
		return stakePrice{Wei: nil, PPS: pps, Source: "est."}
	}

	// Compute StMorPerHourWei from held pair
	wei, err := router.StMorPerHourWei(inputs.Supply, inputs.Budget, pps)
	if err != nil {
		return stakePrice{Wei: nil, PPS: pps, Source: "est."}
	}

	// Check over-limit using StakeLimit
	sp := stakePrice{Wei: wei, PPS: pps, Source: "est."}
	if limit, _ := s.pool.StakeLimit(); limit != nil {
		sp.OverLimit = wei.Cmp(limit) > 0 // the server's strict > (spec §4 Marker)
	}
	return sp
}

// fillStakePrices concurrently fills FirstTries for the given models.
// User action: fires FillFirstTry for each model concurrently.
func (s *Server) fillStakePrices(ctx context.Context, models []catalog.Model) {
	var wg sync.WaitGroup
	for _, m := range models {
		wg.Add(1)
		go func(modelID string) {
			defer wg.Done()
			// Pool bounds concurrent fills to 4 internally.
			_, _ = s.pool.FillFirstTry(ctx, modelID)
		}(m.ID)
	}
	wg.Wait()
}

// stakePriceFields are the API price fields (spec §4 API); the GUI marks
// over limit itself from stakeLimit.
func stakePriceFields(p stakePrice) map[string]any {
	m := make(map[string]any)

	// stMorPerHourWei
	if p.Wei != nil {
		m["stMorPerHourWei"] = p.Wei.String()
	} else {
		m["stMorPerHourWei"] = nil
	}

	// stMorPerHour (float64 MOR = Wei/1e18)
	if p.Wei != nil {
		rat := new(big.Rat).SetFrac(p.Wei, big.NewInt(1e18))
		f, _ := rat.Float64()
		m["stMorPerHour"] = f
	} else {
		m["stMorPerHour"] = nil
	}

	// priceSource: nil when empty
	if p.Source == "" {
		m["priceSource"] = nil
	} else {
		m["priceSource"] = p.Source
	}

	// pricePerSecondWei
	if p.PPS != nil {
		m["pricePerSecondWei"] = p.PPS.String()
	} else {
		m["pricePerSecondWei"] = nil
	}

	return m
}

// sessionStMorPerHourWei returns the StMorPerHourWei string for a specific bid in a model.
// Uses strings.EqualFold for BidID comparison.
func (s *Server) sessionStMorPerHourWei(m catalog.Model, bidID string) *string {
	for _, bd := range m.BidDetail {
		if !strings.EqualFold(bd.BidID, bidID) {
			continue
		}
		pps, ok := new(big.Int).SetString(bd.PricePerSecond, 10)
		if !ok || pps.Sign() <= 0 {
			return nil
		}
		inputs, held := s.pool.HeldStakeInputs() // display: never reads
		if !held {
			return nil
		}
		wei, err := router.StMorPerHourWei(inputs.Supply, inputs.Budget, pps)
		if err != nil {
			return nil
		}
		v := wei.String()
		return &v
	}
	return nil
}

// stakeLimitStatus is /admin/status stakeLimit: the boot-effective value
// (null when off, never "") and whether the saved value is invalid.
func (s *Server) stakeLimitStatus() map[string]any {
	limit, invalid := s.pool.StakeLimit()
	var w any
	if limit != nil {
		w = limit.String()
	}
	return map[string]any{"maxStMorPerHourWei": w, "invalid": invalid}
}
