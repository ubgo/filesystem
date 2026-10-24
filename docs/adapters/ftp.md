# FTP

The FTP driver stores files on an FTP or FTPS server: shared hosting accounts, printers and scanners, cameras, mainframe gateways and the partner drop boxes that speak nothing else. Writes are atomic on POSIX servers, moves are renames, and a pool of connections serves concurrent calls. FTP has no metadata, no visibility, no server-side copy and no signed URLs.

## When to choose FTP

- Choose it when the other side offers nothing else: a partner's drop box, a hosting account, a device that uploads scans or footage by FTP.
- Choose [SFTP](sftp.md) whenever the server offers it: it is encrypted by default, its replaces are always atomic, and it has conditional writes.
- Choose [WebDAV](webdav.md) for a customer's Nextcloud or NAS, and an object store ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md)) when you own the storage.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/ftp`, a module of its own built on `github.com/jlaffaye/ftp`. Its reference is [adapters/ftp/README.md](../../adapters/ftp/README.md).
- The runnable program is [`examples/adapters-ftp`](../../examples/adapters-ftp/). It runs an FTP server in-process (`github.com/fclairamb/ftpserverlib` over a temporary directory), so `go run ./adapters-ftp` from `examples/` needs no remote host. Every snippet below is cut from it.
- Prefer SFTP ([sftp.md](sftp.md)) when the server offers it. When it is FTP, use FTPS unless the network is trusted: plain FTP sends the password readable to anyone on the path.

## Step 1: Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/ftp
```

## Step 2: Open a disk from a DSN

The DSN form is `ftp://<user>@<host>[:<port>]/<absolute root>?password_env=<VAR>`, `ftps://` for explicit FTPS (`AUTH TLS`, the certificate checked against the system roots, or the `ca_file` bundle), and `ftps-implicit://` for implicit FTPS. The port defaults to 21, and to 990 for `ftps-implicit://`. Both FTPS schemes keep a TLS session cache, so each data connection resumes the control connection's TLS session, which vsftpd (`require_ssl_reuse`, on by default) and other hardened servers insist on. The password never appears in the DSN: one in the URL is refused, and `password_env` names the environment variable that holds it. `max_conns` sets the pool size.

| Parameter | Value | Sets |
|---|---|---|
| `password_env` | the name of an environment variable | `Config.Password`, read from that variable; needs a user in the DSN |
| `max_conns` | positive integer | `Config.MaxConns`, the pooled control connections (default 4) |
| `ca_file` | path of a PEM file | the authorities that verify the server, in place of the system roots: an internal CA or a self-signed certificate; FTPS schemes only |

Plus the Mux's own `prefix`, `public_url`, `visibility` and `readonly` (see [configuration](../concepts/configuration.md)). The path is the root directory on the server, absolute, created if missing. Register the opener under both schemes:

