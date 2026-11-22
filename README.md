<p align="center"><img src="docs/logo.svg" width="96" alt="TainerHub logo"></p>

# TainerHub

A self-hosted Docker manager: one hub with a web UI, and a small agent on each Docker host. A Portainer alternative built around:

- **Simple to start, strong when you want it.** Install with one compose file and sign in with a password over plain HTTP. Later, put it behind your reverse proxy for HTTPS and add passkeys. Recovery codes throughout; no external identity provider.
- **One view of every host.** Agents dial *out* to the hub, so managed hosts open no ports.
- **mTLS everywhere between hub and agents**, using certificates from the hub's own private CA.
- **Built-in history and alerts** (phase 3: VictoriaMetrics, 90-day retention, email alerts).

Design doc: see the "TainerHub: A Portainer Alternative — Design" doc.

## Status: phase 1 (foundation)

| Area | State |
| --- | --- |
| Hub ↔ agent protocol: join-token enrollment, CA pinning, mTLS, reconnects, keepalive, revocation | Done, covered by an end-to-end test |
| Agent: container list, Docker events, start/stop/restart/pause/kill/remove, log streaming | Done, tested against a fake Docker Engine |
| First-run setup link, password sign-in (argon2id), optional passkeys once on HTTPS, recovery codes, sessions, rate limiting | Done |
| Postgres store with migrations, audit log | Done |
| Web UI (Forest theme: green-black with a lime accent and clipped-corner "tag" buttons; Space Grotesk + Outfit): Overview with live fleet map, sortable by CPU / memory / name / status, attention card, top consumers and activity feed; Containers board grouped by host, stack or state; ⌘K command palette ("restart pihole"); live logs; hosts; passkeys; audit log | Done |
| Container details (ports, mounts, networks, health-check history, restart policy, labels; environment values admin-only) | Done |
| Resources: images, volumes, networks across hosts with "used by", pull/update, remove, prune (volume deletion admin-only) | Done |
| Live CPU and memory per container (sampled every 10 s by the agent, same formulas as `docker stats`) | Done |
| Roles (owner/admin/operator/viewer) | Enforced by the API; only the owner account exists until user management lands |
| Compose stacks: create, edit and deploy compose files per host from the browser, with streamed output; start / stop / restart / down / pull & redeploy; projects started outside TainerHub are listed and can be taken over (see below) | Done; agent side tested against a real Docker Engine |
| Not yet: exec console, user invites, password + TOTP fallback, agent cert auto-renewal (certs last 1 year), private registries, metrics history, alerts | Phases 2 to 4 |

## How it fits together

```
 Browser ──HTTPS 443──> Nginx Proxy Manager ──HTTP──> tainerhub :8585  (UI + /api)
                                                          │
                                                       Postgres
 tainerhub-agent (each server) ──mTLS, HTTP/2──> tainerhub :7443  (agent gateway, never behind a proxy)
        │
   /var/run/docker.sock
```

Each agent keeps **one long-lived, full-duplex HTTP/2 stream** to the hub, carrying newline-delimited JSON in both directions (see `internal/wire`). This does the same job as the gRPC stream in the design but uses only Go's standard library. Port 7443 must reach the hub directly: a reverse proxy would terminate TLS and strip the agents' client certificates. Agents trust the hub by its pinned private CA rather than by hostname, so they can reach it by IP, LAN name or VPN address.

## Repository layout

```
cmd/tainerhub/          hub entry point
cmd/tainerhub-agent/    agent entry point
internal/wire/          hub <-> agent messages and join tokens
internal/pki/           private CA, server and agent certificates
internal/gateway/       agent-facing mTLS endpoint (+ end-to-end test)
internal/registry/      connected agents, in-flight commands and log streams
internal/agent/         agent: enrollment, connection loop, Docker Engine client
internal/store/         Postgres store and SQL migrations
internal/auth/          passwords, passkeys, recovery codes, sessions
internal/api/           REST API, SSE log streaming, serves the UI
web/                    React + TypeScript UI (Vite, TanStack Router/Query, Tailwind)
deploy/                 compose files for the hub stack and the agent
```

## Getting started

### 1. Install the hub

On any server with Docker:

```bash
mkdir -p ~/tainerhub && cd ~/tainerhub
curl -fsSLO https://raw.githubusercontent.com/prezmc/tainerhub/main/deploy/hub/compose.yml
docker compose up -d
docker compose logs tainerhub
```

