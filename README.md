<p align="center"><img src="docs/logo.svg" width="96" alt="TainerHub logo"></p>

# TainerHub

A self-hosted Docker manager: one hub with a web UI, and a small agent on each Docker host. A Portainer alternative built around:

- **Passkey sign-in** (WebAuthn) with recovery codes. No passwords and no external identity provider.
- **One view of every host.** Agents dial *out* to the hub, so managed hosts open no ports.
- **mTLS everywhere between hub and agents**, using certificates from the hub's own private CA.
- **Built-in history and alerts** (phase 3: VictoriaMetrics, 90-day retention, email alerts).

Design doc: see the "TainerHub: A Portainer Alternative — Design" doc.

## Status: phase 1 (foundation)

| Area | State |
| --- | --- |
| Hub ↔ agent protocol: join-token enrollment, CA pinning, mTLS, reconnects, keepalive, revocation | Done, covered by an end-to-end test |
| Agent: container list, Docker events, start/stop/restart/pause/kill/remove, log streaming | Done, tested against a fake Docker Engine |
| First-run setup link, passkey registration and sign-in, recovery codes, sessions, rate limiting | Done, needs a first real-browser test |
| Postgres store with migrations, audit log | Done |
| Web UI (Aurora theme, Space Grotesk + Outfit): Overview with live fleet map, sortable by CPU / memory / name / status, attention card, top consumers and activity feed; Containers board grouped by host, stack or state; ⌘K command palette ("restart pihole"); live logs; hosts; passkeys; audit log | Done |
| Container details (ports, mounts, networks, health-check history, restart policy, labels; environment values admin-only) | Done |
| Resources: images, volumes, networks across hosts with "used by", pull/update, remove, prune (volume deletion admin-only) | Done |
| Live CPU and memory per container (sampled every 10 s by the agent, same formulas as `docker stats`) | Done |
| Roles (owner/admin/operator/viewer) | Enforced by the API; only the owner account exists until user management lands |
| Compose stacks: create, edit and deploy compose files per host from the browser, with streamed output; start / stop / restart / down / pull & redeploy; projects started outside TainerHub are listed and can be taken over (see below) | Done; agent side tested against a real Docker Engine |
| Not yet: exec console, user invites, password + TOTP fallback, agent cert auto-renewal (certs last 1 year), private registries, metrics history, alerts | Phases 2 to 4 |

## How it fits together

```
 Browser ──HTTPS 443──> Nginx Proxy Manager ──HTTP──> tainerhub :8080  (UI + /api)
                                                          │
                                                       Postgres
 tainerhub-agent (each server) ──mTLS, HTTP/2──> tainerhub :7443  (agent gateway, NOT behind NPM)
        │
   /var/run/docker.sock
```

Each agent keeps **one long-lived, full-duplex HTTP/2 stream** to the hub, carrying newline-delimited JSON in both directions (see `internal/wire`). This does the same job as the gRPC stream in the design but uses only Go's standard library. Port 7443 must reach the hub directly: NPM would terminate TLS and strip the agents' client certificates.

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
internal/auth/          passkeys, recovery codes, sessions
internal/api/           REST API, SSE log streaming, serves the UI
web/                    React + TypeScript UI (Vite, TanStack Router/Query, Tailwind)
deploy/                 compose files for the hub stack and the agent
```

## Getting started

### 1. Push to GitHub and let CI build the images

```bash
cd ~/TainerHub
git init -b main
git add .
git commit -m "TainerHub phase 1 starter"
git remote add origin https://github.com/prezmc/tainerhub.git
git push -u origin main
```

The `ci` workflow resolves Go modules (and commits `go.sum` on its first run), builds the UI, runs `go vet` and the tests, then builds multi-arch images (amd64 + arm64):

- `ghcr.io/prezmc/tainerhub:latest`
- `ghcr.io/prezmc/tainerhub-agent:latest`

The repo is private, so the images are too. Either make the two packages public (GitHub → your profile → Packages → package → Package settings → Change visibility), or on each server run `docker login ghcr.io -u prezmc` with a personal access token that has `read:packages`.

### 2. Run the hub (Server 1, next to NPM)

```bash
mkdir -p ~/tainerhub && cd ~/tainerhub
cp <repo>/deploy/hub/compose.yml .
mkdir -p secrets && chmod 700 secrets
openssl rand -base64 32 | tr -d '\n' > secrets/db_password && chmod 644 secrets/db_password
docker network ls            # find NPM's network name, e.g. npm_default
NPM_NETWORK=npm_default docker compose up -d
docker compose logs -f tainerhub
```

The log prints a one-time setup link: `https://tainerhub.example.com/setup?token=...` (valid 15 minutes; a new one is printed every 15 minutes until setup is done).

