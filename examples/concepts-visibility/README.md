# Public and private files

Product images and avatars should be public and cacheable behind a CDN; invoices and contracts must stay private behind expiring links. This program shows how the library models that: every write is private unless the caller or the disk says otherwise, visibility is per object where the backend has it (file modes on the local driver), and a property of the whole disk where it does not (S3 with ACLs disabled, R2, GCS with uniform access, Azure), in which case asking for another visibility on one object is refused rather than silently ignored. It ends with the two-disk layout `records` uses: public files on a CDN disk, private ones on a disk that signs links, and making a file private moves its object off the CDN disk. It uses the core `filesystem` package, `memory`, `local`, `signedurl`, `upload` and `records` (on SQLite). The guide [Public and private files](../../docs/concepts/public-and-private.md) explains each step and how to choose.

## Run it

```sh
cd examples
go run ./concepts-visibility
go test ./concepts-visibility
```

It needs no network or environment variables. The local-driver step writes into a temporary directory, removed before the program exits, and the records step uses a private in-memory SQLite database, so nothing is left behind. A bucket without per-object ACLs is simulated by `bucketWide`, a memory driver that reports no `SetVisibility` capability. `cdn.example.com` and `app.example.com` are only used to build URLs and are never fetched.

## What it does

```sh
go run ./concepts-visibility
```

```text
default: private
after SetVisibility: public
local public: -rw-r--r--
local private: -rw-------
private write on a public bucket: unsupported
SetVisibility on a public bucket: unsupported
public on cdn: https://cdn.example.com/o43e5spu77sdc5malledvixq4a.png
private on vault: https://app.example.com/files/vault/o43e5spu77sdc5malledvixq4a.png
still on the CDN disk: false
```

The key in the last three lines is random and changes on every run.

1. A write that names no visibility is private; `SetVisibility` publishes it.
2. The local driver stores visibility as a file mode: public files are world-readable (0644) so a web server can serve the directory, private ones are owner-only (0600). The modes shown are from macOS or Linux.
3. On a disk whose backend has no per-object visibility, the disk default describes every object, and a private write or a `SetVisibility` call is `unsupported`.
4. A `records` Store with `WithVisibilityDisks` puts a public upload on the CDN disk and gives its CDN URL; making the file private moves the object to the vault disk, whose URL is a signed link (query left out), and the object is gone from the CDN disk.

## The code

Private by default, then published.

<!-- ds:block id=privatebydefault-xxynnbq9 -->
**privateByDefault** · [`examples/concepts-visibility/main.go:52-71`](main.go#L52-L71)

```go
func privateByDefault(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	e, err := disk.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "default: %s\n", e.Visibility)
	if err := disk.SetVisibility(ctx, "report.pdf", filesystem.VisibilityPublic); err != nil {
		return err
	}
	e, err = disk.Stat(ctx, "report.pdf")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after SetVisibility: %s\n", e.Visibility)
	return nil
}
```
<!-- /ds:block hash=63b174 -->

Visibility as file modes on the local driver.

<!-- ds:block id=filemodes-3vmcc4tw -->
**fileModes** · [`examples/concepts-visibility/main.go:77-100`](main.go#L77-L100)

```go
func fileModes(ctx context.Context, out io.Writer, dir string) error {
	drv, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		_ = drv.Close()
		return err
	}
	defer func() { _ = disk.Close() }()
	for _, v := range filesystem.VisibilityValues {
		key := string(v) + ".txt"
		if _, err := disk.WriteBytes(ctx, key, []byte(key), filesystem.WriteOptions{Visibility: v}); err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(dir, key))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "local %s: %v\n", v, info.Mode().Perm())
	}
	return nil
}
```
<!-- /ds:block hash=be4ed1 -->

A bucket without per-object ACLs, and visibility as a property of the whole disk.

<!-- ds:block id=bucketwide-2wfzq3qx -->
**bucketWide** · [`examples/concepts-visibility/main.go:107`](main.go#L107-L107)

```go
type bucketWide struct{ *memory.Driver }
```
<!-- /ds:block hash=a719b8 -->

<!-- ds:block id=wholedisk-p2thdqbt -->
**wholeDisk** · [`examples/concepts-visibility/main.go:120-133`](main.go#L120-L133)

```go
func wholeDisk(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("cdn", bucketWide{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "logo.png", []byte("png"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "secret.pdf", []byte("pdf"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	fmt.Fprintf(out, "private write on a public bucket: %s\n", filesystem.CodeOf(err))
	err = disk.SetVisibility(ctx, "logo.png", filesystem.VisibilityPrivate)
	fmt.Fprintf(out, "SetVisibility on a public bucket: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=ec328b -->

The two disks: public behind a CDN base URL, private with a signer.

<!-- ds:block id=newlayout-rnmz9jyg -->
**newLayout** · [`examples/concepts-visibility/twodisks.go:44-59`](twodisks.go#L44-L59)

```go
func newLayout() (*filesystem.Manager, error) {
	signer, err := signedurl.New(appBase, []signedurl.Key{{ID: "k1", Secret: demoSecret}})
	if err != nil {
		return nil, err
	}
	cdn, err := filesystem.NewDisk(publicDisk, bucketWide{memory.New()},
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return nil, err
	}
	vault, err := filesystem.NewDisk(privateDisk, bucketWide{memory.New()}, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(cdn, vault)
}
```
<!-- /ds:block hash=3bc001 -->

A public upload made private, moving its object to the private disk.

<!-- ds:block id=twodisks-j35tq85s -->
**twoDisks** · [`examples/concepts-visibility/twodisks.go:93-127`](twodisks.go#L93-L127)

```go
func twoDisks(ctx context.Context, out io.Writer) error {
	disks, err := newLayout()
	if err != nil {
		return err
	}
	store, db, err := openStore(ctx, disks)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := store.Upload(ctx, "acme", "", strings.NewReader("\x89PNG\r\n\x1a\n"), "banner.png", p)
	if err != nil {
		return err
	}
	if err := describe(ctx, store, f, out); err != nil {
		return err
	}
	if err := store.SetVisibility(ctx, "acme", f.ID, filesystem.VisibilityPrivate); err != nil {
		return err
	}
	if f, err = store.File(ctx, "acme", f.ID); err != nil {
		return err
	}
	if err := describe(ctx, store, f, out); err != nil {
		return err
	}
	cdn, err := disks.Disk(publicDisk)
	if err != nil {
		return err
	}
	onCDN, err := cdn.Exists(ctx, f.Key)
	fmt.Fprintf(out, "still on the CDN disk: %t\n", onCDN)
	return err
}
```
<!-- /ds:block hash=d08ade -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: private by default, local file modes, the bucket-wide stand-in and whole-disk visibility |
| `twodisks.go` | The two-disk layout with a `records` Store |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Public and private files](../../docs/concepts/public-and-private.md), with how to choose and the pitfalls.
- Related examples: [concepts-serving](../concepts-serving) for signed links and public files over HTTP, [concepts-capabilities](../concepts-capabilities), [recipe-public-cdn](../recipe-public-cdn), [recipe-private-downloads](../recipe-private-downloads).
