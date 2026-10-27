#!/bin/sh
# shpellcast — pipe command output to a web page.
#
# Install:
#   curl -fsSL {{BASE}}/client.sh -o ~/.local/bin/shpellcast && chmod +x ~/.local/bin/shpellcast
#
# Use:
#   ./long_job.sh 2>&1 | shpellcast           # stream a pipeline
#   shpellcast -- make -j8                    # run a command, show it locally too
#   shpellcast -o -- ./deploy.sh              # and open the page in a browser
#
# Overrides: SHPELLCAST_URL (http base), SHPELLCAST_TCP ("host port").
# This script is POSIX sh: no bashisms, no dependencies beyond curl or nc.

set -eu

BASE="${SHPELLCAST_URL:-}"
[ -n "$BASE" ] || BASE='{{BASE}}'
TCP="${SHPELLCAST_TCP:-}"
[ -n "$TCP" ] || TCP='{{TCP_TARGET}}'
LABEL=""
OPEN=0
QUIET=0

have() { command -v "$1" >/dev/null 2>&1; }

usage() {
  cat >&2 <<EOF
shpellcast — pipe command output to a web page

  command | shpellcast [options]
  shpellcast [options] -- command [args...]

Options:
  -l, --label TEXT   label this session in the UI
  -o, --open         open the session page in a browser
  -q, --quiet        print only the URL
  -s, --server URL   shpellcast base URL (default: $BASE)
  -h, --help         this help
EOF
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    -l|--label)  LABEL="${2:?--label needs a value}"; shift 2 ;;
    -o|--open)   OPEN=1; shift ;;
    -q|--quiet)  QUIET=1; shift ;;
    -s|--server) BASE="${2:?--server needs a value}"; shift 2 ;;
    -h|--help)   usage 0 ;;
    --)          shift; break ;;
    -*)          echo "shpellcast: unknown option $1" >&2; usage 1 ;;
    *)           break ;;
  esac
done

say() { [ "$QUIET" -eq 1 ] || printf '%s\n' "$*" >&2; }

open_url() {
  [ "$OPEN" -eq 1 ] || return 0
  if   have xdg-open; then xdg-open "$1" >/dev/null 2>&1 || true
  elif have open;     then open "$1"     >/dev/null 2>&1 || true
  fi
}

# ---------------------------------------------------------------- transport

urlencode() {
  # Only used for the optional label; keeps the dependency surface at zero.
  printf '%s' "$1" | od -An -tx1 -v | tr -d '\n ' | sed 's/\(..\)/%\1/g'
}

start_http() {
  url="$BASE/new?format=text"
  if [ -n "$LABEL" ]; then
    url="$url&label=$(urlencode "$LABEL")"
  fi
  resp=$(curl -fsS "$url") || {
    echo "shpellcast: cannot reach $BASE" >&2
    exit 1
  }
  SID=$(printf '%s\n' "$resp" | sed -n 1p)
  URL=$(printf '%s\n' "$resp" | sed -n 2p)
  TOK=$(printf '%s\n' "$resp" | sed -n 3p)
  [ -n "$SID" ] && [ -n "$URL" ] || { echo "shpellcast: bad response from server" >&2; exit 1; }
}

pipe_http() {
  # -T- streams stdin with chunked encoding; --no-buffer keeps it live.
  curl -sS --no-buffer -T- -H "X-Shpellcast-Token: $TOK" "$BASE/i/$SID" >/dev/null || true
  curl -fsS -X POST -H "X-Shpellcast-Token: $TOK" "$BASE/s/$SID/close" >/dev/null 2>&1 || true
}

pipe_tcp() {
  # Fall back to raw netcat. The server's banner carries the URL, which we
  # surface on stderr so it survives stdout redirection.
  host=$(printf '%s\n' "$TCP" | awk '{print $1}')
  port=$(printf '%s\n' "$TCP" | awk '{print $2}')
  [ -n "$host" ] && [ -n "$port" ] || { echo "shpellcast: no TCP endpoint configured" >&2; exit 1; }
  exec nc "$host" "$port" >&2
}

# ---------------------------------------------------------------- run

if have curl; then
  start_http
  say "shpellcast: $URL"
  [ "$QUIET" -eq 1 ] && printf '%s\n' "$URL"
  open_url "$URL"

  if [ $# -gt 0 ]; then
    # Run the command, mirror it to this terminal, and stream a copy.
    set +e
    "$@" 2>&1 | tee /dev/stderr | pipe_http
    rc=$?
    set -e
    say "shpellcast: finished — $URL"
    exit "$rc"
  fi
  pipe_http
  say "shpellcast: finished — $URL"
  exit 0
fi

if have nc; then
  if [ $# -gt 0 ]; then
    "$@" 2>&1 | pipe_tcp
  else
    pipe_tcp
  fi
  exit $?
fi

echo "shpellcast: needs curl or nc on PATH" >&2
exit 1
