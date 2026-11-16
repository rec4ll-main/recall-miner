package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"recall-miner/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx)
	if err != nil {
		log.Fatalf("initialize api: %v", err)
	}
	defer application.Close()

	httpServer := &http.Server{
		Addr:              application.ListenAddr(),
		Handler:           application.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       time.Duration(application.ServerReadTimeoutSeconds()) * time.Second,
		WriteTimeout:      time.Duration(application.ServerWriteTimeoutSeconds()) * time.Second,
		IdleTimeout:       time.Duration(application.ServerIdleTimeoutSeconds()) * time.Second,
	}

	go application.RunValidatorLoop(ctx)

	go func() {
		tlsConfig := application.TLSConfig()
		if tlsConfig == nil {
			application.Log().Info("recall miner listening", "addr", application.ListenAddr())
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				application.Log().Error("server failed", "error", err)
				stop()
			}
			return
		}
		// TLS_ENABLED: serve HTTPS only on the configured listener; no
		// plaintext fallback and no HTTP redirect.
		ln, err := net.Listen("tcp", application.ListenAddr())
		if err != nil {
			application.Log().Error("listen failed", "addr", application.ListenAddr(), "error", err)
			stop()
			return
		}
		tlsLn := tls.NewListener(ln, tlsConfig)
		application.Log().Info("recall miner listening (TLS)", "addr", application.ListenAddr())
		if err := httpServer.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			application.Log().Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		application.Log().Error("server shutdown failed", "error", err)
	}
}