The log prints a one-time setup link. Open it with your server's IP, e.g. `http://192.168.1.10:8585/setup?token=...` (valid 15 minutes; a new one is printed every 15 minutes until setup is done). Create your username and password, and save the recovery codes.

### 2. Add your servers

In TainerHub, go to **Hosts → Add host**, name the server, and copy the compose file it shows. On that server:

```bash
mkdir -p ~/tainerhub-agent && cd ~/tainerhub-agent
nano compose.yml          # paste the file
docker compose up -d
```

The host turns green within seconds. Do this for every server, including the hub's own: the hub never touches the Docker socket itself. Agents connect to the hub on port 7443, so that port must be reachable from each server (the Add host dialog pre-fills the hub's address; change it if agents should use a different IP or name).

### Updating

When a new version is published (each green CI run on `main` publishes `ghcr.io/prezmc/tainerhub` and `ghcr.io/prezmc/tainerhub-agent`), run this in `~/tainerhub` and in each `~/tainerhub-agent`:

```bash
docker compose pull && docker compose up -d
```

### Later: HTTPS and passkeys (optional)

Until you do this, TainerHub runs over plain HTTP on your LAN, and you sign in with a password. To put it behind a reverse proxy such as Nginx Proxy Manager (on the same server or another one):

1. **Proxy host:** your domain (e.g. `tainerhub.example.com`) → `http://<hub server IP>:8585`, with Websockets on and Force SSL. Proxy only port 8585; agents keep connecting straight to 7443.
2. **Tell the hub its address.** In `~/tainerhub/compose.yml`, uncomment and fill in:
   ```yaml
   TAINERHUB_PUBLIC_URL: https://tainerhub.example.com
   TAINERHUB_TRUSTED_PROXIES: 192.168.1.20   # the proxy's IP
   ```
   then `docker compose up -d`. This turns on passkeys and HTTPS-only cookies. Passkeys are tied to this address, so pick the name you'll keep.
3. **Add passkeys:** sign in at the https:// address, then **Account & sign-in → Add a passkey**.
4. **Optional:** once everyone uses the https:// address, stop port 8585 being reachable from the rest of the LAN, so only the proxy can use it. Docker-published ports bypass `ufw`, so use the `DOCKER-USER` chain:
   ```bash
   sudo iptables -I DOCKER-USER -p tcp -m conntrack --ctorigdstport 8585 ! -s 192.168.1.20 -j DROP
   ```

## Compose stacks

**Stacks** holds compose files that TainerHub deploys to one host each. The hub stores the compose file and `.env` in Postgres. When a stack is deployed, the agent writes them to `/opt/tainerhub/stacks/<name>/` on the host and runs the official `docker compose` binary that ships in the agent image. Output streams live to the browser. A deploy keeps running if you close the tab or the hub connection drops, and the result is written to the audit log.

- **Who can do what.** Viewers see every stack's status and containers. Operators can deploy, pull & redeploy, start, stop, restart and take down stacks an admin has saved. Only admins can read or edit compose files and `.env`, or create and delete stacks. A compose file can mount the host's disks or run privileged containers, so writing one amounts to root on that host.
- **Same-path mount.** Compose resolves relative bind mounts such as `./data` against the stack folder, and the Docker daemon then looks that path up on the host. So the folder must be mounted at the same path on both sides: `/opt/tainerhub/stacks:/opt/tainerhub/stacks`. The agent checks this at startup and reports any problem on the Hosts page.
- **Updating existing agents.** Agents set up before stacks need the new volume and the new image. Add `- /opt/tainerhub/stacks:/opt/tainerhub/stacks` under the agent's `volumes:`, then run `docker compose pull && docker compose up -d`.
- **Safe edits.** Each deploy runs `docker compose config` on the new file first, so an invalid file never replaces a working one. Edits can be saved without deploying; the stack then shows "changes not deployed". If two admins edit at once, the second save is refused instead of overwriting the first.
- **Deleting** runs `docker compose down` and removes `compose.yaml` and `.env` from the host. Named volumes are deleted only if you tick the box. Anything else in the stack folder, such as bind-mounted data, is left in place. **Remove from TainerHub only** forgets a stack without touching the host (for hosts that are gone).
- **Taking over existing projects.** Compose projects started by hand show up under "Started outside TainerHub". **Manage in TainerHub** opens a new stack with the same name: paste its compose file and deploy. Compose recreates the containers from the new file and keeps their named volumes. Relative bind mounts now point into `/opt/tainerhub/stacks/<name>/`, so move that data there first.
- **Limits.** `build:` sections aren't supported yet (no build context on the host), and neither are private registries. `.env` is stored in plain text in Postgres, alongside the hub's other secrets: back up and protect the database like the `/data` volume.

