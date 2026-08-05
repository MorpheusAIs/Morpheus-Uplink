// UPLINK — Personal API Gateway for a Morpheus consumer proxy-router.
//
// Wraps a C-Node with OpenAI-compatible API keys, automatic session
// management, usage metering, and a small management GUI.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/api"
	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
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
	cat := catalog.New(cfg.ActiveModelsURL)
	pl := pool.New(rc, cfg.SessionDurationSec, cfg.SessionFailover, cfg.DirectPayment)
	srv := api.NewServer(cfg, st, cat, pl, rc)

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	if cfg.CloseSessionsOnExit {
		pl.CloseAll()
	}
}
