// UPLINK — Personal API Gateway for a Morpheus consumer proxy-router.
//
// Wraps a C-Node with OpenAI-compatible API keys, automatic session
// management, usage metering, and a small management GUI.
package main

import (
	"context"
	"errors"
	"log"
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
	api.Version = version
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	cat := catalog.New(cfg.ActiveModelsURL).WithBidsURL(cfg.GatewayBidsURL)
	pl := pool.New(rc, cfg.SessionDurationSec, cfg.SessionFailover, cfg.DirectPayment)
	// Attribute actual session wall-time when a close lands. Poll for ClosedAt
	// (chain index lag is common); fall back to now−OpenedAt after a successful close.
	pl.SetOnSessionClosed(func(sessionID string) {
		detail, sec, err := rc.AwaitActualDuration(sessionID, 8, 2*time.Second)
		if err != nil {
			log.Printf("usage: closed session %s: %v", sessionID, err)
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
		log.Printf("usage: session %s actual %ds for model %s (%s)", sessionID, sec, model, src)
	})

	hk, err := housekeep.New(housekeep.Config{
		Enabled:            cfg.Housekeeping,
		EthNodeAddress:     cfg.EthNodeAddress,
		DiamondAddress:     cfg.DiamondAddress,
		WalletPrivateKey:   cfg.WalletPrivateKey,
		ChainID:            cfg.EthNodeChainID,
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

	hkCtx, hkCancel := context.WithCancel(context.Background())
	defer hkCancel()
	go hk.Start(hkCtx)

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
	hkCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	if cfg.CloseSessionsOnExit {
		pl.CloseAll()
	}
}
