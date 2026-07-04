#!/bin/sh
# domovoi installer/updater.
#
#   curl -fsSL https://raw.githubusercontent.com/AnatolyRugalev/mcp-domovoi/main/install.sh | sh
#
# Installs a per-user systemd service by default -- no root required. Run as
# root (pipe into `sudo sh`) for a system-wide install instead.
#
# Idempotent: the first run installs the binary, systemd unit, and an env file
# with a generated token; later runs just replace the binary and restart,
# leaving config untouched.
#
# Environment overrides:
#   DOMOVOI_VERSION  release tag to install (default: latest), e.g. v0.2.0
#   DOMOVOI_MODE     "user" or "system" (default: system when root, else user)
#   DOMOVOI_USER     service user for a system install (default: domovoi)
set -eu

REPO="AnatolyRugalev/mcp-domovoi"
VERSION="${DOMOVOI_VERSION:-latest}"

say() { printf '%s\n' "==> $*"; }
die() { printf '%s\n' "error: $*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || die "domovoi only supports Linux"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v curl >/dev/null 2>&1 || die "curl is required"

if [ -n "${DOMOVOI_MODE:-}" ]; then
  MODE="$DOMOVOI_MODE"
elif [ "$(id -u)" -eq 0 ]; then
  MODE=system
else
  MODE=user
fi

case "$MODE" in
  system)
    [ "$(id -u)" -eq 0 ] || die "system mode needs root; re-run with sudo, or use DOMOVOI_MODE=user"
    BIN=/usr/local/bin/domovoi
    CONF_DIR=/etc/domovoi
    UNIT=/etc/systemd/system/domovoi.service
    UNIT_SRC=domovoi.service
    sc() { systemctl "$@"; }
    ;;
  user)
    BIN="$HOME/.local/bin/domovoi"
    CONF_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/domovoi"
    UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/domovoi.service"
    UNIT_SRC=domovoi.user.service
    export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
    sc() { systemctl --user "$@"; }
    ;;
  *) die "invalid DOMOVOI_MODE: $MODE (want 'user' or 'system')" ;;
esac
ENV_FILE="$CONF_DIR/domovoi.env"

if [ "$MODE" = user ] && ! sc show-environment >/dev/null 2>&1; then
  die "cannot reach your systemd user instance (XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR).
Log in on the console once, or enable lingering first:
  loginctl enable-linger $(id -un)"
fi

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

if [ "$VERSION" = latest ]; then
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  VERSION=${VERSION##*/}
  case "$VERSION" in
    v*) ;;
    *) die "could not resolve the latest release (none published yet?)" ;;
  esac
fi

TARBALL="domovoi_${VERSION#v}_linux_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

say "downloading domovoi $VERSION ($ARCH)"
curl -fsSL -o "$TMP/$TARBALL" "$BASE/$TARBALL"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"
(cd "$TMP" && grep " $TARBALL\$" checksums.txt | sha256sum -c - >/dev/null) \
  || die "checksum verification failed for $TARBALL"
tar -xzf "$TMP/$TARBALL" -C "$TMP"

say "installing binary to $BIN"
install -D -m 755 "$TMP/domovoi" "$BIN"

FIRST_INSTALL=no
if [ ! -f "$ENV_FILE" ]; then
  FIRST_INSTALL=yes

  if [ "$MODE" = system ]; then
    SERVICE_USER="${DOMOVOI_USER:-domovoi}"
    if ! getent passwd "$SERVICE_USER" >/dev/null; then
      say "creating service user $SERVICE_USER"
      useradd -r -m -s "$(command -v nologin || echo /bin/false)" "$SERVICE_USER"
    fi
  fi

  say "writing $ENV_FILE with a generated token"
  if command -v openssl >/dev/null 2>&1; then
    TOKEN=$(openssl rand -hex 32)
  else
    TOKEN=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
  mkdir -p "$CONF_DIR"
  sed "s/^DOMOVOI_TOKEN=.*/DOMOVOI_TOKEN=$TOKEN/" "$TMP/domovoi.env.example" > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
fi

if [ ! -f "$UNIT" ]; then
  say "installing systemd unit ($MODE)"
  mkdir -p "$(dirname "$UNIT")"
  if [ "$MODE" = system ]; then
    sed "s/^User=.*/User=$SERVICE_USER/" "$TMP/$UNIT_SRC" > "$UNIT"
  else
    cp "$TMP/$UNIT_SRC" "$UNIT"
  fi
elif ! cmp -s "$TMP/$UNIT_SRC" "$UNIT"; then
  say "keeping existing $UNIT (differs from the shipped unit; update by hand if needed)"
fi

sc daemon-reload
if sc is-active --quiet domovoi; then
  say "restarting domovoi"
  sc restart domovoi
else
  say "enabling and starting domovoi"
  sc enable --now domovoi
fi

if [ "$MODE" = user ] && command -v loginctl >/dev/null 2>&1; then
  if loginctl enable-linger "$(id -un)" 2>/dev/null; then
    say "enabled lingering so domovoi keeps running when you log out"
  else
    say "note: could not enable lingering; the service may stop on logout"
    say "      (run: loginctl enable-linger $(id -un))"
  fi
fi

PORT=$(grep -s '^DOMOVOI_LISTEN=' "$ENV_FILE" | sed 's/.*://')
PORT=${PORT:-8811}
sleep 1
if HEALTH=$(curl -fsS --max-time 3 "http://127.0.0.1:$PORT/healthz" 2>/dev/null); then
  say "healthy: $HEALTH"
else
  say "warning: healthz did not respond on port $PORT"
  say "         check logs: $([ "$MODE" = user ] && echo 'journalctl --user -u domovoi -e' || echo 'journalctl -u domovoi -e')"
fi

if [ "$FIRST_INSTALL" = yes ]; then
  say "done. auth token (also stored in $ENV_FILE):"
  grep '^DOMOVOI_TOKEN=' "$ENV_FILE"
else
  say "updated to $VERSION (config untouched)"
fi
