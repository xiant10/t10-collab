// Command t10-collab runs the team collaboration service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/config"
	"t10-collab/internal/db"
	"t10-collab/internal/events"
	"t10-collab/internal/lists"
	"t10-collab/internal/offers"
	"t10-collab/internal/server"
	"t10-collab/internal/sweep"
	"t10-collab/internal/telemetry"
	"t10-collab/internal/workitems"
	"t10-collab/verifier"
)

func main() {
	if err := run(); err != nil {
		slog.Error("t10-collab stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			slog.Warn("flush telemetry", "error", err)
		}
	}()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	tokens, err := verifier.New(verifier.Config{
		Issuer: cfg.AuthIssuer, URL: cfg.AuthURL, Service: cfg.ClientID, ClientSecret: cfg.ClientSecret,
	})
	if err != nil {
		return err
	}
	org := people.New(people.Options{URL: cfg.AuthURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret})
	items := workitems.NewService(pool, org)
	api := server.New(server.Deps{
		Pool: pool, Verifier: tokens, Org: org, Items: items,
		Offers: offers.NewService(pool, org, items), Lists: lists.NewService(pool, org), Activity: activity.NewService(pool),
	})
	go sweep.New(pool, org).Run(ctx)
	go events.New(pool, cfg.AutomationsURL, org.ServiceToken).Run(ctx)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errs := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		errs <- srv.ListenAndServe()
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
