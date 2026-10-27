#!/usr/bin/env bash
# shpellcast installer for Ubuntu/Debian (and anything else with systemd).
#
#   sudo ./install.sh                              # install + start
#   sudo ./install.sh --domain cast.example.com    # ...and set the public URL
#   sudo ./install.sh --uninstall
#
# Installs a prebuilt binary from dist/ when one matches this machine, and
# falls back to building from source when the Go toolchain is available.
# Idempotent: re-running it upgrades the binary and leaves your config alone.

set -euo pipefail

BINARY=shpellcast
ALIAS=shpell
PREFIX=${PREFIX:-/usr/local/bin}
CONFDIR=${CONFDIR:-/etc/shpellcast}
UNITDIR=${UNITDIR:-/etc/systemd/system}
STATEDIR=/var/lib/shpellcast
SRC_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

DOMAIN=""
HTTP_ADDR=""
NO_START=0
UNINSTALL=0

# ---------------------------------------------------------------- output

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_OFF=""
fi
ok()   { printf '%s==>%s %s\n' "$C_OK"   "$C_OFF" "$*"; }
warn() { printf '%s!!!%s %s\n' "$C_WARN" "$C_OFF" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2; exit 1; }
dim()  { printf '%s    %s%s\n'  "$C_DIM" "$*" "$C_OFF"; }

usage() {
  cat <<EOF
shpellcast installer

  --domain HOST     public hostname; sets SHPELLCAST_BASE_URL=https://HOST
                    and binds the HTTP listener to localhost for a proxy
  --http ADDR       HTTP listen address (default :8080)
  --no-start        install but do not enable/start the service
  --uninstall       stop, disable and remove the service and binary
  -h, --help        this help
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --domain)    DOMAIN="${2:?--domain needs a hostname}"; shift 2 ;;
    --http)      HTTP_ADDR="${2:?--http needs an address}"; shift 2 ;;
    --no-start)  NO_START=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           die "unknown option $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (sudo ./install.sh)"
command -v systemctl >/dev/null 2>&1 || die "systemd not found; see README for running shpellcast by hand"

# ---------------------------------------------------------------- uninstall

if [ "$UNINSTALL" -eq 1 ]; then
  systemctl disable --now "$BINARY" 2>/dev/null || true
  rm -f "$UNITDIR/$BINARY.service" "$PREFIX/$BINARY" "$PREFIX/$ALIAS"
  systemctl daemon-reload
  ok "removed $BINARY"
  dim "config kept at $CONFDIR and data at $STATEDIR — delete them yourself if you want them gone"
  exit 0
fi

# ---------------------------------------------------------------- binary

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    armv7l|armv6l) echo arm ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
}

ARCH=$(detect_arch)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
[ "$OS" = "linux" ] || warn "this installer targets Linux; continuing on $OS"

STAGED=""
cleanup() { [ -n "$STAGED" ] && rm -f "$STAGED" || true; }
trap cleanup EXIT

# 1. A matching prebuilt binary shipped with the project.
#
# dist/ accumulates binaries as versions are built, so pick the highest
# version rather than whatever the shell globs first — otherwise an old
# build left in the directory silently wins over the current one.
shopt -s nullglob
candidates=( "$SRC_DIR"/dist/$BINARY-*-"$OS"-"$ARCH" "$SRC_DIR"/dist/$BINARY-*-"$OS"-"$ARCH".gz )
shopt -u nullglob

