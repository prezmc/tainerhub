#!/bin/sh
# TainerHub hub installer.
#
#   curl -fsSL https://raw.githubusercontent.com/prezmc/tainerhub/main/install.sh | sh
#
# Puts the hub's compose file in ~/tainerhub (or $TAINERHUB_DIR), starts it,
# waits until it's ready, and prints the database password and the one-time
# setup link. Safe to run again: it never overwrites an existing compose.yml,
# and it just reprints the link if setup isn't done yet.
set -eu

REPO_RAW="https://raw.githubusercontent.com/prezmc/tainerhub/main"
DIR="${TAINERHUB_DIR:-$HOME/tainerhub}"
PORT=8585

say() { printf '%s\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "Docker isn't installed. Install it first: https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || fail "The Docker Compose plugin is missing ('docker compose version' failed). Install docker-compose-plugin."
docker info >/dev/null 2>&1 || fail "Can't talk to Docker. Run this as root, or as a user in the 'docker' group."

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
	else fail "Need curl or wget to download files."; fi
}

mkdir -p "$DIR"
cd "$DIR"
if [ -f compose.yml ]; then
	say "Using the existing $DIR/compose.yml."
else
	say "Downloading the hub's compose file to $DIR ..."
	fetch "$REPO_RAW/deploy/hub/compose.yml" compose.yml
fi

say "Starting TainerHub (first run downloads the images) ..."
docker compose pull --quiet
docker compose up -d

# Wait for the hub to answer its own health check (it waits for Postgres first).
i=0
until docker exec tainerhub /tainerhub healthcheck >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 60 ]; then
		say ""
		docker compose logs --tail 30 tainerhub || true
		fail "TainerHub didn't become ready within 2 minutes. The log above should say why."
	fi
	sleep 2
done

# The server's LAN address, for the link. Overridable with TAINERHUB_HOST.
host="${TAINERHUB_HOST:-}"
if [ -z "$host" ] && command -v ip >/dev/null 2>&1; then
	host=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n1)
fi
if [ -z "$host" ] && command -v hostname >/dev/null 2>&1; then
	host=$(hostname -I 2>/dev/null | awk '{print $1}') || true
fi
[ -n "$host" ] || host="<this server's IP>"

link=""
i=0
while [ "$i" -lt 10 ]; do
	if link=$(docker exec tainerhub /tainerhub setup-link 2>/dev/null); then break; fi
	link=""
	i=$((i + 1))
	sleep 1
done
dbpw=$(docker exec tainerhub /tainerhub db-password 2>/dev/null || true)

say ""
say "TainerHub is running."
say ""
if [ -n "$dbpw" ]; then
	say "  Database password (save it in your password manager; you won't need it day to day):"
	say "    $dbpw"
	say ""
fi
if [ -n "$link" ]; then
	say "  Create your account here (link works for 15 minutes):"
	say "    http://$host:$PORT$link"
	say ""
	say "  Link expired? Get a fresh one with:"
	say "    docker exec tainerhub /tainerhub setup-link"
else
	say "  Open http://$host:$PORT and sign in."
fi
say ""
say "  Next: in TainerHub, go to Hosts -> Add host to connect this server and any others."
