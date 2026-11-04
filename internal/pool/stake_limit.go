package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Max stMOR per session: a bid whose stake for the session length the open
// asks for (router.StakeForDurationWei) is above the cap is never opened.

// ErrStakeLimitExceeded: no session was opened and at least one candidate
// bid was skipped because its stake is above the cap. The error returned is
// a *StakeLimitError (errors.Is matches this).
var ErrStakeLimitExceeded = errors.New("no provider for this model could be opened within the Max stMOR per session cap")

// ErrStakeLimitInvalid: the saved Max stMOR per session value is invalid;
// every open is refused until it is fixed and Uplink restarts.
var ErrStakeLimitInvalid = errors.New("the Max stMOR per session setting is invalid")

// StakeLimitError is a stake-cap refusal: the least stake any skipped bid
// needed for the session length asked, the cap and that length.
type StakeLimitError struct {
	NeedWei     *big.Int
	CapWei      *big.Int
	DurationSec int
}

func (e *StakeLimitError) Error() string {
	return fmt.Sprintf("%v: a %s session needs at least %s stMOR, the cap is %s stMOR",
		ErrStakeLimitExceeded, FormatDuration(e.DurationSec), FormatMor(e.NeedWei), FormatMor(e.CapWei))
}

func (e *StakeLimitError) Is(target error) bool { return target == ErrStakeLimitExceeded }

// FormatMor is wei as MOR with 2 decimals, rounded up (an amount needed
// is never shown below what it is), or 6 when under 0.01.
func FormatMor(wei *big.Int) string {
	if wei == nil {
		return "?"
	}
	r := new(big.Rat).SetFrac(wei, big.NewInt(1e18))
	if r.Cmp(big.NewRat(1, 100)) < 0 {
		return ceilRat(r, 6)
	}
	return ceilRat(r, 2)
}

func ceilRat(r *big.Rat, dp int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dp)), nil)
	n := new(big.Int).Mul(r.Num(), scale)
	q, m := new(big.Int).QuoRem(n, r.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return new(big.Rat).SetFrac(q, scale).FloatString(dp)
}

// FormatDuration is a session length as "2 h", "90 min" or "1 h 30 min".
func FormatDuration(sec int) string {
	m := (sec + 59) / 60
	switch {
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	default:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
}

// stakeRefusalShowFor is how long the Status page shows the last
// stake-limit refusal (like the chain-wait one).
const stakeRefusalShowFor = chainWaitShowFor

// Held supply/budget pair: boot read backoff, and how long an open or a
// price may use it before it is read again (also on a new UTC day, when
// the budget changes).
const (
	stakeInputsBootFirst = 60 * time.Second
	stakeInputsBootMax   = 5 * time.Minute
	stakeInputsReadTime  = 10 * time.Second
	stakeInputsMaxAge    = time.Hour
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

// SetStakeLimit installs the boot value of the Max stMOR per session
// setting: the cap in wei per session (nil: off), or invalid (every open is
// refused).
func (p *Pool) SetStakeLimit(limit *big.Int, invalid bool) {
	var w *big.Int
	if limit != nil {
		w = new(big.Int).Set(limit)
	}
	p.stakeLim.Store(&stakeLimit{wei: w, invalid: invalid})
}

// StakeLimit returns the installed cap (a copy; nil: off) and whether the
// setting is invalid.
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

// bidStake is bid b's stake for durationSec at in; nil when it has no
// price or cannot be computed.
func bidStake(b router.RatedBid, in StakeInputs, durationSec int) *big.Int {
	w, err := router.StakeForDurationWei(in.Supply, in.Budget, b.PricePerSecond, int64(durationSec))
	if err != nil {
		return nil
	}
	return w
}

// HeldStakeInputs is the last supply/budget pair read; ok false before the
// first one.
func (p *Pool) HeldStakeInputs() (StakeInputs, bool) {
	h := p.heldInputs.Load()
	if h == nil {
		return StakeInputs{}, false
	}
	return *h, true
}

// heldFresh reports whether in may still be used: under an hour old and
// read on the current UTC day.
func (p *Pool) heldFresh(in StakeInputs) bool {
	now := p.now()
	return now.Sub(in.At) < stakeInputsMaxAge && now.UTC().Format(time.DateOnly) == in.At.UTC().Format(time.DateOnly)
}

// FreshStakeInputs is the held pair while it is fresh (heldFresh).
func (p *Pool) FreshStakeInputs() (StakeInputs, bool) {
	in, ok := p.HeldStakeInputs()
	if !ok || !p.heldFresh(in) {
		return StakeInputs{}, false
	}
	return in, true
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

// EnsureStakeInputs is the held pair while fresh, else a new read (two
// router calls, no per-bid read).
func (p *Pool) EnsureStakeInputs(ctx context.Context) (StakeInputs, error) {
	if in, ok := p.FreshStakeInputs(); ok {
		return in, nil
	}
	return p.ReadStakeInputs(ctx)
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

// StakeRefusal is the last stake_limit_exceeded refusal (Status page): the
// least stake needed, the cap and the session length, in wei and seconds.
type StakeRefusal struct {
	At          time.Time `json:"at"`
	NeedWei     string    `json:"needWei,omitempty"`
	CapWei      string    `json:"capWei,omitempty"`
	DurationSec int       `json:"durationSec,omitempty"`
}

func (p *Pool) setStakeRefusal(e *StakeLimitError) {
	r := StakeRefusal{At: p.now(), DurationSec: e.DurationSec}
	if e.NeedWei != nil {
		r.NeedWei = e.NeedWei.String()
	}
	if e.CapWei != nil {
		r.CapWei = e.CapWei.String()
	}
	p.mu.Lock()
	p.stakeRefusal = r
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

// stakeInputsForOpen is the supply/budget pair an open checks the cap
// with while it is on: the held pair while fresh, else a read inside the
// wallet slot (then held). A failed read or a value <= 0 refuses the open.
func (op *openOp) stakeInputsForOpen(ctx context.Context) (StakeInputs, error) {
	p := op.p
	if in, ok := p.FreshStakeInputs(); ok {
		return in, nil
	}
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
