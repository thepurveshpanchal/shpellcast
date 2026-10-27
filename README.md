<div align="center">

# shpellcast

**Turn any command's output into a live, private web page.**

One static binary. No agent, no account, no database.

[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Dependencies](https://img.shields.io/badge/dependencies-none-success)](go.mod)
[![Deploy](https://img.shields.io/badge/deploy-single%20binary-orange)](#self-hosting)

</div>

```console
$ ./nightly-build.sh 2>&1 | nc shpellcast.godtech.in 1337
shpellcast: https://shpellcast.godtech.in/s/9x2r46t0bymtyqhq
```

Open that link and watch the job run — colours intact, progress bars
moving — from a browser, a phone, or a teammate's laptop. Nothing is
installed on the machine producing the output. It only needs a TCP socket
or `curl`.

> **🌐 Hosted service — `https://shpellcast.godtech.in`**
> *Placeholder. Not live yet.*
> Want it on your own box instead? **[Self-hosting →](#self-hosting)**
>
> <!-- TODO before launch: replace shpellcast.godtech.in and thepurveshpanchal throughout
>      this file, and set SHPELLCAST_BASE_URL in deploy/shpellcast.env. -->

---

### Jump to

**Using it** — [What it does](#what-it-does) · [Quick start](#quick-start) ·
[Sending output](#sending-output) · [The viewer](#the-viewer) ·
[Features](#features) · [Pros and cons](#pros-and-cons) · [FAQ](#faq)

**Running it** — [Self-hosting](#self-hosting) · [Configuration](#configuration) ·
[Day-to-day operation](#day-to-day-operation) · [Troubleshooting](#troubleshooting) ·
[Security](#security-model) · [API](#http-api)

---

## What it does

Long-running work happens in terminals nobody can watch: a migration on a
jump box, a release script on a build agent, a model training over SSH, a
data import that takes four hours.

When somebody asks *"how's it going?"*, the usual answers are all bad:

- 📸 **Screenshots** — stale the moment they're taken.
- 🔗 **"SSH in and run `tail -f`"** — needs access, credentials, a VPN.
- 📋 **Pasting 400 lines into chat** — unreadable, and it buries the thread.
- 🖥️ **Screen sharing** — ties up two people in real time.

shpellcast gives that output a URL. The person running the job types one
extra pipe. The person who wants to watch clicks a link.

> **For a non-engineer:** it's a live, private, self-deleting web page that
> shows what a running program is printing — visibility into work you
> can't see, without being given access to the machine doing it.

**How it flows:**

```
   ┌──────────────┐      pipe output       ┌──────────────┐     live page    ┌──────────┐
   │  any machine │ ─────────────────────► │  shpellcast  │ ───────────────► │  browser │
   │  nc / curl   │   TCP :1337 or HTTPS   │  (1 binary)  │   SSE over HTTP  │  or curl │
   └──────────────┘                        └──────────────┘                  └──────────┘
                                           keeps the last N KB
                                           in memory, then expires
```

---

## Quick start

Everything below assumes a server already exists — either the hosted one
or [your own](#self-hosting). Replace `shpellcast.godtech.in` with its address.

**Pipe a command. Get a link. Share it.**

```bash
./deploy.sh 2>&1 | nc shpellcast.godtech.in 1337
```

```console
shpellcast: https://shpellcast.godtech.in/s/9x2r46t0bymtyqhq
```

That's the whole tool. The link works immediately — paste it in chat
while the job is still running.

> 💡 **`2>&1` matters.** It folds error output into the stream. Without it
> you only send stdout, and the interesting part of a failure is usually
> stderr.

---

## Sending output

### The standard way — netcat

```bash
./long_job.sh  2>&1 | nc shpellcast.godtech.in 1337
make -j8       2>&1 | nc shpellcast.godtech.in 1337
pytest -v      2>&1 | nc shpellcast.godtech.in 1337
terraform apply 2>&1 | nc shpellcast.godtech.in 1337
```

Works with `nc`, `ncat`, `socat`, or busybox — anything that opens a TCP
socket.

### Through a restrictive network — HTTPS

Corporate networks often block unusual ports. This is ordinary web
traffic on 443:

```bash
./long_job.sh 2>&1 | curl -sT- https://shpellcast.godtech.in/i
```

curl only prints the response once the upload finishes, so to get the
link *immediately*, reserve a session first:

```bash
read -r id url tok < <(curl -s "https://shpellcast.godtech.in/new?format=text" | tr '\n' ' ')
echo "$url"
./long_job.sh 2>&1 | curl -sT- -H "X-Shpellcast-Token: $tok" "https://shpellcast.godtech.in/i/$id"
```

### When netcat isn't installed

bash can open the socket by itself:

```bash
exec 3<>/dev/tcp/shpellcast.godtech.in/1337; head -1 <&3 & ./long_job.sh 2>&1 >&3
```

### The helper script — easiest option

It handles the two-step flow for you, and can run the command so you
still see output in your own terminal:

```bash
curl -fsSL https://shpellcast.godtech.in/client.sh -o ~/.local/bin/shpell
chmod +x ~/.local/bin/shpell
```

```bash
./long_job.sh 2>&1 | shpell          # stream a pipeline
shpell -- make -j8                   # run it, mirror locally, stream a copy
shpell -o -- ./deploy.sh             # ...and open the page in a browser
shpell -l "nightly build" -- ./ci.sh # ...and label it in the UI
```

> 💡 `shpell` is the short alias for `shpellcast`. The helper works under
> either name.

### Watching from a terminal

No browser needed — this behaves like `tail -f`:

```bash
curl -N https://shpellcast.godtech.in/s/9x2r46t0bymtyqhq/raw
```

<details>
<summary><b>Recipes — CI, cron, contractors, phones</b></summary>

**Post a build link to Slack from CI**

```bash
URL=$(curl -s "$SHPELLCAST/new?format=text" | sed -n 2p)
curl -s -X POST -H 'Content-type: application/json' \
  --data "{\"text\":\"Build #$BUILD_ID running: $URL\"}" "$SLACK_WEBHOOK"
./build.sh 2>&1 | shpell -q
```

**Give a cron job a face**

```cron
0 2 * * * /opt/scripts/nightly.sh 2>&1 | nc shpellcast.godtech.in 1337 | head -1 | mail -s "Nightly started" ops@example.com
```

**Let a vendor watch a migration** without SSH, a VPN, or a shared
screen. Send the link, revoke it when they're done:

```bash
curl -X DELETE "https://shpellcast.godtech.in/s/<id>?token=<token>"
```

**Follow a job from your phone** — open the link. The viewer is
responsive and reconnects itself when you change networks.

</details>

---

## The viewer

The browser page is a small terminal emulator, not a `<pre>` tag:

| Handles | Why it matters |
|---|---|
| `\r` overwrite | Progress bars animate instead of stacking into hundreds of lines |
| ANSI colour | 16, 256 and 24-bit, plus bold, dim, underline, inverse |
| `\b`, `\t`, `ESC[K`, `ESC[2J` | Backspace, tabs, line and screen erase |
| UTF-8 across frames | Emoji and CJK don't break when the network splits them |
| Reconnects | Replays the buffer without duplicating output |

**Follow** sticks to the end and releases when you scroll up. **Wrap**,
**Clear**, **Copy URL**, **Raw** and **Download** are one click. Light and
dark follow your system setting. Scrollback is capped so a chatty job
can't freeze the tab.

---

## Features

| | |
|---|---|
| 🔌 **Zero client install** | If the machine has `nc`, `curl` or bash, it can publish. |
| ⚡ **Instant links** | The URL comes back before the job finishes. |
| 🎨 **Real terminal rendering** | Colour and progress bars, not a wall of escape codes. |
| 📱 **Works everywhere** | Browser, phone, or `curl` from another terminal. |
| 🔐 **Unguessable URLs** | 80 bits of entropy. Never listed, never indexed. |
| ⏳ **Self-expiring** | Sessions delete themselves. Nothing accumulates. |
| 📦 **Single static binary** | ~6 MB. No runtime, no dependencies, no database. |
| 🛡️ **Hardened by default** | Locked-down systemd unit, strict CSP, bounded everything. |
| 📊 **Prometheus metrics** | Sessions, viewers, bytes, rejections, runtime stats. |
| ⚙️ **Configure once** | Every flag has an environment variable twin. |

---

## Pros and cons

A sharp tool with a narrow purpose. Here's the honest shape of it.

**✅ Where it shines**

- **Sharing transient output with humans** — builds, migrations, deploys,
  test runs, one-off scripts.
- **Visibility without access** — a link is cheaper and safer than an SSH
  account or a VPN seat.
- **Environments you don't control** — producers need no agent, no
  package, no root, no config file.
- **Cost** — one small VM serves an entire engineering org. No vendor bill.
- **Auditability** — ~2,500 lines of Go, zero third-party dependencies,
  MIT licensed. Readable in an afternoon.

**⚠️ Where it doesn't fit**

- **It is not a log platform.** No search, indexing, retention policies or
  content alerting. Use Loki, Elasticsearch, Datadog or CloudWatch for
  production telemetry — different problem.
- **It does not cluster.** Sessions live in one process's memory. Two
  instances behind a load balancer serve each other's 404s.
- **Restarts end live sessions.** Disk mirroring preserves the log; the
  live stream dies with the process.
- **No built-in authentication.** The link is the credential — a
  deliberate trade. Put SSO or basic auth in your reverse proxy if you
  need real identity.
- **Not for secrets.** Anyone with the link reads the output.
- **Not a remote shell.** Output flows one way; you can't type back.
- **Full-screen TUIs render approximately.** `top`, `htop` and `vim` use
  absolute cursor positioning against a fixed viewport. Line-oriented
  output — nearly all build and script output — is exact.

**How it compares**

| | shpellcast | Log platform | `tail -f` over SSH | Screen share |
|---|---|---|---|---|
| Setup on the producing machine | none | agent/shipper | SSH access | app + session |
| Recipient needs credentials | no | yes | yes | no |
| Shareable link | ✅ | ✅ | ❌ | ❌ |
| Search & retention | ❌ | ✅ | ❌ | ❌ |
| Works from a phone | ✅ | ✅ | awkward | awkward |
| Ongoing cost | one small VM | $$$ | — | people's time |
| Right for *"watch this finish"* | ✅ | overkill | clumsy | expensive |

They're complements, not competitors. Keep your log platform for
production; use shpellcast for the things that never reach it.

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

The page keeps the most recent 256 KB and marks itself truncated. The
producer is cut off once a session has sent 64 MB, so one job can't
exhaust the server. Both limits are configurable, and disk mirroring keeps
the whole thing if you need it.
</details>

<details>
<summary><b>Can someone else write into my session?</b></summary>

No. Appending to an existing session requires a token only the creator
receives. Knowing the URL gives read access, nothing more.
</details>

<details>
<summary><b>Does it work behind a corporate firewall?</b></summary>

Use the HTTPS path (`curl -T-`) — ordinary web traffic on port 443 through
your proxy. The dedicated port is faster to type but needs 1337 open.
</details>

<details>
<summary><b>Can I use it for production application logs?</b></summary>

No — and you shouldn't want to. There's no search, indexing, retention
policy or alerting. Use a log platform for telemetry and shpellcast for
the interactive, short-lived things that never reach it.
</details>

<details>
<summary><b>Does it support Windows producers?</b></summary>

Anything that opens a TCP socket or runs `curl` works, including
PowerShell. The server targets Linux; prebuilt binaries cover Linux
amd64/arm64/arm and macOS.
</details>

<br>

---

# Self-hosting

> **Everything above assumed a server exists. Here's how to run your own.**
>
> Roughly five minutes on a fresh Ubuntu box. No Go toolchain, no
> database, no container runtime required.

---

## What you need

- A Linux host with **systemd** — Ubuntu 20.04+, Debian 11+, or similar.
  A 1 vCPU / 512 MB VM is comfortably oversized for most teams.
- **Two open ports**: one for the web UI, one for the pipe endpoint.
- *(Optional)* a **domain name** if you want HTTPS and a tidy URL.

---

## Step 1 — Install

```bash
git clone https://github.com/thepurveshpanchal/shpellcast.git
cd shpellcast
sudo ./install.sh
```

That's it. The installer:

- copies the binary to `/usr/local/bin/shpellcast` (plus a `shpell` symlink),
- writes config to `/etc/shpellcast/shpellcast.env`,
- installs a hardened systemd unit and starts it,
- prints a health check and an example command to try.

It uses a prebuilt binary from `dist/` matching your architecture, and
falls back to building from source only if Go happens to be installed.
**Re-run it any time to upgrade** — your config is never overwritten.

**If you have a domain**, say so now and it configures itself for a
reverse proxy:

```bash
sudo ./install.sh --domain shpellcast.godtech.in
```

<details>
<summary><b>Installer options</b></summary>

```
--domain HOST    public hostname; sets the public URL and binds
                 the web listener to localhost for a proxy
--http ADDR      web listen address (default :8080)
--no-start       install without enabling the service
--uninstall      stop, disable and remove the service and binary
```

</details>

---

## Step 2 — Open the ports

```bash
sudo ufw allow 8080/tcp    # web UI   (skip if a reverse proxy fronts it)
sudo ufw allow 1337/tcp    # pipe endpoint
```

On a cloud VM, add the same two rules to the security group or firewall
in your provider's console — that's the step people most often miss.

**Try it now, from any machine:**

```bash
ping -c 5 1.1.1.1 | nc YOUR-SERVER-IP 1337
```

You should get a URL back immediately. Open it.

---

## Step 3 — Put it behind HTTPS

Skip this for a purely internal server on a trusted network. Do it for
anything reachable from the internet.

shpellcast speaks plain HTTP and expects a reverse proxy to terminate
TLS. Both configs ship ready to edit — pick one.

**Caddy** (gets certificates automatically):

```bash
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo nano /etc/caddy/Caddyfile          # change shpellcast.godtech.in
sudo systemctl reload caddy
```

**nginx:**

```bash
sudo cp deploy/nginx.conf /etc/nginx/sites-available/shpellcast
sudo ln -s /etc/nginx/sites-available/shpellcast /etc/nginx/sites-enabled/
sudo nano /etc/nginx/sites-available/shpellcast   # change shpellcast.godtech.in
sudo certbot --nginx -d shpellcast.godtech.in
sudo nginx -t && sudo systemctl reload nginx
```

Then tell shpellcast it's behind a proxy — edit
`/etc/shpellcast/shpellcast.env`:

```ini
SHPELLCAST_HTTP=127.0.0.1:8080
SHPELLCAST_BASE_URL=https://shpellcast.godtech.in
SHPELLCAST_TRUST_PROXY=true
```

```bash
sudo systemctl restart shpellcast
```

> ⚠️ **If you write your own proxy config, disable response buffering on
> the streaming routes.** nginx buffers by default, which leaves the page
> blank until the job finishes — by far the most common way a setup like
> this appears broken. Both shipped configs already handle it.

The pipe port is raw TCP, not HTTP, so it does **not** pass through the
proxy. Leave 1337 open directly, or disable it (`SHPELLCAST_TCP=`) and
have everyone use `curl -T-` over HTTPS.

---

## Step 4 — Verify

```bash
systemctl status shpellcast          # should be active (running)
curl -s localhost:8080/healthz       # {"status":"ok", ...}
```

Then send something real and open the link:

```bash
for i in $(seq 1 20); do echo "line $i"; sleep 0.2; done | nc shpellcast.godtech.in 1337
```

If the page fills in live, you're done. If it doesn't, see
[Troubleshooting](#troubleshooting).

---

## Configuration

One file: `/etc/shpellcast/shpellcast.env`. It ships fully commented.
Edit, then `sudo systemctl restart shpellcast`.

Every setting has a flag twin (`SHPELLCAST_IDLE_TTL` ⇄ `-idle-ttl`), so
the same binary configures identically under systemd, Docker or a shell.

**The ones that matter**

| Setting | Default | What it controls |
|---|---|---|
| `SHPELLCAST_BASE_URL` | *derived* | Origin printed in links. **Always set this in production** — otherwise links show the raw IP or a wrong hostname. |
| `SHPELLCAST_HTTP` | `:8080` | Web UI and API listener. Use `127.0.0.1:8080` behind a proxy. |
| `SHPELLCAST_TCP` | `:1337` | Pipe endpoint. Leave empty to disable it. |
| `SHPELLCAST_TRUST_PROXY` | `false` | Honour `X-Forwarded-*`. Turn on **only** behind your own proxy. |

**Retention and limits**

| Setting | Default | What it controls |
|---|---|---|
| `SHPELLCAST_BUFFER` | `256KB` | Output kept per session |
| `SHPELLCAST_QUOTA` | `64MB` | Output accepted per session before the producer is cut off |
| `SHPELLCAST_IDLE_TTL` | `24h` | Expiry after the last write |
| `SHPELLCAST_CLOSED_TTL` | `6h` | Expiry after the job finishes |
| `SHPELLCAST_MAX_TTL` | `168h` | Hard expiry from creation |
| `SHPELLCAST_MAX_SESSIONS` | `2000` | Server-wide session cap |
| `SHPELLCAST_MAX_PER_CLIENT` | `250` | Sessions **per source IP** — see the warning below |
| `SHPELLCAST_SPILL_DIR` | *off* | Mirror sessions to disk so they survive restarts |
| `SHPELLCAST_INDEX` | `false` | Publish the list of live sessions |
| `SHPELLCAST_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

> ⚠️ **`MAX_PER_CLIENT` counts sessions per source address, not per
> person.** Producers behind NAT, a CI runner fleet, or a reverse proxy
> without `TRUST_PROXY` all arrive as *one* address — so this caps your
> whole fleet. The default of 250 is deliberately generous for that
> reason. Raise it if a single CI host runs more concurrent jobs than
> that.

**Sizing.** Memory is roughly `MAX_SESSIONS × BUFFER`, but buffers grow on
demand from 4 KB, so real usage is far lower — a session printing 3 KB
costs 4 KB, not the 256 KB it's allowed. A few hundred concurrent sessions
fit comfortably in ~100 MB. If you raise the session cap or the buffer
size, raise `MemoryMax` in the systemd unit to match.

**Keeping output longer.** Sessions are ephemeral by design. Download one
while it's alive (**Download** in the UI), or set
`SHPELLCAST_SPILL_DIR=/var/lib/shpellcast` to mirror every session to a
file that survives restarts. Mirrored, not hoarded — the file is removed
when the session expires.

---

## Day-to-day operation

```bash
systemctl status shpellcast        # is it up?
systemctl restart shpellcast       # after a config change
journalctl -u shpellcast -f        # live logs
curl -s localhost:8080/healthz     # health, uptime, live session count
curl -s localhost:8080/metrics     # Prometheus text format
```

**Upgrading** — `git pull && sudo ./install.sh`. Config is preserved.

**Uninstalling** — `sudo ./install.sh --uninstall`. Config and data are
left behind deliberately; delete `/etc/shpellcast` and
`/var/lib/shpellcast` yourself if you want them gone.

**Restarts are graceful.** `SIGTERM` drains: live viewers are released and
in-flight producers closed cleanly rather than killed, so restarting
during someone's build doesn't lose the tail of it.

**If you scrape Prometheus**, the two worth alerting on:

- `shpellcast_viewers_dropped_total` rising → viewers can't keep up.
  Producers are never slowed down, so this is where that pressure shows.
- `go_goroutines` rising while `shpellcast_sessions_live` is flat →
  something is leaking.

---

## Troubleshooting

<details>
<summary><b>The page stays blank until the job finishes</b></summary>

Your reverse proxy is buffering the response. nginx does this by default.
Use the shipped `deploy/nginx.conf`, or copy its `proxy_buffering off;`
and `proxy_request_buffering off;` directives onto the `/s/.../stream`,
`/s/.../raw` and `/i` routes.
</details>

<details>
<summary><b>Links show the wrong hostname, or a raw IP</b></summary>

`SHPELLCAST_BASE_URL` isn't set. Without it, links are built from the
request's `Host` header, and the netcat banner falls back to the listen
address. Set it to your public origin and restart.
</details>

<details>
<summary><b>`nc` just hangs, or connection refused</b></summary>

Port 1337 isn't reachable. Check the host firewall (`sudo ufw status`)
*and* your cloud provider's security group — they're separate, and the
cloud one is usually the culprit. Confirm the service is listening:
`sudo ss -lntp | grep shpellcast`.
</details>

<details>
<summary><b>Producers are being refused</b></summary>

Check which limit hit:

```bash
curl -s localhost:8080/metrics | grep rejected
```

`..._rejected_perclient_total` rising means `MAX_PER_CLIENT` — your
producers share a source address (see the warning in
[Configuration](#configuration)). `..._rejected_capacity_total` means the
server-wide `MAX_SESSIONS` cap.
</details>

<details>
<summary><b>The service won't start</b></summary>

```bash
journalctl -u shpellcast -n 50 --no-pager
```

Most often a port is already in use, or a typo in
`/etc/shpellcast/shpellcast.env` — the binary validates its config at
startup and says exactly what's wrong.
</details>

<details>
<summary><b>Output appears, but colours show as escape codes</b></summary>

The producing program disabled colour because it isn't writing to a
terminal. Force it on: `FORCE_COLOR=1`, `--color=always`, or
`CLICOLOR_FORCE=1`, depending on the tool.
</details>

---

## Other ways to run it

<details>
<summary><b>Docker</b></summary>

```bash
docker build -t shpellcast .
docker run -p 8080:8080 -p 1337:1337 \
  -e SHPELLCAST_BASE_URL=https://shpellcast.godtech.in shpellcast
```

</details>

<details>
<summary><b>No systemd, or just trying it out</b></summary>

```bash
make build
./bin/shpellcast -http :8080 -tcp :1337 \
  -base-url http://$(hostname -I | awk '{print $1}'):8080
```

</details>

<details>
<summary><b>Kubernetes, Nomad, anything else</b></summary>

One static binary that reads environment variables and logs to stderr.
Nothing special required. Note that it doesn't cluster — run one replica,
or pin viewers to an instance by session ID.

</details>

---

## Security model

Be clear-eyed: **anyone who can reach the pipe port can create a session,
and anyone holding a session URL can read it.** That's the right trade for
sharing build output, and the wrong one for secrets.

| Control | Detail |
|---|---|
| **Unguessable links** | IDs from `crypto/rand`; 80 bits at the default length. Shorter than 50 bits is refused. |
| **Never enumerable** | The session list is disabled unless you turn it on. Pages carry `noindex` and `no-referrer`. |
| **Authorised writes** | Appending to a session requires a per-session token, compared in constant time. A URL alone is read-only. |
| **Bounded everything** | Buffer per session, byte quota per session, session cap per source IP, idle timeout on producer sockets, three independent expiry timers. |
| **No forged identity** | `X-Forwarded-For` is ignored unless you declare a trusted proxy — it's trivially spoofed and it gates the per-client limits. |
| **Output can't become script** | Strict CSP (`default-src 'none'`), no inline script or style, DOM built from text nodes. Output is never parsed as HTML. |
| **Hardened service** | `DynamicUser`, zero capabilities, `ProtectSystem=strict`, `MemoryDenyWriteExecute`, syscall filter, `AF_INET`/`AF_INET6` only. |

**Need real authentication?** Put it in the reverse proxy — `basic_auth`
in Caddy, `auth_request` in nginx, or an SSO forward-auth — and keep the
pipe port on a private interface.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

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

## Development

```bash
make build      # bin/shpellcast for this machine
make check      # gofmt + go vet + go test -race
make run        # run locally with debug logging
make release    # static binaries for linux amd64/arm64/arm + macOS
```

```
cmd/shpellcast/     entrypoint: flags, logger, signals
internal/config/    flags + environment, validation
internal/id/        crypto/rand capability IDs
internal/session/   ring buffer, fan-out, store, expiry
internal/server/    pipe ingest, HTTP handlers, embedded web assets
  web/              viewer, landing page, client helper (compiled in)
deploy/             systemd unit, env file, Caddy and nginx configs
```

**No third-party dependencies** — `go.mod` has no `require` block, and
that's deliberate: nothing to audit, nothing to patch urgently, and
`go build` works on any machine with a toolchain and no network.

Tests are end-to-end where it matters — real sockets, real streams, real
reconnects — because the interesting behaviour lives in the seams between
a socket, a buffer and a browser.

Contributions welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

MIT — see [LICENSE](LICENSE). Use it commercially, fork it, rebrand it.

<div align="center">

**[Quick start](#quick-start)** · **[Self-hosting](#self-hosting)** ·
**[Report an issue](https://github.com/thepurveshpanchal/shpellcast/issues)** ·
**[Security](SECURITY.md)**

</div>