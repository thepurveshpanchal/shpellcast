<div align="center">

# shpellcast

**Turn any command's output into a live, private web page.**

One static binary. No agent, no account, no database.

[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Dependencies](https://img.shields.io/badge/dependencies-none-success)](go.mod)
[![Binary](https://img.shields.io/badge/deploy-single%20binary-orange)](#install)

</div>

---

```console
$ ./nightly-build.sh 2>&1 | nc cast.example.com 1337
shpellcast: https://cast.example.com/s/9x2r46t0bymtyqhq
```

Open that link and watch the job run — colours intact, progress bars
moving — from a browser, a phone, or a teammate's laptop. Nothing is
installed on the machine producing the output. It only needs a TCP socket
or `curl`.

> ### 🌐 Hosted service
>
> **`https://YOUR-DOMAIN-HERE`** — *placeholder; update once your DNS is live.*
>
> Prefer to run it yourself? [Self-hosting takes one command.](#install)
>
> <!-- TODO: replace YOUR-DOMAIN-HERE throughout this file, and set
>      SHPELLCAST_BASE_URL in deploy/shpellcast.env, once the domain is live. -->

---

## Contents

| For engineers | For everyone |
|---|---|
| [Install](#install) · [Usage](#usage) · [Configuration](#configuration) | [What problem this solves](#what-problem-this-solves) |
| [TLS & reverse proxy](#tls-and-reverse-proxies) · [API](#http-api) | [Features](#features) · [Pros and cons](#pros-and-cons) |
| [Capacity](#capacity) · [Operating it](#operating-it) | [How it compares](#how-it-compares) · [FAQ](#faq) |
| [Security model](#security-model) · [Development](#development) | [Roadmap](#roadmap) |

---

## What problem this solves

Long-running work happens in terminals nobody can watch: a migration on a
jump box, a release script on a build agent, a model training over SSH, a
data import that takes four hours.

When somebody asks *"how's it going?"*, the usual answers are all bad:

- 📸 **Screenshots of a terminal** — stale the moment they're taken.
- 🔗 **"SSH in and run `tail -f`"** — needs access, credentials, and a VPN.
- 📋 **Pasting 400 lines into chat** — unreadable, and it buries the thread.
- 🖥️ **Screen sharing** — ties up two people in real time.
- 🏗️ **A full log platform** — the right answer for production telemetry,
  heavy machinery for *"watch this one script finish."*

shpellcast gives that output a URL. The person who wants to watch clicks a
link. The person running the job types one extra pipe.

**In one sentence for a non-engineer:** it's a live, private, expiring web
page that shows what a running program is printing — useful for anyone who
needs visibility into work they can't see, without being given access to
the machine doing it.

---

## How it works

```
   ┌──────────────┐      pipe output       ┌──────────────┐     live page    ┌──────────┐
   │  any machine │ ─────────────────────► │  shpellcast  │ ───────────────► │  browser │
   │  nc / curl   │   TCP :1337 or HTTPS   │  (1 binary)  │   SSE over HTTP  │  or curl │
   └──────────────┘                        └──────────────┘                  └──────────┘
                                           keeps the last N KB
                                           in memory, then expires
```

1. **Pipe.** Your command's output goes to the server over a plain socket
   or an HTTPS upload.
2. **Link.** The server hands back a random, unguessable URL — instantly,
   before the job finishes.
3. **Watch.** Anyone with that link sees the output live. It expires on a
   timer and deletes itself.

---

## Install

### Option A — use the hosted service

Nothing to install. Point your pipe at the host:

```bash
./long_job.sh 2>&1 | nc YOUR-DOMAIN-HERE 1337
```

### Option B — self-host (recommended for internal logs)

On any Ubuntu/Debian host with systemd:

```bash
git clone https://github.com/YOUR-ORG/shpellcast.git && cd shpellcast
sudo ./install.sh --domain cast.example.com
```

That installs the binary to `/usr/local/bin`, config to
`/etc/shpellcast/shpellcast.env`, and a hardened systemd service, then
starts it. **No Go toolchain needed** — the installer uses a prebuilt
binary from `dist/` and falls back to building from source if one isn't
there. Re-run it any time to upgrade; your config is preserved.

```bash
sudo ufw allow 8080/tcp    # web UI (or 443 via your reverse proxy)
sudo ufw allow 1337/tcp    # pipe endpoint
sudo ./install.sh --uninstall   # clean removal
```

<details>
<summary><b>Other ways to run it</b></summary>

**Just try it, no install:**

```bash
make build
./bin/shpellcast -http :8080 -tcp :1337 \
  -base-url http://$(hostname -I | awk '{print $1}'):8080
```

**Docker:**

```bash
docker build -t shpellcast .
docker run -p 8080:8080 -p 1337:1337 \
  -e SHPELLCAST_BASE_URL=https://cast.example.com shpellcast
```

**Kubernetes / Nomad / anything else:** it's one static binary that reads
environment variables and logs to stderr. Nothing special is required.

</details>

---

## Usage

### The basics

```bash
# Pipe anything
./deploy.sh 2>&1 | nc cast.example.com 1337
make -j8     2>&1 | nc cast.example.com 1337
pytest -v    2>&1 | nc cast.example.com 1337
```

> 💡 `2>&1` matters — it includes error output. Without it you only send
> stdout, and the interesting part of a failure is usually stderr.

### When the network is restrictive

Corporate networks often block unusual ports. Use HTTPS instead:

```bash
./long_job.sh 2>&1 | curl -sT- https://cast.example.com/i
```

curl only prints the response once the upload ends, so to get the link
immediately, reserve a session first:

```bash
read -r id url tok < <(curl -s "https://cast.example.com/new?format=text" | tr '\n' ' ')
echo "$url"
./long_job.sh 2>&1 | curl -sT- -H "X-Shpellcast-Token: $tok" "https://cast.example.com/i/$id"
```

### When netcat isn't installed

bash can open the socket itself:

```bash
exec 3<>/dev/tcp/cast.example.com/1337; head -1 <&3 & ./long_job.sh 2>&1 >&3
```

### The helper script

Handles the two-step flow for you, and can run the command so you still
see output locally:

```bash
curl -fsSL https://cast.example.com/client.sh -o ~/.local/bin/shpell
chmod +x ~/.local/bin/shpell

./long_job.sh 2>&1 | shpell          # stream a pipeline
shpell -- make -j8                   # run it, mirror locally, stream a copy
shpell -o -- ./deploy.sh             # and open the page in a browser
shpell -l "nightly build" -- ./ci.sh # label it in the UI
```

> 💡 `shpell` is the short alias — the server installs it alongside
> `shpellcast`, and the helper works under either name.

### Watch from a terminal instead of a browser

```bash
curl -N https://cast.example.com/s/9x2r46t0bymtyqhq/raw
```

<details>
<summary><b>Recipes: CI, cron, remote servers, incident calls</b></summary>

**Post a build link to Slack from CI**

```bash
URL=$(curl -s "$SHPELLCAST/new?format=text" | sed -n 2p)
curl -s -X POST -H 'Content-type: application/json' \
  --data "{\"text\":\"Build #$BUILD_ID running: $URL\"}" "$SLACK_WEBHOOK"
./build.sh 2>&1 | shpellcast -q
```

**Give a cron job a face**

```cron
0 2 * * * /opt/scripts/nightly.sh 2>&1 | nc cast.example.com 1337 | head -1 | mail -s "Nightly started" ops@example.com
```

**Let a vendor or contractor watch a migration** without SSH access,
a VPN, or a shared screen — send them the link, revoke it when done:

```bash
curl -X DELETE "https://cast.example.com/s/<id>?token=<token>"
```

**Follow a long job from your phone** — open the link; the viewer is
responsive and reconnects on its own when you switch networks.

</details>

---

## Features

### Core

| | |
|---|---|
| 🔌 **Zero client install** | If the machine has `nc`, `curl`, or bash, it can publish. Nothing to deploy to producers. |
| ⚡ **Instant links** | The URL comes back before the job finishes — paste it while it's still running. |
| 🎨 **Real terminal rendering** | ANSI colour, progress bars that animate, spinners, cursor control. Not a wall of escape codes. |
| 📱 **Works everywhere** | Browser, phone, or `curl` from another terminal. Auto-reconnects. |
| 🔐 **Unguessable URLs** | 80 bits of entropy per link. Never listed, never indexed. |
| ⏳ **Self-expiring** | Sessions delete themselves. Nothing accumulates. |
| 📦 **Single static binary** | ~6 MB, no runtime, no dependencies, no database. |

### Operations

| | |
|---|---|
| 📊 **Prometheus metrics** | Sessions, viewers, bytes, rejections by reason, goroutines, heap, file descriptors. |
| 🛡️ **Hardened by default** | `DynamicUser`, zero capabilities, syscall filter, strict CSP, read-only filesystem. |
| 🧪 **Built-in load tester** | `make loadtest` proves capacity on *your* hardware before you commit to limits. |
| 🚦 **Bounded everything** | Per-session memory, per-session bytes, per-client sessions, three independent expiry timers. |
| 🔁 **Graceful restarts** | `SIGTERM` drains live connections instead of killing them mid-build. |
| 💾 **Optional disk mirror** | Survive restarts without a database — just files that expire with the session. |
| ⚙️ **Config twice over** | Every flag has an environment variable twin. Same binary for systemd, Docker, or a shell. |

---

## Pros and cons

Honest assessment. This is a sharp tool with a narrow purpose.

### ✅ Where it shines

- **Sharing transient output with humans.** Build logs, migrations,
  deploys, test runs, one-off scripts.
- **Giving visibility without giving access.** A link is cheaper and safer
  than an SSH account or a VPN seat.
- **Environments you don't control.** Producers need no agent, no package,
  no root, no config file.
- **Cost and operational footprint.** One small VM serves an entire
  engineering org. There's nothing to patch at 2am and no vendor bill.
- **Auditability.** ~2,500 lines of Go, no third-party dependencies, MIT
  licensed. You can read all of it in an afternoon.

### ⚠️ Where it doesn't fit

- **It is not a log platform.** No search, no indexing, no retention
  policies, no alerting on content. Use Loki, Elasticsearch, Datadog or
  CloudWatch for production telemetry — they solve a different problem.
- **It does not cluster.** Sessions live in one process's memory. Two
  instances behind a load balancer will serve each other's 404s. Run one
  per site, or pin viewers to an instance by session ID.
- **Restarts end live sessions.** Disk mirroring preserves the *log*, but
  the live stream dies with the process.
- **No built-in authentication.** The link is the credential. That's a
  deliberate trade (see [Security model](#security-model)) — put SSO or
  basic auth in your reverse proxy if you need real identity.
- **Not for secrets.** Anyone with the link reads the output. Don't pipe
  anything through it you wouldn't put in a pastebin.
- **Not a remote shell.** Output flows one way. You cannot type back.
- **Full-screen TUIs render approximately.** `top`, `htop` and `vim` use
  absolute cursor positioning against a fixed viewport; the viewer scrolls
  instead. Line-oriented output — which is almost all build and script
  output — is exact.

---

## How it compares

| | shpellcast | Log platform | `tail -f` over SSH | Screen share | Paste in chat |
|---|---|---|---|---|---|
| Setup on the producing machine | none | agent/shipper | SSH access | app + session | none |
| Recipient needs credentials | no | yes | yes | no | no |
| Live updates | ✅ | ✅ | ✅ | ✅ | ❌ |
| Shareable link | ✅ | ✅ | ❌ | ❌ | ❌ |
| Search & retention | ❌ | ✅ | ❌ | ❌ | ❌ |
| Works from a phone | ✅ | ✅ | awkward | awkward | ✅ |
| Ongoing cost | one small VM | $$$ | — | people's time | — |
| Right for production telemetry | ❌ | ✅ | ❌ | ❌ | ❌ |
| Right for "watch this finish" | ✅ | overkill | clumsy | expensive | ❌ |

They're complements, not competitors. Keep your log platform for
production. Use shpellcast for the things that never make it there.

---

## The viewer

The browser page is a small terminal emulator, not a `<pre>` tag:

| Handles | Why it matters |
|---|---|
| `\r` overwrite | Progress bars and spinners animate instead of stacking into hundreds of lines |
| ANSI SGR | 16, 256 and 24-bit colour, bold, dim, underline, inverse |
| `\b`, `\t`, `ESC[K`, `ESC[2J` | Backspace, tabs, line and screen erase |
| UTF-8 across chunk boundaries | Emoji and CJK don't break when a network frame splits them |
| Reconnects | Replays the buffer without duplicating output |

Scrollback is capped at 5,000 lines in the DOM, so a chatty job won't
freeze the tab. **Follow** sticks to the end and releases when you scroll
up. **Wrap**, **Clear**, **Copy URL**, **Raw** and **Download** are one
click. Light and dark themes follow your system setting.

---

## Configuration

Every flag has an environment twin (`-idle-ttl` ⇄ `SHPELLCAST_IDLE_TTL`),
so the same binary configures identically under systemd, Docker, or a
shell. `deploy/shpellcast.env` ships fully commented.

| Setting | Default | What it controls |
|---|---|---|
| `-http` | `:8080` | Web UI and API listener |
| `-tcp` | `:1337` | Pipe endpoint; empty disables it |
| `-base-url` | *derived* | Origin printed in links — **always set this in production** |
| `-buffer` | `256KB` | Output retained per session |
| `-quota` | `64MB` | Output accepted per session before the producer is cut off |
| `-idle-ttl` | `24h` | Expiry measured from the last write |
| `-closed-ttl` | `6h` | Expiry after the producer disconnects |
| `-max-ttl` | `168h` | Hard expiry from creation |
| `-max-sessions` | `2000` | Server-wide session cap |
| `-max-per-client` | `250` | Sessions **per source IP** — [read this](#the-limit-that-will-bite-you) |
| `-spill-dir` | *off* | Mirror sessions to disk |
| `-trust-proxy` | `false` | Honour `X-Forwarded-*` headers |
| `-index` | `false` | Publish the list of live sessions |
| `-id-length` | `16` | Characters per session ID (5 bits each) |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

### Keeping output longer

Sessions are ephemeral by design. To keep one: download it while it's
alive (**Download** in the UI, or `/s/<id>/download`), or set
`SHPELLCAST_SPILL_DIR=/var/lib/shpellcast` so every session is mirrored to a
file that survives restarts. Mirrored, not hoarded — the file is removed
when the session expires.

---

## TLS and reverse proxies

shpellcast speaks plain HTTP and expects a proxy in front of it for any
internet-facing deployment. Both configs ship ready to edit:

```bash
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile            # automatic certificates
sudo cp deploy/nginx.conf /etc/nginx/sites-available/shpellcast
```

Then in `/etc/shpellcast/shpellcast.env`:

```ini
SHPELLCAST_HTTP=127.0.0.1:8080
SHPELLCAST_BASE_URL=https://cast.example.com
SHPELLCAST_TRUST_PROXY=true
```

> ⚠️ **Both shipped configs disable response buffering on streaming
> routes.** nginx buffers by default, which leaves the page blank until
> the job finishes — the single most common way a setup like this appears
> broken. If you write your own config, copy that part.

The pipe port is not HTTP and does not pass through the proxy. Expose
1337 directly in your firewall, or disable it (`SHPELLCAST_TCP=`) and have
producers use `curl -T-` over HTTPS.

---

## Capacity

Measured, not estimated. `cmd/shpellcast-bench` opens real producers and
viewers, stamps every line with a timestamp and reads it back, so the
latency below covers the whole path: socket → buffer → fan-out → HTTP.

On **2 vCPUs**, every session streaming continuously with a live viewer:

| Sessions | Viewers | Delivered | p50 | p99 | Memory |
|---:|---:|---:|---:|---:|---:|
| 100 | 200 | 100% | 238 µs | 912 µs | 48 MB |
| 500 | 500 | 100% | 134 µs | 429 µs | 52 MB |
| 1 000 | 1 000 | 100% | 123 µs | 516 µs | 101 MB |

Throughput: 100 sessions × 2 viewers at 200 lines/s each →
**18,184 lines/s ingested, 126 MB fanned out, zero lines lost, p99 3.2 ms,
42% of one core.**

**Sizing.** Worst-case memory is `max-sessions × buffer` plus ~30%.
Buffers grow on demand from 4 KB, so the typical figure is far lower — a
session printing 3 KB costs 4 KB, not the 256 KB it's allowed. Budget
~2 file descriptors per session.

| Deployment | `-max-sessions` | `-buffer` | `MemoryMax` | `LimitNOFILE` |
|---|---|---|---|---|
| Team (~100 concurrent) | `500` | `256KB` | `256M` | `8192` |
| Shipped defaults | `2000` | `256KB` | `1G` | `65535` |
| Long, noisy logs | `500` | `4MB` | `3G` | `8192` |

Prove it on your own hardware:

```bash
make loadtest SESSIONS=200 VIEWERS=2 RATE=10 DURATION=60s
```

### The limit that will bite you

`-max-per-client` counts sessions **per source address**. Producers behind
NAT, a CI runner fleet, or any reverse proxy without `-trust-proxy` all
arrive as *one* address — so this value caps your whole fleet, not one
laptop.

The default is 250 for exactly that reason. If sessions are being refused,
`shpellcast_sessions_rejected_perclient_total` tells you it's this, and
`shpellcast_sessions_rejected_capacity_total` tells you it's the opposite.

---

## Security model

Be clear-eyed about what this is: **anyone who can reach the pipe port can
create a session, and anyone holding a session URL can read it.** That is
the right trade for sharing build output, and the wrong one for secrets.

What the implementation does about it:

| Control | Detail |
|---|---|
| **Unguessable links** | IDs from `crypto/rand`; 80 bits at the default length. `-id-length` refuses anything under 50 bits. |
| **Never enumerable** | `/sessions` is disabled unless you turn it on. Pages carry `noindex` and `no-referrer`. |
| **Authorised writes** | Appending to an existing session requires a per-session token, compared in constant time. Knowing a URL doesn't let a third party inject into someone's stream. |
| **Bounded everything** | Ring buffer per session, byte quota per session, session cap per source IP, idle timeout on producer sockets, three independent TTLs. |
| **No forged identity** | `X-Forwarded-For` is ignored unless you declare a trusted proxy — it's trivially spoofed and it gates the per-client limits. |
| **Output can't become script** | Strict CSP (`default-src 'none'`), no inline script or style, DOM built from text nodes. Command output is never parsed as HTML. |
| **Hardened service** | `DynamicUser`, zero capabilities, `ProtectSystem=strict`, `MemoryDenyWriteExecute`, `@system-service` syscall filter, `AF_INET`/`AF_INET6` only. |

**Need real authentication?** Put it in the reverse proxy — `basic_auth`
in Caddy, `auth_request` in nginx, or an SSO forward-auth — and keep the
pipe port on a private interface. See [SECURITY.md](SECURITY.md) to report
a vulnerability.

---

## Operating it

```bash
systemctl status shpellcast
journalctl -u shpellcast -f
curl -s localhost:8080/healthz     # {"status":"ok", ...}
curl -s localhost:8080/metrics     # Prometheus text format
```

**Metrics:** `shpellcast_sessions_live`, `_created_total`, `_reaped_total`,
`_rejected_total` (plus `_rejected_capacity_total` and
`_rejected_perclient_total`), `shpellcast_bytes_ingested_total`,
`shpellcast_viewers_live`, `shpellcast_viewers_dropped_total`,
`shpellcast_uptime_seconds`, and standard `go_goroutines`, `go_memstats_*`,
`process_open_fds`.

**The two worth alerting on:**

- `shpellcast_viewers_dropped_total` rising → consumers can't keep up.
  Producers are never slowed down, so this is where that pressure surfaces.
- `go_goroutines` rising while `shpellcast_sessions_live` is flat →
  something is leaking.

**Restarts.** `SIGTERM` drains: live viewers are released and in-flight
producers closed cleanly rather than killed, so restarting during
someone's build doesn't lose the tail of it. The unit allows 20s.

---

## HTTP API

| Endpoint | Purpose |
|---|---|
| `GET /new` | Reserve a session → `{id, url, ingest_url, token}` |
| `PUT\|POST /i` | Stream a body into a new session; URL returned at the end |
| `PUT\|POST /i/{id}` | Stream into a reserved session (token required) |
| `GET /s/{id}` | Browser viewer |
| `GET /s/{id}/stream` | Server-Sent Events: `meta`, `out` (base64), `gap`, `end` |
| `GET /s/{id}/raw` | Plain text, streaming; `?follow=0` for a snapshot |
| `GET /s/{id}/download` | Retained output as a file |
| `GET /s/{id}/meta` | Session metadata as JSON |
| `POST /s/{id}/close` | Mark finished (token required) |
| `DELETE /s/{id}` | Delete immediately (token required) |
| `GET /healthz` · `/metrics` · `/client.sh` | Ops and the client helper |

<details>
<summary><b>Why SSE instead of WebSockets</b></summary>

Data flows one way, so a bidirectional protocol buys nothing. SSE
survives proxies and middleboxes that mangle upgrade handshakes, browsers
reconnect automatically, and it adds no dependency on either end.
Payloads are base64-encoded inside each frame so arbitrary binary output
can never break the line-oriented framing.

</details>

---

## FAQ

<details>
<summary><b>Is my output private?</b></summary>

The URL is the credential — random, unguessable, never listed or indexed.
Anyone you send it to can read the output; nobody who doesn't have it can.
Treat a link like a password: fine for build logs, not for secrets.
</details>

<details>
<summary><b>How long do sessions last?</b></summary>

By default: 24h after the last output, 6h after the job finishes, 7 days
maximum. All three are configurable. After that the session is deleted and
the link stops working permanently.
</details>

<details>
<summary><b>What if the job prints gigabytes?</b></summary>

The page keeps the most recent 256 KB (configurable) and marks itself
truncated. The producer is cut off once a session has sent 64 MB
(configurable) so one job can't exhaust the server. Enable disk mirroring
if you need to keep the whole thing.
</details>

<details>
<summary><b>Can someone else write into my session?</b></summary>

No. Appending to an existing session requires a token that only the
creator receives. Knowing the URL is read-only access.
</details>

<details>
<summary><b>Does it work behind a corporate firewall?</b></summary>

Use the HTTPS path (`curl -T-`) — it's ordinary web traffic on port 443
through your proxy. The dedicated pipe port is faster to type but needs
that port open.
</details>

<details>
<summary><b>What does it cost to run?</b></summary>

One small VM. Measured at 1,000 concurrent sessions on 2 vCPUs using
~100 MB of RAM. For most teams the smallest instance any cloud sells is
oversized.
</details>

<details>
<summary><b>Can I use this for production application logs?</b></summary>

No — and you shouldn't want to. There's no search, indexing, retention
policy or alerting. Use a log platform for production telemetry and
shpellcast for the interactive, short-lived things that never reach it.
</details>

<details>
<summary><b>Does it support Windows producers?</b></summary>

Anything that can open a TCP socket or run `curl` works, including
PowerShell. The server targets Linux; prebuilt binaries cover Linux
amd64/arm64/arm and macOS.
</details>

---

## Roadmap

Deliberately short. The aim is a tool that stays small enough to read.

- [ ] Optional retention beyond expiry (export to object storage)
- [ ] Session labels and a private, authenticated index for teams
- [ ] Download as HTML with colours preserved
- [ ] Prebuilt packages (`.deb`, `.rpm`) and a Homebrew tap
- [ ] Optional sticky-routing guidance for multi-instance deployments

Not planned: search, alerting, long-term retention, or anything that turns
this into a log platform.

---

## Project layout

```
cmd/shpellcast/          entrypoint: flags, logger, signals
cmd/shpellcast-bench/    load generator: N producers + viewers, latency percentiles
internal/config/         flags + environment, validation
internal/id/             crypto/rand capability IDs
internal/session/        ring buffer, fan-out, store, expiry
internal/server/         pipe ingest, HTTP handlers, embedded web assets
  web/                   viewer, landing page, client helper (compiled in)
deploy/                  systemd unit, env file, Caddy and nginx configs
```

## Development

```bash
make build      # bin/shpellcast for this machine
make check      # gofmt + go vet + go test -race
make loadtest   # drive a running server: SESSIONS=, VIEWERS=, RATE=, DURATION=
make release    # static binaries for linux amd64/arm64/arm + macOS, with checksums
make run        # run locally with debug logging
```

**No third-party dependencies** — `go.mod` has no `require` block, and
that's deliberate: nothing to audit, nothing to patch urgently, and
`go build` works on any machine with a toolchain and no network.

Tests are end-to-end where it matters — real sockets, real SSE streams,
real reconnects — because the interesting behaviour lives in the seams
between a socket, a buffer and a browser.

Contributions welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

MIT — see [LICENSE](LICENSE). Use it commercially, fork it, rebrand it.

<div align="center">

**[Get started](#install)** · **[Report an issue](https://github.com/YOUR-ORG/shpellcast/issues)** · **[Security policy](SECURITY.md)**

</div>