<!-- ds:block id=opendisk-k9z3v2xh -->
**openDisk** · [`examples/adapters-ftp/main.go:104-113`](../../examples/adapters-ftp/main.go#L104-L113)

```go
func openDisk(ctx context.Context, addr string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(ftp.Scheme, ftp.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(ftp.SchemeSecure, ftp.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "partner", "ftp://"+ftpUser+"@"+addr+"/exchange?password_env="+passwordEnv+"&max_conns=3")
}
```
<!-- /ds:block hash=418fa2 -->

From code, `ftp.New(ctx, ftp.Config{...})` takes the same settings plus `TLS` (a `*tls.Config`, for a private CA or client certificates), `ImplicitTLS` (port 990), `DialTimeout` and `DataTimeout`. `New` logs in once, creates the root and the staging directory, and probes whether the server folds letter case, so wrong credentials fail there with `CodePermission`.

| `Config` field | Default | What it does |
|---|---|---|
| `Addr` | required | `host:port`; port 21 when missing |
| `Username`, `Password` | `anonymous` | the login; with no `Username` the driver logs in as `anonymous` |
| `Root` | required, absolute | the directory holding the disk; created if missing |
| `TLS` | nil | explicit FTPS (`AUTH TLS`), or implicit with `ImplicitTLS`; without `ServerName` the certificate is checked against the host of `Addr`, on the control and every data connection |
| `ImplicitTLS` | false | TLS from the first byte (port 990); needs `TLS`; the `ftps-implicit://` scheme from a DSN |
| `MaxConns` | `DefaultMaxConns` (4) | pooled control connections |
| `DialTimeout` | `DefaultDialTimeout` (30 seconds) | dial and login when the context has no deadline |
| `DataTimeout` | `DefaultDataTimeout` (2 minutes) | each wait for a download's next bytes; a stall is `CodeUnavailable` |
| `IdleCheck` | `DefaultIdleCheck` (15 seconds) | how long a pooled connection may idle before its next use checks it with `NOOP` |
| `StagingDir` | `".filesystem"` | the reserved directory directly under the root for staged files; one path segment |
| `StagingPath` | empty | stage in this absolute directory outside the root instead, for a drop folder that refuses `MKD` inside it (see Step 9) |
| `WriteInPlace`, `AllowNonAtomicWrites` | false | `STOR` straight to each key with no staging at all; both must be set (see Step 9) |
| `Dial` | a `net.Dialer` with `DialTimeout` | opens the control and every data connection; TLS is added on top |
| `DialOptions` | none | jlaffaye/ftp protocol switches, passed after the driver's own |

## Step 3: Upload, archive and copy

A write `STOR`s the body under a temporary name and renames it into place, so the partner never picks up half a file. `Move` is one `RNFR`/`RNTO` rename. FTP has no copy command, so `Copy` is done by the Disk: it reads the object and writes it back, which costs the object's size twice in bandwidth.

<!-- ds:block id=uploadandarchive-qepk3c5b -->
**uploadAndArchive** · [`examples/adapters-ftp/main.go:148-163`](../../examples/adapters-ftp/main.go#L148-L163)

```go
func uploadAndArchive(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	e, err := disk.Write(ctx, "outbound/orders.csv", strings.NewReader("id,qty\n1,3\n"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "uploaded: %s %d bytes %s\n", e.Key, e.Size, e.ContentType)
	if _, err := disk.Move(ctx, "outbound/orders.csv", "archive/2026-10/orders.csv"); err != nil {
		return err
	}
	c, err := disk.Copy(ctx, "archive/2026-10/orders.csv", "outbound/orders-resend.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "copied by streaming through the client: %s %d bytes\n", c.Key, c.Size)
	return nil
}
```
<!-- /ds:block hash=095ed6 -->

## Step 4: Concurrent calls share a pool

FTP runs one command at a time per control connection, so the driver keeps a pool. Calls take a connection, and wait for one no longer than their context allows. Here six uploads run at once over the three connections the DSN asked for:

<!-- ds:block id=concurrentuploads-v6mkjfcz -->
**concurrentUploads** · [`examples/adapters-ftp/main.go:168-182`](../../examples/adapters-ftp/main.go#L168-L182)

```go
func concurrentUploads(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = disk.Write(ctx, fmt.Sprintf("inbound/batch-%d.txt", i), strings.NewReader("line\n"), filesystem.WriteOptions{})
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Fprintln(out, "six concurrent uploads over a pool of three connections: ok")
	return nil
}
```
<!-- /ds:block hash=5fcbcd -->

## Step 5: Directories

The disk declares `Dirs`: `MakeDir` creates directories with `MKD`, level by level, and `DirExists` asks the server. FTP behaves like local disk here and unlike WebDAV: deleting the last file in a directory removes the directory, and its empty parents, up to the root.

<!-- ds:block id=directories-7d7jwn8h -->
**directories** · [`examples/adapters-ftp/main.go:204-224`](../../examples/adapters-ftp/main.go#L204-L224)

```go
func directories(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if err := disk.MakeDir(ctx, "returns/2026"); err != nil {
		return err
	}
	made, err := disk.DirExists(ctx, "returns/2026")
	if err != nil {
		return err
	}
	if _, err := disk.Write(ctx, "returns/2026/r1.csv", strings.NewReader("id\n7\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if err := disk.Delete(ctx, "returns/2026/r1.csv"); err != nil {
		return err
	}
	kept, err := disk.DirExists(ctx, "returns")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "made returns/2026: exists=%t; after deleting its only file, returns exists=%t\n", made, kept)
	return nil
}
```
<!-- /ds:block hash=f99ab2 -->

The program prints `made returns/2026: exists=true; after deleting its only file, returns exists=false`. A directory you make and leave empty stays until something is written in it and removed again, or `DeletePrefix` clears it.

## Step 6: What FTP refuses

The disk declares `[move,delete-prefix,sweep-abandoned,find-abandoned,dirs]`, plus `case-insensitive` when the probe finds the server's filesystem folds case. Never `Copy` on the server (the Disk streams), `SetVisibility` (`SITE CHMOD` is not standard), `Metadata`, `ContentType` (guessed from the extension), `Headers`, `ConditionalWrite` (no FTP command creates a file only if absent) or URLs. Asking for one is refused before a request:

<!-- ds:block id=refusedwrites-7qbzqw3p -->
**refusedWrites** · [`examples/adapters-ftp/main.go:230-239`](../../examples/adapters-ftp/main.go#L230-L239)

```go
func refusedWrites(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	body := func() io.Reader { return strings.NewReader("x") }
	_, err := disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "conditional write: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, ftp.StagingDir+"/mine.txt", body(), filesystem.WriteOptions{})
	fmt.Fprintf(out, "reserved key: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=4e5140 -->

It prints `conditional write: unsupported`, `metadata: unsupported` and `reserved key: invalid_key`: keys under `.filesystem` belong to the driver's staging directory.

## Step 7: Clean up interrupted writes

A process that dies, or a connection that breaks, between the `STOR` and the rename leaves a file in `.filesystem/tmp`. `FindAbandoned` lists such files older than a cutoff; `SweepAbandoned` removes the same set. Only files named the way `Write` names them are touched. The example plants one, a day old:

<!-- ds:block id=sweepstaged-9ux5wq6y -->
**sweepStaged** · [`examples/adapters-ftp/main.go:246-269`](../../examples/adapters-ftp/main.go#L246-L269)

```go
func sweepStaged(ctx context.Context, out io.Writer, disk *filesystem.Disk, rootDir string) error {
	staged := filepath.Join(rootDir, ftp.StagingDir, "tmp", strings.Repeat("b", 26))
	if err := os.WriteFile(staged, []byte("half an upload"), 0o600); err != nil {
		return err
	}
	dayAgo := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(staged, dayAgo, dayAgo); err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Hour)
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "abandoned: %s %s\n", a.Kind, a.ID)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept: %d\n", n)
	return nil
}
```
<!-- /ds:block hash=da8ac3 -->

Run the sweep from a [maintenance job](../concepts/maintenance-jobs.md) with a cutoff well past your longest upload. If a `Write` or `Move` ever returned a `*ftp.StrandedError` (see Pitfalls), recover its files before the next sweep.

## Step 8: DSN checks

The opener refuses a bad DSN before it dials:

<!-- ds:block id=refuseddsns-j46sn7gr -->
**refusedDSNs** · [`examples/adapters-ftp/main.go:274-286`](../../examples/adapters-ftp/main.go#L274-L286)

```go
func refusedDSNs(ctx context.Context, out io.Writer, addr string) {
	mux := filesystem.NewMux()
	_ = mux.Register(ftp.Scheme, ftp.Opener)
	_ = mux.Register(ftp.SchemeSecure, ftp.Opener)
	for _, c := range []struct{ why, dsn string }{
		{"password in the URL", "ftp://" + ftpUser + ":secret@" + addr + "/exchange"},
		{"max_conns=0", "ftp://" + ftpUser + "@" + addr + "/exchange?max_conns=0&password_env=" + passwordEnv},
		{"no root directory", "ftp://" + ftpUser + "@" + addr + "?password_env=" + passwordEnv},
	} {
		_, err := mux.Open(ctx, "refused", c.dsn)
		fmt.Fprintf(out, "refused DSN (%s): %s\n", c.why, filesystem.CodeOf(err))
	}
}
```
<!-- /ds:block hash=341812 -->

All three print `invalid_argument`. The `Config` prints and logs with the password redacted, and the driver keeps the password inside its dial closure, so a logged `Config` or a printed driver never shows it.

## Step 9: Reach the server another way

The defaults dial the server directly, stage under `.filesystem`, check idle connections after 15 seconds, and speak extended passive mode. Each of these is a `Config` field for servers and networks that need something else, and `Do` reaches what the driver does not cover:

```go
drv, err := ftp.New(ctx, ftp.Config{
	Addr: "files.partner.example:21", Username: "acme", Password: pw, Root: "/inbox",
	// Through a SOCKS5 proxy (golang.org/x/net/proxy): control and data connections alike.
	Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return socks.(proxy.ContextDialer).DialContext(ctx, network, addr)
	},
	// A NAT that breaks EPSV: fall back to PASV.
	DialOptions: []jftp.DialOption{jftp.DialWithDisabledEPSV(true)},
	// The partner's poller ignores dot-directories but not ours by name.
	StagingDir: ".acme-staging",
	// A firewall that drops idle sessions after ten seconds.
	IdleCheck: 5 * time.Second,
})
// Something the driver has no method for; paths are the server's.
err = drv.Do(ctx, func(sc *jftp.ServerConn) error {
	return sc.SetTime("/inbox/report.csv", reportedAt) // MFMT, or MDTM with a time
})
```

**Write-only drop folders.** A bank, EDI or partner inbox often lets the account store files but refuses `MKD` inside the folder, so the default staging directory fails at `New`. Stage beside it, or store in place:

```go
// Staged beside the inbox and renamed in: the inbox only ever sees finished files.
drv, err := ftp.New(ctx, ftp.Config{Addr: addr, Username: "acme", Password: pw, Root: "/inbox", StagingPath: "/upload-tmp"})

// Nothing but the files may be created anywhere: STOR in place.
drv, err = ftp.New(ctx, ftp.Config{Addr: addr, Username: "acme", Password: pw, Root: "/inbox", AllowNonAtomicWrites: true, WriteInPlace: true})
```

`StagingPath` must be absolute, outside the root and not containing it, and on the same filesystem (a rename cannot cross devices); it is created if missing, IIS's moved-aside originals go there too, and `SweepAbandoned` sweeps it. `WriteInPlace` needs `AllowNonAtomicWrites`, which only acknowledges the weaker guarantee: the partner's poller can pick up a file while it is still arriving, an overwrite truncates the old content first, and a failed `STOR` deletes the file (a connection lost mid-`STOR` leaves the partial file). A `Move` onto an existing key on a server that refuses to rename over a file deletes the target first, with no rollback. `New` only logs in, so the root must exist; `SweepAbandoned` and `FindAbandoned` are not declared; and letter case is not probed.

`DialOptions` must not carry `DialWithDialFunc`, `DialWithNetConn` or the TLS options: the driver owns the sockets so a cancelled context can close them, and `Dial` and `TLS` are the way to change them. The client library sends no arbitrary command, so a `SITE` command cannot go through `Do`. Inside `Do`, return a `*filesystem.Error` for your own verdicts; any other error that is not a server reply is taken for a lost connection, and the connection is discarded.

## Behaviour

- A write `STOR`s the body under a temporary name in `.filesystem/tmp` and renames it into place, so a reader never sees half an object on servers whose rename replaces a file (vsftpd, ProFTPD, Pure-FTPd and every POSIX server). A server that refuses to rename over a file and says so (IIS: `550 Cannot create a file when that file already exists.`) gets a replace in steps that never lose the old object: the old object is renamed aside into `.filesystem/tmp`, the new one renamed onto the key, and the old one deleted. If renaming the new one in fails, the old one is renamed back, so the key keeps its old content and the write fails as `CodePermission`; each rename is tried three times. Any other refusal leaves the old object alone and is `CodePermission`. What stays non-atomic: on such a server the key is briefly missing between the two renames, a moved-aside object whose delete fails stays in `.filesystem/tmp` for `SweepAbandoned`, and if renaming the old object back fails too, `Write` returns a `*ftp.StrandedError` whose `Previous` field names the old content and `Staged` the new, both kept, so either can be renamed into place by hand (do it before the next `SweepAbandoned`). `Move` onto an existing key goes through the same steps. Keys under `.filesystem` (or `Config.StagingDir`) are refused; with `StagingPath` or `WriteInPlace` nothing under the root is reserved.
- A body that fails part-way returns its own error and leaves the old object in place; the staged file is removed, or swept by `SweepAbandoned`.
- FTP runs one command at a time per control connection, so the driver keeps a pool (`DefaultMaxConns`, 4): calls take a connection, and wait for one no longer than their context allows. A connection idle for more than `Config.IdleCheck` (`DefaultIdleCheck`, 15 seconds) is checked with `NOOP` before reuse, since servers drop idle sessions.
- A command is bounded by its context: when the context ends the control socket is closed, the call returns the context's error, and the connection is replaced. A failure below the protocol (a reset, a timeout) is `CodeUnavailable` and its connection is closed; a reply from the server keeps its connection. The driver never repeats an operation itself: wrap the disk in `layer.Retry` for that.
- Reads use `REST` for an offset. The reader stays bound to the call's context (cancelling it fails a read in progress), and each wait for data is bounded by `Config.DataTimeout` (`DefaultDataTimeout`, 2 minutes), so a stalled server fails the read with `CodeUnavailable`. Closing a reader before the end aborts the transfer and replaces its connection.
- `Stat` uses `MLST` where the server has it, otherwise `SIZE` and `MDTM`. `List` walks directories (`MLSD`, or `LIST`), O(n) in the files under the directory; a listed name that is not a clean key (a backslash, a control character, `..`) is skipped rather than trusted. Delete and Move remove directories left empty, up to the root.
- Replies map to codes: 550 `CodeNotFound`, 530 `CodePermission`, 552 and 452 `CodeQuotaExceeded`, 553 `CodeInvalidKey`, other 4xx `CodeUnavailable`.
- The `Config` prints and logs with the password redacted, and the driver keeps the password inside its dial closure, so a logged `Config` or a printed driver never shows it.

## Try it

```sh
cd examples
go run ./adapters-ftp
go test ./adapters-ftp
```

On macOS it prints:

```text
capabilities: [move,delete-prefix,sweep-abandoned,find-abandoned,dirs,case-insensitive]
uploaded: outbound/orders.csv 11 bytes text/csv; charset=utf-8
copied by streaming through the client: outbound/orders-resend.csv 11 bytes
six concurrent uploads over a pool of three connections: ok
listed: archive/2026-10/orders.csv
objects: 8
made returns/2026: exists=true; after deleting its only file, returns exists=false
conditional write: unsupported
metadata: unsupported
reserved key: invalid_key
abandoned: staging bbbbbbbbbbbbbbbbbbbbbbbbbb
swept: 1
refused DSN (password in the URL): invalid_argument
refused DSN (max_conns=0): invalid_argument
refused DSN (no root directory): invalid_argument
```

`case-insensitive` appears because the served directory is on macOS's case-folding temp volume; on Linux it does not. The test checks every other line.

## Pitfalls

- **Plain FTP is readable on the wire**, password included. Use `ftps://`, or SFTP, unless the network is one you trust.
- **`ftps://` checks the certificate against the system roots.** A server with a self-signed certificate or a private CA fails the handshake; name that CA with `ca_file`, or open it from code with `Config.TLS` carrying it in `RootCAs`. Implicit FTPS (port 990) is `ftps-implicit://`.
- **`Config.TLS` from code has no session cache unless you give it one.** A server that requires TLS session reuse on data connections refuses every transfer; set `ClientSessionCache: tls.NewLRUClientSessionCache(0)` in the `tls.Config`, as the DSN path does.
- **No user means anonymous.** A DSN without a user logs in as `anonymous`; most servers refuse that with `CodePermission`. An unset `password_env` variable is read as an empty password and fails the same way, at open time.
- **Replaces on IIS are not atomic, but a failed one keeps the old object.** The key is missing for the moment between renaming the old object aside and the new one in. Only when the rename in and the rename back both fail is the key left empty: check the error of `Write` or `Move` with `errors.As(err, &stranded)` for a `*ftp.StrandedError`; its `Previous` path holds the old content and `Staged` the new, both kept, to rename onto `Target` by hand (or write again) before the next sweep.
- **The pool is a queue.** A call waits for a free connection as long as its context allows, so a call without a deadline behind slow uploads waits as long as they take. Give calls deadlines, and keep `max_conns` at or below the connections the server allows per user, since the server, not the driver, refuses the extra logins.
- **Data connections are passive.** `jlaffaye/ftp` asks the server for a data port (`EPSV`, then `PASV`) and dials it, so a firewall in front of the server must open its passive port range, and the server must advertise an address the client can reach.
- **The driver never retries.** A dropped connection is `CodeUnavailable`, a 4xx reply too; wrap the disk in `layer.Retry` to retry them.
- **`Copy` doubles the traffic**: it reads the object and writes it back. For large files, `Move` (a rename) when the source can go, or plan copies as background jobs.
- **Files other tools wrote with names that are not clean keys** (a backslash, a control character, `..`) never appear in a listing and cannot be read through the disk.
- **Closing a reader early is not free**: the transfer is aborted and its connection replaced, so a later call pays for a new login.

## Related

- [adapters/ftp/README.md](../../adapters/ftp/README.md): the reference for `Config`, the DSN and every behaviour.
- [SFTP](sftp.md) and [WebDAV](webdav.md): the other remote-server drivers, and how they differ on replaces and directories.
- [Capabilities](../concepts/capabilities.md): what a disk declares and what happens when an operation is not.
- [Errors](../concepts/errors.md): the codes the replies map to, and retrying.
- [Maintenance jobs](../concepts/maintenance-jobs.md): running `SweepAbandoned` on a schedule.
- [Comparison of every backend](README.md).
