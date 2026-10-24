# Local disk

The local driver stores files in a directory on the machine running your application. Use it in development, in tests that should touch a real filesystem, and in production on a single server with a disk (or a shared volume). Every operation is sandboxed in the directory with `os.Root`, writes are atomic, and visibility is the file mode.

## Before you start

- The package is `github.com/ubgo/filesystem/local`, in the core module (no third-party dependencies). Its reference is [local/README.md](../../local/README.md).
- The runnable program is [`examples/adapters-local`](../../examples/adapters-local/), which works in a temporary directory: `go run ./adapters-local` from `examples/`.

## 1. Install

```sh
go get github.com/ubgo/filesystem
```

## 2. Open a disk from a DSN

```text
file:///var/app/uploads      absolute path
file:uploads                 path relative to the working directory
file:///C:/app/uploads       absolute Windows path (on Windows)
file:C:/app/uploads          the same, opaque form
```

The directory is created (with its parents) if missing. The local opener takes no parameters of its own; the Mux's `prefix`, `public_url`, `visibility` and `readonly` work as for every scheme (see [configuration](../concepts/configuration.md)). Refused with `CodeInvalidArgument`: any other parameter, a host (`file://uploads/x` reads `uploads` as a host; write `file:uploads/x` or `file:///abs/path`; `file://C:/x` puts the drive in the host, write `file:///C:/x`; UNC paths such as `file://server/share` are not opened), and an empty path. On Windows the leading slash of `file:///C:/x` is dropped to give the drive path `C:/x`; on every other system `/C:/x` is an ordinary absolute path and is kept as written. A bare drive (`file:///C:` or `file:C:`) means that drive's root, `C:/`, rather than Windows' per-drive current directory.

## 3. Open a disk in code

`local.New` opens the directory with options; this is the only way to change the file modes, the durability level or the reserved-name rule:

<!-- ds:block id=openwithnew-nqjh7jsb -->
**openWithNew** · [`examples/adapters-local/main.go:97-103`](../../examples/adapters-local/main.go#L97-L103)

```go
func openWithNew(dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir, local.WithPermissions(filePublic, filePrivate, dirMode))
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("uploads", drv)
}
```
<!-- /ds:block hash=502a15 -->

| Option | Default | Rule |
|---|---|---|
| `local.WithPermissions(filePublic, filePrivate, dir)` | `0644`, `0600`, `0755` | The public mode must have the world-read bit (`0o004`) and the private mode must not, because `Stat` reads visibility back from that bit; `New` refuses modes that break this with `CodeInvalidArgument`. |
| `local.WithDurability(level)` | `local.DurabilityFull` | How many fsyncs a write waits for: `DurabilityFull`, `DurabilityFile` or `DurabilityNone` (step 5); any other value is `CodeInvalidArgument`. |
| `local.AllowReservedNames()` | off | Lets keys use names Windows reserves, such as `con.txt` (step 8); no effect on Windows. |
| `local.WithContentType(fn)` | the host's MIME table (`mime.TypeByExtension`) | How `Stat` and `List` derive a file's `Content-Type` from its key, since a local file stores none. The host's table differs between a Mac, CI and production; pass `sniff.TypeByExtension` for a fixed table, or your own map. `fn` returns `""` when it does not know. |

The disk holds an open directory handle: close it with `disk.Close()` (or the Manager's `Close`) when the application stops.

Check: `disk.Capabilities()` prints `[move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap]`. On a filesystem that ignores letter case (macOS by default) it ends with `case-insensitive` too.

Try it: [`examples/adapters-local`](../../examples/adapters-local) runs this section in `contenttype.go` ([`appContentType`](ds:block?id=appcontenttype-v8c3m6tz), [`fixedTypes`](ds:block?id=fixedtypes-p2w7k4na)): `go run ./adapters-local` from `examples/`.

## 4. Visibility is the file mode

A public file gets the public mode and a private one the private mode, set exactly (the process umask does not change them). `Stat` reports `public` when the world-read bit is set. A web server running as another user can serve the public files directly; private ones stay readable only by the owner (or the group, with modes like the example's).

<!-- ds:block id=modes-snss4cwu -->
**modes** · [`examples/adapters-local/main.go:108-131`](../../examples/adapters-local/main.go#L108-L131)

```go
func modes(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.WriteBytes(ctx, "site/logo.svg", []byte("<svg/>"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "users/u1/id.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	root, ok := filesystem.As[*os.Root](disk)
	if !ok {
		return fmt.Errorf("disk %s is not a local disk", disk.Name())
	}
	for _, k := range []string{"site/logo.svg", "users/u1/id.pdf"} {
		info, err := root.Stat(k)
		if err != nil {
			return err
		}
		e, err := disk.Stat(ctx, k)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: mode %v, %s, %s\n", k, info.Mode().Perm(), e.Visibility, e.ContentType)
	}
	return nil
}
```
<!-- /ds:block hash=7d7723 -->

Check:

```text
site/logo.svg: mode -rw-r--r--, public, image/svg+xml
users/u1/id.pdf: mode -rw-r-----, private, application/pdf
```

The content type is not stored: `Stat` derives it from the extension, and `Write` returns the type the Disk detected from the bytes. `filesystem.As[*os.Root](disk)` returns the directory handle, as the example uses it to read the mode.

Visibility by file mode relies on POSIX permission bits. On Windows, Go's `chmod` only toggles the read-only attribute, so modes do not express visibility there; the repository's gate compiles the package and its tests for Windows, and the drive-letter DSN parsing is unit-tested on every OS, but the driver's tests have not yet been run on a Windows machine.

## 5. Atomic, conditional writes and moves

Every write goes to a temporary file under `.filesystem/tmp` inside the root, is synced to disk, gets its mode, and is renamed into place, so readers see the old file or the new one, and a crash cannot leave a half-written or empty file. A failed or cancelled write removes its temporary file.

How long a write waits for the disk is `local.WithDurability`, one of three levels:

| Level | Synced | After a crash or power cut |
|---|---|---|
| `local.DurabilityFull` (the default) | the staged file before the rename, then the directories whose entries changed: the key's directory, and every directory the write created | a write, `Move`, `CompleteMultipart` or `MakeDir` that returned is never lost |
| `local.DurabilityFile` | the staged file only | never a truncated or empty object, but a recent write may be lost or show its previous version |
| `local.DurabilityNone` | nothing | a recent object may be missing, empty or truncated; for caches, scratch space and tests |

`DurabilityFull` is the default because a caller assumes a write that returned is stored; it is what databases and `renameio` do, and costs one directory sync per write (more when the write creates directories). Windows cannot sync a directory, so there it behaves as `DurabilityFile`. A `Move` is durable at its destination; the source's directory is not synced, so a crash can leave the old name too. `New` refuses any other value with `invalid_argument`.

The level is set in code only: a disk opened from a `file:` DSN always uses `DurabilityFull`, since the opener takes no parameters. A directory whose files can be rebuilt, such as generated thumbnails or a download cache, can skip every fsync:

<!-- ds:block id=opencache-ymvzq9g7 -->
**openCache** · [`examples/adapters-local/main.go:290-296`](../../examples/adapters-local/main.go#L290-L296)

```go
func openCache(dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir, local.WithDurability(cacheDurability))
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("cache", drv)
}
```
<!-- /ds:block hash=22d356 -->

`local.DurabilityValues` is the closed set, and a write through the cache disk behaves like any other; only what survives a crash changes:

<!-- ds:block id=durabilitylevels-x36jn8tm -->
**durabilityLevels** · [`examples/adapters-local/main.go:301-316`](../../examples/adapters-local/main.go#L301-L316)

```go
func durabilityLevels(ctx context.Context, out io.Writer, dir string) error {
	fmt.Fprintf(out, "durability levels: %v\n", local.DurabilityValues)
	cache, err := openCache(dir)
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()
	e, err := cache.WriteBytes(ctx, "thumbs/p1.txt", []byte("rebuildable"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cache write (%s): %s is %d bytes\n", cacheDurability, e.Key, e.Size)
	_, err = local.New(dir, local.WithDurability("fast"))
	fmt.Fprintf(out, "unknown durability: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=119031 -->

Check: `durability levels: [full file none]`, `cache write (none): thumbs/p1.txt is 11 bytes` and `unknown durability: invalid_argument`. Which to choose:

- `DurabilityFull` for user uploads, documents and anything a database row points at, where a lost file after a power cut is a broken record.
- `DurabilityFile` when you can tolerate a recent write reverting after a crash but never a truncated file, for example a log of exports you can rerun.
- `DurabilityNone` for caches, scratch space and tests, where the files can be rebuilt and write speed matters more.

`IfNotExists` hard-links the temporary file to the key instead of renaming it; the link fails atomically when the key exists (`CodeExists`). The check is the link itself, after the whole body has been staged, so a losing writer streams its entire body before it learns it lost. The filesystem must support hard links, which every common Linux and macOS filesystem does. The staging name is unlinked under the key's lock before the entry is read, so the ETag the write returns is the object's current one and works in the next `IfMatch`. A write whose key's parent directory is removed by concurrent deletes three times in a row is `unavailable` (retry); a write waiting for the change time to move stops when its context ends. `Move` is an atomic rename inside the root.

<!-- ds:block id=conditionalandmove-es5n5qrq -->
**conditionalAndMove** · [`examples/adapters-local/main.go:136-152`](../../examples/adapters-local/main.go#L136-L152)

```go
func conditionalAndMove(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	once := filesystem.WriteOptions{IfNotExists: true}
	if _, err := disk.WriteBytes(ctx, "locks/job-7", []byte("worker-a"), once); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "locks/job-7", []byte("worker-b"), once)
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	if _, err := disk.Move(ctx, "locks/job-7", "done/job-7"); err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "done/job-7")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "moved: done/job-7 holds %q\n", got)
	return nil
}
```
<!-- /ds:block hash=82945f -->

Check: `second conditional write: exists` and `moved: done/job-7 holds "worker-a"`. This makes a local disk usable for simple claims and locks between processes on one machine.

## 6. Multipart uploads

Multipart works through your server (`UploadPart`); each part is stored under `.filesystem/multipart/<upload id>/` with its MD5 as ETag, and completion concatenates the parts into a staged file committed like any write. There are no presigned parts.

<!-- ds:block id=servermultipart-cqu8hk6y -->
**serverMultipart** · [`examples/adapters-local/main.go:157-177`](../../examples/adapters-local/main.go#L157-L177)

```go
func serverMultipart(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	const key = "exports/data.csv"
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	var parts []filesystem.Part
	for i, chunk := range []string{"id,total\n", "1,42\n"} {
		p, err := disk.UploadPart(ctx, key, id, i+1, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			return err
		}
		parts = append(parts, p)
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %s is %d bytes\n", e.Key, e.Size)
	return nil
}
```
<!-- /ds:block hash=cdeded -->

Check: `multipart: exports/data.csv is 14 bytes`.

An upload that is neither completed nor aborted keeps its parts under `.filesystem/multipart/` until something calls `AbortMultipart` or sweeps it. `disk.SweepAbandoned(ctx, before)` removes every upload whose newest file (its metadata, a part or a part's ETag) was written before `before`, and every staging file under `.filesystem/tmp` last written before `before` (what a crash in the middle of a write leaves behind), and returns how many it removed. Entries the driver did not create are left alone, and stored files are never touched. Nothing runs it automatically: schedule it, with a cutoff well beyond your longest upload, since a recent one aborts uploads still receiving parts. `disk.FindAbandoned(ctx, before)` is its dry run: the same scan, returning each upload (with the key from its metadata) and staging file it would remove, and removing nothing.

## 7. Signed and public URLs

A directory cannot sign URLs, so the disk gets a signer from your application: `signedurl` signs, and `fshttp.Signed` serves the URLs it issues (checking the signature, then streaming the file with ranges, ETag and `304` support). The same code then works on S3, where the backend signs instead.

<!-- ds:block id=opensigned-8jywgss3 -->
**openSigned** · [`examples/adapters-local/main.go:196-207`](../../examples/adapters-local/main.go#L196-L207)

```go
func openSigned(ctx context.Context, dir, base string) (*filesystem.Disk, *signedurl.Signer, error) {
	signer, err := signedurl.New(base, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, nil, err
	}
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return nil, nil, err
	}
	disk, err := mux.Open(ctx, "private", local.Scheme+"://"+filepath.ToSlash(dir), filesystem.WithURLSigner(signer))
	return disk, signer, err
}
```
<!-- /ds:block hash=e97bf0 -->

<!-- ds:block id=signeddownload-373dhs6j -->
**signedDownload** · [`examples/adapters-local/main.go:212-240`](../../examples/adapters-local/main.go#L212-L240)

```go
func signedDownload(ctx context.Context, out io.Writer, dir string) error {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	disk, signer, err := openSigned(ctx, dir, srv.URL+"/files")
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	srv.Config.Handler = http.StripPrefix("/files", fshttp.Signed(mgr, signer))
	if _, err := disk.WriteBytes(ctx, "contracts/c1.txt", []byte("signed copy"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	link, err := disk.SignedURL(ctx, "contracts/c1.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, err := http.Get(link.URL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "signed GET: %d %q\n", resp.StatusCode, body)
	return err
}
```
<!-- /ds:block hash=275b7a -->

Check: `signed GET: 200 "signed copy"`.

Public files served by a web server or CDN get their URL from `public_url`, without touching the disk:

<!-- ds:block id=publicurls-t4f5xshw -->
**publicURLs** · [`examples/adapters-local/main.go:246-262`](../../examples/adapters-local/main.go#L246-L262)

```go
func publicURLs(ctx context.Context, out io.Writer, dir string) error {
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return err
	}
	q := url.Values{}
	q.Set(filesystem.ParamPublicURL, cdnBase)
	q.Set(filesystem.ParamVisibility, string(filesystem.VisibilityPublic))
	disk, err := mux.Open(ctx, "public", local.Scheme+"://"+filepath.ToSlash(dir)+"?"+q.Encode())
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	u, err := disk.PublicURL(ctx, "img/a b.png")
	fmt.Fprintf(out, "public URL: %s\n", u)
	return err
}
```
<!-- /ds:block hash=112cd9 -->

Check: `public URL: https://cdn.example.com/media/img/a%20b.png`. See [serving files](../concepts/serving-files.md) for key rotation, upload URLs and `fshttp.Public`.

## 8. Keys, limits and refusals

- **Sandbox.** Every operation goes through an `os.Root`, so no key, `..` or symlink can reach outside the directory, even if the Disk's own key checks were bypassed. Directories and symlinks are never objects.
- **Reserved names.** Keys under `.filesystem/` belong to the staging directory and are refused with `CodeInvalidKey`; listings skip it.
- **Names Windows reserves.** A segment that is a Windows device name before its first dot, in any case (`CON`, `PRN`, `AUX`, `NUL`, `COM1` to `COM9`, `LPT1` to `LPT9`, so `con.txt` and `docs/Nul.tar.gz` too), or that ends in a dot or a space, is refused with `CodeInvalidKey` on every system, so a directory written on Linux or macOS can be copied to Windows. `local.AllowReservedNames()` lifts the refusal (it has no effect on Windows, which cannot create them). Files that already have such names, copied in from elsewhere, stay readable, movable and deletable, so they can be renamed away.
- **A key cannot be both a file and a directory.** `a` and `a/b` cannot both exist; writing `a/b` when `a` is a file is `CodeInvalidArgument`. Object stores allow both, so portable code should not rely on it.
- **No metadata or stored headers**: refused with `CodeUnsupported`, never dropped.
- **No server-side copy**: `Copy` streams the bytes, as fast as a local copy would.
- **Listing walks the tree**: `List` is O(n) in the files under the listed directory per page. Fine for development and moderate trees; large trees belong on an object store.
- **Empty directories are removed** after a delete or move, so the tree mirrors the keys.

<!-- ds:block id=refusedkeysandoptions-2u29xn83 -->
**refusedKeysAndOptions** · [`examples/adapters-local/main.go:182-190`](../../examples/adapters-local/main.go#L182-L190)

```go
func refusedKeysAndOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, local.StagingDir+"/tmp/x", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "staging key: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=8e6c81 -->

Check: `staging key: invalid_key`, `metadata: unsupported`, `headers: unsupported`.

<!-- ds:block id=reservednames-jp8rg5h9 -->
**reservedNames** · [`examples/adapters-local/main.go:269-279`](../../examples/adapters-local/main.go#L269-L279)

```go
func reservedNames(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	for _, k := range []string{"exports/con.txt", "exports/console.txt"} {
		_, err := disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{})
		code := filesystem.CodeOf(err)
		if err == nil {
			code = "ok"
		}
		fmt.Fprintf(out, "write %s: %s\n", k, code)
	}
	return nil
}
```
<!-- /ds:block hash=a484c3 -->

Check: `write exports/con.txt: invalid_key` and `write exports/console.txt: ok`.

## 9. In production

- **One server, or a shared volume.** Several application instances can share a local disk only through a shared filesystem (NFS, EFS, a Kubernetes volume). Atomic rename and hard links then depend on that filesystem; check its guarantees before relying on `IfNotExists` across machines.
- **Durability.** Keep the default `DurabilityFull` for anything you cannot rebuild; `DurabilityNone` suits a cache directory.
- **Directories.** The driver has real directories (`Capabilities().Dirs`): `disk.MakeDir` creates an empty one, `disk.DirExists` reports it, and deleting the last file in a directory removes it again, since the tree always mirrors the key space. See [keys](../concepts/keys.md).
- **Backups** are yours: the directory is plain files. [Backups and mirrors](../recipes/backups-and-mirrors.md) shows writing every file to a second backend as well.
- **Moving to object storage later** is a DSN change plus copying the files: [move to S3](../recipes/move-to-s3.md).

## Pitfalls

- **Directory modes go through the umask; file modes do not.** Files get their public or private mode exactly, because the driver sets it after staging. Directories are created with the `dir` mode of `WithPermissions` filtered by the process umask, so with a umask of `077` a `0750` directory becomes `0700` and a web server in the group can no longer enter it. Set the umask of the service (systemd's `UMask=`, for example) to match the modes you chose. Changing `WithPermissions` later does not rewrite existing files: only new writes and `SetVisibility` get the new modes.
- **Visibility is the world-read bit, whoever set it.** A file copied into the directory by `cp`, `rsync` or a restore with mode `0644` reads as `public`, and one with `0600` as `private`. After restoring a backup or importing files, check the modes, or call `SetVisibility` on each key. On Windows modes do not express visibility at all.
- **The mode protects from local users, not from the internet.** A private file is safe from the web only because nothing serves it. A web server running as the file's owner, or in its group with modes like `0640`, can read every private file.
- **Symlinks.** Every operation goes through an `os.Root`, so a symlink inside the directory that points outside it (`uploads/archive` to `/mnt/archive`, say) is refused on read and write, and listings never follow it. Mount the other volume at a path inside the directory, or open a second disk on it. The directory you pass to `New` may itself be a symlink, because it is resolved once when the root is opened.
- **The root is an open handle.** `New` opens the directory once and keeps it open. If a deploy then renames, deletes or replaces it (switching a `current` symlink to a new release, for example), the disk keeps reading and writing the old directory until it is closed and opened again. Keep uploads in a directory outside the release tree. `filesystem.As[*os.Root](disk)` gives you that handle, but writes through it skip the staging directory, the per-key locks and the ETag rules, so use it to read.
- **Case-insensitive filesystems.** On macOS and Windows by default, `a.PNG` and `a.png` are one file: writing the second overwrites the first. `New` probes for this and the disk then declares the `case-insensitive` capability, so code can check `disk.Capabilities().CaseInsensitive`. If keys can differ only by case (user-chosen names, for example), normalise them with `filesystem.WithKeyNormalizer`, or keep such data on a case-sensitive filesystem. Unicode spellings differ too: macOS file pickers send `café` decomposed, and `contrib/keynorm`'s `NFC` makes both spellings one key (see [keys](../concepts/keys.md)).
- **Windows reserved names** are refused everywhere by default (see step 8), so a key a user chooses, such as `aux.txt`, can fail on Linux too. Map user input to safe keys, or pass `local.AllowReservedNames()` if the directory never leaves a Unix system.
- **fsync cost.** With the default `DurabilityFull`, every write costs a file sync plus a directory sync, and one more for each directory it creates, so writing many small files into new directories is dominated by syncs, especially on network or cloud block storage where each sync can take milliseconds. For bulk imports into rebuildable data, open a disk with a lower level (step 5) rather than dropping durability for everything.
- **Temporary files after a crash.** A process killed mid-write leaves its staging file under `.filesystem/tmp`, and an upload nobody completes leaves its parts under `.filesystem/multipart`. Nothing removes them automatically. Schedule `disk.SweepAbandoned(ctx, time.Now().Add(-maxAge))` with an age well beyond your longest upload, and run `disk.FindAbandoned` first to see what it would remove (step 6, and [maintenance jobs](../concepts/maintenance-jobs.md)). Exclude `.filesystem/` from backups and from anything that syncs the directory elsewhere.
- **Network filesystems.** `IfNotExists` needs hard links and `Move` needs an atomic rename; NFS supports both, but some SMB mounts and FUSE filesystems (s3fs, for one) do not, and then conditional writes fail. The per-key locks that make `IfMatch` and compare-and-swap safe are held in this process only: two processes or machines writing the same key with `IfMatch` can both pass the check. `IfNotExists` stays safe across processes, because the hard link itself is the check. Test on the real mount before relying on any of this.
- **Disk full or read-only.** Each write stages the whole new body before replacing the old one, so overwriting a 1 GB file needs 1 GB free. When the disk fills mid-write, the write fails, its staging file is removed and the previous version stays intact. The driver does not classify `ENOSPC` or a read-only mount, so `filesystem.CodeOf` reports `unknown` (a permission error is `permission`); match the underlying error with `errors.Is(err, syscall.ENOSPC)` if you need to tell them apart, and monitor free space.
- **Serving through fshttp, not the directory.** Pointing a web server's static root at the directory publishes private files it can read and the staging directory with every in-progress upload. Serve downloads through `fshttp.Signed` (step 7) and public files through `fshttp.Public`, which answers 404 for private keys and refuses staging keys. For large files, `fshttp.Sendfile` lets nginx or Apache send the bytes after the handler has checked the request; give the proxy an internal location that only that header can reach. See [serving files](../concepts/serving-files.md).

## Next

- [In memory](memory.md), for tests.
- [Serving files](../concepts/serving-files.md) and [private downloads](../recipes/private-downloads.md).
- [Comparison of every backend](README.md).