## Development

```bash
make test          # Go tests (needs Go 1.26+)
TAINERHUB_REAL_DOCKER=1 DOCKER_SOCKET=/var/run/docker.sock go test ./internal/agent -run Real   # stack lifecycle against a real Docker Engine
make agent         # bin/tainerhub-agent
make hub           # builds the UI, then bin/tainerhub with the UI embedded
cd web && npm run dev   # UI on :5173, proxying /api to a hub on :8585
```

To run a hub locally: start Postgres, then
`TAINERHUB_DATABASE_URL=postgres://... TAINERHUB_DATA_DIR=./data ./bin/tainerhub`.
Add `TAINERHUB_PUBLIC_URL=http://localhost:8585` to try passkeys, which work on `http://localhost` without HTTPS.

## Security, speed and reliability

**Built in**

- Passwords hashed with argon2id (at least 10 characters); passkeys once HTTPS is set up (user verification required, discoverable credentials); recovery codes are single-use and stored hashed.
- Sessions: random 256-bit tokens, stored hashed; `HttpOnly`, `SameSite=Strict` cookies, plus `Secure` and the `__Host-` prefix once `TAINERHUB_PUBLIC_URL` is https; 12-hour idle and 7-day absolute timeouts.
- CSRF: `SameSite=Strict`, plus a required custom header and an `Origin` check on every state-changing request.
- Headers: strict Content-Security-Policy (no inline scripts or styles), `frame-ancestors 'none'`, COOP/CORP, `nosniff`, `Referrer-Policy`.
- Input limits: 64 KiB request bodies, strict container ID validation (blocks path tricks against the Docker API), 8 MiB cap per hub↔agent message.
- Memory caps against floods: pending sign-in ceremonies, the rate-limiter table, and 16 log streams per host.
- Hub ↔ agent: TLS 1.3 only, mutual certificates, CA pinned through the join token (agents trust only the hub's own CA, so no hostname setup is needed), certificate serial checked on every connection. Agents accept only whitelisted actions, so even a compromised hub cannot run arbitrary Docker API calls through them.
- Containers: distroless images, read-only root filesystem, all Linux capabilities dropped, `no-new-privileges`; the hub runs as a non-root user.
- CI: `govulncheck`, `npm audit`, a Trivy image scan (fails on fixable high/critical CVEs), SBOM + provenance on images, and Dependabot for Go, npm, Actions and base images.
- Speed: the UI gets live pushes over Server-Sent Events instead of polling; JS/CSS are pre-compressed (Brotli/gzip) with immutable caching; the whole UI is about 125 KB gzipped.
- Reliability: agents reconnect with backoff and resend full state; hub↔agent keepalives drop dead links within 90 s; Docker health checks for the hub (database ping) and the agent (recent hub contact); graceful shutdown; Postgres starts healthy before the hub.

No software can promise zero vulnerabilities. The plan is defense in depth plus automated scanning on every push, and Dependabot keeps dependencies current.

## Security notes

- **Plain HTTP is for your LAN only.** Until you add HTTPS, passwords and session cookies cross the network unencrypted. Don't expose port 8585 to the internet; use the reverse-proxy steps above for remote access.
- The database is only reachable from the hub (an internal Docker network) and uses a default password. To set your own, put `DB_PASSWORD=...` in a `.env` file next to the hub's `compose.yml` **before the first start**.
- **Back up the hub's `/data` volume.** It holds the CA key. Losing it means re-enrolling every agent; leaking it lets someone impersonate the hub to agents.
- Join tokens are single-use, expire after 1 hour, and pin the hub CA's SHA-256 fingerprint, so an agent can't be tricked into enrolling with a fake hub.
- Removing a host deletes its record; its certificate is refused from then on and its live connection is dropped.
- Browser sessions are server-side: 12-hour idle timeout, 7-day maximum, `HttpOnly` + `SameSite=Strict` cookies. Every state-changing request also needs an `X-TainerHub` header and a same-origin `Origin`.
- Failed sign-ins are rate-limited per client IP (10 per 15 minutes). The real client IP is taken from `X-Forwarded-For` only when the request comes from `TAINERHUB_TRUSTED_PROXIES`.
- The agent holds the Docker socket, which is root-equivalent on that host. Treat the hub as a high-value system.
