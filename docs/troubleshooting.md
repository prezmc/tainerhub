# Troubleshooting

Common problems, and how to fix them. Most answers start with the logs, so that comes first.

## Where are the logs?

On the hub's server:

```bash
cd ~/tainerhub
docker compose logs -f tainerhub            # the hub
docker compose logs -f tainerhub-agent      # this server's agent
docker compose logs -f postgres             # the database
docker compose logs --tail 100              # all three, last 100 lines each
```

On any other managed server:

```bash
docker logs -f tainerhub-agent
```

For more detail, set `TAINERHUB_LOG_LEVEL: debug` in the service's `environment:` and run `docker compose up -d`. Set it back to `info` when you're done.

Docker Engine upgrades started from TainerHub run outside Docker. Their log is on the server itself:

```bash
journalctl -u tainerhub-docker-upgrade
cat /var/log/tainerhub-docker-upgrade.log
```

## A host shows "Agent offline"

The card says **Agent offline. Check docker logs tainerhub-agent on this host.** Do that first:

```bash
docker logs --tail 50 tainerhub-agent
```

Then work through these:

1. **Is the agent running?** `docker ps -a --filter name=tainerhub-agent`. If it keeps restarting, the log says why.
2. **Can the server reach the hub on port 7443?** From the server: `nc -vz 192.168.1.10 7443` (use your hub's address). If not, check firewalls on both machines, and that nothing in between (a reverse proxy, for example) sits on 7443.
3. **Did the hub's address change?** The agent connects to the address in `TAINERHUB_HUB` in `~/tainerhub-agent/compose.yml`. Fix it there and run `docker compose up -d`. Since agents trust the hub's certificate authority rather than a hostname, any address that reaches the hub works.
4. **"certificate revoked or unknown"** in the log: the host was removed in TainerHub. Add it again (see [The join token expired](#the-join-token-expired-or-was-already-used)), deleting the agent's old certificate first.
5. **"the client certificate expired too long ago for the hub to renew it"**: the server was off for more than 180 days after its certificate expired. Re-enroll it with a new join token.
6. **The agent says "disconnected from hub" and retries.** It reconnects by itself with backoff (up to a minute between tries). Dead connections are noticed within 90 seconds.

If the hub's own server shows offline, check the `tainerhub-agent` container in `~/tainerhub`.

A host that never connected shows **Waiting for its agent to connect** instead. Run its install command, or check that it ran successfully.

## The join token expired or was already used

Join tokens work once and expire one hour after they're made. Signs:

- The install command prints "this join link was already used or has expired".
- The agent's log says "join token invalid, used or expired", and the container keeps restarting.

To fix it:

1. In TainerHub, open **Hosts**, the host's `…` menu, and click **New join token**.
2. On the server, remove the old agent first. The installer refuses to run while a `tainerhub-agent` container exists:

   ```bash
   cd ~/tainerhub-agent && docker compose down -v
   ```

   `-v` also deletes the agent's old certificate, if it had one.
3. Paste the new command.

Prefer to edit by hand? Replace the `TAINERHUB_JOIN_TOKEN` value in `~/tainerhub-agent/compose.yml` with the new token (the `thj1_...` part of the new command, or copy it from **Set it up by hand instead**), then run `docker compose up -d`.

Too many failed enrollments from one IP address (10 in 15 minutes) are refused with "too many failed enrollment attempts; try again later". Wait 15 minutes.

## "Stacks unavailable" on a host card

The agent couldn't set up compose stacks on that server. The message after "Stacks unavailable:" says why.

**"the stacks folder is not mounted"** or **"the stacks folder must be bind-mounted from the same path on the host"**

Compose resolves relative paths such as `./data` against the stack's folder, and Docker then looks for that path on the host. So the folder must be mounted at the *same* path inside the agent and on the host. Add this line under the agent's `volumes:` in its compose file:

```yaml
      - /opt/tainerhub/stacks:/opt/tainerhub/stacks
```

Then, in the agent's folder (`~/tainerhub-agent`, or `~/tainerhub` on the hub's server):

```bash
docker compose pull && docker compose up -d
```

Agents installed before stacks existed need this once.

**"The agent on ... is too old for stacks"**: update the agent from the Hosts page, or with `docker compose pull && docker compose up -d`.

**"stacks folder is not writable"**: check that `/opt/tainerhub/stacks` on the host exists and isn't on a read-only filesystem.

**"docker compose is not available"**: the agent image ships its own `docker compose`. Update to the latest agent image.

The agent re-checks each time it's asked, so once the mount is fixed and the agent restarted, the message goes away.

## Passkeys aren't offered

The **Sign in with passkey** button and the **Add** passkey form only appear when all of this is true:

1. **HTTPS is turned on** in **Settings**, or with `TAINERHUB_PUBLIC_URL`. The Settings page shows **passkeys on**.
2. **You're on the secure address**, exactly as configured, for example `https://tainerhub.example.com`. On the LAN address (`http://192.168.1.10:8585`) passwords work but passkeys don't. The Account page links you to the secure address.
3. **Your browser sees the page as secure**: a valid certificate, no warnings clicked through.

Also check:

- You've **added a passkey on this device** (or on a phone or security key you have with you). Passkeys are per device.
- The secure address **hasn't changed** since you added the passkey. Passkeys are tied to the exact address.
- If HTTPS was **turned off** and on again, passkeys are kept and work again once it's back on the same address.

To set HTTPS up, see [HTTPS and passkeys](https-and-passkeys.md).

## "Sign in again to continue" on the Account page

Adding or removing a passkey, generating new recovery codes and setting a first password need a sign-in from the last 10 minutes. Click **Sign in again**, sign in, and try once more.

## Too many sign-in attempts

After 10 failed sign-ins from one IP address within 15 minutes, further attempts are refused for a while. Wait up to 15 minutes.

If everyone gets locked out together behind a reverse proxy, the hub may be seeing the proxy's IP for every visitor. Make sure the proxy's IP is set as trusted, in **Settings** or `TAINERHUB_TRUSTED_PROXIES`. See [HTTPS and passkeys, step 4](https-and-passkeys.md#step-4-turn-on-https).

## The update card shows an error

The Hosts page asks GitHub for the newest TainerHub version. It caches the answer for 15 minutes, and retries 2 minutes after a failure.

| Message | Meaning |
| --- | --- |
| "couldn't reach GitHub to check for updates" | The hub can't reach `api.github.com`. Check the hub server's internet access and DNS. |
| "GitHub answered 403 ... while checking for updates" (or 429) | Usually GitHub's limit for anonymous requests from your IP address. It clears by itself within an hour. |
| "no published version found" | GitHub answered but listed no build. Try again later. |

When updating a host:

| Message | Fix |
| --- | --- |
| "host is offline" | The agent isn't connected. See [Agent offline](#a-host-shows-agent-offline). |
| "this agent is too old to update itself" | Update it once on the server: `docker compose pull && docker compose up -d` in its folder. |
| "this agent wasn't started with docker compose, so it can't update itself" | Update it by hand, or reinstall it with the compose file from **Add host**. |
| "can't find this agent's own container (was its hostname changed?)" | The agent identifies its container by its hostname. Remove any `hostname:` setting from the agent's compose service. |
| "TainerHub didn't come back with a new version within 5 minutes" | Check the hub: `cd ~/tainerhub && docker compose logs`. |
| "This hub was installed without its built-in agent, so it can't update itself" | Run `cd ~/tainerhub && docker compose pull && docker compose up -d` on the hub's server. |

The "update" badge on *containers* comes from a separate check: each agent asks the image registries every 6 hours. If **Check for updates** says "couldn't check" for a host, that host's agent couldn't reach the registry.

## High CPU on a server

TainerHub's agents are designed to be light, but a few things make a difference:

1. **Turn on lighter stats.** On **Hosts**, look at the **Container stats** section of the update card. Hosts marked **from Docker** ask the Docker Engine for CPU and memory, which costs more. Click **Turn on lighter stats** (or **Use lighter stats**). See [Getting started](getting-started.md#7-turn-on-lighter-stats-older-installs).
2. **Close TainerHub tabs you aren't using.** Agents sample stats every 10 seconds while anyone has TainerHub open, and only once a minute when nobody does. A forgotten tab on another computer keeps every agent on the fast rate.
3. **Check whether it's really TainerHub.** `docker stats tainerhub-agent tainerhub` shows their own CPU use.

If it's still high, a CPU profile helps: see [Profiling](#profiling-with-pprof).

## Lost access to the owner account

**If you have a recovery code:** on the sign-in page, click **Use a recovery code**, enter your username and one code, and sign in. TainerHub takes you to **Account & sign-in**. Set a new password, or add a passkey, and click **Generate new codes** to replace the codes you've used.

**If you have a passkey but forgot the password** (or the other way round): sign in with what you have, then set a new password on the Account page.

**If you have none of them:** there's no command-line or email password reset. Another admin can keep managing your servers, but nobody can change the owner account. The only way to get an owner account back is to [start over](#starting-over). Save your recovery codes somewhere safe so this never happens.

## The setup link doesn't work

- The link is valid for **15 minutes**. Get a fresh one with `docker exec tainerhub /tainerhub setup-link`.
- "No setup link: the owner account already exists, or the hub hasn't finished starting": either setup is already done (sign in instead), or wait a few seconds and try again.
- Setup links only exist until the owner account is created.

## The hub doesn't start

Check its log: `cd ~/tainerhub && docker compose logs --tail 50 tainerhub`.

- **"the database refused the password saved in ..."**: the database's password doesn't match the one in the hub's `/data` volume. This happens if you set `DB_PASSWORD` after the first start, or restored only one of the two volumes. Put back the matching `/data` volume or database backup. The saved password is shown by `docker exec tainerhub /tainerhub db-password` while the hub container exists.
- **"TAINERHUB_PUBLIC_URL must be https"**: fix or remove that line in the compose file.
- **The installer said "TainerHub didn't become ready within 2 minutes"**: it printed the last 30 log lines; they say why.

## Starting over

To reset the hub completely, deleting all users, stacks, settings, the audit log and the certificate authority:

```bash
cd ~/tainerhub
docker compose down -v
docker compose up -d
docker exec tainerhub /tainerhub db-password
docker exec tainerhub /tainerhub setup-link
```

There is no undo. Your containers on other servers keep running, but every agent must enroll with the new hub. On each server:

```bash
cd ~/tainerhub-agent && docker compose down -v
```

Then add it again from **Hosts → Add host**. The hub's own server enrolls by itself.

Stack folders in `/opt/tainerhub/stacks` stay on each server. A new stack with the same name and host uses the same folder again.

## Profiling with pprof

For diagnosing CPU or memory use, the hub and the agent can serve Go's profiler. It's off by default. **Don't expose it to your network.**

The address is inside the container, so publish it to the server's loopback only. For example, for an agent, in its compose service:

```yaml
    environment:
      TAINERHUB_PPROF_ADDR: 0.0.0.0:6060
    ports:
      - "127.0.0.1:6060:6060"
```

Run `docker compose up -d`, then on that server:

```bash
curl -o cpu.pprof "http://127.0.0.1:6060/debug/pprof/profile?seconds=30"
```

Open the file with `go tool pprof cpu.pprof`, on any machine with Go installed, or share it with whoever is helping you. Remove both lines and run `docker compose up -d` when you're done.

The same works for the `tainerhub` service on the hub (use a different host port if the agent on that server also has it on).

## Still stuck?

Collect the last 100 lines of the hub's and the agent's logs, the host card's message, and what you clicked. Remove passwords, tokens and IP addresses you don't want to share before posting them anywhere.
