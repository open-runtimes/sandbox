// Command sandbox serves the open-runtimes sandbox contract: exec and whole-file
// access over HTTP, scoped to a single workspace directory. One process is one
// sandbox seat, so execs are serialized.
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
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	workspace := env("SANDBOX_WORKSPACE", "/workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		log.Error("create workspace", "path", workspace, "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              ":" + env("SANDBOX_PORT", "3000"),
		Handler:           newServer(workspace, env("SANDBOX_SHELL", "/bin/sh")),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Error("shutdown", "error", err)
		}
	}()

	log.Info("listening", "addr", srv.Addr, "workspace", workspace)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
