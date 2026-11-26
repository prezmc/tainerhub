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

OS updates installed from TainerHub run the same way:

```bash
journalctl -u tainerhub-os-upgrade
cat /var/log/tainerhub-os-upgrade.log
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

## The install command fails with "curl: (22) ... 403"

TainerHub never answers the install link with 403: a bad or used token gets a short script that prints the reason. A bare `403` comes from something in front of the hub, almost always your reverse proxy. The command downloads the installer from the address your browser uses, so if you opened TainerHub at `https://tainerhub.example.com`, the new server asks Nginx Proxy Manager. An NPM **Access List** that allows only your LAN range (as suggested in [Keeping it LAN-only](https-and-passkeys.md#keeping-it-lan-only)) refuses a server on another subnet, VLAN or VPN.

Pick one:

- **Download from the hub directly.** In the **Add host** dialog, under **Download the installer from**, choose **the hub directly** (the default since this option was added). The command then uses `http://<hub address for agents>:8585`, which skips the proxy. The new server must be able to reach port 8585 on the hub (check any `DOCKER-USER` rule you added).
- **Allow the server's network in the proxy.** In NPM, edit the Access List and add the server's subnet (for example `192.168.20.0/24`) under **Allow**.
- **Set it up by hand.** Open **Set it up by hand instead** in the dialog, save the compose file on the server and run `docker compose up -d`. This needs no download from the hub.

The agent itself always connects to port 7443 directly, so the proxy never affects it after the install.

## The agent can't reach the hub: "connection refused" on port 7443

The installer says "The agent didn't reach the hub ... within 90 seconds", and the agent's log shows `dial tcp <IP>:7443: connect: connection refused`. Most often, **Hub address for agents** was your reverse proxy's HTTPS name (`tainerhub.example.com:7443`), so the agent called the proxy, which doesn't listen on 7443. Agents use mutual TLS and must reach the hub server itself. Current versions refuse the proxy's name in that field.

The token wasn't used, because the agent never enrolled. To fix it on the new server, put the hub's own address into the agent's compose file and start it again:

```bash
cd ~/tainerhub-agent && sed -i 's/tainerhub.example.com:7443/192.168.1.10:7443/' compose.yml && docker compose up -d
```

Or remove the agent (`cd ~/tainerhub-agent && docker compose down -v`) and use **New join token** with the right address. If the address is right and the connection is still refused, check that the hub publishes port 7443 and that no firewall between the networks blocks it.

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

## "Sign in again to continue"

Some changes need a sign-in from the last 10 minutes: passkeys, recovery codes and setting a first password; creating an API token; invites, role changes and removing users; adding a host or making a new join token; and turning HTTPS on or off. On the Account page this shows as a card, elsewhere as a dialog.

Click **Sign in again**. TainerHub signs you out, shows the sign-in page, and brings you back to the page you were on. Then try once more. **Not now** closes the dialog without changing anything.

If it keeps asking right after you signed in, check that you signed in on the same address you're using. The LAN address and the `https://` address have separate sessions.

