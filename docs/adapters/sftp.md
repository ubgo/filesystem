# SFTP

The SFTP driver stores files on any SSH server with the SFTP subsystem: a partner's drop folder, a NAS, a legacy file server. Writes are atomic, moves are atomic renames, conditional writes use hard links, and visibility is the file mode. SFTP has no metadata, no stored headers and no signed URLs of its own.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/sftp`, a module of its own built on `github.com/pkg/sftp` and `golang.org/x/crypto/ssh`. Its reference is [adapters/sftp/README.md](../../adapters/sftp/README.md).
- The runnable program is [`examples/adapters-sftp`](../../examples/adapters-sftp/). It starts an in-process SSH server with the SFTP subsystem on a loopback port, writes a `known_hosts` file and a client key, and opens the disk through the DSN exactly as a deployment does. `go run ./adapters-sftp` from `examples/` needs no remote host.
- The server must offer the `posix-rename@openssh.com` extension (every OpenSSH server does). Without it `New` fails with `CodeUnsupported`, because writes could not be atomic, unless `Config.AllowNonAtomicWrites` accepts the weaker replace described in [Adapt the driver to the server](#10-adapt-the-driver-to-the-server).

## 1. Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/sftp
```

## 2. Open a disk from a DSN

The DSN form is `sftp://<user>@<host>[:<port>]/<absolute root>?<parameters>`. The path is the directory on the server that holds the disk, absolute, and created if missing. The port defaults to 22.

<!-- ds:block id=sftpdsn-3chvpnn6 -->
**sftpDSN** · [`examples/adapters-sftp/main.go:96-101`](../../examples/adapters-sftp/main.go#L96-L101)

```go
func sftpDSN(addr, root string, creds credentials) string {
	q := url.Values{}
	q.Set(fssftp.ParamKnownHosts, creds.KnownHosts)
	q.Set(fssftp.ParamIdentity, creds.Identity)
	return fssftp.Scheme + "://" + sftpUser + "@" + addr + root + "?" + q.Encode()
}
```
<!-- /ds:block hash=5751e7 -->

<!-- ds:block id=openwithdsn-n7y5ygk9 -->
**openWithDSN** · [`examples/adapters-sftp/main.go:106-112`](../../examples/adapters-sftp/main.go#L106-L112)

```go
func openWithDSN(ctx context.Context, addr, root string, creds credentials) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fssftp.Scheme, fssftp.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "partner", sftpDSN(addr, root, creds))
}
```
<!-- /ds:block hash=2dd9bf -->

A production DSN looks like:

```text
sftp://acme@sftp.partner.example:2222/upload/acme?known_hosts=/etc/app/known_hosts&identity=/run/secrets/acme_ed25519
```

Every parameter the opener understands, read from `adapters/sftp/opener.go`:

| Parameter | Value | Default | Meaning |
|---|---|---|---|
| `known_hosts` | path of a known_hosts file | required unless `insecure_ignore_host_key=true` | verifies the server's host key, so a man in the middle is refused |
| `insecure_ignore_host_key` | boolean | `false` | skips host key verification; only for tests and throwaway environments |
| `identity` | path of a private key file (OpenSSH or PEM) | none | public-key authentication |
| `identity_passphrase_env` | name of an environment variable | none | the passphrase of an encrypted `identity` key, read from that variable |
| `password_env` | name of an environment variable | none | password authentication, with the password read from that variable |
| `agent` | boolean | `false` | authenticates with the keys of the running ssh-agent (the socket `SSH_AUTH_SOCK` names), so hardware keys and certificates the agent holds work without a key file; the socket is dialled for each handshake |

Plus the Mux's own `prefix`, `public_url`, `visibility` and `readonly` (see [configuration](../concepts/configuration.md)). At least one of `identity` and `password_env` is required; with both, the key is tried first.

Secrets never appear in the DSN: a password in the URL (`sftp://acme:secret@…`) is refused, and the key and password are read from files and variables the DSN only names.

Opening dials the server, so errors are classified by where they happened:

| Failure | Code |
|---|---|
| No user, a password in the DSN, an unknown or malformed parameter, no `known_hosts` (without `insecure_ignore_host_key`), an unreadable `known_hosts` or key file, a key that does not parse (an encrypted one without its passphrase, or with a wrong one), `identity_passphrase_env` without `identity`, `agent=true` without `SSH_AUTH_SOCK`, no authentication method | `CodeInvalidArgument` |
| The TCP connection fails, the agent's socket cannot be reached, or the server has no SFTP subsystem | `CodeUnavailable` |
| The SSH handshake fails: wrong password or key, or a host key that does not match `known_hosts` | `CodePermission` |
| The root cannot be created (a file is in the way, no permission) | the server's error, `CodeUnknown` |

## 3. Host keys

`known_hosts` uses the OpenSSH format. Collect the server's key once, check its fingerprint with the server's owner, and ship the file with your deployment:

```sh
ssh-keyscan -p 2222 sftp.partner.example > known_hosts
ssh-keygen -lf known_hosts        # compare the fingerprint out of band
```

For a non-standard port, the line's host is written `[host]:port`, which `ssh-keyscan -p` produces. The program writes the same line with `knownhosts.Line`:

<!-- ds:block id=writeknownhosts-jg44gy3b -->
**writeKnownHosts** · [`examples/adapters-sftp/main.go:216-219`](../../examples/adapters-sftp/main.go#L216-L219)

```go
func writeKnownHosts(c credentials, addr string) error {
	line := knownhosts.Line([]string{addr}, c.hostKey.PublicKey())
	return os.WriteFile(c.KnownHosts, []byte(line+"\n"), 0o600)
}
```
<!-- /ds:block hash=5220d7 -->

A server whose host key changed (or an attacker in the middle) fails the handshake with `CodePermission`; the driver never trusts a key on first use.

## 4. Open a disk in code

Use `New` when you manage the SSH connection yourself: a jump host, an SSH agent, keep-alives, an encrypted key. Give it a connected `*sftp.Client`, the absolute root, and the SSH connection as `Closer` so `disk.Close()` closes both. To have the disk reconnect after a dropped connection, give it `Dial` as well: a function that opens a new connection the same way (see [connections](#8-connections-and-performance)).

<!-- ds:block id=openwithconfig-k4zbz2j8 -->
**openWithConfig** · [`examples/adapters-sftp/main.go:157-186`](../../examples/adapters-sftp/main.go#L157-L186)

```go
func openWithConfig(ctx context.Context, out io.Writer, addr, root string, creds credentials) error {
	hostKeys, err := knownhosts.New(creds.KnownHosts)
	if err != nil {
		return err
	}
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            sftpUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(creds.clientKey)},
		HostKeyCallback: hostKeys,
	})
	if err != nil {
		return err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		return errors.Join(err, conn.Close())
	}
	drv, err := fssftp.New(fssftp.Config{Client: client, Root: root, Closer: conn})
	if err != nil {
		return errors.Join(err, client.Close(), conn.Close())
	}
	disk, err := filesystem.NewDisk("partner-config", drv)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	ok, err := disk.Exists(ctx, "archive/2026-10/orders.csv")
	fmt.Fprintf(out, "via Config: archived file exists=%t\n", ok)
	return err
}
```
<!-- /ds:block hash=7f517c -->

| Field | Default | What it does |
|---|---|---|
| `Client` | required unless `Dial` is set | A connected `*sftp.Client`. |
| `Root` | required, absolute | The directory on the server holding the disk; created if missing. A relative path is `CodeInvalidArgument`. |
| `Closer` | nil | Closed by `Close`, or when `Dial` has replaced the connection. Without it, `Close` closes nothing and the client is yours to close. |
| `Dial` | nil | A `sftp.Dialer`, `func(ctx) (*sftp.Client, io.Closer, error)`, that opens a new connection and returns the closer that ends it. The driver calls it when the current connection has dropped (and for the first one when `Client` is nil) and closes what it returns. An error without a `filesystem.Code` is reported as `CodeUnavailable`. |

Close the disk (or the Manager holding it) when the application stops: an open SSH connection holds a session on the server.

## 5. Writes, conditional writes and moves

Every write goes to a temporary file under the reserved directory `.filesystem/tmp` inside the root, gets its mode, and is then renamed into place with `posix-rename@openssh.com`, so a reader sees the old file or the new one, never part of one. A failed or cancelled write removes its temporary file. A write that cannot clean up, because the process crashed or the connection dropped between staging and the rename, leaves its temporary file behind. Those are invisible to `List` and nothing else removes them, so sweep them on a schedule:

```go
removed, err := disk.SweepAbandoned(ctx, time.Now().Add(-24*time.Hour))
```

The sweep removes staged files last modified before the cutoff and returns how many. A staged file's modification time advances while its bytes arrive, so an upload still in progress is kept as long as its last chunk is newer than the cutoff; choose a cutoff well beyond your slowest upload. It removes only regular files named the way the driver names them, never stored files or anything else in `.filesystem/tmp`. The library starts no goroutine: run it from your own scheduler. `disk.FindAbandoned(ctx, cutoff)` is its dry run: the same scan, returning each staged file's name and modification time, removing nothing.

`IfNotExists` hard-links the temporary file to the key instead (`hardlink@openssh.com`), which fails atomically when the key exists. The driver declares `ConditionalWrite` only when the server offers that extension; OpenSSH does.

SFTP cannot report whether the server's filesystem folds letter case, so `New` finds out: it creates a probe file in `.filesystem/tmp`, looks it up under its upper-case name and removes it. When the lookup succeeds (macOS, Windows, many NAS shares), the disk declares `CaseInsensitive`, so `A.txt` and `a.txt` are one file and `filesystem.Mirror` can warn before mixing it with a case-sensitive store. A read-only account cannot write the probe and is reported case-sensitive.

`Move` is an atomic rename. Parent directories are created as needed, and directories left empty by a delete or a move are removed, so the tree mirrors the keys.

The disk has real directories (`Capabilities().Dirs`): `disk.MakeDir(ctx, dir)` creates one with its parents (a file at `dir` is `exists`, a file on its path `invalid_argument`), and `disk.DirExists(ctx, dir)` asks the server, so an empty directory counts. As with local disk, deleting the last file under a directory removes it again, and `DeletePrefix` removes it with everything below. See [keys](../concepts/keys.md).

<!-- ds:block id=dropandpickup-hrcthtrj -->
**dropAndPickUp** · [`examples/adapters-sftp/main.go:118-140`](../../examples/adapters-sftp/main.go#L118-L140)

```go
func dropAndPickUp(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	report := []byte("sku,qty\nA-1,4\n")
	if _, err := disk.WriteBytes(ctx, "outbound/orders.csv", report, filesystem.WriteOptions{IfNotExists: true}); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "outbound/orders.csv", report, filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	e, err := disk.Stat(ctx, "outbound/orders.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %d bytes %s (type from extension: %s)\n", e.Key, e.Size, e.Visibility, e.ContentType)
	if _, err := disk.Move(ctx, "outbound/orders.csv", "archive/2026-10/orders.csv"); err != nil {
		return err
	}
	for entry, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s\n", entry.Key)
	}
	return nil
}
```
<!-- /ds:block hash=91df28 -->

Check:

```text
capabilities: [move,set-visibility,delete-prefix,sweep-abandoned,find-abandoned,dirs,conditional-write]
second conditional write: exists
stat: outbound/orders.csv 14 bytes private (type from extension: text/csv; charset=utf-8)
listed: archive/2026-10/orders.csv
```

The capabilities line comes from the case probe `New` runs: it writes an empty staging-shaped file in `.filesystem/tmp`, looks it up under its upper-case name and removes it. When the server's filesystem folds case, as on macOS, the line ends with `case-insensitive`; a read-only account cannot write the probe, and the disk then assumes case-sensitive keys.

## 6. What SFTP stores

- **Visibility** is the file mode: public files are `0644`, private ones `0600` (`DefaultFilePublic`, `DefaultFilePrivate`), and `Stat` reads visibility from the world-read bit. `SetVisibility` is a `chmod`. The server's umask does not apply because the mode is set explicitly after the upload. `Config.FilePublic` and `FilePrivate` choose other modes, and `SkipChmod` leaves the server's (see [Adapt the driver to the server](#10-adapt-the-driver-to-the-server)).
- **Content type** is not stored: `Stat` derives it from the key's extension. `Write` returns the type the Disk detected from the bytes.
- **Metadata and headers** cannot be stored, so asking for them is `CodeUnsupported`:

<!-- ds:block id=refusedoptions-kha3m599 -->
**refusedOptions** · [`examples/adapters-sftp/main.go:145-151`](../../examples/adapters-sftp/main.go#L145-L151)

```go
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=b6fce9 -->

Check: `metadata: unsupported` and `headers: unsupported`.

- **No signed or public URLs** from the server itself. Serve files through your application with `signedurl` and `fshttp` (give the disk `filesystem.WithURLSigner`), or with `public_url` when the same files are published by a web server. See [serving files](../concepts/serving-files.md).
- **No multipart**, and no batch delete: `DeleteKeys` is one delete per key. `DeletePrefix` removes the directory recursively. `Copy` streams the bytes through your application (SFTP has no server-side copy).

## 7. Keys, symlinks and confinement

- Keys under `.filesystem/` (or the `Config.StagingDir` you name) are reserved for the staging directory and refused with `CodeInvalidKey`; listings skip it. With `StagingPath` or `WriteInPlace` nothing under the root is reserved.
- A key cannot be both a file and a directory (`a` and `a/b`): writing under a file is `CodeInvalidArgument`.
- Symlinks and directories are never objects: `Stat` and `Read` of one are `CodeNotFound`, and listings skip them.
- Keys are cleaned and traversal (`..`) is refused before the driver sees them. But SFTP has no equivalent of `os.Root`, so a symlink created on the server can point outside the root. Containment against server-side symlinks is the server's job: give the application an account chrooted to the root (sshd's `ChrootDirectory`). With a chroot, the DSN's path is the path inside the chroot.

## 8. Connections and performance

- The driver uses one SSH connection for every operation; `pkg/sftp` multiplexes concurrent requests over it.
- A lost connection is `CodeUnavailable` for the operations in flight when it dropped, so `layer.Retry` and `fshttp` (503) treat it as transient.
- A disk opened from a DSN reconnects: the next operation after a drop dials the server again with the same host, credentials and host key check, then runs on the new connection. One operation dials at a time and the others wait for it to share its connection, but each waits only as long as its own context allows and then fails with `CodeUnavailable`, so a server that hangs during the handshake cannot freeze every caller. The dial itself stops when the dialing operation's context ends, by deadline or cancellation (the socket is closed, which also interrupts the SSH handshake and the SFTP start); a context without a deadline is bounded by `DefaultDialTimeout` (30 seconds). `Close` never waits for a dial in progress: that dial's connection is closed when it finishes. A dial that fails returns its error (`CodeUnavailable` when the server is unreachable or too slow, `CodePermission` when the handshake is refused) and the operation after it tries again.
- The driver never repeats an operation for you: a write's body has already been read and a rename may have landed before the connection went, so only the caller can decide. Wrap the disk in `layer.Retry` to retry the transient failure.
- Every reconnect re-checks the server's extensions. Capabilities are fixed when the disk opens, so a server that comes back without `hardlink@openssh.com` makes conditional writes `CodeUnsupported` rather than silently unconditional, and one without `posix-rename@openssh.com` is refused (`CodeUnsupported`) and redialled on the next call.
- A disk built with `New` and only `Client` cannot reconnect: every later operation is `CodeUnavailable` until you open it again. Give it `Dial` to reconnect.
- `Close` closes the current connection and stops reconnecting. `filesystem.As[*sftp.Client]` returns the current client, which a reconnect replaces, so fetch it per use.
- A write whose body fails (a client's upload cut short) fails with the body's error and stores nothing, never a truncated file; that error is not `CodeUnavailable`, since the server connection is fine.
- `List` walks the directory tree on every page, so it is O(n) in the files under the listed directory. Fine for drop folders; large trees belong on an object store.
- Context cancellation is checked between operations and between chunks of a write; a single SFTP request already sent is not interrupted.

## 9. Test locally

`task test` runs the driver's tests and the conformance suite against `pkg/sftp`'s server over in-process pipes, plus an in-process SSH server for the opener's tests (host keys, keys, passwords, refused handshakes). The reconnect tests kill the in-process server mid-run, and cut the TCP connection under a disk opened from a DSN, to prove the redial. No Docker is needed. To try a real server, point the example's DSN at it, or run the conformance suite through your own test with `filesystemtest.Run` and a disk opened from your DSN (see [testing](../start/testing.md)).

## 10. Adapt the driver to the server

The defaults suit an OpenSSH server you control. Six `Config` fields fit the servers and drop folders that differ; each is off by default, so a disk without them behaves as described above.

```go
drv, err := sftp.New(sftp.Config{
	Dial: dial, // or Client
	Root: "/outbox",
	// A partner's poller picks up every new directory: stage under a name it ignores.
	StagingDir: ".upload-tmp",
	// Group-shared files: public means group-readable, not world-readable.
	FilePublic:  0o640,
	FilePrivate: 0o600,
})
```

- `StagingDir` names the reserved directory directly under the root (one path segment, `".filesystem"` by default). Changing it on a disk with staged files leaves those under the old name, so sweep first.
- `FilePublic` and `FilePrivate` are the modes visibility maps to. With custom modes a file reads as public when its mode has a bit `FilePublic` has and `FilePrivate` lacks (`0640` against `0600` makes group-read mean public), so `New` refuses a pair without such a bit.
- `SkipChmod` is for servers that refuse `SETSTAT`, where every write would otherwise fail: files keep the mode the server gives them, the disk declares no `SetVisibility`, and a write asking for a visibility other than the disk's default is `CodeUnsupported`.
- `AllowNonAtomicWrites` accepts a server without `posix-rename@openssh.com`. Such a server's plain rename refuses an existing target, so replacing a key removes the old file and renames the new one in two steps: a reader between them finds no file, and a connection lost between them leaves the key empty (the new bytes stay staged for `SweepAbandoned`). Writing a new key is still atomic, and a server that does offer the extension keeps using it. Turn it on only for a server you cannot change, and only when that window is acceptable.

**Write-only drop folders.** A bank, EDI or partner inbox often lets the account write files into the folder but refuses mkdir inside it, so the default staging directory fails at `New`. Two fields open such a folder:

```go
// Stage beside the inbox: writes stay atomic, the inbox only ever sees finished files.
drv, err := sftp.New(sftp.Config{Dial: dial, Root: "/inbox", StagingPath: "/upload-tmp"})

// Nothing but the files may be created anywhere: write in place.
drv, err = sftp.New(sftp.Config{Dial: dial, Root: "/inbox", AllowNonAtomicWrites: true, WriteInPlace: true})
```

- `StagingPath` stages in an absolute directory outside the root (created if missing) and renames into it, so writes stay atomic; it must be on the same filesystem as the root, since a rename cannot cross devices, and it may not be the root, inside it or contain it. No key is reserved, and `SweepAbandoned` sweeps that directory.
- `WriteInPlace` writes each file straight to its key, and needs `AllowNonAtomicWrites` because the guarantee is weaker: the partner's poller can pick up a file while its bytes are still arriving, an overwrite truncates the old content first, and a failed write removes the file, so a failure while replacing a key leaves it empty. `IfNotExists` stays atomic (an exclusive create, declared even without `hardlink@openssh.com`). `New` creates nothing, so the root must exist; there is nothing to sweep, so `SweepAbandoned` and `FindAbandoned` are not declared; and letter case cannot be probed without writing into the folder, so the disk reports case-sensitive.

## Pitfalls

- **`insecure_ignore_host_key=true` in production** lets anyone who can intercept the connection read and replace your files. Ship a `known_hosts` file.
- **No chroot on the server**: a symlink planted inside the root can expose files outside it.
- **A dropped connection fails the operations in flight**: the next one reconnects, but nothing is retried for you; use `layer.Retry`. A disk built with `New` and no `Dial` does not reconnect at all.
- **Metadata or headers**: refused; keep them in your database (the [records module](../records/README.md) does).
- **`AllowNonAtomicWrites` on a server that has the extension** changes nothing, but on one without it an overwrite has a moment with no file at the key; readers polling the folder can see it missing.

## Next

- [Serving files](../concepts/serving-files.md): signed downloads for disks without native signing.
- [Backups and mirrors](../recipes/backups-and-mirrors.md): copying files to a partner's server as well as your own store.
- [Local disk](local.md), which shares the file-mode model.
- [Comparison of every backend](README.md).
