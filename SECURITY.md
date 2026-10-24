# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Report them privately:

- Use **[GitHub Security Advisories](https://github.com/ubgo/filesystem/security/advisories/new)** (preferred), or
- Email **khanakia@gmail.com** with the details.

Please include a description of the vulnerability and its impact, steps to reproduce (a failing test is ideal), and the affected version and configuration (driver, DSN parameters with secrets removed, records engine and plugins, handlers in use).

We will acknowledge your report within a few days and keep you updated on the fix. Please give us a reasonable window to release a fix before any public disclosure; we are happy to credit you in the advisory. Every fixed vulnerability is listed in [CHANGELOG.md](CHANGELOG.md) under a **[security]** tag.

## Supported versions

| Version | Supported |
|---|---|
| latest `v0.x` | ✅ |
| any earlier `v0.x` | ❌ |

The library is pre-release and may still break, so the only supported version is the newest one; fixes are not backported.

## Scope

This is a storage library that sits between untrusted clients and an application's files, so a defect in what it lets a caller read, write or learn counts as a vulnerability. In scope, among others:

- Reaching an object outside the key space the application gave: path traversal, a key the `Disk` cleans differently from what it checks, escaping a scoped disk or a tenant's scope, writing to a key reserved by a driver.
- A signed URL that is accepted after its expiry, with a changed key, method, content type or pinned header, or with a signature from a key the signer no longer holds.
- An upload stored although its size, type, declared hash or policy should have refused it, or an upload refused but left behind in storage.
- Content a browser would run (HTML, SVG, scripts) served inline from the application's origin, or served without `nosniff` and the sandboxing content security policy.
- A private file reachable through a public URL, a variant, a share link or a CDN path after it was made private, trashed or purged, beyond what the documentation states a cache may keep.
- A secret (signing key, cloud credential, connection string) or a storage key, disk name or internal address written to an error a client sees, a log line or a formatted value.
- A server-side request (an import from a URL) that reaches an address the guard should have refused.
- Request data reaching SQL text rather than a bound parameter.

Out of scope: weaknesses that need the attacker to control the application's configuration, its credentials or its database, denial of service by volume alone (size limits, quotas and the application's rate limits are the mitigation), and what a backend or CDN does outside this library.