See [Features: the fresh sign-in rule](features.md#the-fresh-sign-in-rule) for why.

## Logs won't open: "too many log streams"

Each person can have 4 log streams open per host and 8 in all. Every open logs drawer counts, in every tab and on every device, and so does a script following logs with your API token. Close logs drawers and tabs you don't need, and stop scripts that follow logs, then try again.

"too many open log streams for this host" means everyone together has 16 open on that host. Ask others to close theirs, or wait.

A logs drawer, terminal or live view can also close by itself about a minute after your role was lowered, your account removed, or you were signed out elsewhere. Reload the page.

## API requests fail with 401, 403 or 429

| Answer | What it means |
| --- | --- |
| `401` "invalid or expired API token" | The token is wrong, revoked or expired. Check for a missing `thp_` or a stray space or newline. Create a new token on **Account & sign-in → API tokens**. |
| `429` "too many failed attempts; wait 15 minutes" | More than 20 bad tokens came from your IP address within 15 minutes. Fix the token in your script, then wait. Valid tokens keep working meanwhile. |
| `429` "you have too many log streams open..." | See [Logs won't open](#logs-wont-open-too-many-log-streams). |
| `403` "not available with an API token; sign in to the web UI" | Tokens can't invite users, change roles, change sign-in settings, add hosts, change HTTPS or manage tokens. Do it in the browser. |
| `403` with another message | The token's access level, or your current role, is too low. A token never acts above its owner's role, so if you were made a viewer, an admin token acts as a viewer. |

Behind a reverse proxy, the bad-token limit counts the visitor's IP only if the proxy is trusted in **Settings** or `TAINERHUB_TRUSTED_PROXIES`. Otherwise every request seems to come from the proxy. See [HTTPS and passkeys, step 4](https-and-passkeys.md#step-4-turn-on-https).

More on tokens and every endpoint is in the [API guide](api.md).

## Too many sign-in attempts

After 10 failed sign-ins from one IP address within 15 minutes, further attempts are refused for a while. Wait up to 15 minutes.

"Too many failed sign-ins for this account" means someone, from any number of addresses, got one account's password or recovery code wrong 10 times in 15 minutes. Sign in with a passkey (not limited), wait up to 15 minutes, or use the LAN address, which is counted separately. If it keeps happening, someone is guessing: turn on **Accounts with a passkey must use it on the secure address** in Settings.

If everyone gets locked out together behind a reverse proxy, the hub may be seeing the proxy's IP for every visitor. Make sure the proxy's IP is set as trusted, in **Settings** or `TAINERHUB_TRUSTED_PROXIES`. See [HTTPS and passkeys, step 4](https-and-passkeys.md#step-4-turn-on-https). Settings shows a warning with the address to trust when it spots this.

## "This account signs in with its passkey on this address"

**Accounts with a passkey must use it on the secure address** is on in Settings, and your account has a passkey. Use **Sign in with passkey**, or a recovery code. On a device without your passkey, use the LAN address, where passwords still work.

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

The "update" badge on *containers* comes from a separate check: the hub has each host ask the image registries every 6 hours (`TAINERHUB_UPDATE_CHECK_HOURS`). If **Check for updates** says "couldn't check" for a host, that host's agent couldn't reach the registry.

## A stack or container wasn't updated automatically

Look at **Settings → Automatic updates**: each opted-in target shows its last result, and **Recent automatic updates** lists the last run's outcome for each, with the reason. Every update tried is also in the **Audit** log (`update.auto_applied`, `update.auto_rolled_back`, `update.auto_failed`). The usual reasons:

| What you see | Why |
| --- | --- |
| It isn't in the **Opted in** list | Automatic updates are off by default. Turn them on with the switch on the stack's page or in the container's details panel. |
| "already up to date" | The registry has no newer image for the tag it uses. A pinned tag (`postgres:16.4`) never changes; use a moving tag (`postgres:16`) if you want updates. |
| "builds: ..." | The stack builds its images, and TainerHub can't tell whether their base images changed. See [Stacks that build images](features.md#automatic-updates). |
| "host offline" | The host's agent wasn't connected during the window. |
| "a job is running for this stack" | A deploy, pull or move was already running; it gets its turn in the next window. |
| "it has saved changes that haven't been deployed" | Deploy (or discard) the edits yourself first: Pull & redeploy would deploy them too. |
| "the window ended first" | Earlier updates used up the window. Make it longer under Settings. |
| "the update check failed" | The host couldn't reach the registry. With a private image, check its login under **Settings → Container registries**. |
| failed: "not running and healthy 2m0s after the update" | The new version didn't come up. Check the container's logs. For a stack with automatic rollback off, use **Roll back** on its page. |
| rolled back: "the update failed (…), so the previous images were put back" | The new version didn't come up, and the stack went back to its old images. Fix the cause, then use **Return to latest** on the stack's page. |
| "it runs a rollback: use Return to latest on its page first" | The stack was rolled back (by hand or after a failed update); automatic updates leave it alone until it returns to the latest images. |
| Nothing ran at all | The window is paused (the switch at the top of the card), or the hub was stopped through the whole window (missed windows aren't caught up). Check the days, time and time zone, or use **Run now**. |

## Rollback says "the previous image is gone"

A rollback needs the images the stack ran before. TainerHub keeps them by tagging them `tainerhub-rollback/<stack>:<service>`, and its own prunes leave them alone, but a `docker image prune -a` (or `docker rmi`) run on the host outside TainerHub removes them like any unused image. The rollback then stops before changing anything, and names the services whose image is missing.

To go back anyway, pin the old version in the compose file (`image: app:1.4.2` instead of `app:1`) and deploy. To keep it from happening, prune from TainerHub's **Resources** page rather than with `docker image prune -a`; `docker images 'tainerhub-rollback/*'` lists the images TainerHub keeps.

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
