# Guide: a download stack on your NAS, behind a VPN

This walks through a common homelab setup from start to finish: **qBittorrent, Prowlarr, Sonarr and Radarr** (and FlareSolverr if your indexers need it), with all their internet traffic going through a VPN through [gluetun](https://github.com/qdm12/gluetun), and the files on your NAS over SMB. The VPN steps use NordVPN as the example; gluetun supports many other providers, and only steps 2 and 3 and the `VPN_…`/`SERVER_…` settings differ (see gluetun's wiki for yours).

Each piece is simple. What makes it hard is doing them in the wrong order, or one small setting being off. So the steps below are in the order that works, with a check after each one. Don't skip the checks: every problem in the [table at the end](#when-something-goes-wrong) was found by one.

The examples use a NAS at `192.168.10.20` on the LAN `192.168.10.0/24`, and UID/GID `1000`. Replace them with your own values.

- [What you'll end up with](#what-youll-end-up-with)
- [Before you start](#before-you-start)
- [Step 1: mount the NAS shares](#step-1-mount-the-nas-shares)
- [Step 2: get the VPN key](#step-2-get-the-vpn-key)
- [Step 3: give gluetun a fresh server list](#step-3-give-gluetun-a-fresh-server-list)
- [Step 4: create the stack](#step-4-create-the-stack)
- [Step 5: deploy and check the VPN](#step-5-deploy-and-check-the-vpn)
- [Step 6: set up the apps, in this order](#step-6-set-up-the-apps-in-this-order)
- [When something goes wrong](#when-something-goes-wrong)
- [Reaching it from your phone](#reaching-it-from-your-phone)

## What you'll end up with

```
            internet (only through the VPN tunnel)
                          ▲
                      ┌───┴────┐
 LAN browser ───────▶ │gluetun │  ports 8080 9696 8989 7878 published here
                      └───┬────┘
      one shared network: the apps reach each other at localhost
   ┌─────────────┬──────────┴──┬────────────┬──────────────┐
 qBittorrent   Prowlarr      Sonarr       Radarr       FlareSolverr
      │                        │            │
      └──────── /data ─────────┴────────────┘   one SMB share on the NAS,
                                                 mounted by the Docker host
```

- **One stack**, so TainerHub updates, rolls back and restarts the apps together with gluetun. On their own, they'd lose all networking whenever gluetun restarted.
- **The VPN is a kill switch**: if the tunnel drops, the apps have no internet at all, rather than going out unprotected.
- **The NAS is reached over your LAN**, not through the VPN: the Docker host mounts the share, not the containers.

## Before you start

Check these first. Each one has stopped this setup before.

| Check | Why | How |
| --- | --- | --- |
| **The host is a VM or a real machine**, or an LXC container with `/dev/net/tun` passed through | gluetun needs the TUN device to build the tunnel | `ls -l /dev/net/tun` on the host shows a line starting with `crw`. In an LXC container, pass the device through in the container's settings (on Proxmox: `dev0: /dev/net/tun`). A VM needs nothing. |
| **A NAS user for the apps, with read/write** on the share(s) | the apps download, import and rename files | in your NAS's shared folder permissions. Give it its own password: Docker keeps it in plain text on the host (see [Security](security.md#network-share-passwords)). |
| **One share for downloads and media**, if you can | Sonarr and Radarr then *move* finished files instantly, instead of copying them | a share (say `data`) with `downloads` and `media` folders in it. Two separate shares work too, but every import is a full copy and takes twice the space until the torrent is removed. |
| **The ports are free** on the host | a port in use stops the stack | the host card on Hosts lists every port in use. This guide uses 8080, 9696, 8989 and 7878. |
| **Your VPN account** | for the key in step 2 | NordVPN: you'll make an access token in your account (Advanced settings → Access token). |

## Step 1: mount the NAS shares

In TainerHub: **Resources → Volumes → New network share**.

1. **Host:** the host that will run the stack. A share volume exists only on the host it was made on.
2. **SMB / CIFS.** **Server:** `192.168.10.20` (an IP address is safest). **Share name:** `data`. You can also paste a whole address like `\\192.168.10.20\data` into either field.
3. **Username / Password:** the NAS user from above.
4. **Files owned by UID / GID: `1000` / `1000`.** This must match the `PUID`/`PGID` the apps run as (step 4). Inside the containers the share's files appear to belong to this user; any other number and the apps can read but not write.
5. **Leave Read-only unticked.** A read-only share can't be fixed later without deleting and recreating the volume.
6. **Volume name:** `data`. The compose file refers to it by this exact name; capitals matter.
7. **Test and create.** TainerHub mounts it once, lists it, and writes and removes a test file as UID 1000.

**Check:** the success message says *writable*. If it warns that files can't be saved, the NAS user only has read access to the share: give it read/write on the NAS, nothing to change in TainerHub. If the host can't mount SMB shares at all, the form offers to install `cifs-utils` for you.

In the volume list the share shows as **SMB //192.168.10.20/data**. If it also says **read-only**, delete it and create it again without Read-only.

## Step 2: get the VPN key

gluetun connects to NordVPN with WireGuard, which needs your **NordLynx private key**. It is not the access token, and not your NordVPN password: you use the access token once, to fetch the key.

1. In your NordVPN account: **Advanced settings → Access token → Generate new token**. Copy it right away; it's shown only once.
2. On the Docker host (any Linux or Mac shell works), run this. It asks for the token without showing it and prints only the key:

   ```bash
   read -rs -p "NordVPN access token: " T; echo; curl -s -u "token:$T" https://api.nordvpn.com/v1/users/services/credentials | grep -o '"nordlynx_private_key": *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/'; unset T
   ```

   On a Mac's default shell (zsh), replace `read -rs -p "NordVPN access token: " T` with `printf "NordVPN access token: "; read -rs T`.

**Check:** it prints one line of about 44 characters ending in `=`. That's the key for `.env` in step 4. If it prints nothing, run it without everything after `credentials` to see NordVPN's answer: an error means the token was mistyped or revoked.

Treat the key like a password: don't paste it into chats, issues or screenshots. Once the VPN works, revoke the access token.

## Step 3: give gluetun a fresh server list

gluetun ships with a built-in list of NordVPN servers from when its image was built. NordVPN retires and renumbers servers often, so after a few months many entries point at addresses that no longer answer, and gluetun keeps cycling through dead servers. Fetch the current list once, into the stack's folder (`media` is the stack name you'll use in step 4):

```bash
mkdir -p /opt/tainerhub/stacks/media/gluetun && docker run --rm -v /opt/tainerhub/stacks/media/gluetun:/gluetun qmcgaw/gluetun:v3 update -enduser -providers nordvpn
```

Warnings like `no region found for server …` are harmless.

**Check:** `ls -l /opt/tainerhub/stacks/media/gluetun` shows a `servers.json` of several MB. The compose file mounts this folder, so gluetun uses it. Run the command again whenever gluetun starts timing out on every server.

## Step 4: create the stack

**Stacks → New stack**, on the same host as the share, named `media`.

`compose.yaml`:

```yaml
services:
  gluetun:
    image: qmcgaw/gluetun:v3
    cap_add: [NET_ADMIN]
    devices:
      - /dev/net/tun:/dev/net/tun
    environment:
      VPN_SERVICE_PROVIDER: nordvpn
      VPN_TYPE: wireguard
      WIREGUARD_PRIVATE_KEY: ${NORDVPN_PRIVATE_KEY}
      SERVER_COUNTRIES: ${NORDVPN_COUNTRY}
      SERVER_CITIES: ${NORDVPN_CITY}
      FIREWALL_OUTBOUND_SUBNETS: ${LAN_SUBNETS}
      TZ: ${TZ}
    volumes:
      - ./gluetun:/gluetun          # the server list from step 3
    ports:
      - "8080:8080"   # qBittorrent
      - "9696:9696"   # Prowlarr
      - "8989:8989"   # Sonarr
      - "7878:7878"   # Radarr
    restart: unless-stopped

  qbittorrent:
    image: lscr.io/linuxserver/qbittorrent:latest
    network_mode: "service:gluetun"
    depends_on:
      gluetun:
        condition: service_healthy
    environment:
      PUID: ${PUID}
      PGID: ${PGID}
      TZ: ${TZ}
      WEBUI_PORT: 8080              # must equal the published port above
    volumes:
      - ./config/qbittorrent:/config
      - data:/data
    restart: unless-stopped

  prowlarr:
    image: lscr.io/linuxserver/prowlarr:latest
    network_mode: "service:gluetun"
    depends_on:
      gluetun:
        condition: service_healthy
    environment:
      PUID: ${PUID}
      PGID: ${PGID}
      TZ: ${TZ}
    volumes:
      - ./config/prowlarr:/config
    restart: unless-stopped

  sonarr:
    image: lscr.io/linuxserver/sonarr:latest
    network_mode: "service:gluetun"
    depends_on:
      gluetun:
        condition: service_healthy
    environment:
      PUID: ${PUID}
      PGID: ${PGID}
      TZ: ${TZ}
    volumes:
      - ./config/sonarr:/config
      - data:/data
    restart: unless-stopped

  radarr:
    image: lscr.io/linuxserver/radarr:latest
    network_mode: "service:gluetun"
    depends_on:
      gluetun:
        condition: service_healthy
    environment:
      PUID: ${PUID}
      PGID: ${PGID}
      TZ: ${TZ}
    volumes:
      - ./config/radarr:/config
      - data:/data
    restart: unless-stopped

  # Only if an indexer is behind Cloudflare. It must share the VPN address
  # with Prowlarr, so it runs in gluetun's network too; nothing to publish.
  flaresolverr:
    image: ghcr.io/flaresolverr/flaresolverr:latest
    network_mode: "service:gluetun"
    depends_on:
      gluetun:
        condition: service_healthy
    environment:
      LOG_LEVEL: info
      TZ: ${TZ}
    restart: unless-stopped

volumes:
  data:
    external: true                  # the share volume from step 1
```

`.env`:

```dotenv
# The NordLynx private key from step 2 (~44 characters, ending in =). Not the access token.
NORDVPN_PRIVATE_KEY=<your NordLynx private key>
# Full names, as gluetun lists them (e.g. "United States", not "USA").
# Remove the city line, and SERVER_CITIES in compose.yaml, to use any server in the country.
NORDVPN_COUNTRY=<a country, e.g. Netherlands>
NORDVPN_CITY=<a city in it, e.g. Amsterdam>
# Your LAN, and any other range that opens the web UIs (another VLAN, Tailscale's 100.64.0.0/10)
LAN_SUBNETS=192.168.10.0/24
TZ=<your time zone, e.g. Europe/Amsterdam>
# Must equal "Files owned by UID / GID" on the share volume
PUID=1000
PGID=1000
```

Why it's written this way:

- **Every app's port is on `gluetun`.** An app in gluetun's network can't publish its own ports; TainerHub's check blocks that.
- **App settings (`./config/...`) stay on the host**, not on the NAS. The apps keep SQLite databases, which get corrupted on network shares.
- **One `/data` mount for all three**: qBittorrent saves to `/data/downloads`, and Sonarr and Radarr see the file at that same path and move it to `/data/media` on the NAS. No remote path mappings needed.
- **If a port is taken** (the check before deploying says by what), change it in **both** places for qBittorrent, e.g. `"8090:8090"` and `WEBUI_PORT: 8090`. qBittorrent's web page refuses logins when the two differ. For the other apps, change only the left-hand number.

With two separate shares (say `downloads` and `media`), mount them as `downloads:/downloads` in qBittorrent, Sonarr and Radarr, and `media:/media` in Sonarr and Radarr. Sonarr and Radarr need the downloads share too, at the same path as qBittorrent, to import from it.

## Step 5: deploy and check the VPN

Click **Save & deploy**. The check before deploying may remind you that gluetun uses `/dev/net/tun`; you checked that already.

**Check 1, gluetun's log** (Containers → `media-gluetun-1` → Logs). Within about 30 seconds it should show `Public IP address is …` in the country you chose, then report healthy. The other apps start only after that.

**Check 2, the address the apps use.** Open a **Terminal** on the qbittorrent container:

```bash
curl -s https://ipinfo.io
```

It should show the VPN server's city and a hosting company, not your home address.

If gluetun keeps restarting instead, see [When something goes wrong](#when-something-goes-wrong). A container stuck restarting has **Stop** on its row, and the stack has **Stop** and **Down**, so you can halt the loop while you fix `.env`.

## Step 6: set up the apps, in this order

The order matters because each app needs something from the one before it. All of them are at `http://<host address>:<port>`. Inside the stack they reach each other at `localhost`.

### 1. qBittorrent (`:8080`)

1. Sign in as `admin` with the temporary password from its **Logs** in TainerHub. It changes on every restart, so set your own under **Options → WebUI**.
2. Under **Options → WebUI → Authentication**, tick **Bypass authentication for clients on localhost**. In this stack "localhost" means only the other apps in gluetun's network, and it saves typing the password into each of them (where a browser's password manager tends to fill in the wrong one).
3. Under **Options → Downloads**:
   - **Default Torrent Management Mode: Automatic**, so Sonarr's and Radarr's categories get their own folders.
   - **Default Save Path:** `/data/downloads`. Optionally, incomplete torrents in `/data/downloads/incomplete`.
4. Under **Options → BitTorrent → Seeding Limits**, decide how long to seed, for example a ratio of 1 or 60 minutes, then **Remove torrent**. Finished torrents waiting for a seeding slot show as *Queued*; that's normal, and Sonarr and Radarr import them anyway.

**Check:** a **Terminal** on qbittorrent can write to the share:

```bash
touch /data/downloads/test && ls -l /data/downloads/test && rm /data/downloads/test
```

### 2. Sonarr (`:8989`) and Radarr (`:7878`)

1. **The first page asks you to set up authentication.** Pick *Forms (Login Page)* and *Disabled for Local Addresses* (your home network is then let in without a password), and set a username and password. **Leave Allowed Hosts empty**: it doesn't take subnets, and anything in it that isn't a host name blocks saving. If Save still stays grey, see the table below.
2. **Settings → Media Management → Root Folders:** `/data/media/tv` in Sonarr, `/data/media/movies` in Radarr. If it says the folder *is not writable by user 'abc'*, see the table.
3. **Settings → Download Clients → + → qBittorrent:** host `localhost`, port `8080`, username and password empty (thanks to the bypass above), category `tv` in Sonarr and `movies` in Radarr. **Test** should go green.
4. **Settings → General:** copy the **API Key** for Prowlarr.

If you change a security setting and the pages start missing pictures (blank posters), sign out and back in.

### 3. Prowlarr (`:9696`)

1. Set up authentication as for Sonarr.
2. **Settings → Apps → +:** add **Sonarr** (`http://localhost:8989`) and **Radarr** (`http://localhost:7878`) with their API keys, Prowlarr server `http://localhost:9696`, Sync Level *Full Sync*.
3. **Indexers → Add Indexer** for each site you use. They appear in Sonarr and Radarr by themselves, marked *(Prowlarr)*; edit them in Prowlarr only.
4. **For indexers behind Cloudflare:** **Settings → Indexers → +** under Indexer Proxies → **FlareSolverr**, host `http://localhost:8191/`, tag `flaresolverr`. A proxy without a tag shows as *Disabled*, which only means no indexer uses it yet: add the same tag to the indexers that need it.

## When something goes wrong

| What you see | Cause | Fix |
| --- | --- | --- |
| **New network share:** "this host can't mount SMB shares" or "rejected the mount" | `cifs-utils` (or `nfs-common`) missing on the host | click **Install cifs-utils on &lt;host&gt; and try again** in the form |
| **New network share:** created, but "files can't be saved" | the NAS user has read-only access to the share | give it read/write on the NAS; nothing to change in TainerHub |
| Volume list shows the share as **read-only**, apps get *Read-only file system* | Read-only was ticked when creating it | bring the stack **Down**, delete the volume, create it again unticked with the same name, deploy |
| Deploy: `external volume "data" not found` | the share volume is missing on this host, or named differently (capitals count) | `docker volume ls` on the host; create it on the right host with the exact name |
| Deploy: `dependency failed to start: container …gluetun… is unhealthy` | gluetun couldn't build the tunnel; its log says why | read gluetun's log and find the line in the rows below |
| gluetun log: `the value "USA" is not one of the possible choices` | country or city name not as gluetun spells it | use the full name, e.g. `United States`; the error lists the valid ones |
| gluetun log: key errors, `illegal base64` | the access token (or a typo) instead of the NordLynx key | redo [step 2](#step-2-get-the-vpn-key); the key is about 44 characters ending in `=` |
| gluetun log: `Connecting to …:51820`, then `lookup cloudflare.com: i/o timeout`, over and over with different servers | gluetun's built-in server list is out of date (dead servers) | [step 3](#step-3-give-gluetun-a-fresh-server-list), then deploy again |
| gluetun log (OpenVPN): `connect to …:443 failed: Host is unreachable` | same: a server that no longer exists | step 3 |
| Still timing out with a fresh list | something blocks the VPN on the way out | `docker run --rm alpine traceroute -n 1.2.3.4` with a server address from the log shows where it stops: your router (a "block VPN" or threat-protection feature), or a firewall on the hypervisor |
| The check before deploying: *gluetun uses /dev/net/tun* | a reminder; TainerHub can't see host devices | `ls -l /dev/net/tun` on the host; on LXC pass it through (see [Before you start](#before-you-start)) |
| The check before deploying: *Port 8080 is already in use by …* | another container has it | change the port as described in [step 4](#step-4-create-the-stack) |
| qBittorrent page loads but logins or buttons fail | `WEBUI_PORT` differs from the published port | make both sides and `WEBUI_PORT` the same number |
| Sonarr/Radarr: *Unable to connect to qBittorrent* | no login sent | the localhost bypass in qBittorrent, or fill in its username and password |
| Works in Radarr, *Authentication Failure* in Sonarr | the browser filled in Sonarr's own saved password | type it by hand, or use the localhost bypass; restart qBittorrent if it banned localhost after failed tries |
| Root folder: *is not writable by user 'abc'* | share mounted read-only, wrong UID, or NAS user without write access | `touch /data/media/test` in the app's Terminal: *Read-only file system* → recreate the volume unticked; *Permission denied* → fix the NAS permissions or the UID |
| Authentication page won't save | something in **Allowed Hosts**, or the password fields don't match | empty Allowed Hosts; retype both passwords by hand. Last resort: stop the app and set `<AuthenticationMethod>Forms</AuthenticationMethod>` and `<AuthenticationRequired>DisabledForLocalAddresses</AuthenticationRequired>` in `config/<app>/config.xml` |
| Sonarr/Radarr show blank posters | an old login session after changing security settings | sign out and back in |
| An app's settings are broken | — | stop it, then fix `config/<app>/config.xml`, restore a zip from `config/<app>/Backups/`, or move the folder aside to start fresh |

## Reaching it from your phone

On your home network, any browser works with `http://<host address>:<port>`, and so do phone apps for Sonarr and Radarr, which ask for each app's address and API key.

Away from home, don't open these ports to the internet. Use a VPN into your network instead, such as Tailscale on the Docker host, and add its range to `LAN_SUBNETS` in `.env` (for example `192.168.10.0/24,100.64.0.0/10`), then redeploy. Without that, gluetun sends the replies into the VPN tunnel and the page times out.
