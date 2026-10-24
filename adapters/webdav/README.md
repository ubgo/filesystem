# adapters/webdav

`github.com/ubgo/filesystem/adapters/webdav` is the `filesystem.Driver` for WebDAV servers (Nextcloud, ownCloud, Box, NAS boxes, Apache and nginx dav), over `net/http` with no dependency beyond the library. It is its own module.

The guide is [docs/adapters/webdav.md](../../docs/adapters/webdav.md).

```sh
go get github.com/ubgo/filesystem/adapters/webdav
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Endpoint` | `string` | required | http or https URL of the collection holding the disk; no credentials in it |
| `Client` | `*http.Client` | a client with `DefaultTimeout` (10 min) | Transport, proxy, TLS |
| `Username`, `Password` | `string` | none | Basic authentication when `Username` is set |
| `AllowInsecureHTTP` | `bool` | false | Permits an http `Endpoint` on a host other than loopback |
| `StagingDir` | `string` | `".filesystem"` (`StagingDir`) | The reserved collection directly under the endpoint for staged files; one path segment. Keys under it are refused and listings skip it |
| `StagingPath` | `string` | empty | Stage in this absolute path on the endpoint's server, outside the endpoint's collection, instead (its parent must exist; the server must allow the `MOVE` in), for a write-only drop folder that refuses `MKCOL` inside the endpoint; created if missing; reserves no key. Exclusive with `StagingDir` and `WriteInPlace` |
| `WriteInPlace` | `bool` | false | No staging at all: each write is a `PUT` straight to its key (a nested key's parents are made first), for a drop folder where nothing else may be created. Needs `AllowNonAtomicWrites`: readers can see a file while it arrives and a failed `PUT` deletes the key. `New` sends no request, `SweepAbandoned` and `FindAbandoned` are not declared, and letter case is not probed |
| `AllowNonAtomicWrites` | `bool` | false | Acknowledges `WriteInPlace`'s weaker guarantee; required with it, no other effect |

`New(ctx, cfg)` returns `CodeInvalidArgument` for a bad endpoint, a plain http one off loopback without `AllowInsecureHTTP`, a `StagingDir` that is not one path segment, a `StagingPath` that is relative, the endpoint's path, inside it or containing it, or `WriteInPlace` without `AllowNonAtomicWrites` or with a staging field, then (unless `WriteInPlace`) creates the staging collection and probes letter case within ctx (wrong credentials: `CodePermission`). `Config` prints and logs with the password redacted.

## DSN

`webdavs://<user>@<host>/<path>?password_env=<VAR>` (https) or `webdav://…` (http), registered with `mux.Register(webdav.Scheme, webdav.Opener)` and `mux.Register(webdav.SchemeSecure, webdav.Opener)`. A password in the URL is refused.

| Constant | Parameter | Meaning |
|---|---|---|
| `ParamPasswordEnv` | `password_env` | environment variable holding the password; needs a user |
| `ParamInsecureHTTP` | `insecure_http` | boolean, `Config.AllowInsecureHTTP`; needed for `webdav://` to a host other than loopback |

## Capabilities

`Copy`, `Move`, `DeletePrefix`, `SweepAbandoned`, `FindAbandoned`, `Dirs`; `CaseInsensitive` when the probe finds the server folds case. Never: visibility, metadata, content type, headers, conditional writes, URLs, multipart.

## Behaviour

- Writes `PUT` to `StagingDir/tmp/<random>` and `MOVE` into place (`Overwrite: T`); keys under `.filesystem` (or `Config.StagingDir`) are refused and listings skip it. A replace is not atomic: the server deletes the old object before the move, so a reader can briefly get `CodeNotFound`. The staging `PUT` carries `Expect: 100-continue`, so no body byte leaves before the server has started the `PUT` and a failed body's cleanup `DELETE` cannot overtake it and leave the staged file behind; that costs one extra round trip per write (and up to `ExpectContinueTimeout`, one second, on a server that ignores `Expect`). `New` sets that timeout on a copy of a caller's `*http.Transport` that has none.
- Redirects are followed only within the endpoint's scheme and host.
- Parent collections are created on demand and never removed.
- `List` walks with depth-1 `PROPFIND`s; hrefs outside the endpoint or not clean keys are dropped.
- Reads use `Range`; a server answering 200 is cut to the range.
- `SweepAbandoned(ctx, before)` removes staged files older than `before`, only those named the way `Write` names them; `FindAbandoned(ctx, before)` lists the same files without removing them.
- `MakeDir(ctx, dir)` creates `dir` and its parents as collections (`MKCOL`); a file at `dir` is `CodeExists`, a file on its path `CodeInvalidArgument`. `DirExists(ctx, dir)` reports a collection, empty or not. Deleting files never removes a collection; `DeletePrefix` does.
- `Native()` returns the `*http.Client`.

## Tests

`go test ./...` runs the conformance suite against `golang.org/x/net/webdav` served in-process with Basic authentication (a test-only dependency), plus redaction, staging, escaping, status mapping, hostile hrefs, the opener, the probe and the sweep. `deps_test.go` keeps production code to the standard library and the library.
