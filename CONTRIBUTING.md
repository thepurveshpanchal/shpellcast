# Contributing

Thanks for considering it. This project is intentionally small, and the
goal is to keep it that way.

## Design principles

Changes are judged against these. A patch that conflicts with one needs a
good argument, not just working code.

1. **No third-party dependencies.** `go.mod` has no `require` block and
   should stay that way. The standard library has covered everything so
   far. A dependency means an audit surface and an upgrade treadmill for
   a tool people deploy and forget.
2. **One binary, no runtime.** Assets are embedded. Deploying is copying
   a file. Nothing may require a sidecar, a database, or a package
   manager on the host.
3. **Everything is bounded.** This service accepts unauthenticated writes.
   Any new code path that allocates, spawns, or holds a descriptor needs a
   documented limit and a way to observe it.
4. **Producers are never slowed down.** A slow or hostile *viewer* must
   never apply backpressure to a running job. Dropping a viewer is always
   the correct choice over blocking a write.
5. **It is not a log platform.** Search, indexing, retention policies and
   alerting are out of scope — see the roadmap in the README.

## Getting set up

```bash
git clone https://github.com/YOUR-ORG/shpellcast.git && cd shpellcast
make build
make check        # gofmt + go vet + go test -race
./bin/shpellcast -log-level debug
```

In another terminal:

```bash
printf 'hello\n' | nc 127.0.0.1 1337
```

Go 1.22 or newer. No other tooling required.

## Before opening a pull request

- [ ] `make check` passes (formatting, vet, and tests under the race detector)
- [ ] New behaviour has a test. Prefer end-to-end tests over mocks —
      real sockets and real HTTP, as the existing suite does.
- [ ] New limits, flags or endpoints are documented in `README.md` **and**
      `deploy/shpellcast.env`
- [ ] Anything affecting concurrency or memory says in the PR description
      how it was measured
- [ ] Commit messages explain *why*, not just *what*

## Code style

Standard `gofmt`, standard library idioms, no surprises.

The one local convention: **comments explain decisions, not mechanics.**
`// increment the counter` is noise. `// Drop the viewer rather than
block the producer — a stalled browser must never stall someone's build`
is the kind of comment this codebase wants, because it carries reasoning
that isn't recoverable from the code.

## Reporting bugs

Include:

- `shpellcast -version`
- OS and how you're running it (systemd, Docker, by hand)
- Relevant config (redact your domain if you like)
- What you expected and what happened
- `journalctl -u shpellcast` output around the event, if applicable

For anything security-related, use [SECURITY.md](SECURITY.md) instead of
a public issue.

## Proposing features

Open an issue describing the *problem* before writing code. Given
principle 5, the most useful contribution is often a convincing argument
that something does **not** belong here.

## License

Contributions are accepted under the MIT License, the same terms as the
rest of the project.
