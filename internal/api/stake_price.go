package api

// Max stMOR per session refusals and the prices the API and GUI show.
// Prices come from the bids feed (no router read per bid): each model's
// cheapest bid an open could use, staked for the session length at the
// held supply/budget pair.

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

const stakeLimitInvalidMsg = "The Uplink Max stMOR per session setting is invalid; fix the value and restart"

// stakeLimitExceededMsg is the 503 text: the stake needed, the cap, the
// session length and what to change.
func stakeLimitExceededMsg(modelName string, e *pool.StakeLimitError) string {
	if e == nil {
		return "No provider for this model could be opened within the Uplink Max stMOR per session cap. Raise Max stMOR per session or ask for a shorter session."
	}
	need := pool.FormatMor(e.NeedWei) + " stMOR"
	if e.NeedWei == nil {
		need = "an unknown stake (no priced bid)"
	}
	return fmt.Sprintf("Opening %s for %s needs at least %s; the Uplink cap is %s stMOR per session. Raise Max stMOR per session or ask for a shorter session.",
		modelName, pool.FormatDuration(e.DurationSec), need, pool.FormatMor(e.CapWei))
}

// writeStakeRefusal audits and writes the error for a stake-cap refusal.
// It returns false when err is not one.
func (s *Server) writeStakeRefusal(w http.ResponseWriter, modelID string, err error) bool {
	if errors.Is(err, pool.ErrStakeLimitExceeded) {
		var e *pool.StakeLimitError
		errors.As(err, &e)
		name := s.catalog.NameByID(modelID)
		if name == "" {
			name = "this model"
		}
		s.auditProviderUnavailable(modelID, "stake_limit_exceeded")
		writeTypedError(w, http.StatusServiceUnavailable, "stake_limit_exceeded", stakeLimitExceededMsg(name, e))
		return true
	}
	if errors.Is(err, pool.ErrStakeLimitInvalid) {
		s.auditProviderUnavailable(modelID, "stake_limit_invalid")
		writeTypedError(w, http.StatusServiceUnavailable, "stake_limit_invalid", stakeLimitInvalidMsg)
		return true
	}
	return false
}

// stakePrice is a model's shown price.
type stakePrice struct {
	PPS      *big.Int // cheapest usable bid's price per second; nil: no priced bid
	HourWei  *big.Int // stMOR per hour at the held pair; nil before the first read
	StakeWei *big.Int // stake for the session length at the held pair; nil likewise
}

// feedBid is the model's cheapest bid in the feed that an open could use:
// healthy in the health feed, provider not excluded (the catalog drops
// those) and not cooling for the model. When the health feed cannot be
// read, or no bid passes, it is the catalog's cheapest bid, so the model
// still lists with a price.
func (s *Server) feedBid(m catalog.Model) *big.Int {
	healthy, herr := s.catalog.HealthyBidIDs(m.ID)
	var best *big.Int
	if herr == nil {
		for _, b := range m.BidDetail {
			if _, ok := healthy[strings.ToLower(strings.TrimSpace(b.BidID))]; !ok {
				continue
			}
			if s.pool.IsCooling(m.ID, b.ProviderID) {
				continue
			}
			pps, ok := new(big.Int).SetString(strings.TrimSpace(b.PricePerSecond), 10)
			if !ok || pps.Sign() <= 0 {
				continue
			}
			if best == nil || pps.Cmp(best) < 0 {
				best = pps
			}
		}
	}
	if best == nil {
		best = m.LowestPricePerSecondWei()
		if best != nil && best.Sign() <= 0 {
			best = nil
		}
	}
	return best
}

// feedStakePrice prices m for a session of durationSec at the held pair.
// It never reads the router.
func (s *Server) feedStakePrice(m catalog.Model, durationSec int) stakePrice {
	p := stakePrice{PPS: s.feedBid(m)}
	if p.PPS == nil {
		return p
	}
	in, ok := s.pool.HeldStakeInputs()
	if !ok {
		return p
	}
	if w, err := router.StMorPerHourWei(in.Supply, in.Budget, p.PPS); err == nil {
		p.HourWei = w
	}
	if w, err := router.StakeForDurationWei(in.Supply, in.Budget, p.PPS, int64(durationSec)); err == nil {
		p.StakeWei = w
	}
	return p
}

// stakeGroup is a priced model's list group: overBudget when the stake is
// above liquid MOR (liquid known), overCap when above the cap (cap on),
// else affordable; "" when the stake is unknown.
func stakeGroup(stake, capWei, liquid *big.Int) string {
	switch {
	case stake == nil:
		return ""
	case liquid != nil && stake.Cmp(liquid) > 0:
		return "overBudget"
	case capWei != nil && stake.Cmp(capWei) > 0:
		return "overCap"
	default:
		return "affordable"
	}
}

// weiStr is w as a decimal string, or null when unknown.
func weiStr(w *big.Int) any {
	if w == nil {
		return nil
	}
	return w.String()
}

func weiMor(w *big.Int) any {
	if w == nil {
		return nil
	}
	f, _ := new(big.Rat).SetFrac(w, big.NewInt(1e18)).Float64()
	return f
}

// stakePriceFields are the API price fields: stakeWei and stMor for the
// session length (the unit shown everywhere), plus the hourly rate kept
// for API compatibility.
func stakePriceFields(p stakePrice) map[string]any {
	return map[string]any{
		"stakeWei":          weiStr(p.StakeWei),
		"stMor":             weiMor(p.StakeWei),
		"stMorPerHourWei":   weiStr(p.HourWei),
		"stMorPerHour":      weiMor(p.HourWei),
		"pricePerSecondWei": weiStr(p.PPS),
		"priceSource":       priceSource(p),
	}
}

func priceSource(p stakePrice) any {
	if p.PPS == nil {
		return nil
	}
	return "feed"
}

// stakeLimitStatus is /admin/status stakeLimit: the boot-effective cap
// per session (null when off, never "") and whether the saved value is
// invalid.
func (s *Server) stakeLimitStatus() map[string]any {
	limit, invalid := s.pool.StakeLimit()
	return map[string]any{"maxStMorPerSessionWei": weiStr(limit), "invalid": invalid}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
