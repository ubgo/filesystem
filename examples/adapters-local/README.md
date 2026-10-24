# Local disk driver

The local driver stores files in a directory on the machine, every operation confined to it through an `os.Root`. This program opens a directory with custom file modes and reads visibility back from them, writes conditionally and moves atomically, assembles a server-side multipart upload, shows the reserved staging directory, the names Windows reserves and the options local disk cannot store, serves a private file through a signed URL that the application itself checks, builds public URLs from a CDN base, and opens a cache directory that skips every fsync. It uses the core `filesystem`, `local`, `signedurl` and `fshttp` packages. The guide [Local disk](../../docs/adapters/local.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-local
go test ./adapters-local
```

It needs no network or environment variables. It works in a fresh temporary directory (`adapters-local-*` under the system temp dir), deleted when the program exits, and starts an `httptest` server on a random local port for the signed download, closed before it returns.

## What it does

```text
capabilities: [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap]
site/logo.svg: mode -rw-r--r--, public, image/svg+xml
users/u1/id.pdf: mode -rw-r-----, private, application/pdf
second conditional write: exists
moved: done/job-7 holds "worker-a"
multipart: exports/data.csv is 14 bytes
staging key: invalid_key
metadata: unsupported
headers: unsupported
write exports/con.txt: invalid_key
write exports/console.txt: ok
signed GET: 200 "signed copy"
public URL: https://cdn.example.com/media/img/a%20b.png
durability levels: [full file none]
cache write (none): thumbs/p1.txt is 11 bytes
unknown durability: invalid_argument
fixed types: Stat studies/chest.dcm -> "application/dicom"
fixed types: Stat notes/readme.md -> "text/markdown"
fixed types: Stat notes/data.unknownext -> ""
fixed types: List studies/chest.dcm -> "application/dicom"
```

On a filesystem that ignores letter case (macOS by default) the capabilities line ends with `,case-insensitive`.

1. The disk declares what a directory can do; it has no `copy`, `metadata` or `headers`, and signed URLs only when given a signer.
2. A public and a private file are written with modes `0644` and `0640`; `Stat` reads visibility back from the world-read bit and sniffs the content type from the bytes.
3. A conditional write (a hard link, atomic) succeeds once and then fails with `exists`; `Move` renames atomically.
4. Two parts uploaded through the server are assembled into one 14-byte file; they wait in the reserved staging directory until completion.
5. A key inside the staging directory is `invalid_key`, and metadata or headers are `unsupported`: refused, not silently dropped.
6. `con.txt` is a device name on Windows, so it is `invalid_key` on every system by default; `console.txt` only starts like one and is stored.
7. A second disk opened from a DSN with a URL signer issues a signed link; `fshttp.Signed` checks it and streams the private file with status 200.
8. A third disk opened with `public_url` returns base plus escaped key from `PublicURL` without touching the disk.
9. A cache disk opened with `local.WithDurability(local.DurabilityNone)` writes without any fsync, and `local.New` refuses a level outside `DurabilityValues` with `invalid_argument`.
10. A disk opened with `local.WithContentType(appContentType)` takes `Stat` and `List` types from the app's own table (`.dcm`), then `sniff.TypeByExtension` (`.md`), never from the host's MIME files, so the answer is the same on every machine; an extension neither knows reports an empty type.

## The code

`local.New` creates the directory if missing and sets the modes for public files, private files and directories.

<!-- ds:block id=openwithnew-nqjh7jsb -->
**openWithNew** · [`examples/adapters-local/main.go:97-103`](main.go#L97-L103)

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

Visibility as file modes, read through `filesystem.As[*os.Root]`, the escape hatch to the confined root.

<!-- ds:block id=modes-snss4cwu -->
**modes** · [`examples/adapters-local/main.go:108-131`](main.go#L108-L131)

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

A once-only write and an atomic rename, the building blocks of a simple lock or job handoff.

<!-- ds:block id=conditionalandmove-es5n5qrq -->
**conditionalAndMove** · [`examples/adapters-local/main.go:136-152`](main.go#L136-L152)

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

A server-side multipart upload, the same API as on S3 or Azure.

<!-- ds:block id=servermultipart-cqu8hk6y -->
**serverMultipart** · [`examples/adapters-local/main.go:157-177`](main.go#L157-L177)

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

The staging directory refused as a key, and the options a directory cannot store refused rather than lost.

<!-- ds:block id=refusedkeysandoptions-2u29xn83 -->
**refusedKeysAndOptions** · [`examples/adapters-local/main.go:182-190`](main.go#L182-L190)

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

Names Windows reserves are refused on every system, so the directory can be copied to Windows later; `local.AllowReservedNames()` lifts this.

<!-- ds:block id=reservednames-jp8rg5h9 -->
**reservedNames** · [`examples/adapters-local/main.go:269-279`](main.go#L269-L279)

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

A local disk has no service to sign for it, so the application does: a `signedurl.Signer` makes the URLs, and they point back at the application.

<!-- ds:block id=opensigned-8jywgss3 -->
**openSigned** · [`examples/adapters-local/main.go:196-207`](main.go#L196-L207)

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

`fshttp.Signed` behind `/files` checks the signature and streams the file.

<!-- ds:block id=signeddownload-373dhs6j -->
**signedDownload** · [`examples/adapters-local/main.go:212-240`](main.go#L212-L240)

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

Public files served by a web server or CDN: the `public_url` parameter is all `PublicURL` needs.

<!-- ds:block id=publicurls-t4f5xshw -->
**publicURLs** · [`examples/adapters-local/main.go:246-262`](main.go#L246-L262)

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

A cache directory whose files can be rebuilt skips every fsync with `local.WithDurability`.

<!-- ds:block id=opencache-ymvzq9g7 -->
**openCache** · [`examples/adapters-local/main.go:290-296`](main.go#L290-L296)

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

The closed set of levels, a write through the cache disk, and an unknown level refused.

<!-- ds:block id=durabilitylevels-x36jn8tm -->
**durabilityLevels** · [`examples/adapters-local/main.go:301-316`](main.go#L301-L316)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: modes, conditional write and move, multipart, refusals, reserved names, signed download through fshttp, public URLs, durability levels |
| `contenttype.go` | A fixed content-type table for `Stat` and `List` with `local.WithContentType` |
| `main_test.go` | Runs the program in a temporary directory and checks each line the guide quotes |

## Next

- The guide: [Local disk](../../docs/adapters/local.md).
- [Serving files](../concepts-serving) for signed links and `fshttp` in depth, and [public and private files](../concepts-visibility).
- Other backends: [memory](../adapters-memory), [S3](../adapters-s3), [GCS](../adapters-gcs), [Azure](../adapters-azure), [SFTP](../adapters-sftp).