### 3. Nginx Proxy Manager

Add a proxy host:

- Domain: `tainerhub.example.com` → `http://tainerhub:8080`
- Websockets support on; SSL: Force SSL, HTTP/2, HSTS
- Certificate: reuse your `*.example.com` wildcard, or request one with the Cloudflare DNS challenge
- DNS: point `tainerhub.example.com` at the server's LAN IP in local DNS (or DNS-only, grey cloud, in Cloudflare)

`TAINERHUB_PUBLIC_URL` must match the browser address exactly, because passkeys are bound to that hostname.

### 4. Create your account and add hosts

1. Open the setup link, pick a username and register a passkey. Save the recovery codes.
2. **Hosts → Add host** → name it → copy the generated compose file.
3. On that server: save it as `compose.yml` and run `docker compose up -d`. The host turns green within seconds.
4. Repeat for every server, **including Server 1**: the hub never touches the Docker socket itself.

The agent's compose file mounts `/opt/tainerhub/stacks` from the host at the **same path** inside the agent. That is where compose stacks live (see below); keep the path identical on both sides.

Agents need to reach `tainerhub.example.com:7443` over the LAN or VPN. If they connect by IP instead, add the IP to `TAINERHUB_AGENT_SANS` on the hub and use `IP:7443` as `TAINERHUB_HUB`.

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
cd web && npm run dev   # UI on :5173, proxying /api to a hub on :8080
```

To run a hub locally: start Postgres, then
`TAINERHUB_PUBLIC_URL=http://localhost:8080 TAINERHUB_DATABASE_URL=postgres://... TAINERHUB_DATA_DIR=./data ./bin/tainerhub`.
Passkeys work on `http://localhost` without HTTPS.

## Security, speed and reliability

**Built in**

- Passkeys only (user verification required, discoverable credentials); recovery codes are single-use and stored hashed.
- Sessions: random 256-bit tokens, stored hashed; `__Host-` cookies (`Secure`, `HttpOnly`, `SameSite=Strict`); 12-hour idle and 7-day absolute timeouts.
- CSRF: `SameSite=Strict`, plus a required custom header and an `Origin` check on every state-changing request.
- Headers: strict Content-Security-Policy (no inline scripts or styles), `frame-ancestors 'none'`, COOP/CORP, `nosniff`, `Referrer-Policy`.
- Input limits: 64 KiB request bodies, strict container ID validation (blocks path tricks against the Docker API), 8 MiB cap per hub↔agent message.
- Memory caps against floods: pending sign-in ceremonies, the rate-limiter table, and 16 log streams per host.
- Hub ↔ agent: TLS 1.3 only, mutual certificates, CA pinned through the join token, certificate serial checked on every connection. Agents accept only whitelisted actions, so even a compromised hub cannot run arbitrary Docker API calls through them.
- Containers: distroless images, read-only root filesystem, all Linux capabilities dropped, `no-new-privileges`; the hub runs as a non-root user.
- CI: `govulncheck`, `npm audit`, a Trivy image scan (fails on fixable high/critical CVEs), SBOM + provenance on images, and Dependabot for Go, npm, Actions and base images.
- Speed: the UI gets live pushes over Server-Sent Events instead of polling; JS/CSS are pre-compressed (Brotli/gzip) with immutable caching; the whole UI is about 125 KB gzipped.
- Reliability: agents reconnect with backoff and resend full state; hub↔agent keepalives drop dead links within 90 s; Docker health checks for the hub (database ping) and the agent (recent hub contact); graceful shutdown; Postgres starts healthy before the hub.

No software can promise zero vulnerabilities. The plan is defense in depth plus automated scanning on every push, and Dependabot keeps dependencies current.

## Security notes

- **Back up the hub's `/data` volume.** It holds the CA key. Losing it means re-enrolling every agent; leaking it lets someone impersonate the hub to agents.
- Join tokens are single-use, expire after 1 hour, and pin the hub CA's SHA-256 fingerprint, so an agent can't be tricked into enrolling with a fake hub.
- Removing a host deletes its record; its certificate is refused from then on and its live connection is dropped.
- Browser sessions are server-side: 12-hour idle timeout, 7-day maximum, `HttpOnly` + `SameSite=Strict` cookies. Every state-changing request also needs an `X-TainerHub` header and a same-origin `Origin`.
- Failed sign-ins are rate-limited per client IP (10 per 15 minutes). The real client IP is taken from `X-Forwarded-For` only when the request comes from `TAINERHUB_TRUSTED_PROXIES`.
- The agent holds the Docker socket, which is root-equivalent on that host. Treat the hub as a high-value system.
