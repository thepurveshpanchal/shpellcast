# Security Policy

## Reporting a vulnerability

Please report security issues privately rather than opening a public
issue.

**Email:** `security@YOUR-DOMAIN-HERE`
*(placeholder — replace with a real address before publishing)*

Include what you can: affected version (`shpellcast -version`), a
description of the issue, and steps to reproduce. A proof of concept is
welcome but not required.

**What to expect:**

| | |
|---|---|
| Acknowledgement | within 3 working days |
| Initial assessment | within 7 working days |
| Fix or mitigation plan | communicated before public disclosure |

We will credit you in the release notes unless you'd rather stay
anonymous. Please give us a reasonable window to ship a fix before
disclosing publicly.

## Supported versions

The latest released version receives security fixes. This project is small
enough that back-porting is rarely necessary — upgrading is replacing one
binary.

## Threat model — read this first

shpellcast is **unauthenticated by design**. Before reporting, check
whether the behaviour is one of these deliberate trades:

**In scope — please report:**

- Session IDs that are predictable, enumerable, or leak between sessions
- Reading or writing a session without its URL or token
- Injecting output into a session you don't own
- Cross-site scripting from piped command output, or any CSP bypass
- Path traversal, SSRF, or reading files outside the configured directories
- Resource exhaustion that survives the documented limits (session caps,
  byte quotas, TTLs) — e.g. a memory or goroutine leak
- Privilege escalation from the shipped systemd unit
- Timing or other side channels that reveal session tokens

**Out of scope — working as intended:**

- *Anyone with the link can read the output.* The URL is the credential.
  This is the core design; see "Security model" in the README.
- *Anyone who can reach the ingest port can create a session.* Restrict
  network access or put authentication in your reverse proxy if that
  matters for your deployment.
- *Session listing is exposed when `-index` is enabled.* That flag is
  off by default and documented as a capability leak.
- *`X-Forwarded-For` is trusted when `-trust-proxy` is enabled.* That flag
  is off by default and documented as proxy-only.
- *Output is not encrypted at rest when `-spill-dir` is set.* Mirrored
  files inherit the permissions of the directory you configure.
- Missing rate limiting beyond the documented per-client session cap.

## Hardening checklist for operators

If you run a public instance:

- [ ] Terminate TLS at a reverse proxy; never expose plain HTTP publicly
- [ ] Set `SHPELLCAST_BASE_URL` so links are correct and unambiguous
- [ ] Enable `SHPELLCAST_TRUST_PROXY` **only** when the service is reachable
      exclusively through your proxy
- [ ] Keep `SHPELLCAST_INDEX=false`
- [ ] Restrict `/metrics` to your monitoring network (the shipped nginx
      config shows how)
- [ ] Keep the shipped systemd unit's hardening directives intact
- [ ] Set `MemoryMax` to match your session cap and buffer size
- [ ] Decide whether the pipe port should be internet-facing at all —
      `SHPELLCAST_TCP=` disables it and leaves HTTPS ingest only
- [ ] Tell your users: links are shareable secrets, and nothing sensitive
      should be piped through a public instance
