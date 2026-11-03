package pool

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Max stMOR/hr limit: a bid whose price, as the MOR a session locks per
// hour (router.StMorPerHourWei), is above the limit is never opened.

// ErrStakeLimitExceeded: no session was opened and at least one candidate
// bid was skipped because its stMOR/hr is above the limit.
var ErrStakeLimitExceeded = errors.New("no provider for this model could be opened within the Max stMOR/hr limit")

// ErrStakeLimitInvalid: the saved Max stMOR/hr value is invalid; every
// open is refused until it is fixed and Uplink restarts.
var ErrStakeLimitInvalid = errors.New("the Max stMOR/hr setting is invalid")

// stakeRefusalShowFor is how long the Status page shows the last
// stake-limit refusal (like the chain-wait one).
const stakeRefusalShowFor = chainWaitShowFor

// Held supply/budget boot read backoff (display only).
const (
	stakeInputsBootFirst = 60 * time.Second
	stakeInputsBootMax   = 5 * time.Minute
	stakeInputsReadTime  = 10 * time.Second
)

type stakeLimit struct {
	wei     *big.Int // nil: off
	invalid bool
}

// StakeInputs is a supply/budget pair (wei) and when it was read (pool
// clock).
type StakeInputs struct {
	Supply, Budget *big.Int
	At             time.Time
}

// SetStakeLimit installs the boot value of the Max stMOR/hr setting: limit
// in wei per hour (nil: off), or invalid (every open is refused).
func (p *Pool) SetStakeLimit(limit *big.Int, invalid bool) {
	var w *big.Int
	if limit != nil {
		w = new(big.Int).Set(limit)
	}
	p.stakeLim.Store(&stakeLimit{wei: w, invalid: invalid})
}

// StakeLimit returns the installed limit (a copy; nil: off) and whether
// the setting is invalid.
func (p *Pool) StakeLimit() (*big.Int, bool) {
	l := p.stakeLim.Load()
	if l == nil {
		return nil, false
	}
	if l.wei == nil {
		return nil, l.invalid
	}
	return new(big.Int).Set(l.wei), l.invalid
}

// withinLimit reports whether bid b may be opened under limit (wei per
// hour, nil: off). While on, a bid with no price, or whose price cannot be
// computed, is skipped; the compare is whole wei, strictly greater skips.
func withinLimit(b router.RatedBid, in StakeInputs, limit *big.Int) bool {
	if limit == nil {
		return true
	}
	w, err := router.StMorPerHourWei(in.Supply, in.Budget, b.PricePerSecond)
	if err != nil {
		return false
	}
	return w.Cmp(limit) <= 0
}

// HeldStakeInputs is the last supply/budget pair read by an open, a user
// action or the boot read; ok false before the first one. Display only:
// an open never uses it.
func (p *Pool) HeldStakeInputs() (StakeInputs, bool) {
	h := p.heldInputs.Load()
	if h == nil {
		return StakeInputs{}, false
	}
	return *h, true
}

// SetHeldStakeInputs records a fresh supply/budget pair (both > 0).
func (p *Pool) SetHeldStakeInputs(supply, budget *big.Int) {
	if supply == nil || budget == nil || supply.Sign() <= 0 || budget.Sign() <= 0 {
		return
	}
	p.heldInputs.Store(&StakeInputs{Supply: new(big.Int).Set(supply), Budget: new(big.Int).Set(budget), At: p.now()})
}

// ReadStakeInputs reads supply and budget from the router (ctx bounds both
// reads) and holds the pair when both are above zero.
func (p *Pool) ReadStakeInputs(ctx context.Context) (StakeInputs, error) {
	s, err := p.client.TokenSupplyCtx(ctx)
	if err != nil {
		return StakeInputs{}, err
	}
	b, err := p.client.TodaysBudgetCtx(ctx)
	if err != nil {
		return StakeInputs{}, err
	}
	if s == nil || b == nil || s.Sign() <= 0 || b.Sign() <= 0 {
		return StakeInputs{}, errors.New("supply or budget not above zero")
	}
	p.SetHeldStakeInputs(s, b)
	return StakeInputs{Supply: s, Budget: b, At: p.now()}, nil
}

