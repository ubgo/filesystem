# adapters/sftp

`github.com/ubgo/filesystem/adapters/sftp` is the `filesystem.Driver` for SFTP servers, on `github.com/pkg/sftp` and `golang.org/x/crypto/ssh`. Writes are atomic (`posix-rename@openssh.com`), conditional writes use hard links (`hardlink@openssh.com`), and visibility is the file mode. It is its own module.

The guide is [docs/adapters/sftp.md](../../docs/adapters/sftp.md).

```sh
go get github.com/ubgo/filesystem/adapters/sftp
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Client` | `*sftp.Client` | required unless `Dial` is set | A connected client |
| `Root` | `string` | required, absolute | The directory on the server holding the disk; created if missing |
| `Closer` | `io.Closer` | nil | Closed by `Close`, or when `Dial` replaces `Client`'s dropped connection |
| `Dial` | `sftp.Dialer` | nil | `func(ctx) (*sftp.Client, io.Closer, error)`; makes the driver reconnect after a dropped connection, and dials the first connection when `Client` is nil |
| `AllowNonAtomicWrites` | `bool` | false | Accepts a server without `posix-rename@openssh.com`: replacing an existing file then removes it and renames the new one in two steps, so a reader can find no file between them and a connection lost there leaves the key empty. New keys stay atomic; servers with the extension still use it |
| `StagingDir` | `string` | `".filesystem"` (`StagingDir`) | The reserved directory directly under `Root` that holds staged files; one path segment. Keys under it are refused and listings skip it |
| `StagingPath` | `string` | empty | Stage in this absolute server directory outside `Root` instead (same filesystem, so writes stay atomic renames), for a write-only drop folder that refuses mkdir inside `Root`; created if missing; reserves no key. Exclusive with `StagingDir` and `WriteInPlace` |
| `WriteInPlace` | `bool` | false | No staging at all: each write goes straight to its key, for a drop folder where nothing else may be created. Needs `AllowNonAtomicWrites`: readers can see a file while it arrives, an overwrite truncates first, and a failed write removes the file. `IfNotExists` stays atomic (exclusive create), `New` creates nothing (`Root` must exist), `SweepAbandoned` and `FindAbandoned` are not declared, and letter case is not probed |
| `FilePublic`, `FilePrivate` | `fs.FileMode` | `0644`, `0600` | Modes for public and private files. With either set, a file reads as public when its mode has a bit `FilePublic` has and `FilePrivate` lacks; a pair without one is refused |
| `SkipChmod` | `bool` | false | Writes keep the mode the server gives new files, for servers that refuse `SETSTAT`; `SetVisibility` is then not declared |

`New` returns `CodeInvalidArgument` without `Client` or `Dial`, with a relative `Root`, a `StagingDir` that is not one path segment, a `StagingPath` that is relative, `Root`, inside it or containing it, `WriteInPlace` without `AllowNonAtomicWrites` or with a staging field, or modes whose visibility could not be read back, `CodeUnsupported` when the server lacks `posix-rename@openssh.com` and `AllowNonAtomicWrites` is off, and the server's error when `Root` cannot be created.

## DSN

`sftp://<user>@<host>[:<port>]/<absolute root>?<parameters>`, registered with `mux.Register(sftp.Scheme, sftp.Opener)`. The port defaults to 22. A password in the URL is refused.

| Constant | Parameter | Meaning |
|---|---|---|
| `ParamKnownHosts` | `known_hosts` | known_hosts file verifying the server; required unless `insecure_ignore_host_key=true` |
| `ParamInsecure` | `insecure_ignore_host_key` | boolean; skips host key verification (tests only) |
| `ParamIdentity` | `identity` | private key file for public-key authentication |
| `ParamPasswordEnv` | `password_env` | environment variable holding the password |
| `ParamIdentityPassphraseEnv` | `identity_passphrase_env` | environment variable holding the passphrase of an encrypted `identity` key |
| `ParamAgent` | `agent` | boolean; authenticates with the keys of the ssh-agent at `SSH_AUTH_SOCK` (hardware keys, agent-held certificates), dialled for each handshake |

