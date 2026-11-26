# Security

How TainerHub protects your servers, what it can't protect against, and how to harden an install.

**The short version:** TainerHub controls Docker on every server it manages, and Docker access is root access. Anyone with the admin role, and to a lesser degree the operator role, effectively controls those servers. Treat the hub, its data and its admin accounts like the keys to your whole homelab.

## How the pieces connect

```
 Browser ──HTTPS 443──> reverse proxy ──HTTP──> tainerhub :8585  (UI + API)
                                                    │
                                                 Postgres (internal network only)

 tainerhub-agent (each server) ──mTLS──> tainerhub :7443  (agent gateway)
        │
   /var/run/docker.sock
```

- **The hub** serves the web UI and API on port 8585 and the agent gateway on port 7443. It has no Docker access of its own.
- **Postgres** sits on an internal Docker network. Only the hub can reach it.
- **Each agent** keeps one long-lived connection *out* to the hub's port 7443. Managed servers open no ports.
- **Each agent** talks to Docker through the Docker socket on its server.

## Hub and agents: mutual TLS

The connection between agents and the hub is locked down at both ends:

- **TLS 1.3 only**, with certificates on both sides (mutual TLS).
- **The hub runs its own private certificate authority (CA).** It's created on first start and kept in the hub's `/data` volume. The CA is valid for 10 years.
- **Agents pin that CA.** The join token carries the CA's SHA-256 fingerprint. An agent only trusts a hub that proves it holds that exact CA, so it can't be tricked into enrolling with a fake hub. Because trust comes from the pinned CA, not from a hostname, agents can reach the hub by IP, LAN name or VPN address.
- **Each agent has its own certificate**, valid for one year. The hub checks the certificate's serial number on every connection. When you remove a host, its certificate is refused from then on, and its live connection is dropped at once.
- **Agents accept only a fixed list of operations** (list containers, start, stop, read logs, deploy a stack, and so on). The hub can't send raw Docker API calls through them. Note that this is not a sandbox: some of those operations, like deploying a compose file or upgrading Docker, are powerful by design. See [What TainerHub can't protect against](#what-tainerhub-cant-protect-against).

### Join tokens

When an admin adds a host, the hub creates a join token:

- It works **once**.
- It expires **1 hour** after it was made.
- Only a hash of its secret is stored in the database.
- It carries the hub CA's fingerprint, so the new agent can check the hub.

After enrolling, the agent keeps its certificate in its `/data` volume. The token is no longer needed and can be removed from the agent's compose file.

Failed enrollments are limited to 10 per IP address per 15 minutes. Successful ones aren't counted, so enrolling many servers behind one NAT address works. Every enrollment is written to the audit log.

### Certificate renewal

Agents renew their certificate automatically:

- They start renewing **30 days** before it expires, and check every 12 hours.
- Renewal happens in two steps. The old certificate keeps working until the agent first connects with the new one. If the hub's reply is lost, or the agent can't save the new certificate, the agent simply carries on with the old one and tries again.
- If a server was off for a long time and its certificate has already expired, the agent can still renew it for up to **180 days** after expiry. In that window an expired certificate is accepted only for renewal, never for a normal connection.
- After that, the agent logs that it must be re-enrolled. Create a new join token for it on the Hosts page.

Renewals are written to the audit log.

### Copying data between hosts

Moving a stack copies its volumes and folder from one agent to another; moving a standalone container copies its volumes. Agents still only connect out: the sending agent uploads the data to the hub's port 7443 and the receiving agent downloads it, both over the same mutual TLS. The hub passes it through without storing it.

- Each copy has a random ID that only the two hosts it was set up for can use, once each.
- Both agents report the size and SHA-256 of what they sent and received; the copy fails if they differ. It also stops after 2 minutes without data, or if it grows past what was measured.
- The receiving agent never overwrites an existing volume or a stack folder with files in it, and refuses archives with paths outside the data, hard links out of it, or device files.
- The agent itself has no capabilities, so Docker reads and writes the data through short-lived helper containers made from the agent's own image (never started for copying, and without network). Deleting an old copy starts one that keeps Docker's default capabilities, so it can delete files of other users.

### Moving a container

Moving a standalone container adds two agent operations. **Export** reads the container's settings (what `docker inspect` shows, including environment values) and sends them to the hub, which passes them to the new host and doesn't store them. **Create** makes a container from those settings and, if needed, the bridge networks it uses. It refuses compose containers, TainerHub's own container names and networks of other drivers. Admins can already run any container through a stack, so this doesn't give the hub or admins more power over a host than they had, but a hub that's been taken over could use it the same way. Moving is admin-only and audit-logged (`container.move_started`, `container.moved`, `container.move_failed`, `container.move_leftovers_removed`).

### Limits on agents

An enrolled agent is trusted with its own server, not with the hub. What an agent sends is rate-limited, so a buggy or compromised one can't overload the database or flood every open browser. Single messages are capped at 8 MiB. Each IP address can hold at most 32 connections open before presenting a certificate, and slow enrollment requests are dropped after 10 seconds.

## Users and roles

There are four roles: **owner**, **admin**, **operator** and **viewer**. The full table is in [Features: Users and roles](features.md#roles). In short:

- **Viewers** see everything except secrets, and can read logs. They change nothing.
- **Operators** also start, stop, restart, remove and update containers, run saved stacks, and clean up images and networks.
- **Admins** also edit stacks, open terminals, delete volumes, see secrets, manage hosts and users, update TainerHub and Docker, and read the audit log.
- **The owner** is the account created at setup. It can also manage admins. Nobody can change or remove it.

Roles are enforced by the hub's API on every request, not just hidden in the UI. Admins can only manage users with a lower role than their own, can't change their own account from the Users page, and can't hand out a role above their own. A role change or removal only applies if the user's role is still the one the admin saw; otherwise it's refused and the admin reloads.

Lowering someone's role or removing them takes effect on their open pages too: see [Open streams are re-checked](#open-streams-are-re-checked).

### Users limited to some hosts

An admin can limit a user to some hosts ([how](features.md#limiting-a-user-to-some-hosts)). The hub enforces it on every request: each API route declares which hosts it concerns, and the check runs in one place before the route's own code. A test goes over every route, so a new one can't skip it. Other hosts answer `404`, as if they didn't exist, and the user's API tokens are limited the same way.

An admin limited to some hosts still has root on those hosts through compose files and terminals, so treat them as an admin of those machines. They can't reach hub-wide settings, users or tokens, and they can't be given the server the hub runs on: root there would be root on the hub, and admins see container settings such as the hub's database password. If such a user would somehow reach that server anyway, the hub treats them as an operator there.

## What non-admins can't see

Secrets often live in places Docker shows freely. For everyone below admin, the hub removes them before they leave the server:

- **Environment variable values.** Names are shown, values are not.
- **Label values**, except Compose and image labels (`com.docker.compose.*`, `org.opencontainers.image.*`). Reverse-proxy labels, for example, can hold password hashes or API tokens.
- **Command arguments.** The program is shown, its arguments are not. Think `--password=...`.
- **Health-check output.** Pass or fail and exit codes are shown; whatever the check printed is not.
- **Compose files and `.env`** of stacks.

Terminals are admin-only for the same reason: a shell in a container sees all of its environment.

Container **logs** are visible to every role, including viewers. If your apps print secrets to their logs, keep that in mind when you invite viewers.

## Sign-in and sessions

### Passwords, passkeys and recovery codes

- **Passwords** must be at least 10 characters. They're hashed with **argon2id** (19 MiB memory, 2 passes, the OWASP minimum).
- **Passkeys** are available once TainerHub is on HTTPS. They require user verification (fingerprint, face or PIN) and are discoverable, so you don't type a username.
- **Recovery codes**: 10 per user, each single-use, stored only as hashes. Making new ones cancels the old ones.
- There's no external identity provider and no email reset. Recovery codes are the way back in.

### Sessions

- Session tokens are random 256-bit values. Only a hash is stored in the database.
- Cookies are `HttpOnly` and `SameSite=Strict`. On HTTPS they're also `Secure` and use the `__Host-` prefix.
- A session ends after **12 hours of inactivity**, or **7 days** after sign-in, whichever comes first.
- Changing or setting your password signs out all your other sessions. **Account & sign-in → Sign out other sessions** does the same on demand. Neither revokes API tokens.

### Open streams are re-checked

Live updates, log views, terminals and stack output are long-lived connections. About once a minute the hub checks that the session or token behind each one is still valid and still has the role it needs. If the user was removed or signed out, the token was revoked or expired, the role was lowered, or they lost a host they could reach, the stream is closed within that minute. A terminal is closed once its user is no longer an admin.

### Fresh sign-in for sensitive changes

Some changes would give a stolen session lasting access, or more access. They need a sign-in from **the last 10 minutes**:

- adding or removing a passkey, generating new recovery codes, and setting a first password,
- creating an API token,
- creating invites, changing roles or hosts and removing users,
- adding a host and making a new join token,
- turning HTTPS on or off.

A stolen session cookie therefore can't be used to add the thief's own passkey, mint a token, invite a second account or lock you out. If the sign-in is older, the hub answers with a "sign in again" error and the UI shows a **Sign in again** dialog. Changing an existing password needs the current password instead.

## API tokens

Users can create API tokens for scripts on **Account & sign-in → API tokens**. See [Features: API tokens](features.md#api-tokens) and the [API guide](api.md).

- **Random and hashed.** A token is `thp_` plus 256 random bits. It's shown once; the database keeps only a SHA-256 hash and the first few characters, to tell tokens apart.
- **Never more than its owner.** Each token has an access level up to its owner's role. It acts with the lower of that level and the owner's *current* role, so demoting a user demotes their tokens. Removing a user removes their tokens.
- **Can't mint lasting access.** Tokens are refused for anything that creates access or changes how someone signs in: invites, role changes and user removal, passwords, passkeys, recovery codes, sessions, adding hosts and join tokens, HTTPS settings, and creating or revoking tokens. Creating a token needs a fresh browser sign-in.
- **Expiry.** 30 days, 90 days, 1 year or never. An expired token is refused.
- **Visible and revocable.** The list shows when and from which IP each token was last used. Admins can see and revoke the tokens of users with a lower role on the **Users** page; the owner sees all of them.
- **Audited.** Every action taken with a token is logged under its owner, with **via token** and the token's name.
- **No CSRF exposure.** Tokens are sent in the `Authorization` header, which browsers never add by themselves, so another site can't use one. Cookie sessions keep their CSRF checks.
- **Survive password changes.** Changing your password or signing out other sessions ends browser sessions only. If you think your account is compromised, revoke your tokens too.

### Rate limits

- **Failed sign-ins**: 10 per client IP per 15 minutes. This covers passwords, passkeys, recovery codes and guessing the current password when changing it.
- **Passkey ceremonies** (signing in with or adding a passkey): at most 30 started per client IP per minute.
- **Bad API tokens**: 20 per client IP per 15 minutes, counted separately from sign-ins, so a misconfigured script doesn't lock people out of signing in. After that, bad tokens get `429` instead of `401`. A valid token always works, even from an address that has sent bad ones.
- **Log streams**: each user can have 4 open per host and 8 in all. More get `429`.

The client IP comes from `X-Forwarded-For` only when the request arrives from a trusted proxy (set in Settings or `TAINERHUB_TRUSTED_PROXIES`). Otherwise it's the connecting address. That's why Settings only accepts narrow proxy ranges: anything in a trusted range could claim to be any visitor.

## Browser protections

- **CSRF.** Every request that changes something must carry a custom `X-TainerHub` header, which forms on other sites can't send, and a same-origin `Origin` header. Together with `SameSite=Strict` cookies, another site can't make your browser act in TainerHub.
- **Strict Content-Security-Policy.** Scripts, styles and fonts load only from TainerHub itself. No inline scripts or styles, no plugins, and TainerHub can't be framed by another site.
- **Other headers.** `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, cross-origin opener and resource policies, and a `Permissions-Policy` that turns off camera, microphone and location.
- **Input limits.** Request bodies are capped at 64 KiB (compose files up to 256 KB). Container IDs are strictly validated, which blocks path tricks against the Docker API.
- **Memory caps.** Pending passkey ceremonies and the rate-limiter tables are capped. Each host has its own caps on open log streams (16), terminals (8) and stack operations (8), so many open log views can't block a deploy or a terminal. A flood of requests can't exhaust the hub's memory.

## Self-protection

TainerHub won't cut off its own branch:

- An agent refuses to **stop, kill, pause or remove** its own container, or any container in its own compose project. On the hub's server, that project includes the hub and its database. Restart is allowed, because it comes back.
- **Pull & recreate** refuses TainerHub's own containers. They're updated from the Hosts page.
- A **stack can't use the name** of the compose project TainerHub runs in on that host, so a stack can't take over (and later remove) the hub, its database or the agent.

To stop TainerHub on purpose, use `docker compose` on the server.

## The audit log

Admins can read the audit log under **Audit**. It records sign-ins (and failed ones), account changes, API tokens created and revoked, every container and stack action, terminal sessions with the command used, host changes, agent enrollments and certificate renewals, updates, user management, and HTTPS changes. Each entry has the time, user, action, target and client IP. Actions taken with an API token also name the token.

Failed sign-ins that hit the rate limit aren't logged, so an attacker can't flood the log.

Entries older than 365 days are deleted. Set `TAINERHUB_AUDIT_RETENTION_DAYS` on the hub to keep them longer or shorter (`0` keeps them forever).

## Data at rest

| Where | What | How it's protected |
| --- | --- | --- |
| Hub `/data` volume | CA private key, database password, first-run setup token | File permissions; the hub runs as a non-root user. **Back this up.** |
| Postgres | Users, password hashes (argon2id), passkey public keys | Hashes and public keys only |
| Postgres | Session tokens, API tokens, recovery codes, join-token secrets, invite tokens | SHA-256 hashes only |
| Postgres | Stack compose files and `.env` | **Plain text** |
| Postgres | Audit log, settings, host records | Plain text |
| Agent `/data` volume | The agent's certificate and private key | File permissions |

Back up the hub's `/data` volume. Losing it means re-enrolling every agent. Leaking it lets someone pretend to be your hub to your agents.

Protect database backups like the `/data` volume: they hold your stacks' `.env` files.

### The database password

- Postgres is on an internal Docker network (`internal: true`). Nothing outside the hub's compose project can reach it.
- Postgres first starts with a well-known default password. On first start the hub replaces it with a random 192-bit one, saves it in `/data/db_password`, and never writes it to the logs. See it with `docker exec tainerhub /tainerhub db-password`.
- If you set `DB_PASSWORD` in a `.env` file before the first start, the hub uses yours and leaves it alone.
- If the database volume is recreated but `/data` survives, the hub re-applies the saved password by itself.

## What TainerHub can't protect against

Be clear-eyed about this:

- **The agent holds the Docker socket, which is root on that server.** The agent container itself runs as root (with all Linux capabilities dropped, a read-only filesystem and `no-new-privileges`), but control of the Docker socket means control of the host.
- **Admins effectively have root on every managed server.** A compose file can mount `/`, run privileged containers or use the host's network. Admins can also open terminals, and upgrading Docker runs a privileged helper on the host. This is why only admins can write stacks.
- **Operators have a lot of power too.** They can stop, remove and recreate any container (except TainerHub's own) and redeploy saved stacks. They can't write new compose files or open terminals.
- **Whoever controls the hub controls every agent.** A compromised hub, or a stolen admin session, can deploy a compose file to any host. Keep the hub patched, keep the number of admins small, and add passkeys.
- **Plain HTTP is for your LAN only.** Until you set up HTTPS, passwords and session cookies cross the network unencrypted.

## Supply chain

- Images are built from source by GitHub Actions on every push to `main`.
- Base images in the Dockerfiles are pinned by digest. The hub and agent run on distroless images; the hub's runs as a non-root user.
- CI runs `govulncheck` on the Go code and `npm audit` on the web UI.
- Each build is pushed under an immutable version tag (`0.1.0-<commit>`) and scanned with Trivy, failing on fixable high or critical CVEs. Only after the scan passes does `:latest` move to it.
- Images carry an SBOM and build provenance attestations.
- Dependabot keeps Go, npm, GitHub Actions and base-image dependencies current.
- The helper container used to upgrade Docker is pinned by digest.

No software can promise zero vulnerabilities. This is defence in depth, plus automated checks on every change.

## Hardening checklist

- [ ] **Save your recovery codes** outside TainerHub, and the database password in your password manager.
- [ ] **Back up the hub's `/data` volume** and the database. Protect the backups like the hub itself.
- [ ] **Don't expose port 8585 to the internet.** Use [HTTPS through a reverse proxy](https-and-passkeys.md) for remote access, ideally with an NPM Access List or a VPN.
- [ ] **Set up HTTPS and add passkeys** for every admin.
- [ ] **Trust only your proxy's single IP** in Settings or `TAINERHUB_TRUSTED_PROXIES`.
- [ ] **Once everyone uses HTTPS, block port 8585** from everything except the proxy (`DOCKER-USER` chain; see [Getting started](getting-started.md#5-open-the-right-ports)).
- [ ] **Limit who can reach port 7443** to your managed servers, if your firewall allows it. Never put 7443 behind a proxy.
- [ ] **Keep admins few.** Give people the lowest role that does the job: viewer for looking, operator for restarting things.
- [ ] **Remove users and hosts you no longer need.** A removed host's certificate stops working at once.
- [ ] **Review the audit log** now and then, especially failed sign-ins and terminal sessions.
- [ ] **Use "Sign out other sessions"** if you signed in on a device you don't control.
- [ ] **Give API tokens the lowest access that works** (viewer for dashboards), with an expiry. Revoke the ones you no longer use.
- [ ] **Keep TainerHub updated** from the Hosts page.
- [ ] **Don't run the profiler** (`TAINERHUB_PPROF_ADDR`) on an address others can reach.