// RunStakeInputsBoot fills the held pair at boot: it reads at once, then
// retries with backoff from 60 s up to 5 min (with jitter) until the pair
// is held (by this read or any other) or ctx ends.
func (p *Pool) RunStakeInputsBoot(ctx context.Context) {
	wait := stakeInputsBootFirst
	for {
		if _, ok := p.HeldStakeInputs(); ok || ctx.Err() != nil {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, stakeInputsReadTime)
		_, err := p.ReadStakeInputs(rctx)
		cancel()
		if err == nil || ctx.Err() != nil {
			return
		}
		slog.Warn("pool: supply/budget read failed; retrying", "in", wait.Round(time.Second), "err", router.SafeError(err))
		if p.sleep(ctx, wait+time.Duration(rand.Int64N(int64(wait/5)+1))) != nil {
			return
		}
		wait = min(wait*2, stakeInputsBootMax)
	}
}

// StakeRefusal is the last stake_limit_exceeded refusal (Status page).
type StakeRefusal struct {
	At time.Time `json:"at"`
}

func (p *Pool) setStakeRefusal() {
	p.mu.Lock()
	p.stakeRefusal = StakeRefusal{At: p.now()}
	p.mu.Unlock()
}

// clearStakeRefusal forgets it (an open succeeded).
func (p *Pool) clearStakeRefusal() {
	p.mu.Lock()
	p.stakeRefusal = StakeRefusal{}
	p.mu.Unlock()
}

// LastStakeRefusal is the last stake_limit_exceeded refusal while it is
// under 5 min old and no open has succeeded since; ok false otherwise.
func (p *Pool) LastStakeRefusal() (StakeRefusal, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.stakeRefusal
	if r.At.IsZero() || p.now().Sub(r.At) >= stakeRefusalShowFor {
		return StakeRefusal{}, false
	}
	return r, true
}

// openSucceeded clears the refusal slots shown on Status.
func (p *Pool) openSucceeded() {
	p.clearChainWait()
	p.clearStakeRefusal()
}

// readStakeInputsInSlot reads supply and budget fresh inside the wallet
// slot for an open while the limit is on. Any failure or a value <= 0
// refuses the open; the held pair is never used for it.
func (op *openOp) readStakeInputsInSlot(ctx context.Context) (StakeInputs, error) {
	p := op.p
	rctx, cancel := p.slot.callCtx(ctx, op.tok, readCallTimeout)
	defer cancel()
	in, err := p.ReadStakeInputs(rctx)
	if err != nil {
		if ctx.Err() != nil {
			return StakeInputs{}, ctx.Err()
		}
		slog.Warn("pool: not opening", "model", op.model, "reason", "stake_inputs_unreadable", "err", router.SafeError(err))
		return StakeInputs{}, &NoAvailableError{Reason: "stake_inputs_unreadable"}
	}
	return in, nil
}

// FirstTry is the bid an open would try first, for display.
type FirstTry struct {
	Bid router.RatedBid
	// StMorPerHourWei: the bid's price per hour from the held pair; nil
	// when no pair is held yet or the bid has no price.
	StMorPerHourWei *big.Int
	// OverLimit: no candidate is within the limit; Bid is the router's
	// first candidate.
	OverLimit bool
}

// firstTryReadTimeout bounds FirstTry's rated-bids read.
const firstTryReadTimeout = 10 * time.Second

// FirstTry reads the model's rated bids outside the wallet slot (own
// deadline of at most 10 s), applies the open's filters quietly, then the
// limit with the held supply/budget pair: the first passing bid, else the
// router's first candidate marked over limit. It never opens anything.
func (p *Pool) FirstTry(ctx context.Context, modelID string) (FirstTry, error) {
	rctx, cancel := context.WithTimeout(ctx, firstTryReadTimeout)
	rows, rerr := p.client.RatedBidsCtx(rctx, modelID)
	cancel()
	cands, _, err := p.candidatesFromQuiet(modelID, "", rows, rerr)
	if err != nil {
		return FirstTry{}, err
	}
	in, held := p.HeldStakeInputs()
	price := func(b router.RatedBid) *big.Int {
		if !held {
			return nil
		}
		w, err := router.StMorPerHourWei(in.Supply, in.Budget, b.PricePerSecond)
		if err != nil {
			return nil
		}
		return w
	}
	limit, _ := p.StakeLimit()
	if limit == nil || !held {
		return FirstTry{Bid: cands[0], StMorPerHourWei: price(cands[0])}, nil
	}
	for _, b := range cands {
		if withinLimit(b, in, limit) {
			return FirstTry{Bid: b, StMorPerHourWei: price(b)}, nil
		}
	}
	return FirstTry{Bid: cands[0], StMorPerHourWei: price(cands[0]), OverLimit: true}, nil
}
