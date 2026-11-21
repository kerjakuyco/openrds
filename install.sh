#!/bin/sh
set -eu
cd "$(dirname "$0")"

die() { printf '%s\n' "$*" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || die "Run this installer on the Linux VPS that will host your databases."
command -v docker >/dev/null || die "Install Docker Engine and its Compose plugin first: https://docs.docker.com/engine/install/"
version=$(docker info --format '{{.ServerVersion}}') || die "Cannot access the Docker daemon. Run as a user with access to the local Docker socket."
major=${version%%.*}
case "$major" in ''|*[!0-9]*) die "Could not determine the Docker Engine version: $version";; esac
[ "$major" -ge 28 ] || die "Use Docker Engine 28 or newer for loopback-only published ports."
docker compose version >/dev/null || die "Install the Docker Compose plugin: https://docs.docker.com/compose/install/linux/"
docker compose --profile helper build
docker compose up --detach --wait --wait-timeout 120
address=$(docker compose port openrds 8080)
port=${address##*:}
printf '\nOpenRDS is ready at http://%s on this VPS.\n' "$address"
printf 'From your computer: ssh -N -L %s:127.0.0.1:%s user@your-vps\n' "$port" "$port"
printf 'Read the admin token: docker compose exec openrds cat /var/lib/openrds/admin-token\n'
