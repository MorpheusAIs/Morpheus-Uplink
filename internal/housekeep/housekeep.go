// Package housekeep runs the consumer-wallet reclaim loop that the hosted
// C-Node performs in Lambda (consumer_wallet_housekeeping):
//
//  1. close expired / past-end sessions (via pool.Rehydrate → router close)
//  2. withdrawUserStakes for daylocked MOR that is past releaseAt (00:00 UTC)
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

	"github.com/absgrafx/morpheus-uplink/internal/chain"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
)

type Config struct {
	Enabled            bool
	EthNodeAddress     string
	DiamondAddress     string
	WalletPrivateKey   string
	ChainID            int64
	// Minute-of-day UTC after which the daily run may fire (default 00:05).
	AfterUTCMinute     int
	WithdrawThreshold  *big.Int // wei; skip dust
	WithdrawIterations uint8
	MaxWithdrawRounds  int
	CheckEvery         time.Duration
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
}

type Runner struct {
	cfg  Config
	pool *pool.Pool

	mu       sync.Mutex
	last     Result
	lastDay  string // YYYY-MM-DD UTC of last successful daily attempt
	signer   *chain.Signer
}

func New(cfg Config, pl *pool.Pool) (*Runner, error) {
	r := &Runner{cfg: cfg, pool: pl}
	if cfg.Enabled && cfg.WalletPrivateKey != "" && cfg.EthNodeAddress != "" {
		s, err := chain.NewSigner(cfg.EthNodeAddress, cfg.WalletPrivateKey, cfg.ChainID)
		if err != nil {
			return nil, err
		}
		r.signer = s
		log.Printf("housekeep: reclaim enabled (wallet %s, after 00:%02d UTC)",
			s.From.Hex(), cfg.AfterUTCMinute%60)
	} else if cfg.Enabled {
		log.Printf("housekeep: close-expired only (reclaim not wired)")
	}
	return r, nil
}

// ReclaimReady is true when withdrawUserStakes can be signed from this process.
func (r *Runner) ReclaimReady() bool {
	return r != nil && r.signer != nil && r.cfg.EthNodeAddress != ""
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
	// Startup pass: reclaim if anything is already claimable (covers redeploys after midnight).
	r.maybeRun(ctx, true)

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

func (r *Runner) maybeRun(ctx context.Context, startup bool) {
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	mins := now.Hour()*60 + now.Minute()
	after := r.cfg.AfterUTCMinute
	if after <= 0 {
		after = 5 // 00:05 UTC — same window as the Lambda cron
	}

	r.mu.Lock()
	already := r.lastDay == day
	r.mu.Unlock()

	if !startup && (mins < after || already) {
		return
	}
	res := r.Run(ctx)
	r.mu.Lock()
	r.last = res
	if res.Status != "error" {
		r.lastDay = day
	}
	r.mu.Unlock()
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

	// 1) Close past-end sessions via router (same intent as Lambda close_expired).
	if err := r.pool.RehydrateForce(); err != nil {
		log.Printf("housekeep: rehydrate/close scan: %v", err)
		res.Message = "close scan: " + err.Error()
		// Continue to withdraw — closes may still succeed next pass.
	} else {
		res.ClosedViaScan = true
	}

	if r.signer == nil || r.cfg.EthNodeAddress == "" {
		res.Status = "skipped"
		if res.Message == "" {
			res.Message = "closed past-end sessions; daylocked reclaim not available in this deploy"
		}
		log.Printf("housekeep: %s", res.Message)
		return res
	}

	addr := r.signer.From.Hex()
	hold, err := chain.UserStakesOnHold(r.cfg.EthNodeAddress, r.cfg.DiamondAddress, addr, 1)
	if err != nil {
		res.Status = "error"
		res.Message = "getUserStakesOnHold: " + err.Error()
		log.Printf("housekeep: %s", res.Message)
		return res
	}
	res.ClaimableBefore = hold.Claimable.String()
	res.LockedBefore = hold.Locked.String()

	thresh := r.cfg.WithdrawThreshold
	if thresh == nil {
		thresh = big.NewInt(0)
	}
	if hold.Claimable.Cmp(thresh) < 0 {
		res.Status = "skipped"
		res.Message = "claimable below threshold (" + formatMOR(hold.Claimable) + " MOR)"
		log.Printf("housekeep: %s", res.Message)
		return res
	}

	iters := r.cfg.WithdrawIterations
	if iters == 0 {
		iters = 255
	}
	maxRounds := r.cfg.MaxWithdrawRounds
	if maxRounds <= 0 {
		maxRounds = 8
	}

	startClaimable := new(big.Int).Set(hold.Claimable)
	var hashes []string
	for round := 1; round <= maxRounds; round++ {
		select {
		case <-ctx.Done():
			res.Status = "error"
			res.Message = ctx.Err().Error()
			return res
		default:
		}
		txHash, err := r.signer.WithdrawUserStakes(ctx, r.cfg.DiamondAddress, addr, iters)
		if err != nil {
			if chain.IsNothingToWithdraw(err) {
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
			log.Printf("housekeep: withdraw error: %v", err)
			return res
		}
		hashes = append(hashes, txHash)
		log.Printf("housekeep: withdraw round %d/%d tx=%s", round, maxRounds, txHash)

		hold, err = chain.UserStakesOnHold(r.cfg.EthNodeAddress, r.cfg.DiamondAddress, addr, 1)
		if err != nil {
			log.Printf("housekeep: post-withdraw on-hold read: %v", err)
			break
		}
		if hold.Claimable.Cmp(thresh) < 0 {
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
