# WebDAV

The WebDAV driver stores files on any WebDAV server: Nextcloud, ownCloud, Box, Synology and QNAP NAS boxes, and Apache's or nginx's dav modules. It is the way to reach a customer's own cloud drive or an office NAS with nothing installed beside it. Writes are staged so a reader never sees half an object, copies and moves happen on the server, and the driver speaks plain `net/http`, with no dependency beyond the library.

## When to choose WebDAV

- Choose it when the files must land somewhere the user or customer already owns and browses: their Nextcloud, an office NAS, a Box folder. They see what your application stores in their own client.
- Choose [SFTP](sftp.md) instead when the remote is a server you reach over SSH; it has atomic replaces and no HTTP round trip per directory.
- Choose an object store ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md)) when you own the storage: WebDAV has no signed URLs, no metadata, no conditional writes, and a listing costs one request per directory.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/webdav`, a module of its own. Its reference is [adapters/webdav/README.md](../../adapters/webdav/README.md).
- The runnable program is [`examples/adapters-webdav`](../../examples/adapters-webdav/). It serves a temporary directory over WebDAV in-process, with Basic authentication and Nextcloud's URL layout, so `go run ./adapters-webdav` from `examples/` needs no remote server. Every snippet below is cut from it.
- For Nextcloud and ownCloud the endpoint is `https://<host>/remote.php/dav/files/<user>/<folder>`, and the password should be an app password created for this application, not the account's own.

## Step 1: Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/webdav
```

## Step 2: Open a disk from a DSN

The DSN form is `webdavs://<user>@<host>/<path>?password_env=<VAR>` for https, and `webdav://` for plain http: allowed to a loopback host, and to any other only with `insecure_http=true` (`Config.AllowInsecureHTTP`), since Basic authentication over http sends the password readable. The password never appears in the DSN: one in the URL is refused, and `password_env` names the environment variable that holds it.

| Parameter | Value | Sets |
|---|---|---|
| `password_env` | the name of an environment variable | `Config.Password`, read from that variable; needs a user in the DSN |
| `insecure_http` | boolean | `Config.AllowInsecureHTTP`, needed for `webdav://` to a host other than loopback |

Plus the Mux's own `prefix`, `public_url`, `visibility` and `readonly` (see [configuration](../concepts/configuration.md)). Register the opener under both schemes:

