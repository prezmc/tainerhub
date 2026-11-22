// Command tainerhub-agent runs on each Docker host and connects out to the hub.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prezmc/tainerhub/internal/agent"
	"github.com/prezmc/tainerhub/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if agent.Healthy(envOr("TAINERHUB_DATA_DIR", "/data"), 2*time.Minute) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "not connected to the hub in the last 2 minutes")
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	cfg := agent.Config{
		HubAddr:   os.Getenv("TAINERHUB_HUB"),
		JoinToken: os.Getenv("TAINERHUB_JOIN_TOKEN"),
		DataDir:   envOr("TAINERHUB_DATA_DIR", "/data"),
		Version:   version.Version,
		Docker:    agent.NewSocketDocker(envOr("DOCKER_SOCKET", "/var/run/docker.sock")),
		Log:       log,
	}
	a, err := agent.New(cfg)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("tainerhub agent starting", "version", version.Version, "hub", cfg.HubAddr)
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(envOr("TAINERHUB_LOG_LEVEL", "info"))); err != nil {
		return slog.LevelInfo
	}
	return l
}
