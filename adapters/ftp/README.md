# adapters/ftp

`github.com/ubgo/filesystem/adapters/ftp` is the `filesystem.Driver` for FTP and FTPS servers, on `github.com/jlaffaye/ftp`, with a connection pool and atomic writes. It is its own module.

The guide is [docs/adapters/ftp.md](../../docs/adapters/ftp.md).

```sh
go get github.com/ubgo/filesystem/adapters/ftp
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Addr` | `string` | required | `host:port`; port 21 when missing |
| `Username`, `Password` | `string` | `anonymous` | Login |
| `Root` | `string` | required, absolute | The directory holding the disk; created if missing |
| `TLS` | `*tls.Config` | nil | Explicit FTPS (`AUTH TLS`), or implicit with `ImplicitTLS`; without `ServerName` the certificate is checked against the host of `Addr`, on the control and every data connection |
| `ImplicitTLS` | `bool` | false | TLS from the first byte (port 990); needs `TLS` |
| `MaxConns` | `int` | `DefaultMaxConns` (4) | Pooled control connections |
| `DialTimeout` | `time.Duration` | `DefaultDialTimeout` (30 s) | Dial and login without a context deadline |
| `DataTimeout` | `time.Duration` | `DefaultDataTimeout` (2 min) | Each wait for a download's next bytes; a stall is `CodeUnavailable` |
| `IdleCheck` | `time.Duration` | `DefaultIdleCheck` (15 s) | How long a pooled connection may idle before its next use checks it with `NOOP` |
| `StagingDir` | `string` | `".filesystem"` (`StagingDir`) | The reserved directory directly under `Root` for staged and moved-aside files; one path segment |
| `StagingPath` | `string` | empty | Stage in this absolute server directory outside `Root` instead (same filesystem, so writes are still renamed in), for a write-only drop folder that refuses `MKD` inside `Root`; created if missing; reserves no key. Exclusive with `StagingDir` and `WriteInPlace` |
| `WriteInPlace` | `bool` | false | No staging at all: each write is a `STOR` straight to its key, for a drop folder where nothing else may be created. Needs `AllowNonAtomicWrites`: readers can see a file while it arrives, an overwrite truncates first, and a failed `STOR` deletes the file. A `Move` onto an existing key on a server that refuses to rename over a file deletes the target first, with no rollback. `New` creates nothing (`Root` must exist), `SweepAbandoned` and `FindAbandoned` are not declared, and letter case is not probed |
| `AllowNonAtomicWrites` | `bool` | false | Acknowledges `WriteInPlace`'s weaker guarantee; required with it, no other effect |
| `Dial` | `func(ctx, network, addr) (net.Conn, error)` | a `net.Dialer` with `DialTimeout` | Opens the control and every data connection (SOCKS, a bastion, a source address); TLS is added on top |
| `DialOptions` | `[]ftp.DialOption` | none | jlaffaye/ftp switches after the driver's own (`DialWithDisabledEPSV`, `DialWithDisabledMLSD`, ...); never the connection or TLS options |

`New(ctx, cfg)` returns `CodeInvalidArgument` for a bad config (a negative `IdleCheck`, a `StagingDir` that is not one path segment, a `StagingPath` that is relative, `Root`, inside it or containing it, `WriteInPlace` without `AllowNonAtomicWrites` or with a staging field), logs in, creates `Root` and the staging directory and probes letter case (under `WriteInPlace` it only logs in) (wrong credentials: `CodePermission`). `Config` prints and logs with the password redacted. `Close` closes the pool.

## DSN

`ftp://<user>@<host>[:<port>]/<absolute root>?<parameters>`, `ftps://` for explicit FTPS, or `ftps-implicit://` for implicit FTPS (port 990 when none is given); register each with `mux.Register(ftp.Scheme, ftp.Opener)`, `mux.Register(ftp.SchemeSecure, ftp.Opener)` and `mux.Register(ftp.SchemeImplicit, ftp.Opener)`. Both FTPS schemes verify the certificate against the system roots or `ca_file`, and keep a TLS session cache, so data connections resume the control connection's session as vsftpd's default `require_ssl_reuse` demands. A password in the URL is refused.

| Constant | Parameter | Meaning |
|---|---|---|
| `ParamPasswordEnv` | `password_env` | environment variable holding the password; needs a user |
| `ParamMaxConns` | `max_conns` | positive integer, `Config.MaxConns` |
| `ParamCAFile` | `ca_file` | PEM bundle of the authorities verifying the server (an internal CA, a self-signed certificate); FTPS schemes only |

## Capabilities

`Move`, `DeletePrefix`, `SweepAbandoned`, `FindAbandoned`, `Dirs`; `CaseInsensitive` when the probe finds the server folds case. Never: copy, visibility, metadata, content type, headers, conditional writes, URLs, multipart.

## Behaviour

- Writes `STOR` to `StagingDir/tmp/<random>` and rename into place. Only a server that refuses because the target exists (IIS's `550 … already exists`) gets a replace: the old object is renamed aside into `StagingDir/tmp`, the new one renamed in and the old one deleted; if renaming in fails, the old object is renamed back and the write is `CodePermission`. The key is briefly missing between the two renames. Only if renaming back fails too does `Write` (or `Move`) return a `*StrandedError` naming both kept contents (`Staged`, `Previous`, `Target`). Another refusal is `CodePermission` and leaves the old object. Keys under the staging directory (`.filesystem`, or `Config.StagingDir`) are refused and listings skip it.
- A reader is bound to its call's context and to `DataTimeout`.
- Each call takes a pooled connection; an idle one older than `IdleCheck` (`DefaultIdleCheck`) is checked with `NOOP`. A context ending closes the control socket; a non-reply failure is `CodeUnavailable` and its connection is closed.
- `Stat`: `MLST`, else `SIZE` and `MDTM`. `List`: a walk with `MLSD` or `LIST`, skipping names that are not clean keys. Delete and Move prune empty parent directories.
- `SweepAbandoned(ctx, before)` removes staged files older than `before`, only those named the way `Write` names them; `FindAbandoned(ctx, before)` lists the same files without removing them.
- `Do(ctx, fn)` runs `fn` on a pooled `*ftp.ServerConn`, the escape hatch for what the driver does not cover (setting a modification time, recursive removal, raw listings, paths outside `Root`; the client library sends no arbitrary command, so not `SITE`); paths there are the server's, not keys. A server reply keeps its code and the connection, a `*filesystem.Error` from `fn` passes through, any other error is read as a lost connection.
- `MakeDir(ctx, dir)` creates `dir` and its parents (`MKD`); a file at `dir` is `CodeExists`, a file on its path `CodeInvalidArgument`. `DirExists(ctx, dir)` reports a directory, empty or not. Deleting the last file in a directory removes it.

## Tests

`go test ./...` runs the conformance suite against `github.com/fclairamb/ftpserverlib` in-process (a test-only dependency, passive ports 31000-31999), plus explicit FTPS with a self-signed certificate, redaction, the pool, atomic writes, early-closed reads, context cancellation of a stalled command, the opener, the probe and the sweep. `deps_test.go` keeps production code to the standard library, the library and jlaffaye/ftp.
