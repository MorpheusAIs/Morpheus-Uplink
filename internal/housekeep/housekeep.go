// Package housekeep runs the consumer-wallet reclaim loop:
//
//  1. close expired / past-end sessions (via pool.Rehydrate → router close)
//  2. withdrawUserStakes for daylocked MOR that is past releaseAt (00:00 UTC)
//     via the proxy-router (router signs; Uplink holds no wallet key)
//
// Allowance top-up stays on the router (/blockchain/approve) — operators can
// approve once; personal gateways rarely race allowance like APIGW.
package housekeep

import (
	"context"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// DefaultGasLowWei is ~0.0005 ETH — below this, reclaim may fail on Base.
var DefaultGasLowWei = new(big.Int).Mul(big.NewInt(5e14), big.NewInt(1)) // 5e14 = 0.0005 ETH

type Config struct {
	Enabled bool
	// Router performs on-hold reads and withdraw (Uplink holds no wallet key).
	Router *router.Client
	// Minute-of-day UTC after which the daily run may fire (default 00:05).
	AfterUTCMinute     int
	WithdrawThreshold  *big.Int // wei; skip dust
	WithdrawIterations uint8
	MaxWithdrawRounds  int
	CheckEvery         time.Duration
	// GasLowWei: when router ETH balance is below this, set GasLow on Result.
	GasLowWei *big.Int
	// Startup retry: the first scan at boot often races the router coming
	// up (connection refused). Retry with exponential backoff from
	// StartupBackoff (default 2s) up to StartupBackoffMax (default 30s),
	// for at most StartupRetryFor in total (default 5m).
	StartupBackoff    time.Duration
	StartupBackoffMax time.Duration
	StartupRetryFor   time.Duration
}

type Result struct {
	At              time.Time `json:"at"`
	ClosedViaScan   bool      `json:"closedViaScan"`
	ClaimableBefore string    `json:"claimableBeforeWei,omitempty"`
	LockedBefore    string    `json:"lockedBeforeWei,omitempty"`
	MovedWei        string    `json:"movedWei,omitempty"`
	TxHashes        []string  `json:"txHashes,omitempty"`
	Rounds          int       `json:"rounds"`
	Status          string    `json:"status"` // ok | actioned | skipped | error
	Message         string    `json:"message"`
	GasLow          bool      `json:"gasLow,omitempty"`
	EthBalanceWei   string    `json:"ethBalanceWei,omitempty"`
}

type Runner struct {
	cfg  Config
	pool *pool.Pool

	mu      sync.Mutex
	last    Result
	lastDay string // YYYY-MM-DD UTC of last successful daily attempt
}

func New(cfg Config, pl *pool.Pool) (*Runner, error) {
	r := &Runner{cfg: cfg, pool: pl}
	if cfg.Enabled && cfg.Router != nil {
		log.Printf("housekeep: reclaim via router (after 00:%02d UTC, max %d rounds)",
			cfg.AfterUTCMinute%60, maxRounds(cfg.MaxWithdrawRounds))
	} else if cfg.Enabled {
		log.Printf("housekeep: close-expired only (reclaim not wired)")
	}
	return r, nil
}

func maxRounds(n int) int {
	if n <= 0 {
		return 8
	}
	return n
}

// ReclaimReady is true when withdrawUserStakes can be requested via the router.
func (r *Runner) ReclaimReady() bool {
	return r != nil && r.cfg.Enabled && r.cfg.Router != nil
}

func (r *Runner) Last() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Start runs the periodic loop until ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	if !r.cfg.Enabled {
		return
	}
	every := r.cfg.CheckEvery
	if every <= 0 {
		every = 15 * time.Minute
	}
	r.startupRun(ctx)

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.maybeRun(ctx, false)
		}
	}
}

