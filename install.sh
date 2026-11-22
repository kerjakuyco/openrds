#!/bin/sh
# Install or update OpenRDS. Run it from a checkout, or straight from the web:
#   curl -fsSL https://raw.githubusercontent.com/kerjakuyco/openrds/main/install.sh | sh
# Set OPENRDS_YES=1 to accept the questions it asks, and OPENRDS_DIR to choose
# where a downloaded copy goes (default ~/openrds).
set -eu

REPO="https://github.com/kerjakuyco/openrds"
say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() { printf '\nOpenRDS was not installed: %s\n' "$*" >&2; exit 1; }

# Ask on the terminal, which works even when this script arrives through a pipe.
ask() {
  [ "${OPENRDS_YES:-}" = 1 ] && return 0
  ( : < /dev/tty ) 2>/dev/null || return 1 # no terminal, as in CI
  printf '%s [Y/n] ' "$1" > /dev/tty
  read -r answer < /dev/tty || return 1
  case "$answer" in "" | y | Y | yes | Yes | YES) return 0 ;; *) return 1 ;; esac
}

os=$(uname -s)
here=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || here=$(pwd)

# 1. Fetch the source when this script was downloaded on its own.
if [ ! -f "$here/compose.yaml" ]; then
  dir="${OPENRDS_DIR:-$HOME/openrds}"
  if [ -d "$dir/.git" ] && command -v git >/dev/null 2>&1; then
    step "Updating OpenRDS in $dir"
    git -C "$dir" pull --ff-only
  elif command -v git >/dev/null 2>&1; then
    step "Downloading OpenRDS into $dir"
    git clone --depth 1 "$REPO.git" "$dir"
  else
    command -v curl >/dev/null 2>&1 || die "install git or curl first, then run this again."
    step "Downloading OpenRDS into $dir"
    mkdir -p "$dir"
    curl -fsSL "$REPO/archive/refs/heads/main.tar.gz" | tar -xz -C "$dir" --strip-components 1
  fi
  exec sh "$dir/install.sh"
fi
cd "$here"

# 2. Docker, installed for you on Linux if it is missing.
if ! command -v docker >/dev/null 2>&1; then
  [ "$os" = Linux ] || die "install Docker Desktop first: https://www.docker.com/products/docker-desktop/"
  say "Docker is not installed on this server."
  ask "Install it now with Docker's official script (get.docker.com)?" ||
    die "install Docker Engine and the Compose plugin, then run this again: https://docs.docker.com/engine/install/"
  command -v curl >/dev/null 2>&1 || die "install curl first, then run this again."
  step "Installing Docker"
  if [ "$(id -u)" -eq 0 ]; then
    curl -fsSL https://get.docker.com | sh
  else
    curl -fsSL https://get.docker.com | sudo sh
  fi
fi

if ! version=$(docker info --format '{{.ServerVersion}}' 2>/dev/null); then
  if [ "$os" = Linux ] && [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    say "Docker needs administrator rights for this user, so the installer continues with sudo."
    exec sudo OPENRDS_YES="${OPENRDS_YES:-}" sh "$here/install.sh"
  fi
  die "Docker is not running. Start Docker (or Docker Desktop), then run this again."
fi
if [ "$os" = Linux ]; then
  major=${version%%.*}
  case "$major" in "" | *[!0-9]*) die "could not read the Docker Engine version ($version)." ;; esac
  [ "$major" -ge 28 ] || die "OpenRDS needs Docker Engine 28 or newer; this server has $version. Upgrade: https://docs.docker.com/engine/install/"
fi
docker compose version >/dev/null 2>&1 || die "install the Docker Compose plugin, then run this again: https://docs.docker.com/compose/install/linux/"

# 3. Build and start. Existing databases, backups and accounts are kept.
step "Building OpenRDS (the first time takes a few minutes)"
docker compose --profile helper build --quiet
step "Starting OpenRDS"
docker compose up --detach --wait --wait-timeout 180
address=$(docker compose port openrds 8080)
port=${address##*:}
status=$(docker compose exec -T openrds wget -qO- http://127.0.0.1:8080/api/status 2>/dev/null || true)

printf '\nOpenRDS is running.\n\n'
if [ "$os" = Linux ]; then
  host=$(hostname -I 2>/dev/null | awk '{print $1}')
  user=${SUDO_USER:-$(id -un)}
  say "  From your computer, open a tunnel and keep it running:"
  say "      ssh -N -L $port:127.0.0.1:$port $user@${host:-your-server}"
  say "  Then open http://127.0.0.1:$port in your browser."
  say "  (On this server itself, the address is the same.)"
else
  say "  Open http://127.0.0.1:$port in your browser."
fi
case "$status" in
  *'"setup":true'*) printf '\nOpen it now and create your administrator account.\n' ;;
esac
printf '\nForgot the administrator password later? From %s run:\n  docker compose exec openrds openrds reset-password YOUR_USERNAME\n' "$here"
