# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing yet.

## [1.2.0] — 2026-10-27

Naming release. No behavioural change to the server beyond the rename and
the new alias — the wire protocol, API shape and defaults are unchanged.

### Changed

- **The project is now `shpellcast`.** Every identifier moved with it:
  the module path, both binaries, the systemd unit, the config directory
  (`/etc/shpellcast`), the state directory (`/var/lib/shpellcast`), the
  TCP banner, the Prometheus metric prefix (`shpellcast_*`), the HTTP
  headers (`X-Shpellcast-Token`, `X-Shpellcast-Url`) and the environment
  variable prefix (`SHPELLCAST_*`).
- **Documentation rewritten for a mixed audience.** The README now opens
  with the problem rather than the implementation, and adds a feature
  matrix, an honest pros-and-cons section, a comparison table, an FAQ and
  a roadmap. Engineering detail moved below the parts a non-engineer
  needs.

### Added

- **`shpell` short alias.** The installer symlinks it next to the real
  binary, and the client helper works under either name.
- **Hosted-service placeholder** in the README, ready for a domain.

### Upgrading from 1.1.0

The rename is not backward compatible, because every name changed. On an
existing host:

```bash
sudo systemctl stop shellcast && sudo systemctl disable shellcast
sudo rm -f /etc/systemd/system/shellcast.service /usr/local/bin/shellcast
sudo mv /etc/shellcast /etc/shpellcast 2>/dev/null || true
sudo sed -i 's/^SHELLCAST_/SHPELLCAST_/' /etc/shpellcast/shpellcast.env
sudo ./install.sh
```

Rename `shellcast.env` to `shpellcast.env` inside that directory first if
you kept the old filename. Scrape configs and dashboards need the metric
prefix updated; any script sending `X-Shellcast-Token` needs the new
header name. Live sessions do not survive the switch — they are in-memory
by design, so drain the host before upgrading if that matters.

## [1.1.0] — 2026-10-27

Capacity release. Everything here came out of load-testing the 1.0.0
defaults against 100+ concurrent sessions.

### Fixed

- **`-max-per-client` default raised from 50 to 250.** The limit counts
  sessions *per source address*, so a CI fleet behind NAT — or any
  deployment behind a reverse proxy without `-trust-proxy` — arrived as a
  single client and was capped at 50. A 100-session load test had exactly
  half its producers refused. A regression test now asserts the default
  can't drift back below 100.
- **`MemoryMax` in the systemd unit raised from 512M to 1G.** The previous
  value was below the worst case at the shipped 2000-session cap, so the
  service could have been OOM-killed precisely when busiest. The sizing
  formula is now documented in the unit file.
- **`install.sh` picks the highest version in `dist/`** instead of
  whatever the shell globs first, so a stale build left in the directory
  no longer silently wins over the current one.
- **`make race` works** — it now enables cgo, which the rest of the build
  deliberately disables to keep the shipped binary static.

### Changed

- **Session buffers grow on demand** (4 KB, doubling, up to the configured
  limit) instead of allocating their full size up front. A session that
  prints 3 KB now costs 4 KB of memory rather than 256 KB. At 1,000
  concurrent sessions this is the difference between ~380 MB and ~100 MB.

### Added

- **`cmd/shpellcast-bench`** — a load generator that opens N real producers
  and viewers, stamps every line with a timestamp and reads it back,
  reporting end-to-end latency percentiles, delivery rate, and the
  server's own metrics. Exposed as `make loadtest`.
- **Runtime and fan-out metrics:** `shpellcast_bytes_ingested_total`,
  `shpellcast_viewers_live`, `shpellcast_viewers_total`,
  `shpellcast_viewers_dropped_total`, plus standard `go_goroutines`,
  `go_memstats_*` and `process_open_fds`.
- **Rejection metrics split by reason:**
  `shpellcast_sessions_rejected_capacity_total` and
  `shpellcast_sessions_rejected_perclient_total`, so "the server is full"
  and "one client is capped" are distinguishable without reading logs.
- **Capacity line at startup**, stating the session cap, the per-client
  cap, and the worst-case memory at that cap — plus a warning if
  `-max-per-client` is set below 100.
- Published capacity figures in the README, measured rather than estimated.

### Measured

On 2 vCPUs, all sessions streaming with a live viewer attached:

| Sessions | Viewers | Delivered | p50 | p99 | Memory |
|---:|---:|---:|---:|---:|---:|
| 100 | 200 | 100% | 238 µs | 912 µs | 48 MB |
| 500 | 500 | 100% | 134 µs | 429 µs | 52 MB |
| 1 000 | 1 000 | 100% | 123 µs | 516 µs | 101 MB |

Throughput: 18,184 lines/s ingested across 100 sessions, 126 MB fanned
out, zero lines lost, p99 3.2 ms, 42% of one core.

## [1.0.0] — 2026-10-26

Initial release.

### Added

- Raw TCP ingest (`nc host 1337`) returning the session URL on connect.
- HTTP chunked ingest (`curl -T-`) for restrictive networks, with
  token-authorised appends to a reserved session.
- Browser viewer with a real terminal renderer: `\r` overwrite for
  progress bars, ANSI SGR colour (16/256/24-bit), `\b`, `\t`, line and
  screen erase, UTF-8 across chunk boundaries.
- Server-Sent Events transport with base64 framing, so arbitrary binary
  output can't break the protocol, and idempotent reconnects.
- Plain-text streaming endpoint for `curl -N`, plus download and metadata.
- Unguessable session IDs from `crypto/rand` with a configurable length
  floor of 50 bits.
- Per-session ring buffer, byte quota, per-client session cap, and three
  independent expiry timers.
- Optional disk mirroring so output survives restarts.
- Prometheus metrics, health endpoint, and structured logging.
- Hardened systemd unit, `install.sh`, Caddy and nginx configurations,
  Dockerfile, and cross-compiled release binaries.
- Zero third-party dependencies.

[Unreleased]: https://github.com/thepurveshpanchal/shpellcast/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/thepurveshpanchal/shpellcast/releases/tag/v1.1.0
[1.0.0]: https://github.com/thepurveshpanchal/shpellcast/releases/tag/v1.0.0