// startupRun performs the boot scan, retrying with bounded backoff while the
// router is not ready yet. Each attempt replaces Last(), so a stale error
// clears as soon as a later attempt succeeds.
func (r *Runner) startupRun(ctx context.Context) {
	delay := r.cfg.StartupBackoff
	if delay <= 0 {
		delay = 2 * time.Second
	}
	maxDelay := r.cfg.StartupBackoffMax
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	budget := r.cfg.StartupRetryFor
	if budget <= 0 {
		budget = 5 * time.Minute
	}
	deadline := time.Now().Add(budget)
	for attempt := 1; ; attempt++ {
		res := r.maybeRun(ctx, true)
		if !startupNeedsRetry(res) {
			if attempt > 1 {
				log.Printf("housekeep: startup scan ok after %d attempt(s)", attempt)
			}
			return
		}
		if time.Now().Add(delay).After(deadline) {
			warnf("housekeep: startup scan still failing after %d attempt(s); next try on the regular schedule", attempt)
			return
		}
		warnf("housekeep: startup scan attempt %d failed (%s); retrying in %s", attempt, res.Message, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// startupNeedsRetry: the boot scan failed to reach the router (on-hold read
// error or the close scan could not list sessions).
func startupNeedsRetry(res Result) bool {
	return res.Status == "error" || !res.ClosedViaScan
}

func (r *Runner) maybeRun(ctx context.Context, startup bool) Result {
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	mins := now.Hour()*60 + now.Minute()
	after := r.cfg.AfterUTCMinute
	if after <= 0 {
		after = 5
	}

	r.mu.Lock()
	already := r.lastDay == day
	r.mu.Unlock()

	if !startup && (mins < after || already) {
		return Result{Status: "skipped", ClosedViaScan: true}
	}
	res := r.Run(ctx)
	r.mu.Lock()
	r.last = res
	if res.Status != "error" {
		r.lastDay = day
	}
	r.mu.Unlock()
	return res
}

// RunOnce forces a reclaim pass (admin button / API).
func (r *Runner) RunOnce(ctx context.Context) Result {
	res := r.Run(ctx)
	r.mu.Lock()
	r.last = res
	if res.Status != "error" {
		r.lastDay = time.Now().UTC().Format("2006-01-02")
	}
	r.mu.Unlock()
	return res
}

func (r *Runner) Run(ctx context.Context) Result {
	res := Result{At: time.Now().UTC(), Status: "ok"}

	if err := r.pool.RehydrateForce(); err != nil {
		warnf("housekeep: rehydrate/close scan: %v", err)
		res.Message = "close scan: " + err.Error()
	} else {
		res.ClosedViaScan = true
	}

	if r.cfg.Router == nil {
		res.Status = "skipped"
		if res.Message == "" {
			res.Message = "closed past-end sessions; daylocked reclaim not available in this deploy"
		}
		log.Printf("housekeep: %s", res.Message)
		return res
	}

	// Gas low alert (read-only balance via router).
	gasThresh := r.cfg.GasLowWei
	if gasThresh == nil {
		gasThresh = DefaultGasLowWei
	}
	if eth, err := r.cfg.Router.EthBalanceWei(); err == nil {
		res.EthBalanceWei = eth.String()
		if eth.Cmp(gasThresh) < 0 {
			res.GasLow = true
			warnf("housekeep: alert gas low (eth wei=%s threshold=%s)", eth.String(), gasThresh.String())
		}
	}

	hold, err := r.cfg.Router.UserStakesOnHold(1)
	if err != nil {
		res.Status = "error"
		res.Message = "stakes/onhold: " + err.Error()
		log.Printf("housekeep: %s", res.Message)
		return res
	}
	res.ClaimableBefore = hold.Claimable.String()
	res.LockedBefore = hold.Locked.String()

	thresh := r.cfg.WithdrawThreshold
	if thresh == nil {
		thresh = big.NewInt(0)
	}
	if hold.Claimable.Cmp(thresh) <= 0 {
		res.Status = "skipped"
		if hold.Locked.Sign() > 0 {
			res.Message = "nothing to reclaim yet — " + formatMOR(hold.Locked) + " MOR still on hold until 00:00 UTC"
		} else {
			res.Message = "nothing to reclaim (" + formatMOR(hold.Claimable) + " MOR claimable)"
		}
		log.Printf("housekeep: %s", res.Message)
		return res
	}

	iters := r.cfg.WithdrawIterations
	if iters == 0 {
		iters = 255
	}
	maxR := maxRounds(r.cfg.MaxWithdrawRounds)

	startClaimable := new(big.Int).Set(hold.Claimable)
	var hashes []string
	for round := 1; round <= maxR; round++ {
		select {
		case <-ctx.Done():
			res.Status = "error"
			res.Message = ctx.Err().Error()
			return res
		default:
		}
		txHash, err := r.cfg.Router.WithdrawUserStakes(iters)
		if err != nil {
			if isNothingToWithdraw(err) {
				res.Message = "withdraw stopped: nothing releasable left"
				break
			}
			if len(hashes) > 0 && strings.Contains(strings.ToLower(err.Error()), "execution reverted") {
				res.Message = "withdraw stopped after revert (likely drained): " + err.Error()
				break
			}
			res.Status = "error"
			res.Message = err.Error()
			res.TxHashes = hashes
			moved := new(big.Int).Sub(startClaimable, hold.Claimable)
			if moved.Sign() < 0 {
				moved = big.NewInt(0)
			}
			res.MovedWei = moved.String()
			res.Rounds = len(hashes)
			warnf("housekeep: withdraw error: %v", err)
			return res
		}
		hashes = append(hashes, txHash)
		log.Printf("housekeep: withdraw round %d/%d tx=%s", round, maxR, txHash)

		hold, err = r.cfg.Router.UserStakesOnHold(1)
		if err != nil {
			warnf("housekeep: post-withdraw on-hold read: %v", err)
			break
		}
		if hold.Claimable.Cmp(thresh) <= 0 {
			break
		}
	}

	moved := new(big.Int).Sub(startClaimable, hold.Claimable)
	if moved.Sign() < 0 {
		moved = big.NewInt(0)
	}
	res.TxHashes = hashes
	res.Rounds = len(hashes)
	res.MovedWei = moved.String()
	if len(hashes) == 0 {
		res.Status = "skipped"
		if res.Message == "" {
			res.Message = "no withdraw txs sent"
		}
	} else {
		res.Status = "actioned"
		res.Message = "reclaimed " + formatMOR(moved) + " MOR in " + itoa(len(hashes)) + " tx(s)"
	}
	log.Printf("housekeep: %s", res.Message)
	return res
}

func isNothingToWithdraw(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "sessionuseramounttowithdrawiszero") ||
		strings.Contains(s, "amounttowithdrawiszero") ||
		strings.Contains(s, "nothing to withdraw")
}

func formatMOR(n *big.Int) string {
	if n == nil {
		return "0"
	}
	rat := new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	return rat.FloatString(4)
}

func itoa(n int) string {
	return big.NewInt(int64(n)).String()
}
