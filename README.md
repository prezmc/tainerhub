<p align="center"><img src="docs/logo.svg" width="96" alt="TainerHub logo"></p>

# TainerHub

A self-hosted Docker manager for your homelab. One hub gives you a web UI for every Docker server you own; a small agent on each server connects out to the hub, so managed servers open no ports. Start containers, read logs, open a terminal, deploy compose stacks, clean up images and keep everything updated, from one page. It installs with one compose file, works with a password over plain HTTP on your LAN, and adds HTTPS and passkeys through the reverse proxy you already run.

<p align="center"><img src="docs/images/overview.png" width="900" alt="The TainerHub Overview page: fleet map, attention card, top consumers and live activity"></p>

## Features

- **Every host in one view.** A live fleet map, an attention card for unhealthy containers and offline hosts, and an activity feed.
- **Containers.** Start, stop, restart, pause, kill and remove; group by host, stack or state; live logs; details with health-check history; a terminal in the browser.
- **Updates.** Image update checks every 6 hours with an "update" badge; Pull & recreate with automatic rollback. Update TainerHub itself, and Docker Engine, from the Hosts page.
- **Compose stacks.** Create, edit and deploy compose files per host, with live output. Take over projects you started by hand.
- **New container from Docker Hub.** Search, pick a version, set ports, environment and volumes.
- **Resources.** Images, volumes and networks across hosts, with "used by", pull, remove and prune.
- **Users and roles.** Invite people by one-time link as viewer, operator or admin. Secrets stay hidden from non-admins.
- **Secure by default.** mTLS between hub and agents with a pinned private CA, argon2id passwords, passkeys, recovery codes, an audit log.
- **Light on your servers.** CPU and memory read from the kernel's cgroup files, and live updates pushed to the browser instead of polling.
- **⌘K command palette.** Type `restart pihole` or `logs npm`.

## Quick start

On a Linux server with Docker and the Compose plugin:

```bash
curl -fsSL https://raw.githubusercontent.com/prezmc/tainerhub/main/install.sh | sh
```

It installs the hub in `~/tainerhub`, then prints the database password (save it) and a one-time setup link. Open the link within 15 minutes and create your owner account. Lost it?

```bash
docker exec tainerhub /tainerhub setup-link
```

The hub's own server is managed automatically. To add more, go to **Hosts → Add host** and paste the one-line command it gives you on each server. Agents connect to the hub on port 7443, directly, never through a reverse proxy.

## Guides

| Guide | What's in it |
| --- | --- |
| [Getting started](docs/getting-started.md) | Requirements, installing the hub (one line or by hand), first sign-in, adding servers, ports, updating, lighter stats, uninstalling. |
| [Features](docs/features.md) | A tour of every page: Overview, Containers, Stacks, Resources, Hosts, Users and roles, Audit, Settings, Account, the command palette. |
| [HTTPS and passkeys](docs/https-and-passkeys.md) | Putting TainerHub behind Nginx Proxy Manager from the Settings page, keeping it LAN-only, and fixing test failures. |
| [Security](docs/security.md) | The security model, what it can't protect against, and a hardening checklist. |
| [API](docs/api.md) | API tokens, calling the JSON API with curl, live event streams, every endpoint with its role, recipes and security notes. |
| [Troubleshooting](docs/troubleshooting.md) | Agent offline, expired join tokens, "Stacks unavailable", passkeys, update errors, high CPU, lost access, logs and profiling. |

## Status

TainerHub is early software. Each green build of `main` is published as `ghcr.io/prezmc/tainerhub:latest` and `ghcr.io/prezmc/tainerhub-agent:latest`.

Not there yet: private registry credentials, `build:` sections in stacks, metrics history and alerts.

## How it fits together

```
 Browser ──HTTPS 443──> Nginx Proxy Manager ──HTTP──> tainerhub :8585  (UI + /api)
                                                          │
                                                       Postgres
 tainerhub-agent (each server) ──mTLS, HTTP/2──> tainerhub :7443  (agent gateway, never behind a proxy)
        │
   /var/run/docker.sock
```

