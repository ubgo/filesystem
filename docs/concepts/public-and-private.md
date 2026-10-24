# Public and private files

Some files are meant for everyone (a product photo, a public avatar, a logo) and most are not (an invoice, a contract, a chat attachment). This page explains how the library models that difference, how each backend stores it (per object, or per disk), and the layout most production applications end up with: a public bucket behind a CDN and a private one reached through signed URLs. Read it before deciding where your files go.

The snippets come from `examples/concepts-visibility`. Run it with `go run ./concepts-visibility` from the `examples` directory.

## The model

`filesystem.Visibility` has two values:

| Value | Meaning | How a reader gets the file |
|---|---|---|
| `VisibilityPrivate` | readable only with the backend's credentials | a signed URL that expires, or your application streaming it |
| `VisibilityPublic` | anyone with the URL may read it | its public URL, usually through a CDN |

Every object has one or the other. A write that does not choose gets the disk's default, which is private unless the disk was configured with `WithDefaultVisibility(VisibilityPublic)` or `visibility=public` in its DSN. Private by default because publishing a file by accident is worse than having to opt in.

## Step 1 — Per-object visibility

On a backend with the `SetVisibility` capability, each object has its own visibility: `WriteOptions.Visibility` sets it at write time, `Disk.SetVisibility` changes it later, and `Stat` reports it.

<!-- ds:block id=privatebydefault-xxynnbq9 -->
**privateByDefault** · [`examples/concepts-visibility/main.go:52-71`](../../examples/concepts-visibility/main.go#L52-L71)

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

Check: `default: private` and `after SetVisibility: public`.

How each backend stores it:

| Backend | Mechanism |
|---|---|
| local, SFTP | the file mode: public files are world-readable, private ones owner-only |
| memory | a field on the object |
| S3 | per-object ACLs, only with `acl=true` on the DSN (AWS buckets with ACLs enabled); otherwise per disk |
| GCS | per-object ACLs in the ACL mode (fine-grained buckets); otherwise per disk |
| Azure | per disk: a container is public or private as a whole |

On local disk the modes are 0644 and 0600 by default, so a web server serving the directory can read public files and nobody but the application's user can read private ones. `local.WithPermissions` changes the modes; the public mode must have the world-read bit and the private mode must not, because `Stat` reads visibility back from that bit.

<!-- ds:block id=filemodes-3vmcc4tw -->
**fileModes** · [`examples/concepts-visibility/main.go:77-100`](../../examples/concepts-visibility/main.go#L77-L100)

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

Check: `local public: -rw-r--r--` and `local private: -rw-------`.

## Step 2 — Per-disk visibility

Most buckets today have no per-object visibility: AWS disables ACLs on new buckets by default, R2 and most self-hosted stores have none, GCS buckets with uniform access have none, and Azure decides per container. On such a disk the disk's default visibility is a description of every object on it, and the Disk refuses to pretend otherwise: a write asking for another visibility, or `SetVisibility`, fails with `CodeUnsupported`. The demo stands in for such a bucket by hiding the memory driver's capability:

<!-- ds:block id=bucketwide-2wfzq3qx -->
**bucketWide** · [`examples/concepts-visibility/main.go:107`](../../examples/concepts-visibility/main.go#L107-L107)

```go
type bucketWide struct{ *memory.Driver }
```
<!-- /ds:block hash=a719b8 -->

<!-- ds:block id=wholedisk-p2thdqbt -->
**wholeDisk** · [`examples/concepts-visibility/main.go:120-133`](../../examples/concepts-visibility/main.go#L120-L133)

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

Check: `private write on a public bucket: unsupported` and `SetVisibility on a public bucket: unsupported`.

The disk's default must match how the bucket is really configured: the library cannot see a bucket policy. A disk declared `visibility=public` over a private bucket hands out public URLs that answer 403; a disk declared private over a public bucket leaves files readable by anyone who guesses a key. Set the DSN to what the bucket is.

## Step 3 — Two disks: public behind a CDN, private behind signed URLs

The layout that works on every provider is two buckets: one public, fronted by a CDN, for files meant for everyone; one private, never public, for everything else, reached through signed URLs. The records module supports it directly with `records.WithVisibilityDisks(public, private)`: `Store.Upload` with an empty disk name chooses the disk by the policy's visibility (private when it has none), `Store.URL` returns the public URL for a public file and a signed one for a private file, and `Store.SetVisibility` moves the file's object to the other disk, with its kept versions and regenerated thumbnails, so nothing of a file made private stays reachable through the CDN.

<!-- ds:block id=newlayout-rnmz9jyg -->
**newLayout** · [`examples/concepts-visibility/twodisks.go:44-59`](../../examples/concepts-visibility/twodisks.go#L44-L59)

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

<!-- ds:block id=twodisks-j35tq85s -->
**twoDisks** · [`examples/concepts-visibility/twodisks.go:93-127`](../../examples/concepts-visibility/twodisks.go#L93-L127)

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

```text
public on cdn: https://cdn.example.com/sz5mikal7rlcqareatfubl3hke.png
private on vault: https://app.example.com/files/vault/sz5mikal7rlcqareatfubl3hke.png
still on the CDN disk: false
```

The move follows the consistency rules: the object is copied to the other disk first, the row changes in a transaction, and the old object is deleted only after the commit; a failed transaction puts it back, so the row never claims a visibility the object does not have. A CDN may still serve its cached copy until the cache entry expires: purge the CDN path when you make a file private, or keep public cache lifetimes short for files that might be withdrawn. See [Replace and visibility](../records/replace-and-visibility.md).

## Choosing

| File | Visibility | Why |
|---|---|---|
| Product photos, public profile pictures, logos | public, on the CDN disk | read on every page view; a signed URL per view would defeat the CDN |
| Invoices, contracts, exports, chat attachments | private | a link must stop working; access depends on who is asking |
| Drafts that will be published | private, moved on publish | an unpublished draft must not be reachable by guessing its key |
| Files of a deleted or suspended account | private, or purged | `SetVisibility` or `PurgeOwner` |

Random keys (`upload.RandomKey`) make public URLs unguessable but not secret: anyone who has seen one can share it. That is fine for public files and the reason private files use signed URLs.

## Pitfalls

- Making a whole bucket public to serve thumbnails, then storing private files on it. Keep two disks.
- Asking for `VisibilityPrivate` on a public-default disk without per-object visibility and expecting it to work: it fails with `CodeUnsupported`, by design.
- Serving private files through `fshttp.Public`: it serves only public objects and answers 404 for private ones. Private files go through `fshttp.Signed` or the backend's signed URLs.
- Changing the visibility of a file without records on a two-bucket layout: `Disk.SetVisibility` does not move objects between disks. Use `CopyAcross` and `Delete`, or the records Store, which does it in the right order.

## Next

- [Serving files](serving-files.md): public URLs, signed URLs and the handlers that serve them.
- [Capabilities](capabilities.md): `SetVisibility` and the other capabilities per backend.
- Recipes: [Public files behind a CDN](../recipes/public-cdn.md), [Private downloads](../recipes/private-downloads.md), [Product images](../recipes/product-images.md).