At least one of `identity`, `password_env` and `agent=true` is required. Configuration errors are `CodeInvalidArgument`; a failed dial or a server without the SFTP subsystem is `CodeUnavailable`; a failed handshake (credentials or host key) is `CodePermission`.

## Capabilities

Always: `Move`, `SetVisibility` (unless `SkipChmod`), `DeletePrefix`, `SweepAbandoned` and `FindAbandoned` (the sweep's dry run, from the same scan; not with `WriteInPlace`), `Dirs` (`MakeDir` creates directories on the server, `DirExists` reports them; deleting the last file in one removes it). When the server offers `hardlink@openssh.com`, or with `WriteInPlace`: `ConditionalWrite`. When the server's filesystem folds letter case (macOS, Windows, many NAS shares), found by a probe file in the staging directory when the disk opens: `CaseInsensitive` (a read-only account cannot write the probe and is reported case-sensitive). Never: `Copy` (the Disk streams), `DeleteKeys`, `Presign`, `PublicURL`, `Multipart`, `Metadata`, `ContentType` (derived from the extension), `Headers`.

## Behaviour

- Writes stage a file under the staging directory (`.filesystem/tmp` inside `Root`, `Config.StagingDir`, or `Config.StagingPath` outside it), chmod it (unless `SkipChmod`), and rename or hard-link it into place. Keys under the staging directory are refused with `CodeInvalidKey`; listings skip it.
- A crash or a dropped connection between staging and the rename leaves the staged file behind. `SweepAbandoned(ctx, before)` removes staged files last modified before `before` (a staged file's modification time advances while its bytes arrive) and returns how many. It removes only regular files named the way `Write` names them, never stored files or anything else placed in the staging directory.
- Modes: `DefaultFilePublic` (`0644`) and `DefaultFilePrivate` (`0600`) unless `FilePublic` and `FilePrivate` say otherwise; `Stat` reads visibility from the world-read bit with the defaults, and from the bits public has and private lacks with custom modes.
- `Stat` and `Read` use `Lstat`: directories and symlinks are never objects (`CodeNotFound`).
- Delete and Move remove directories left empty, up to `Root`.
- `List` walks the tree (O(n) per page).
- One SSH connection serves every call. A lost connection is `CodeUnavailable` for the calls in flight when it dropped. With `Dial` set (the opener always sets it), the next call redials; one call dials at a time and the others wait for it only as long as their own context allows, the dial stops when the dialing call's context ends (the opener closes the socket, which interrupts the handshake and the SFTP start; `DefaultDialTimeout` bounds a context without a deadline), and `Close` never waits for it, re-checks the server's extensions and continues on the new connection; a failed redial returns the dial's error and the call after tries again. The driver never repeats an operation itself, since a write's body is consumed and a rename may have landed: retry with `layer.Retry`. Without `Dial`, every later call is `CodeUnavailable` until the disk is reopened.
- Capabilities are fixed when the disk opens. If a reconnect reaches a server without `hardlink@openssh.com`, conditional writes are `CodeUnsupported`; one without `posix-rename@openssh.com` is refused (`CodeUnsupported`) and redialled on the next call, unless `AllowNonAtomicWrites` accepts it.
- `Close` closes the current connection and stops reconnecting; a dial finishing after it is closed, and a second `Close` does nothing.
- A write whose body fails returns the body's error and stores nothing; it is not mistaken for a lost connection.
- Symlinks created on the server can point outside `Root`; chroot the account (`ChrootDirectory`) for containment.
- `Native()` returns the current `*sftp.Client` for `filesystem.As[*sftp.Client]`; a reconnect replaces it, so do not hold it across operations.

## Tests

`go test ./...` runs the conformance suite against `pkg/sftp`'s server over in-process pipes, and the opener against an in-process SSH server (host keys, key and password authentication, refused handshakes, a server without SFTP). `reconnect_test.go` kills the in-process server's connection mid-run and checks the redial, the shared single dial under concurrency, failed redials, the extension re-check, `Close`, and the opener's redial through a TCP proxy whose connections it cuts.
