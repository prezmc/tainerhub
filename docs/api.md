# API

Everything the TainerHub UI does goes through a JSON API under `/api` on the hub. You can call the same API from scripts, dashboards and home-automation tools with an **API token**.

This guide covers signing requests with a token, the conventions every endpoint follows, the live streams, a reference of every route, and some ready-to-use recipes.

- [Authentication](#authentication)
- [Quick start](#quick-start)
- [Conventions](#conventions)
- [Streams (Server-Sent Events)](#streams-server-sent-events)
- [Long-running jobs](#long-running-jobs)
- [Endpoint reference](#endpoint-reference)
- [Recipes](#recipes)
- [Security notes](#security-notes)

## Authentication

### API tokens

1. Open account menu → **Account & sign-in** and scroll to **API tokens**.
2. Under **Create a token**, give it a name (for example `home-assistant`), an **Access** level and when it **Expires**, then click **Create**:
   - Access: **viewer**, **operator** or **admin**, up to your own role (the owner can also pick **owner**).
   - Expires: **30 days**, **90 days**, **1 year** or **Never**.
3. Copy the token. It's shown **once**. It looks like `thp_` followed by 43 characters. The dialog also gives you a `curl` command to try it.

Creating a token needs a browser sign-in from the last 10 minutes. If yours is older, TainerHub asks you to sign in again first.

Admins can see and revoke the tokens of users with a lower role on the **Users** page. The owner sees every token.

Send the token in the `Authorization` header:

```
Authorization: Bearer thp_...
```

That's all a token request needs. The session cookie and the `X-TainerHub: 1` header (plus the `Origin` check) are only for browser sessions, where they guard against cross-site requests.

How a token's access works:

- **Effective access is the lower of the token's level and your current role.** An admin token belonging to someone who is later made a viewer acts as a viewer. Making them an admin again restores it.
- **Removing a user removes their tokens.**
- An expired or revoked token gets `401` with `{"error": "..."}`.
- Bad tokens are counted per IP address, separately from sign-in attempts. After 20 in 15 minutes, further bad tokens get `429` until the window passes. A valid token always works, even from that address.
- Every action taken with a token appears in the **Audit** log under its owner, marked **via token** with the token's name.

### What tokens can't do

Tokens can't do anything that grants lasting access to TainerHub. These endpoints answer `403` with an explanation when called with a token, even an admin token:

- creating invites,
- changing a user's role or removing a user,
- changing your password, passkeys or recovery codes, and "sign out other sessions",
- turning HTTPS on or off,
- adding a host or issuing a host join token,
- creating or revoking API tokens (listing them works),
- saving registry logins, and changing automatic updates (opting in or out, the window, Run now).

Do these in the browser. Everything else follows the [role table](features.md#roles). The [reference](#endpoint-reference) marks each route.

### Browser sessions

The UI signs in with a password, passkey or recovery code and gets a session cookie. If you script against a cookie session instead of a token, every request other than `GET`/`HEAD`/`OPTIONS` must carry `X-TainerHub: 1`, and any `Origin` header must be the hub's own address, or the hub answers `403`. Tokens are simpler and are what this guide uses.

## Quick start

Create a **viewer** token (see above), then in a shell:

```sh
export TH=http://192.168.1.10:8585      # or https://tainerhub.example.com
export TOKEN=thp_...                    # the token you copied

curl -s -H "Authorization: Bearer $TOKEN" "$TH/api/hosts"
```

You get a JSON array of your hosts, for example:

```json
[
  {
    "id": "3f2a…",
    "name": "server-1",
    "online": true,
    "containers": 14,
    "running": 13,
    "unhealthy": 0,
    "load": { "cpu": 7.5, "memAvailable": 6123456512, "load1": 0.42, "samples": 30 }
  }
]
```

The examples below use `jq` to pick fields out of responses. It's optional.

## Conventions

### Base URL

The API lives at `/api` on the hub's address: `http://192.168.1.10:8585/api/...` on the LAN, or `https://tainerhub.example.com/api/...` behind your reverse proxy. Unknown paths under `/api/` answer `404 {"error": "not found"}`.

### JSON

Request bodies are JSON (`Content-Type: application/json`), at most 64 KB (1 MB for stack create, update and takeover check). Field names in request bodies are matched without regard to case, so `{"name": ...}` and `{"Name": ...}` both work. Responses are JSON with `Cache-Control: no-store`. Times are RFC 3339 strings (`2026-11-23T09:30:00Z`). Many changes answer `204 No Content` with no body.

### Errors

Errors have a JSON body with an `error` message that's safe to show to a person:

```json
{ "error": "host is offline" }
```

Some add fields: `"reauth": true` (needs a recent browser sign-in), `"jobId"` (an operation already running), `"report"` or `"takeover"` (a move or takeover check that found problems).

| Status | Meaning |
| --- | --- |
| `400` | The body or a parameter is invalid (`"invalid JSON body"`, a bad image reference, a missing host...). |
| `401` | Not signed in: no token, or an expired or revoked one. |
| `403` | Your role (or the token's level) is too low, the action isn't allowed with a token, or a browser request is missing `X-TainerHub`. |
| `404` | No such host, stack, job, user or route. |
| `409` | Conflict: a name is taken, someone saved first, an operation is already running for that stack, a check found blockers. |
| `429` | Rate limited: too many sign-in attempts or bad tokens from your IP, or too many log streams or terminals open. |
| `500` | Internal error (details are in the hub's log, not the response). |
| `502` | The host's agent ran the command and it failed (Docker's error is in `error`). |
| `503` | The host is offline, or the hub can't reach its database. |

> A host ID that doesn't match any connected host answers `503 {"error": "host is offline"}` on container and resource routes, not `404`.

### IDs

| Thing | Path parameter | What it is |
| --- | --- | --- |
| Host | `{id}` | The host's UUID, from `GET /api/hosts` (`id`). Host names are not accepted. |
| Container | `{cid}` | A Docker container ID (full or short) **or** a container name, as on that host. Letters, digits, `_`, `.` and `-`, up to 128 characters. |
| Stack | `{sid}` | The stack's UUID, from `GET /api/stacks` (`id`). Only stacks TainerHub manages have one. |
| Job | `{jid}` | From the `jobId` an operation returns. |
| Image | `{rid}` on `/images/` | An image ID: `sha256:<hex>` or bare hex, at least 12 characters. |
| Volume, network | `{rid}` | The volume name, or the network name or ID. |
| User, invite, move | `{uid}`, `{iid}`, `{mid}` | UUIDs from the matching list. |

To act on a host or stack by **name**, list first and look the ID up (see [Recipes](#recipes)).

### Pagination

There's none: lists return everything. The one limit is the audit log: `GET /api/audit?limit=N` returns the newest `N` entries (default 200, at most 1000).

## Streams (Server-Sent Events)

Four endpoints stream with [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events) (`Content-Type: text/event-stream`). Each message is an `event:` line and a `data:` line with JSON, followed by a blank line. Lines starting with `:` are keepalives, sent every 25 seconds; ignore them.

With curl, add `-N` so output isn't buffered:

```sh
curl -sN -H "Authorization: Bearer $TOKEN" "$TH/api/events"
```

Open streams re-check your access about once a minute. If the token is revoked or expires, or your role drops below what the stream needs, the hub closes it. Reconnect with backoff if a stream ends unexpectedly.

| Stream | Events |
| --- | --- |
| `GET /api/events` (fleet) | `fleet`: `{"hosts": [...], "containers": [...], "activity": [...]}`. The whole fleet, sent on connect and again whenever anything changes (changes are batched over 250 ms). `hosts` and `containers` are the same shapes as `GET /api/hosts` and `GET /api/containers`; `activity` is the 40 most recent events: `{"at", "kind": "container"\|"host"\|"user", "action", "hostId", "host", "container", "actor"}`. |
| `GET /api/hosts/{id}/containers/{cid}/logs` | `log`: a JSON string with a chunk of output (may hold several lines, or part of one). `error`: a JSON string. `end`: `{}` when the logs stop (the container exited, or `follow=0` reached the end). Query: `tail` (lines of history, default 300, at most 10000), `follow=0` to stop after the history. |
| `GET /api/jobs/{jid}` | `job`: `{"id", "stackId", "stack", "host", "action", "actor", "startedAt"}` first. `output`: a JSON string of output. `done`: `{"ok": true\|false, "error": "..."}`, then the stream ends. |
| `GET /api/exec/{sid}` (terminal, admin) | `ready`: `{}`. `out`: base64-encoded terminal bytes (not JSON). `end`: `{}` or `{"error": "..."}`. Closing the stream closes the terminal. |

Log streams are capped at 4 per host and 8 in all for each user; the next one gets `429`.

## Long-running jobs

Stack operations (deploy, pull, start, stop, restart, down, remove) and moves run on the hub as **jobs**, independent of the request that started them. The request answers `202 Accepted` at once:

```json
{ "jobId": "9c1f…" }
```

Follow the output with `GET /api/jobs/{jid}` (see above). It replays everything from the start, so you can attach late or reattach. Finished jobs stay readable for an hour, then answer `404`. Only one job runs per stack at a time; starting another answers `409` with the running one's `jobId`.

Saving a stack with `"deploy": true` returns the `jobId` in the save response (or a `deployError`). Moves return `{"jobId", "moveId"}`.

## Endpoint reference

**Role** is the minimum role: viewer < operator < admin < owner. **Token** says whether an API token may call it; with a token, the role check uses the token's effective access level. Public routes need no authentication.

### Public (no sign-in)

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/healthz` | `{"ok": true, "agents": N}` (`N` connected agents), or `503` if the database is unreachable. For health checks. |
| GET | `/api/state` | `{"setupRequired", "version", "passkeys", "publicURL"}`. |
| POST | `/api/setup/begin` | First-run owner setup with a passkey (UI only). |
| POST | `/api/setup/finish` | " |
| POST | `/api/setup/password` | First-run owner setup with a password. Body: `token`, `username`, `displayName`, `password`. |
| POST | `/api/login/begin` | Passkey sign-in (UI only). |
| POST | `/api/login/finish` | " |
| POST | `/api/login/password` | Body: `username`, `password`. Sets the session cookie; returns `{"user"}`. |
| POST | `/api/login/recovery` | Body: `username`, `code`. Returns `{"user", "recoveryCodesLeft"}`. |
| POST | `/api/logout` | Ends the browser session. `204`. |
| POST | `/api/invite/check` | Body: `token`. Returns `{"role"}`, or `404` if the invite is invalid or used. |
| POST | `/api/invite/accept` | Body: `token`, `username`, `displayName`, `password`. Returns `{"user", "recoveryCodes"}`. |
| GET | `/join/{token}` | Shell script that installs an agent with a join token. Query: `hub=<addr:7443>`. |
| GET | `/api/https-check` | The public half of the HTTPS test on the Settings page. Query: `nonce`. |

Public `POST`s still need `X-TainerHub: 1`. Sign-in endpoints are rate limited (`429`).

### Fleet and hosts

| Method | Path | Role | Token | Response |
| --- | --- | --- | --- | --- |
| GET | `/api/hosts` | viewer | yes | Array of hosts (below). |
| GET | `/api/containers` | viewer | yes | Array of every container on every connected host (below). |
| GET | `/api/events` | viewer | yes | SSE fleet stream. |
| GET | `/api/placement` | viewer | yes | Hosts ranked by room for new work (below). |
| POST | `/api/hosts` | admin | **no** | Add a host and issue its join token. |
| POST | `/api/hosts/{id}/token` | admin | **no** | New join token for an existing host. |
| PUT | `/api/hosts/{id}` | admin | yes | Rename. `204`. |
| DELETE | `/api/hosts/{id}` | admin | yes | Remove the host; its agent's certificate stops working. `204`. |
| PUT | `/api/hosts/{id}/stats-source` | admin | yes | Returns `{"source"}`. |
| POST | `/api/hosts/{id}/lighter-stats` | admin | yes | `202 {"result"}`. |

- **Host:** `id`, `name`, `createdAt`, `enrolledAt`, `lastSeenAt`, `info`, `online`, `address` (the agent's IP), `hubHost` (also runs the hub), `hello` (agent and Docker versions, from the agent), `containers`, `running`, `unhealthy` (containers whose status says `(unhealthy)`, or restarting), `snapshotAt`, `load` (`cpu`, `cpuNow`, `memAvailable`, `memAvailableNow`, `load1`, `diskFree`, `diskTotal`, `samples`, `since`; missing while offline).
- **Container:** `id`, `name`, `image`, `state` (`running`, `exited`, `paused`, `restarting`, `created`, `dead`), `status` (Docker's text, e.g. `Up 3 hours (healthy)`), `created` (Unix seconds), `project` (compose project), `ports`, `cpu` (% of one core), `mem`, `mem_limit` (bytes), `update` (a newer image is available), `hostId`, `hostName`. Sorted by host name, then container name.
- **Placement:** query `stack=<sid>` (moving a stack) or `host=<id>&container=<cid>` (moving a container), or nothing (new work). Returns `{"footprint": {"cpu", "mem"}, "hosts": [{"hostId", "hostName", "eligible", "reason", "current", "cpu", "mem", "cpuAfter", "memAfter", "diskFree", "score", "suggested"}]}`, best first.
- `POST /api/hosts` body: `name` (up to 64 characters), optional `agentAddr` (`192.168.1.10:7443`). Needs a recent sign-in. Returns `{"host", "joinToken", "expiresAt", "agentAddr", "compose"}`. `POST /api/hosts/{id}/token` takes the optional `agentAddr` and returns the same.
- `PUT /api/hosts/{id}` body: `name`. `409` if the name is taken.
- `PUT /api/hosts/{id}/stats-source` body: `source`: `"docker"` or `"cgroup"`.

### Containers

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/hosts/{id}/containers/{cid}` | viewer | yes | Details. |
| GET | `/api/hosts/{id}/containers/{cid}/logs` | viewer | yes | SSE log stream. |
| POST | `/api/hosts/{id}/containers/{cid}/{action}` | operator | yes | `start`, `stop`, `restart`, `pause`, `unpause`, `kill`, `remove`. `204`. |
| POST | `/api/hosts/{id}/containers/{cid}/recreate` | operator | yes | Pull the image and recreate a standalone container. |
| POST | `/api/hosts/{id}/containers/{cid}/remove-with-volumes` | admin | yes | Remove with its anonymous volumes and the named ones you list. |
| GET | `/api/hosts/{id}/containers/{cid}/move` | admin | yes | Check a move. Query: `host=<target id>`. |
| POST | `/api/hosts/{id}/containers/{cid}/move` | admin | yes | Start a move: `202 {"jobId", "moveId"}`. |
| GET | `/api/container-moves` | admin | yes | Recent container moves. |
| POST | `/api/hosts/{id}/containers/{cid}/exec` | admin | yes | Open a terminal: `201 {"session"}`. |
| GET | `/api/exec/{sid}` | admin | yes | SSE terminal output. Attach within 30 seconds of opening. |
| POST | `/api/exec/{sid}/input` | admin | yes | Body: `data` (base64 bytes). `204`. |
| POST | `/api/exec/{sid}/resize` | admin | yes | Body: `cols`, `rows`. `204`. |
| DELETE | `/api/exec/{sid}` | admin | yes | Close the terminal. `204`. |

- **Details** returns `{"detail", "envRevealed", "hostId", "hostName"}`. `detail` has `id`, `name`, `image`, `imageId`, `created`, `state`, `cmd`, `entrypoint`, `workingDir`, `user`, `env`, `labels`, `restartPolicy`, `privileged`, `networkMode`, `memoryLimit`, `nanoCpus`, `mounts`, `networks`, `ports`. Below admin, environment values, label values and command arguments are hidden and `envRevealed` is `false`.
- `remove` force-removes. The agent refuses to stop, kill, pause or remove TainerHub's own containers (`502` with the reason).
- `recreate` returns `{"status": "updated" | "current", "oldImage", "newImage"}`.
- `remove-with-volumes` body: `{"volumes": ["name", ...]}` (up to 32). Returns `{"anonymous", "removed", "kept": [{"name", "reason"}]}`.
- Move body: `hostId` (target), `outside` (the host folders the check listed, to confirm you've seen them; must match the check). `409` with `{"error", "report"}` if the check finds blockers.
- Exec body: `shell` (`auto`, `bash`, `sh`, `ash`, `zsh`, or a command), `cols`, `rows`.

### Stacks

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/stacks` | viewer | yes | Every stack, managed or not. |
| GET | `/api/stacks/{sid}` | viewer | yes | One managed stack. |
| POST | `/api/stacks/{sid}/{action}` | operator | yes | `deploy`, `pull`, `start`, `stop`, `restart`, `down`. `202 {"jobId"}`. |
| POST | `/api/stacks` | admin | yes | Create (and optionally deploy). `201`. |
| PUT | `/api/stacks/{sid}` | admin | yes | Save the compose file, `.env` and other files. |
| DELETE | `/api/stacks/{sid}` | admin | yes | Remove: `compose down`, then delete its files. `202 {"jobId"}`. |
| GET | `/api/stacks/{sid}/move` | admin | yes | Check a move. Query: `host=<target id>`. |
| POST | `/api/stacks/{sid}/move` | admin | yes | Start a move: `202 {"jobId", "moveId"}`. |
| DELETE | `/api/moves/{mid}/leftovers` | admin | yes | Delete what a finished stack or container move left behind. `204`. |
| POST | `/api/stacks/takeover-check` | admin | yes | Compare a pasted compose file with a running project. |
| GET | `/api/stacks/takeover-source` | admin | yes | Read a running project's own files. Query: `host`, `name`. |
| GET | `/api/jobs/{jid}` | viewer | yes | SSE job output. |
| GET | `/api/dockerhub/search` | admin | yes | Query: `q` (1-100 characters). `[{"name", "description", "stars", "pulls", "official"}]`. |
| GET | `/api/dockerhub/tags` | admin | yes | Query: `repo` (`nginx`, `linuxserver/plex`). `[{"name", "updated", "size"}]`. |
| PUT | `/api/stacks/{sid}/auto-update` | admin | **no** | Turn [automatic updates](#automatic-updates) on or off for the stack. Body: `{"enabled": true}`. |
| GET | `/api/stacks/{sid}/rollback` | viewer | yes | Preview a rollback (below). `404` when there's nothing to roll back to. |
| POST | `/api/stacks/{sid}/rollback` | operator | yes | Roll back to the newest rollback point. `202 {"jobId"}`; `404` when there's none. Audited as `stack.rolled_back`. |

- **Stack summary** (list): `id` (empty for projects TainerHub doesn't manage), `name`, `hostId`, `hostName`, `managed`, `tainerhub` (TainerHub's own project), `online`, `status` (`running`, `partial`, `stopped`, `down`, `not deployed`, `unknown`), `containers`, `running`, `unhealthy`, `updatedAt`, `updatedBy`, `deployedAt`, `pending` (saved changes not yet deployed), `autoUpdate` (opted in to automatic updates), `job` (`{"id", "action"}` while one runs).
- **Get** returns `{"stack", "summary", "filesVisible", "moves", "rollbackPoint"}`. `stack` has `id`, `hostId`, `name`, `compose`, `env`, `files` (`[{"path", "content", "executable"}]`), `createdAt`, `updatedAt`, `updatedBy`, `deployedAt`, and `rolledBackTo` while the stack runs a rollback (when the point it went back to was recorded). `rollbackPoint` is the newest rollback point, `{"id", "createdAt", "trigger", "createdBy", "services"}` (`services` is a count), or `null`. Below admin, `compose` and `env` are empty, `files` is `[]` and `filesVisible` is `false`.
- **Actions:** `pull` pulls newer images, then deploys (with `build:` services: `build --pull`, `pull --ignore-buildable`, `up -d`). `deploy` writes the files and runs `up -d --remove-orphans` (adding `--build` when a service builds its image). The first deploy of a project taken over from outside TainerHub needs an admin and a body `{"ack": "..."}` from the takeover check.
- **Create** body: `hostId`, `name` (lowercase letters, digits, `-`, `_`; up to 63), `compose` (up to 256 KB), `env` (up to 64 KB), `files` (optional: up to 30 `{"path", "content", "executable"}`, each up to 256 KB of UTF-8 text, 2 MB together; `path` relative with `/` between folders, letters, digits, `.`, `_`, `-`, at most 4 parts and 200 characters), `deploy` (bool), `adopt` (bool: take over a running project of that name), `ack`. Returns `{"stack", "jobId"?, "deployError"?}`. `409` if the name is taken on that host or belongs to a project started elsewhere (unless `adopt`).
- **Update** body: `compose`, `env`, `files` (leave it out to keep the saved files; `[]` removes them all), `updatedAt` (the `updatedAt` you loaded; `409` if someone saved since), `deploy`, `ack`. The name can't change. Returns `{"stack", "jobId"?, "deployError"?}`.
- **Rollback points** are recorded right before each `deploy`, `pull`, automatic update and rollback (`trigger`: `deploy`, `pull`, `auto`, `rollback`), while the stack has containers; the last 5 per stack are kept. Each job's output says whether one was recorded.
- **Rollback preview** returns `{"point": {"id", "createdAt", "trigger", "createdBy"}, "services": [{"service", "current", "previous", "same"}], "rolledBackTo", "composeChanged", "currentError"?}`. `current` and `previous` are `{"ref", "imageId", "digest", "created"}` (`current` is `null` for a service that isn't running). `composeChanged` is true when the compose file was deployed again since the point (a rollback changes images only).
- **Rollback** runs `up -d --remove-orphans` with an extra compose file that pins each recorded service to its old image (`pull_policy: never`; nothing is pulled or built). It fails before changing anything when an old image is no longer on the host. Afterwards the stack is pinned: `start`, `stop`, `restart` and `down` keep the old images until the next `deploy` or `pull`, which returns it to the latest images and clears `rolledBackTo`. The stack's files and deployed version don't change, so `pending` stays as it was.
- **Delete** query: `volumes=1` also deletes its named volumes; `forget=1` only forgets the stack and leaves the host alone (`204`).
- **Takeover check** body: `hostId`, `name`, `compose`, `env`. Returns `{"issues": [{"key", "level", "title", "detail", "fix"}], "ack", "blocked", "workingDir", "dir", "running"}`.
- **Move** body: `hostId`, `outside`. `409` if the stack has undeployed changes, or with `{"error", "report"}` if the check finds blockers.

### Resources

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/hosts/{id}/images` | viewer | yes | `[{"id", "tags", "size", "created", "usedBy", "keptFor"?}]`; `keptFor` names the stacks the image is kept for, to roll back to. |
| GET | `/api/hosts/{id}/volumes` | viewer | yes | `[{"name", "driver", "mountpoint", "created", "project", "usedBy"}]` |
| GET | `/api/hosts/{id}/networks` | viewer | yes | `[{"id", "name", "driver", "scope", "internal", "builtin", "project", "usedBy"}]` |
| POST | `/api/hosts/{id}/images/pull` | operator | yes | Body: `ref` (`nginx:1.27`, `ghcr.io/org/app:tag`). |
| DELETE | `/api/hosts/{id}/images/{rid}` | operator | yes | Remove an image. One kept for a stack's rollback is refused (`502` with the reason) unless `?force=1`, which needs an admin. |
| DELETE | `/api/hosts/{id}/networks/{rid}` | operator | yes | Remove a network. |
| POST | `/api/hosts/{id}/images/prune` | operator | yes | Dangling images; `?all=1` for every unused image. |
| POST | `/api/hosts/{id}/networks/prune` | operator | yes | Unused networks. |
| DELETE | `/api/hosts/{id}/volumes/{rid}` | admin | yes | Remove a volume (its data is gone). |
| POST | `/api/hosts/{id}/volumes/prune` | admin | yes | Unused anonymous volumes; `?all=1` for named ones too. |

Prune returns `{"deleted", "reclaimed" (bytes), "kept": [{"name", "reason"}]}`. Removes answer `204`. Neither prune removes images kept for a stack's rollback (tagged `tainerhub-rollback/…`).

### Updates and Docker Engine

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| POST | `/api/hosts/{id}/check-updates` | operator | yes | Check the host's images for newer versions now. `{"checked", "images": {"<image>": true\|false}, "errors"}` |
| GET | `/api/updates` | admin | yes | TainerHub and Docker Engine versions (below). |
| POST | `/api/updates/check` | admin | yes | Ask GitHub now instead of using the cached answer; same response. |
| POST | `/api/hosts/{id}/update` | admin | yes | Update the host's agent (on the hub's server, the hub too). `202 {"result": {"project", "dir", "helper"}, "hubHost"}` |
| POST | `/api/hosts/{id}/docker/plan` | admin | yes | What a Docker Engine upgrade would do (below). |
| POST | `/api/hosts/{id}/docker/upgrade` | admin | yes | Upgrade Docker Engine. Restarts every container on the host. |
| POST | `/api/hosts/{id}/os/check` | admin | yes | Refresh the host's package lists and list pending OS package updates (below). Installs nothing. |

- **Updates** returns `{"hubVersion", "hubCurrent", "latest": {"version", "commit", "title", "at"}, "checkedAt", "error"?, "dockerLatest", "hubManaged", "hosts": [{"id", "name", "online", "version", "docker", "dockerBehind", "dockerStatus", "dockerCandidate", "dockerManager", "os", "current", "hubHost", "canUpdate", "cgroupStats", "statsFromDocker", "note"}]}`.
  `dockerBehind` compares with Docker's newest release; `dockerStatus` says what an upgrade can install: `available` (the host's repository has `dockerCandidate`), `not-in-repo` (newer release, not in the `dockerManager` repository yet), `released` (newer release, repository not asked, or not visible to TainerHub), or `current`. It comes from the host's last **Docker plan**, kept for 12 hours.
- **Docker plan** returns `{"manager", "installed", "candidate", "systemd", "supported", "available", "reason", "command", "refresh_failed", "refresh_error"}`. `refresh_failed` means the host couldn't refresh its package lists, so `candidate` may be out of date.
- **OS check** returns `{"os", "kernel", "manager", "packages": [{"name", "from", "to", "origin", "security"}], "security", "reboot_required", "reboot_packages", "refresh_failed", "refresh_error", "checkedAt"}`. `manager` is `apt`, `dnf`, `yum` or `unknown`; `from` is empty for a package that isn't installed yet (a new kernel). **Updates** includes the host's last answer as `os` (absent until checked; kept until the next check or a hub restart). Agents from before this check answer `502`.
- **Docker upgrade** body: `{"confirm": "<the host's exact name>"}`. Returns `202` with the plan.

### Automatic updates

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/auto-updates` | admin | yes | `{"targets", "window", "run"}`: what's opted in, the window, and the current or last run. |
| GET | `/api/auto-updates/settings` | admin | yes | The window: `{"settings", "zone", "next", "nextText"}`. |
| PUT | `/api/auto-updates/settings` | admin | **no** | Change the window. Body: `enabled`, `days`, `time`, `timeZone`, `maxMinutes`. Returns the same as GET. |
| POST | `/api/auto-updates/run` | admin | **no** | Run now (even while paused). `202 {"runId"}`; `409 {"error", "runId"}` while a run is going. |
| GET | `/api/auto-updates/runs/latest` | admin | yes | `{"run"}`: the current or last run since the hub started, or `null`. |
| PUT | `/api/stacks/{sid}/auto-update` | admin | **no** | Opt a managed stack in or out. Body: `{"enabled": true\|false, "autoRollback": true\|false}` (`autoRollback` optional). Returns `{"enabled", "autoRollback"}`. |
| PUT | `/api/hosts/{id}/containers/{name}/auto-update` | admin | **no** | Opt a standalone container in or out, by name. Body: `{"enabled": true\|false}`. |

- **Settings:** `enabled` (false pauses every scheduled window), `days` (`mon` … `sun`, at least one), `time` (`HH:MM`, 24-hour), `timeZone` (an IANA name like `Europe/Berlin`; empty for the hub's local time), `maxMinutes` (15 to 720; default 120). The default is every day at 03:00 for 2 hours. `400` with the reason when something is invalid.
- **Target:** `id`, `kind` (`stack` or `container`), `stackId`, `stackName`, `containerName`, `hostId`, `hostName`, `enabledBy`, `createdAt`, `lastRunAt`, `lastResult` (`ok`, `rolled_back`, `failed`, `skipped`, or empty before the first run), `lastError` (the error, or why it was skipped), `autoRollback` (stacks: roll back right away when the update fails; on by default).
- **Run:** `id`, `trigger` (`window` or `manual`), `startedBy`, `startedAt`, `deadline`, `finishedAt`, `running`, `error`, `outcomes`: `[{"targetId", "kind", "name", "stackId", "hostId", "host", "result", "reason", "images", "oldImage", "newImage", "jobId", "rollbackJobId", "at"}]`. `result` is `rolled_back` when a stack's failed update was rolled back automatically (`rollbackJobId` is the rollback's job) or a container's recreate put the old container back; a rollback that failed leaves `failed` with `rollback failed: …` in `reason`.
- **Opting in** a container needs its host online: `404` if there's no container of that name, `409` if it belongs to a compose project (opt its stack in instead) or is one of TainerHub's own, `400` if its image is pinned by digest, `503` if the host is offline. Opting out always works. A stack that is TainerHub's own project is refused with `409`.
- All changes need a recent sign-in (`403 {"reauth": true}` otherwise) and refuse API tokens.

### Users and invites

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/users` | admin | yes | `{"users": [...], "invites": [...]}` |
| POST | `/api/invites` | admin | **no** | Create an invite link. |
| DELETE | `/api/invites/{iid}` | admin | yes | Revoke a pending invite. `204`. |
| PUT | `/api/users/{uid}/role` | admin | **no** | Change a role. |
| DELETE | `/api/users/{uid}` | admin | **no** | Remove a user. |
| GET | `/api/tokens` | admin | yes | API tokens of users with a lower role, and your own (the owner sees all). |
| DELETE | `/api/tokens/{id}` | admin | **no** | Revoke one of those tokens. |

- **User:** `id`, `username`, `displayName`, `role` (`viewer`, `operator`, `admin`, `owner`), `createdAt`, `hasPassword`, `manageable` (you may change or remove them). **Invite:** `id`, `role`, `note`, `createdBy`, `createdAt`, `expiresAt`, `usedAt`.
- You can't manage yourself, the owner, or anyone of your rank or above, nor give a role above your own.

### Audit

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/audit` | admin | yes | Newest first. Query: `limit` (default 200, at most 1000). |

Each entry: `{"id", "at", "userId", "actor", "action", "target", "detail", "ip"}`. `action` is a dotted name such as `login`, `container.restart`, `stack.updated` or `host.renamed`; `detail` is an object that varies by action.

### Settings

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/settings/https` | admin | yes | `{"publicURL", "trustedProxies", "lockedByEnv", "passkeys", "httpPort", "viaHTTPS"}` |
| POST | `/api/settings/https/test` | admin | yes | Test an address through your proxy. Body: `url`. |
| PUT | `/api/settings/https` | admin | **no** | Turn HTTPS on. Body: `url`, `trusted` (comma-separated IPs or CIDRs), `force`. |
| DELETE | `/api/settings/https` | admin | **no** | Turn HTTPS off. |

Both changes answer `409` when the address is set by `TAINERHUB_PUBLIC_URL`. See [HTTPS and passkeys](https-and-passkeys.md).

#### Container registries

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/registries` | admin | yes | Saved logins: `id`, `registry`, `username`, `passwordSet`, `createdBy`, `createdAt`, `updatedAt`, `lastUsedAt`. Passwords are never returned. |
| POST | `/api/registries` | admin | **no** | Add a login. Body: `registry` (e.g. `ghcr.io`, `myregistry.lan:5000`), `username`, `password`. Needs a recent sign-in. |
| PUT | `/api/registries/{id}` | admin | **no** | Change `username` and/or `password` (empty keeps the saved one). Needs a recent sign-in. |
| DELETE | `/api/registries/{id}` | admin | **no** | Remove a login. Needs a recent sign-in. |
| POST | `/api/registries/{id}/test` | admin | **no** | Log in from a host to check it. Body: optional `hostId`. `{"ok", "host", "status"}` or an error. |

See [Private registries](features.md#private-registries).

### Account

| Method | Path | Role | Token | Notes |
| --- | --- | --- | --- | --- |
| GET | `/api/me` | viewer | yes | `{"user", "passkeys", "recoveryCodesLeft", "hasPassword", "passkeysEnabled", "publicURL"}`. With a token, `user` is the token's owner. |
| POST | `/api/me/password` | viewer | **no** | Body: `current`, `new`. Signs out other sessions. `204`. |
| POST | `/api/me/passkeys/begin` | viewer | **no** | Add a passkey (UI only). |
| POST | `/api/me/passkeys/finish` | viewer | **no** | " |
| DELETE | `/api/me/passkeys/{id}` | viewer | **no** | Remove a passkey. |
| POST | `/api/me/recovery-codes` | viewer | **no** | New recovery codes. |
| POST | `/api/me/sessions/sign-out-others` | viewer | **no** | `{"signedOut": N}` |
| GET | `/api/me/tokens` | viewer | yes | Your API tokens (below). Never includes the secret. |
| POST | `/api/me/tokens` | viewer | **no** | Create a token. Needs a recent sign-in. `201` with the token and `token`, the secret, in this response only. |
| DELETE | `/api/me/tokens/{id}` | viewer | **no** | Revoke one of your tokens. |

- **API token:** `id`, `name`, `prefix` (the start of the secret, to tell tokens apart), `role` (its access level), `createdAt`, `expiresAt` (`null`: never), `lastUsedAt`, `lastUsedIp`, `userId`. `GET /api/tokens` adds `username` and `userRole`.
- `POST /api/me/tokens` body: `name` (up to 60 characters), `role` (`viewer`, `operator`, `admin` or `owner`, up to your own), `expiresInDays` (1 to 3650; the UI offers 30, 90 or 365; `0` for never).

Passkey and recovery-code changes, `POST /api/me/tokens`, adding a host, issuing a join token, invites, role changes, removing users and HTTPS changes need a sign-in from the last 10 minutes; otherwise they answer `403 {"error": ..., "reauth": true}`. See [the fresh sign-in rule](features.md#the-fresh-sign-in-rule).

## Recipes

All recipes assume `TH` and `TOKEN` are set as in the [quick start](#quick-start). For brevity:

```sh
th() { curl -sf -H "Authorization: Bearer $TOKEN" "$@"; }
```

### List unhealthy containers (viewer)

```sh
th "$TH/api/containers" | jq -r '
  .[] | select((.status | contains("(unhealthy)")) or .state == "restarting")
  | "\(.hostName)\t\(.name)\t\(.status)"'
```

Stopped containers too:

```sh
th "$TH/api/containers" | jq -r '.[] | select(.state != "running") | "\(.hostName)\t\(.name)\t\(.state)"'
```

### Restart a container by name (operator)

Look up the host's ID, then use the container's name directly:

```sh
HOST=$(th "$TH/api/hosts" | jq -r '.[] | select(.name == "nas") | .id')
th -X POST "$TH/api/hosts/$HOST/containers/jellyfin/restart" && echo restarted
```

### Pull and redeploy a stack, and follow its output (operator)

```sh
SID=$(th "$TH/api/stacks" | jq -r '.[] | select(.name == "media" and .hostName == "server-1") | .id')
JOB=$(th -X POST "$TH/api/stacks/$SID/pull" | jq -r .jobId)
curl -sN -H "Authorization: Bearer $TOKEN" "$TH/api/jobs/$JOB"
```

The stream prints `event: output` lines as compose runs and ends with `event: done` and `{"ok":true,"error":""}`. Use `deploy` instead of `pull` to redeploy without pulling.

### Tail a container's logs (viewer)

The last 100 lines, then follow:

```sh
curl -sN -H "Authorization: Bearer $TOKEN" \
  "$TH/api/hosts/$HOST/containers/jellyfin/logs?tail=100" |
  sed -un 's/^data: //p' | jq -rj --unbuffered .
```

Add `&follow=0` to print the history and stop. The `end` event's `{}` prints as an empty object at the end.

### Watch fleet events (viewer)

Print a line each time a container's state changes:

```sh
curl -sN -H "Authorization: Bearer $TOKEN" "$TH/api/events" |
  sed -un 's/^data: //p' |
  jq -r --unbuffered '.containers[] | "\(.hostName)/\(.name) \(.state)"'
```

Each `fleet` event carries the whole fleet, so for change notifications compare with the previous event, or read `activity`, which lists recent Docker events and user actions.

### Check for TainerHub updates (admin)

```sh
th "$TH/api/updates" | jq '{hubVersion, hubCurrent, latest: .latest.version, dockerLatest,
  behind: [.hosts[] | select(.current | not) | .name]}'
```

Use `POST /api/updates/check` to ask GitHub now instead of using the cached answer.

### Python

Restarts every unhealthy container, using only the standard library:

```python
import json, os, urllib.request

TH = os.environ["TH"]          # e.g. http://192.168.1.10:8585
TOKEN = os.environ["TOKEN"]    # an operator token


def call(method, path):
    req = urllib.request.Request(TH + path, method=method,
                                 headers={"Authorization": "Bearer " + TOKEN})
    with urllib.request.urlopen(req, timeout=90) as resp:
        body = resp.read()
        return json.loads(body) if body else None


for c in call("GET", "/api/containers"):
    if "(unhealthy)" in c["status"]:
        print(f"restarting {c['hostName']}/{c['name']}")
        call("POST", f"/api/hosts/{c['hostId']}/containers/{c['id']}/restart")
```

On an error, `urlopen` raises `urllib.error.HTTPError`; `json.load(err)` gives you the `{"error": ...}` body.

## Security notes

- **Treat a token like a password.** Anyone who has it can do what it allows until it expires or you revoke it. Don't commit it to Git, paste it into chats, or put it in a URL. Keep it in an environment variable or your tool's secret store.
- **Use the least access that works.** A dashboard or monitoring check needs only **viewer**. Use **operator** for restart and redeploy scripts, and **admin** only when a script needs admin endpoints.
- **Set an expiry.** Prefer 30 or 90 days, and use **never** only for tokens you track.
- **Use HTTPS when you're not on a trusted LAN.** Over plain HTTP the token crosses the network readable. Call `https://tainerhub.example.com` behind your reverse proxy (see [HTTPS and passkeys](https-and-passkeys.md)).
- **Revoke a token as soon as it may have leaked**, on the Account page (or, for admins, the Users page). Then check the **Audit** log for actions under the token's name.
- **Tokens survive password changes and "Sign out other sessions".** Those end browser sessions only. If you think your account is compromised, revoke your tokens separately. Removing a user removes their tokens.

See also [Security](security.md) for TainerHub's overall security model.
