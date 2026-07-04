#!/bin/sh
# domovoi installer/updater.
#
#   curl -fsSL https://raw.githubusercontent.com/AnatolyRugalev/mcp-domovoi/main/install.sh | sudo sh
#
# Idempotent: first run installs binary + systemd unit + env file with a
# generated token; later runs replace the binary and restart the service,
# leaving config untouched.
#
# Environment overrides:
#   DOMOVOI_VERSION  release tag to install (default: latest), e.g. v0.2.0
#   DOMOVOI_USER     service user for a first install (default: domovoi)
set -eu

REPO="AnatolyRugalev/mcp-domovoi"
BIN=/usr/local/bin/domovoi
ENV_FILE=/etc/domovoi/domovoi.env
UNIT=/etc/systemd/system/domovoi.service
SERVICE_USER="${DOMOVOI_USER:-domovoi}"
VERSION="${DOMOVOI_VERSION:-latest}"

say() { printf '%s\n' "==> $*"; }
die() { printf '%s\n' "error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root: curl ... | sudo sh"
[ "$(uname -s)" = "Linux" ] || die "domovoi only supports Linux"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v curl >/dev/null 2>&1 || die "curl is required"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  VERSION=${VERSION##*/}
  case "$VERSION" in
    v*) ;;
    *) die "could not resolve the latest release (no releases published yet?)" ;;
  esac
fi

if [ -x "$BIN" ] && "$BIN" --help 2>&1 | grep -q domovoi; then
  CURRENT=$(curl -fsS --max-time 2 http://127.0.0.1:8811/healthz 2>/dev/null || true)
  [ -n "$CURRENT" ] && say "current: $CURRENT"
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
install -m 755 "$TMP/domovoi" "$BIN"

FIRST_INSTALL=no
if [ ! -f "$ENV_FILE" ]; then
  FIRST_INSTALL=yes

  if ! getent passwd "$SERVICE_USER" >/dev/null; then
    say "creating service user $SERVICE_USER"
    useradd -r -m -s "$(command -v nologin || echo /bin/false)" "$SERVICE_USER"
  fi

  say "writing $ENV_FILE with a generated token"
  if command -v openssl >/dev/null 2>&1; then
    TOKEN=$(openssl rand -hex 32)
  else
    TOKEN=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
  mkdir -p /etc/domovoi
  sed "s/^DOMOVOI_TOKEN=.*/DOMOVOI_TOKEN=$TOKEN/" "$TMP/domovoi.env.example" > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
fi

if [ ! -f "$UNIT" ]; then
  say "installing systemd unit"
  sed "s/^User=.*/User=$SERVICE_USER/" "$TMP/domovoi.service" > "$UNIT"
else
  if ! cmp -s "$TMP/domovoi.service" "$UNIT"; then
    say "keeping existing $UNIT (differs from the shipped unit; see repo for changes)"
  fi
fi

systemctl daemon-reload
if systemctl is-active --quiet domovoi; then
  say "restarting domovoi"
  systemctl restart domovoi
else
  say "enabling and starting domovoi"
  systemctl enable --now domovoi
fi

PORT=$(grep -s '^DOMOVOI_LISTEN=' "$ENV_FILE" | sed 's/.*://' || true)
PORT=${PORT:-8811}
sleep 1
if HEALTH=$(curl -fsS --max-time 3 "http://127.0.0.1:$PORT/healthz" 2>/dev/null); then
  say "healthy: $HEALTH"
else
  say "warning: healthz did not respond on port $PORT; check: journalctl -u domovoi -e"
fi

if [ "$FIRST_INSTALL" = yes ]; then
  say "done. token (also in $ENV_FILE):"
  grep '^DOMOVOI_TOKEN=' "$ENV_FILE"
  say "add this machine to your gateway; see README 'Adding a machine to agentgateway'"
else
  say "updated to $VERSION (config untouched)"
fi