Each agent keeps **one long-lived, full-duplex HTTP/2 stream** to the hub, carrying newline-delimited JSON in both directions (see `internal/wire`). It uses only Go's standard library. Port 7443 must reach the hub directly: a reverse proxy would terminate TLS and strip the agents' client certificates. Agents trust the hub by its pinned private CA rather than by hostname, so they can reach it by IP, LAN name or VPN address.

The hub has no Docker access of its own. Everything that touches Docker, including updating TainerHub itself, is done by an agent: the hub's compose file includes an agent that enrolls itself through a shared volume.

Compose stacks are stored in Postgres. On deploy, the agent writes `compose.yaml` and `.env` to `/opt/tainerhub/stacks/<name>/` on the host and runs the `docker compose` binary shipped in the agent image. That folder must be mounted at the same path in the agent and on the host, because compose resolves relative bind mounts against it.

### Speed and reliability

- The UI gets live pushes over Server-Sent Events instead of polling. JS and CSS are pre-compressed (Brotli and gzip) with immutable caching; the whole UI is about 125 KB gzipped.
- Agents read container CPU and memory from the kernel's cgroup files (the compose files mount `/sys/fs/cgroup` read-only), which costs the Docker Engine nothing. They sample every 10 seconds while someone has TainerHub open and once a minute otherwise. Without the mount, or on cgroup v1, they fall back to one-shot stats from Docker.
- Agents reconnect with backoff and resend their full state. Keepalives drop dead hub-agent links within 90 seconds.
- Docker health checks for the hub (database ping) and the agent (recent hub contact); graceful shutdown; Postgres is healthy before the hub starts.

## Repository layout

```
cmd/tainerhub/          hub entry point (also: setup-link, db-password, healthcheck)
cmd/tainerhub-agent/    agent entry point
internal/wire/          hub <-> agent messages and join tokens
internal/pki/           private CA, server and agent certificates
internal/gateway/       agent-facing mTLS endpoint: enrollment, renewal, limits (+ end-to-end test)
internal/registry/      connected agents, in-flight commands and log streams
internal/agent/         agent: enrollment, connection loop, Docker Engine client, stacks, self-update
internal/store/         Postgres store and SQL migrations
internal/auth/          passwords, passkeys, recovery codes, sessions, invites
internal/api/           REST API, SSE streams, HTTPS setup, updates; serves the UI
internal/debugsrv/      optional pprof server (TAINERHUB_PPROF_ADDR)
internal/version/       build version
web/                    React + TypeScript UI (Vite, TanStack Router/Query, Tailwind)
deploy/                 compose files for the hub stack and the agent
docs/                   user guides
```

## Development

```bash
make test          # Go tests with the race detector (needs Go 1.26+)
TAINERHUB_REAL_DOCKER=1 DOCKER_SOCKET=/var/run/docker.sock go test ./internal/agent -run Real   # stack lifecycle against a real Docker Engine
make agent         # bin/tainerhub-agent
make hub           # builds the UI, then bin/tainerhub with the UI embedded
make images        # both container images
cd web && npm run dev   # UI on :5173, proxying /api to a hub on :8585
```

To run a hub locally, start Postgres, then:

```bash
TAINERHUB_DATABASE_URL=postgres://... TAINERHUB_DATA_DIR=./data ./bin/tainerhub
```

Add `TAINERHUB_PUBLIC_URL=http://localhost:8585` to try passkeys, which work on `http://localhost` without HTTPS.

The hub deletes audit log entries older than `TAINERHUB_AUDIT_RETENTION_DAYS` (default `365`; `0` keeps them forever).

Set `TAINERHUB_PPROF_ADDR=127.0.0.1:6060` on the hub or an agent to serve Go's profiler there (`go tool pprof http://127.0.0.1:6060/debug/pprof/profile`). Inside a container, see [Troubleshooting: profiling](docs/troubleshooting.md#profiling-with-pprof).

CI runs `govulncheck`, `npm audit` and a Trivy image scan on every push; images are published with an SBOM and provenance, and `:latest` only moves after the scan passes. See [Security: supply chain](docs/security.md#supply-chain).
