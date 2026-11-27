// Command tainerhub-agent runs on each Docker host and connects out to the hub.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prezmc/tainerhub/internal/agent"
	"github.com/prezmc/tainerhub/internal/debugsrv"
	"github.com/prezmc/tainerhub/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "compose-helper" { // see agent.ComposeHelper
		err := agent.ComposeHelper(os.Args[2:])
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "rm-helper" { // see agent.RmHelper
		if err := agent.RmHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "share-check" { // see agent.ShareCheck
		if err := agent.ShareCheck(len(os.Args) > 2 && os.Args[2] == "write"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if agent.Healthy(envOr("TAINERHUB_DATA_DIR", "/data"), 2*time.Minute) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "not connected to the hub in the last 2 minutes")
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)
	debugsrv.Start(log)

	socket := envOr("DOCKER_SOCKET", "/var/run/docker.sock")
	docker := agent.NewSocketDocker(socket)
	// Container CPU and memory come from the host's cgroup files when they're
	// mounted (see deploy/agent/compose.yml); asking Docker costs the server more.
	if cgDir := envOr("TAINERHUB_CGROUP_DIR", "/host/sys/fs/cgroup"); docker.UseCgroups(cgDir) {
		log.Info("reading container stats from cgroup files", "dir", cgDir)
	} else {
		log.Info("reading container stats from Docker; mount /sys/fs/cgroup:" + cgDir + ":ro to lower the cost")
	}
	dataDir := envOr("TAINERHUB_DATA_DIR", "/data")
	cfg := agent.Config{
		HubAddr:       os.Getenv("TAINERHUB_HUB"),
		JoinToken:     os.Getenv("TAINERHUB_JOIN_TOKEN"),
		JoinTokenFile: os.Getenv("TAINERHUB_JOIN_TOKEN_FILE"),
		DataDir:       dataDir,
		Version:       version.Version,
		Docker:        docker,
		Stacks: &agent.StackRunner{
			// Must be mounted at the same path on the host (see deploy/agent/compose.yml).
			Dir:        envOr("TAINERHUB_STACKS_DIR", "/opt/tainerhub/stacks"),
			Bin:        envOr("TAINERHUB_COMPOSE_BIN", "/usr/local/bin/docker-compose"),
			Socket:     socket,
			WorkDir:    filepath.Join(dataDir, "compose"),
			CheckMount: docker.CheckSameMount,
			OwnProject: docker.OwnProject,
			Images:     docker,
		},
		Log: log,
	}
	a, err := agent.New(cfg)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Look for newer container images every 6 hours, unless the hub checked
	// recently (it schedules checks with the saved registry logins). The
	// first one waits 5 minutes so a hub that checks 2-3 minutes after the
	// agent connects gets there first.
	go docker.RunUpdateChecks(ctx, 5*time.Minute, 6*time.Hour)
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
