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

	"recall-miner/internal/services/storage/api"
	"recall-miner/internal/services/storage/config"
	"recall-miner/internal/services/storage/registry"
)

func main() {
	cfg := config.FromEnv()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel()}))

	if err := cfg.Validate(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	handler, claim, err := api.NewServerWithClaim(cfg, logger)
	if err != nil {
		logger.Error("failed to build db service", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := claim.Keep(ctx); err != nil {
			logger.Error("db service stopped: storage shared with another db service", "error", err)
			stop()
		}
	}()

	reg := registry.NewClient(cfg, logger)
	if cfg.RegisterOnStart {
		go reg.Run(ctx)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("db service listening", "addr", cfg.ListenAddr, "engine", cfg.DBEngine)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down db service")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", "error", err)
	}

	if cfg.RegisterOnStart {
		unregisterCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := reg.Unregister(unregisterCtx); err != nil {
			logger.Warn("unregister failed", "error", err)
		}
	}
}
