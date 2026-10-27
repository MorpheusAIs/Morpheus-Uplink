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
	for _, name := range cfg.MalformedBoolVars {
		// Name only: the value is never logged.
		slog.Warn(name + " is not true or false; using the safe value (MASTER_KEY_ADMIN: off). Set it to true or false.")
	}
	if cfg.DirectPaymentIgnored {
		slog.Warn("SESSION_DIRECT_PAYMENT is ignored (set to true): Uplink only opens staking-mode sessions (stake locked and returned, never paid out)")
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	cat := catalog.New(cfg.ActiveModelsURL).WithBidsURL(cfg.GatewayBidsURL)
	pl := pool.New(rc, cfg.SessionDurationSec)
	pl.SetProviderCooldown(time.Duration(cfg.ProviderCooldownSec) * time.Second)
	// Attribute actual session wall-time when a close lands. Poll for ClosedAt
	// (chain index lag is common); fall back to now−OpenedAt after a successful close.
	pl.SetOnSessionClosed(func(sessionID string) {
		detail, sec, err := rc.AwaitActualDuration(sessionID, 8, 2*time.Second)
		if err != nil {
			log.Printf("usage: closed session %s: %v", pool.ShortID(sessionID), err)
			return
		}
		// Usage is keyed by the registration id (#56); the name is for the log.
		st.RecordSessionClose(sessionID, sec, detail.ModelAgentID)
		model := cat.NameByID(detail.ModelAgentID)
		if model == "" {
			model = pool.ShortID(detail.ModelAgentID)
		}
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
	// sessions were listed once, adopting usable ones and closing expired
	// and duplicate ones; sessions for models not in the catalog are left
	// to expire (see internal/pool/reconcile.go).
	pl.SetOnReconcileState(srv.WriteReconcileStatus)
	pl.SetOnRescanEvent(srv.AuditSessionReconcileEvent)
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

	// Write the summaries of ended audit aggregation windows, and apply the
	// audit retention once a day (it also ran at boot).
	go func() {
		t := time.NewTicker(api.AuditFlushInterval)
		defer t.Stop()
		lastPrune := time.Now()
		for {
			select {
			case <-hkCtx.Done():
				return
			case now := <-t.C:
				srv.FlushAudit()
				if now.Sub(lastPrune) >= 24*time.Hour {
					srv.PruneAudit()
					lastPrune = now
				}
			}
		}
	}()

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
		// Finish inside the 45 s stop grace: 40 s from SIGTERM in total,
		// at most 8 s per close. Whatever is left is closed (expired) or
		// adopted by the next boot reconcile.
		r := pl.CloseAllWithin(t0.Add(shutdownBudget), shutdownPerClose)
		log.Printf("shutdown: closed %d/%d session(s), %d failed, %d left for boot reconcile, total %s",
			r.Closed, r.Attempted, r.Failed, r.Left, time.Since(t0).Round(time.Millisecond))
	}
}

const (
	shutdownBudget   = 40 * time.Second
	shutdownPerClose = 8 * time.Second
)
