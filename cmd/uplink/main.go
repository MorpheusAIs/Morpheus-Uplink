// UPLINK — Personal API Gateway for a Morpheus consumer proxy-router.
//
// Wraps a C-Node with OpenAI-compatible API keys, automatic session
// management, usage metering, and a small management GUI.
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/api"
	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/housekeep"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// version is injected at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	api.Version = version
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	// Effective router MOR allowance (wei + source only; never a secret).
	if cfg.DirectPaymentIgnored {
		slog.Warn("SESSION_DIRECT_PAYMENT is ignored (set to true): Uplink only opens staking-mode sessions (stake locked and returned, never paid out)")
	}
	slog.Info("mor allowance",
		"wei", cfg.MORAllowanceWei,
		"source", cfg.MORAllowanceSource,
		"file", cfg.MORAllowanceFile)
	if cfg.MORAllowanceWarning != "" {
		slog.Warn("mor allowance warning", "source", cfg.MORAllowanceSource, "message", cfg.MORAllowanceWarning)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass).WithAuthFile(cfg.RouterAuthFile)
	if cfg.RouterWalletAddress != "" {
		slog.Info("router wallet recorded at init", "file", cfg.RouterWalletFile)
	}
	cat := catalog.New(cfg.ActiveModelsURL).WithBidsURL(cfg.GatewayBidsURL)
	pl := pool.New(rc, cfg.SessionDurationSec, cfg.SessionFailover)
	// Attribute actual session wall-time when a close lands. Poll for ClosedAt
	// (chain index lag is common); fall back to now−OpenedAt after a successful close.
	pl.SetOnSessionClosed(func(sessionID string) {
		detail, sec, err := rc.AwaitActualDuration(sessionID, 8, 2*time.Second)
		if err != nil {
			log.Printf("usage: closed session %s: %v", pool.ShortID(sessionID), err)
			return
		}
		model := cat.NameByID(detail.ModelAgentID)
		if model == "" {
			model = detail.ModelAgentID
		}
		st.RecordSessionClose(sessionID, sec, model)
		src := "receipt"
		if detail.ClosedAt <= 0 {
			src = "open+now"
		}
		log.Printf("usage: session %s actual %ds for model %s (%s)", pool.ShortID(sessionID), sec, model, src)
	})

	hk, err := housekeep.New(housekeep.Config{
		Enabled:            cfg.Housekeeping,
		Router:             rc,
		AfterUTCMinute:     5,
		WithdrawThreshold:  big.NewInt(0), // personal gateway: reclaim any dust
		WithdrawIterations: 255,
		MaxWithdrawRounds:  8,
		CheckEvery:         15 * time.Minute,
	}, pl)
	if err != nil {
		log.Fatalf("housekeep: %v", err)
	}

	srv := api.NewServer(cfg, st, cat, pl, rc, hk)

	// Boot session reconcile: gate session opens until the wallet's open
	// sessions were listed once, adopting usable ones and closing expired,
	// duplicate and unconfigured ones (see internal/pool/reconcile.go).
	pl.SetOnReconcileState(srv.WriteReconcileStatus)
	pl.Gate()
	reconcileCtx, reconcileCancel := context.WithCancel(context.Background())
	defer reconcileCancel()
	go pl.StartupReconcile(reconcileCtx, pool.ConfiguredFrom(func() ([]string, error) {
		models, err := cat.List()
		ids := make([]string, 0, len(models))
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		return ids, err
	}), pool.ReconcileConfig{
		GateFor:     time.Duration(cfg.ReconcileGateSec) * time.Second,
		RetryEvery:  time.Duration(cfg.ReconcileRetrySec) * time.Second,
		OnReconcile: srv.AuditSessionReconcile,
		OnEvent:     srv.AuditSessionReconcileEvent,
		OnGiveUp:    srv.AuditSessionReconcileDeferred,
	})

	hkCtx, hkCancel := context.WithCancel(context.Background())
	defer hkCancel()
	go hk.Start(hkCtx)

	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		srv.CheckOutflow()
		for {
			select {
			case <-hkCtx.Done():
				return
			case <-t.C:
				srv.CheckOutflow()
			}
		}
	}()

	go routerAuthWatchdog(hkCtx, rc, time.Duration(cfg.Router401ExitSec)*time.Second)

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("uplink %s listening on %s (router: %s)", version, cfg.Listen, cfg.RouterURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down…")
	t0 := time.Now()
	hkCancel()
	reconcileCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	log.Printf("shutdown: in-flight requests finished in %s", time.Since(t0).Round(time.Millisecond))
	if cfg.CloseSessionsOnExit {
		r := pl.CloseAll()
		log.Printf("shutdown: closed %d/%d session(s), %d failed, total %s",
			r.Closed, r.Attempted, r.Failed, time.Since(t0).Round(time.Millisecond))
	}
}

// routerAuthWatchdog exits the process when the router keeps rejecting the
// scoped credential (HTTP 401) for longer than limit while the router itself
// is up. The container restart policy then re-runs the entrypoint, which
// copies the credential router-user-init wrote most recently. limit <= 0
// disables the watchdog.
func routerAuthWatchdog(ctx context.Context, rc *router.Client, limit time.Duration) {
	if limit <= 0 {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		since, _, failing := rc.AuthFailure()
		if !failing || time.Since(since) < limit {
			continue
		}
		// Confirm with a fresh authenticated call (it also re-reads the
		// auth file) before giving up.
		if _, err := rc.WalletAddress(); router.ErrorKind(err) != router.KindAuth {
			continue
		}
		slog.Error("router rejected the scoped credential; exiting so the entrypoint re-reads the current one",
			"failing_for", time.Since(since).Round(time.Second).String())
		os.Exit(3)
	}
}