if [ ${#candidates[@]} -gt 0 ]; then
  if command -v sort >/dev/null 2>&1 && sort --help 2>&1 | grep -q -- '-V'; then
    candidate=$(printf '%s\n' "${candidates[@]}" | sort -V | tail -1)
  else
    candidate=${candidates[${#candidates[@]}-1]}
  fi
  STAGED=$(mktemp)
  if [ "${candidate%.gz}" != "$candidate" ]; then
    gunzip -c "$candidate" > "$STAGED"
  else
    cp "$candidate" "$STAGED"
  fi
  ok "using prebuilt binary $(basename "$candidate")"
  if [ ${#candidates[@]} -gt 1 ]; then
    dim "(${#candidates[@]} builds in dist/; picked the highest version)"
  fi
fi

# 2. Otherwise build from source.
if [ -z "$STAGED" ]; then
  if command -v go >/dev/null 2>&1; then
    ok "building from source with $(go version | awk '{print $3}')"
    STAGED=$(mktemp)
    ( cd "$SRC_DIR" && CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=$(git -C "$SRC_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)" \
        -o "$STAGED" ./cmd/shpellcast )
  else
    die "no prebuilt binary for $OS/$ARCH in dist/ and no Go toolchain found.
     Install Go (sudo apt install golang-go) or run 'make release' on a build host."
  fi
fi

chmod 0755 "$STAGED"
"$STAGED" -version >/dev/null || die "the built binary does not run on this machine"

install -d "$PREFIX" "$CONFDIR"
install -m 0755 "$STAGED" "$PREFIX/$BINARY"
ok "installed $PREFIX/$BINARY ($("$PREFIX/$BINARY" -version))"

# Short alias. The full name is the product; `shpell` is what people
# actually type. A symlink rather than a copy so upgrades hit both.
if ln -sf "$PREFIX/$BINARY" "$PREFIX/$ALIAS" 2>/dev/null; then
  ok "linked $PREFIX/$ALIAS -> $BINARY"
fi

# ---------------------------------------------------------------- config

if [ ! -f "$CONFDIR/shpellcast.env" ]; then
  install -m 0644 "$SRC_DIR/deploy/shpellcast.env" "$CONFDIR/shpellcast.env"
  ok "wrote $CONFDIR/shpellcast.env"
else
  ok "kept existing $CONFDIR/shpellcast.env"
fi

set_env() {
  local key=$1 val=$2 file=$CONFDIR/shpellcast.env
  if grep -qE "^#?${key}=" "$file"; then
    sed -i "s|^#\?${key}=.*|${key}=${val}|" "$file"
  else
    printf '%s=%s\n' "$key" "$val" >> "$file"
  fi
}

if [ -n "$DOMAIN" ]; then
  set_env SHPELLCAST_BASE_URL "https://$DOMAIN"
  set_env SHPELLCAST_TRUST_PROXY "true"
  set_env SHPELLCAST_HTTP "${HTTP_ADDR:-127.0.0.1:8080}"
  ok "configured for https://$DOMAIN behind a reverse proxy"
  dim "put deploy/Caddyfile or deploy/nginx.conf in front of it for TLS"
elif [ -n "$HTTP_ADDR" ]; then
  set_env SHPELLCAST_HTTP "$HTTP_ADDR"
fi

install -d -m 0750 "$STATEDIR"

# ---------------------------------------------------------------- service

install -m 0644 "$SRC_DIR/deploy/shpellcast.service" "$UNITDIR/$BINARY.service"
systemctl daemon-reload
ok "installed $UNITDIR/$BINARY.service"

if [ "$NO_START" -eq 1 ]; then
  dim "not starting (--no-start); run: systemctl enable --now $BINARY"
  exit 0
fi

systemctl enable "$BINARY" >/dev/null 2>&1 || true
systemctl restart "$BINARY"

sleep 1
if ! systemctl is-active --quiet "$BINARY"; then
  warn "service failed to start; last log lines:"
  journalctl -u "$BINARY" --no-pager -n 25 || true
  exit 1
fi

HTTP=$(grep -E '^SHPELLCAST_HTTP=' "$CONFDIR/shpellcast.env" | cut -d= -f2- || true)
TCP=$(grep -E '^SHPELLCAST_TCP=' "$CONFDIR/shpellcast.env" | cut -d= -f2- || true)
PROBE="127.0.0.1${HTTP#*:}"
case "$HTTP" in
  :*) PROBE="127.0.0.1$HTTP" ;;
  *)  PROBE="$HTTP" ;;
esac

if command -v curl >/dev/null 2>&1 && curl -fsS --max-time 3 "http://$PROBE/healthz" >/dev/null 2>&1; then
  ok "shpellcast is running and healthy"
else
  warn "service is up but the health check did not answer on http://$PROBE/healthz"
fi

echo
ok "try it:"
HOSTNAME_GUESS=${DOMAIN:-$(hostname -I 2>/dev/null | awk '{print $1}')}
dim "ping -c 5 1.1.1.1 | nc ${HOSTNAME_GUESS:-<host>} ${TCP#:}"
dim "journalctl -u $BINARY -f        # logs"
dim "$CONFDIR/shpellcast.env          # settings"