<!-- ds:block id=opendisk-nf626p2g -->
**openDisk** · [`examples/adapters-webdav/main.go:71-80`](../../examples/adapters-webdav/main.go#L71-L80)

```go
func openDisk(ctx context.Context, host string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(webdav.Scheme, webdav.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(webdav.SchemeSecure, webdav.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "cloud", "webdav://"+davUser+"@"+host+davPrefix+"/app?password_env="+passwordEnv)
}
```
<!-- /ds:block hash=c2c78a -->

From code, `webdav.New(ctx, webdav.Config{Endpoint, Username, Password, Client})` does the same; `Client` is where a custom transport, proxy or TLS configuration goes. `New` talks to the server once: it creates the staging collection and probes whether the server folds letter case, so wrong credentials fail there with `CodePermission`.

| `Config` field | Default | What it does |
|---|---|---|
| `Endpoint` | required | http or https URL of the collection holding the disk; no credentials in it; a missing trailing slash is added |
| `Client` | a client with `DefaultTimeout` (10 minutes) | transport, proxy, TLS; the driver uses a copy whose redirects stay on the endpoint's scheme and host |
| `Username`, `Password` | none | Basic authentication, sent when `Username` is set |
| `AllowInsecureHTTP` | false | permits an http `Endpoint` off loopback |
| `StagingDir` | `".filesystem"` | the reserved collection directly under the endpoint for staged files; one path segment |
| `StagingPath` | empty | stage in this absolute path on the server, outside the endpoint's collection, instead, for a drop folder that refuses `MKCOL` inside it (see below) |
| `WriteInPlace`, `AllowNonAtomicWrites` | false | `PUT` straight to each key with no staging at all; both must be set (see below) |

Every write stages under `.filesystem/tmp`. When that name collides with something on the server, or a sync client (Nextcloud's desktop client, a partner's poller) must not pick the collection up under it, name another:

```go
drv, err := webdav.New(ctx, webdav.Config{
	Endpoint: "https://cloud.example.com/remote.php/dav/files/acme/app/",
	Username: "acme", Password: pw,
	StagingDir: ".acme-staging", // one path segment, directly under the endpoint
})
```

Keys under the collection you name are refused, and listings skip it; `.filesystem` becomes an ordinary key. Staged files left under the old name are not swept once you change it, so sweep before switching.

**Write-only drop folders.** A partner's upload collection often accepts `PUT` but refuses `MKCOL` inside it, so the staging collection cannot be made there. Stage beside it, or `PUT` in place:

```go
// Staged beside the inbox and MOVEd in: the inbox only ever sees finished files.
drv, err := webdav.New(ctx, webdav.Config{
	Endpoint:    "https://dav.partner.example/drop/acme/inbox/",
	Username:    "acme", Password: pw,
	StagingPath: "/drop/acme/.staging", // absolute, outside the endpoint; its parent must exist
})

// Nothing but the files may be created anywhere: PUT in place.
drv, err = webdav.New(ctx, webdav.Config{
	Endpoint: "https://dav.partner.example/drop/acme/inbox/",
	Username: "acme", Password: pw,
	AllowNonAtomicWrites: true, WriteInPlace: true,
})
```

`StagingPath` is created if missing, reserves no key, and is where `SweepAbandoned` looks; the server must allow a `MOVE` from it into the endpoint. `WriteInPlace` needs `AllowNonAtomicWrites`, which only acknowledges the weaker guarantee: a poller or sync client can see a file while it is still arriving, and a failed `PUT` deletes the key, so a failure while replacing one leaves it empty. A nested key's parents are made with `MKCOL` before its `PUT`. `New` then sends no request (wrong credentials surface on the first call), `SweepAbandoned` and `FindAbandoned` are not declared, and letter case is not probed.

## Step 3: Store, copy, move and list

A write `PUT`s the body under a temporary name and `MOVE`s it into place, so the report below is never visible half-written. `Copy` and `Move` are a single server-side `COPY` and `MOVE`: no byte travels through your process.

<!-- ds:block id=storereport-mmrnx7vt -->
**storeReport** · [`examples/adapters-webdav/main.go:116-135`](../../examples/adapters-webdav/main.go#L116-L135)

```go
func storeReport(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	e, err := disk.Write(ctx, "reports/2026-10.csv", strings.NewReader("month,total\n10,42\n"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored: %s %d bytes %s\n", e.Key, e.Size, e.ContentType)
	if _, err := disk.Copy(ctx, "reports/2026-10.csv", "shared/latest.csv"); err != nil {
		return err
	}
	if _, err := disk.Move(ctx, "reports/2026-10.csv", "archive/2026/10.csv"); err != nil {
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
<!-- /ds:block hash=32196c -->

The content type comes from the extension (`text/csv; charset=utf-8`), not from the server: WebDAV servers differ in what they keep, so the driver stores none.

## Step 4: Directories

WebDAV has real collections, so the disk declares `Dirs`: `MakeDir` creates a collection (with `MKCOL`, parents first) and `DirExists` asks the server. Unlike local disk and FTP, deleting the last file under a collection keeps the collection, because a WebDAV `DELETE` of a collection is recursive and checking it is empty first would race a concurrent write into it. `DeletePrefix` removes it.

<!-- ds:block id=directories-uqurjtmh -->
**directories** · [`examples/adapters-webdav/main.go:140-162`](../../examples/adapters-webdav/main.go#L140-L162)

```go
func directories(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if err := disk.MakeDir(ctx, "inbox/2026"); err != nil {
		return err
	}
	made, err := disk.DirExists(ctx, "inbox/2026")
	if err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "inbox", filesystem.ListOptions{Recursive: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "made inbox/2026: exists=%t, listed entries=%d\n", made, len(page.Entries))
	if err := disk.Delete(ctx, "shared/latest.csv"); err != nil {
		return err
	}
	kept, err := disk.DirExists(ctx, "shared")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after deleting its last file, shared exists=%t\n", kept)
	return disk.DeletePrefix(ctx, "shared")
}
```
<!-- /ds:block hash=184eec -->

A listing is built from files only, so the empty `inbox/2026` collection never appears in one (`listed entries=0`), though `DirExists` finds it.

## Step 5: What WebDAV refuses

The disk declares `[copy,move,delete-prefix,sweep-abandoned,find-abandoned,dirs]`, plus `case-insensitive` when the probe finds the server's storage folds case. There is no visibility, metadata, stored content type or headers (servers differ in what they keep), no signed URLs, and no conditional write: a `MOVE` with `Overwrite: F` is a check-then-rename on common servers, not an atomic create. Asking for any of them is refused before a request, never silently dropped:

<!-- ds:block id=refusedwrites-k5jge7dv -->
**refusedWrites** · [`examples/adapters-webdav/main.go:168-177`](../../examples/adapters-webdav/main.go#L168-L177)

```go
func refusedWrites(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	body := func() io.Reader { return strings.NewReader("x") }
	_, err := disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "if-not-exists: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, webdav.StagingDir+"/mine.txt", body(), filesystem.WriteOptions{})
	fmt.Fprintf(out, "reserved key: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=6de67d -->

The program prints `metadata: unsupported`, `if-not-exists: unsupported` and `reserved key: invalid_key`: keys under `.filesystem` belong to the driver's staging collection.

## Step 6: Clean up interrupted writes

A process that dies between the staging `PUT` and the `MOVE` leaves a file in `.filesystem/tmp`. `FindAbandoned` lists such files older than a cutoff without touching them; `SweepAbandoned` removes the same set. Only files named the way `Write` names them (26 characters of lower-case base32) are ever touched. The example plants one, a day old, the way a crash would:

<!-- ds:block id=sweepstaged-8xge4twb -->
**sweepStaged** · [`examples/adapters-webdav/main.go:184-207`](../../examples/adapters-webdav/main.go#L184-L207)

```go
func sweepStaged(ctx context.Context, out io.Writer, disk *filesystem.Disk, appDir string) error {
	staged := filepath.Join(appDir, webdav.StagingDir, "tmp", strings.Repeat("a", 26))
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
<!-- /ds:block hash=7872d5 -->

Run the sweep from a maintenance job (see [maintenance jobs](../concepts/maintenance-jobs.md)) with a cutoff well past your longest upload, so a write still in flight is never removed.

## Step 7: DSN checks and redaction

The opener refuses a DSN that would put the password at risk before it sends anything:

<!-- ds:block id=refuseddsns-2pq3n5d3 -->
**refusedDSNs** · [`examples/adapters-webdav/main.go:213-225`](../../examples/adapters-webdav/main.go#L213-L225)

```go
func refusedDSNs(ctx context.Context, out io.Writer, host string) {
	mux := filesystem.NewMux()
	_ = mux.Register(webdav.Scheme, webdav.Opener)
	_ = mux.Register(webdav.SchemeSecure, webdav.Opener)
	for _, c := range []struct{ why, dsn string }{
		{"password in the URL", "webdavs://alice:secret@cloud.example.com/remote.php/dav/files/alice"},
		{"plain http off loopback", "webdav://alice@nas.internal/dav?password_env=" + passwordEnv},
		{"password_env without a user", "webdav://" + host + davPrefix + "?password_env=" + passwordEnv},
	} {
		_, err := mux.Open(ctx, "refused", c.dsn)
		fmt.Fprintf(out, "refused DSN (%s): %s\n", c.why, filesystem.CodeOf(err))
	}
}
```
<!-- /ds:block hash=dd7fcc -->

All three print `invalid_argument`. The `Config` prints and logs with the password redacted, and the driver keeps the password inside a closure, so a logged `Config` or a printed driver never shows it; the example's last line logs a `Config` with `slog` and shows `webdav.password=[redacted]`.

## Behaviour

- A write `PUT`s the body to a temporary name under `.filesystem/tmp` and `MOVE`s it into place, so a reader never sees half an object. Replacing an existing key is not atomic: RFC 4918 defines a `MOVE` with `Overwrite: T` as a delete of the destination followed by the move, and servers do exactly that (`golang.org/x/net/webdav` runs `RemoveAll`, then `Rename`), so a reader can briefly get `CodeNotFound` for a key being replaced. A body that fails part-way returns its own error and leaves nothing under the key; the staged file is removed, or swept by `SweepAbandoned` if the process died first. The staging `PUT` is sent with `Expect: 100-continue`, so the body only follows once the server has started the `PUT` (and created the staged file); without it, a body failing early could have its cleanup `DELETE` overtake the aborted `PUT`, which would then leave the file behind. The price is one extra round trip per write, or up to one second (`webdav.ExpectContinueTimeout`) on a server that ignores `Expect`. Keys under `.filesystem` are refused.
- Missing parent collections are created (`MKCOL`) when the server reports them missing, and never removed: a WebDAV `DELETE` of a collection is recursive, and checking it is empty first would race a concurrent write into it. Listings are built from files only, so an empty collection never appears in one.
- `List` walks collections with depth-1 `PROPFIND`s (many servers disable depth infinity), O(n) in the files under the directory. Hrefs outside the endpoint, or that are not clean keys, are dropped rather than trusted.
- Reads send a `Range` header; a server that ignores it is cut to the range by the driver.
- Statuses map to codes: 404 `CodeNotFound`, 401 and 403 `CodePermission`, 507 `CodeQuotaExceeded`, 413 `CodeTooLarge`, 423, 429 and 5xx `CodeUnavailable` (with the server's `Retry-After`).
- Redirects are followed only within the endpoint's scheme and host (a copy of `Config.Client` gets that policy, then the caller's own), so neither the credentials nor a body is ever sent elsewhere.
- The `Config` prints and logs with the password redacted, and the driver keeps the password inside a closure, so a logged `Config` or a printed driver never shows it. Use https: Basic authentication over plain http sends the password readable to anyone on the path.

## Try it

```sh
cd examples
go run ./adapters-webdav
go test ./adapters-webdav
```

On macOS it prints:

```text
capabilities: [copy,move,delete-prefix,sweep-abandoned,find-abandoned,dirs,case-insensitive]
stored: reports/2026-10.csv 18 bytes text/csv; charset=utf-8
listed: archive/2026/10.csv
listed: shared/latest.csv
made inbox/2026: exists=true, listed entries=0
after deleting its last file, shared exists=true
metadata: unsupported
if-not-exists: unsupported
reserved key: invalid_key
abandoned: staging aaaaaaaaaaaaaaaaaaaaaaaaaa
swept: 1
refused DSN (password in the URL): invalid_argument
refused DSN (plain http off loopback): invalid_argument
refused DSN (password_env without a user): invalid_argument
log line: level=INFO msg=opening webdav.endpoint=http://SERVER/remote.php/dav/files/alice/app webdav.username=alice webdav.password=[redacted]
```

`case-insensitive` appears because the served directory is on macOS's case-folding temp volume; on Linux it does not. The test checks every line above except that one.

## Pitfalls

- **An unset `password_env` variable is not an error at parse time.** The opener reads an empty password and `New`'s probe then fails with `CodePermission`. When opening fails with a permission error, check the variable before the account.
- **A replace is briefly a delete.** While an existing key is overwritten, a reader can get `CodeNotFound`. If readers must never see a gap, write each version under a new key and switch a pointer (a database row, the [records](../records/README.md) layer) instead of overwriting.
- **Collections accumulate.** Deleting files never removes the collections they were in. A tree of per-day folders keeps growing empty directories on the server until `DeletePrefix` clears them.
- **Case-folding servers.** A NAS on a case-insensitive volume, or a server on macOS or Windows, stores `Report.pdf` and `report.pdf` as one file. The disk declares `CaseInsensitive` when the probe sees it, and `filesystem.Sync` to such a target refuses to copy two source keys that differ only in case (each is reported as a failed copy with `CodeConflict`); check the capability yourself before mirroring onto it. A read-only account cannot run the probe and is reported case-sensitive.
- **A server that ignores `Expect: 100-continue`** costs each write up to one second. The driver sets `ExpectContinueTimeout` only on an `*http.Transport` (a copy, never yours); if `Config.Client` uses another `RoundTripper`, configuring it is yours.
- **Redirects to another host are not followed.** A Nextcloud behind a proxy that redirects `http` to `https`, or to another host name, gets the 3xx back as an error. Put the final URL, with `https`, in the DSN.
- **423 Locked** (another client holds a WebDAV lock, common with desktop sync clients editing the file) is `CodeUnavailable`: transient. Wrap the disk in `layer.Retry` if your writes may meet it; the driver never retries itself.
- **No signed URLs.** Serve downloads through your own handler, `fshttp` (see [serving files](../concepts/serving-files.md)), which streams from the disk after your authorisation check.
- **Listing is one `PROPFIND` per directory.** A recursive listing of a deep tree is slow; keep keys shallow, or keep an index in your database.

## Related

- [adapters/webdav/README.md](../../adapters/webdav/README.md): the reference for `Config`, the DSN and every behaviour.
- [SFTP](sftp.md) and [FTP](ftp.md): the other remote-server drivers, and how they differ on replaces and directories.
- [Capabilities](../concepts/capabilities.md): what a disk declares and what happens when an operation is not.
- [Maintenance jobs](../concepts/maintenance-jobs.md): running `SweepAbandoned` on a schedule.
- [Security](../concepts/security.md): keys, traversal and secrets in DSNs.
- [Comparison of every backend](README.md).
