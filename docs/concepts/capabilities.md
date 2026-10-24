# Capabilities

Backends differ: S3 signs URLs and copies server-side, local disk renames atomically but stores no metadata, an Azure container has no per-object visibility. A capability is one thing only some backends can do. This page lists every capability, what the Disk does when a backend lacks it (a portable fallback, or an error with `CodeUnsupported`), how to check before calling, and how the library makes sure a driver cannot claim what it does not do. Read it before relying on a feature that is not in the six-method `Driver` contract.

The snippets come from `examples/concepts-capabilities`. Run it with `go run ./concepts-capabilities` from the `examples` directory.

## The rule: never silently drop

A driver declares what it can do in one `filesystem.Capabilities` struct, returned by `Capabilities()` and constant for the driver's life. The Disk calls an optional method only when its capability is true. When it is false, the Disk does one of two things and never a third:

- **Fall back** to a portable implementation that gives the same result: Copy, Move, DeleteKeys and DeletePrefix.
- **Fail** with an error whose code is `CodeUnsupported`, before anything is written: every other capability.

What it never does is accept an option and ignore it. A write with `Metadata` on local disk does not store the object without the metadata and report success; it fails, so the application finds out at the first test run rather than when a feature built on that metadata misbehaves in production.

## Step 1 — What each driver declares

`Capabilities.String()` renders the true bits, for logs and diagnostics:

```text
local:  [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap]
memory: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]
```

Every capability, the Disk methods that use it, and what happens without it:

| Capability | Interface | Used by | Without it |
|---|---|---|---|
| `Copy` | `Copier` | `Copy`, `CopyAcross` on one disk | fallback: Stat, Read and Write through the application, keeping content type, metadata, headers and visibility where the driver stores them |
| `Move` | `Mover` | `Move` | fallback: Copy then Delete; not atomic, a failure between the two leaves both keys |
| `DeleteKeys` | `KeysDeleter` | `DeleteKeys` | fallback: one `Delete` per key, with the same `*BatchError` reporting |
| `DeletePrefix` | `PrefixDeleter` | `DeletePrefix` | fallback: a recursive List, deleting as it goes |
| `SetVisibility` | `VisibilitySetter` | `SetVisibility`; a `Write` or `CreateMultipart` whose visibility differs from the disk default | `CodeUnsupported`; every object has the disk's default visibility |
| `Presign` | `Presigner` | `SignedURL` | the disk's `URLSigner` (`WithURLSigner`) signs instead; with neither, `CodeUnsupported`. A `URLSigner` set on a presigning disk signs downloads anyway, see [Serving files](serving-files.md#which-path-signs-which-method) |
| `PublicURL` | `PublicURLer` | `PublicURL` | `WithPublicURL(base)` builds it instead; with neither, `CodeUnsupported` |
| `Multipart` | `MultipartUploader` | `CreateMultipart`, `UploadPart`, `ListParts`, `CompleteMultipart`, `AbortMultipart` | `CodeUnsupported` |
| `PresignPost` | `PostPresigner` | `PresignPost`, `upload.Presign` with `Policy.PostForm` | `CodeUnsupported`; scoped disks prefix the form's key, mirrored disks never forward it |
| `PresignPart` | `PartPresigner` | `PresignPart`, `upload.Uppy` | `CodeUnsupported`; `upload.Uppy` answers 501 on every route |
| `Append` | `Appender` | `Append` | `CodeUnsupported` |
| `SetModTime` | `ModTimeSetter` | `SetModTime` | `CodeUnsupported` |
| `ConditionalRead` | `ConditionalReader` | `ReadIf` | the Disk compares against a `Stat` |
| `Tags` | `Tagger` | `Tags`, `SetTags` | `CodeUnsupported` |
| `Hold` | `Holder` | `Hold`, `SetHold` | `CodeUnsupported` |
| `Versions` | `VersionLister` | `Versions` | `CodeUnsupported` |
| `VersionRead` | `VersionReader` (needs `Versions`) | `ReadVersion` opens one stored version by the ID `Versions` reported; `RestoreVersion` makes it the current content again as a new latest version, losing none | `CodeUnsupported` |
| `StorageClass` | `StorageClasser` | `SetStorageClass` moves an object to a class named by the backend (S3 `STANDARD_IA`, GCS `NEARLINE`, Azure `Cool`); `Entry.StorageClass` reports it | `CodeUnsupported`; an empty class is `CodeInvalidArgument` on every disk |
| `Restore` | `Restorer` | `Restore` | `CodeUnsupported` |
| `CompareAndSwap` | none | `WriteOptions.IfMatch` | `CodeUnsupported` |
| `CaseInsensitive` | none | reported, not asked for: keys differing only in case are one object (local disk on macOS and Windows by default) | keys are case-sensitive |
| `SweepAbandoned` | `AbandonedSweeper` | `SweepAbandoned`: removes multipart uploads nobody completed or aborted, and staging files of interrupted writes | `CodeUnsupported`; scoped and mirrored disks never forward it, because the leftovers are backend-wide |
| `FindAbandoned` | `AbandonedFinder` (needs `SweepAbandoned`) | `FindAbandoned`: the dry run of `SweepAbandoned`, listing what it would remove now (kind, upload ID or staging name, the upload's key, last activity) and removing nothing | `CodeUnsupported`; never forwarded by a wrapper disk, like the sweep |
| `Dirs` | `DirMaker` | `MakeDir` creates a real, possibly empty directory; `DirExists` asks the backend | `MakeDir` does nothing (no marker object), and `DirExists` lists the directory with a limit of one entry: a directory exists while a key is under it |
| `Metadata` | none | `WriteOptions.Metadata`, `Entry.Metadata` | a non-empty `Metadata` is `CodeUnsupported` |
| `ContentType` | none | `WriteOptions.ContentType`, `Entry.ContentType` | the type is not stored; `Write` still returns it in the entry, and some drivers derive one from the extension on `Stat` |
| `Headers` | none | `WriteOptions.Headers`, `Entry.Headers` | non-empty `Headers` are `CodeUnsupported` |
| `PresignHeaders` | none (needs `Presign` and `Headers`) | `PresignOptions.Headers` on a driver-signed PUT: the backend refuses an upload without them and stores them | non-empty `PresignOptions.Headers` are `CodeUnsupported`; through a `URLSigner` instead, the driver must store headers and the signer implement `HeaderPinner` |
| `PresignVisibility` | none (needs `SetVisibility` and `Presign` or `PresignPost`) | `PresignOptions.Visibility` on a driver-signed PUT and `PostPolicy.Visibility` on a POST form: the backend stores the upload with that visibility and refuses a request that changes it (S3 and GCS in ACL mode) | a visibility the disk must pin is `CodeUnsupported`; on a disk without `SetVisibility` the default visibility needs no pin and is accepted; a `URLSigner` never pins one |
| `ChecksumSHA256` | none | `WriteOptions.SHA256`: the driver sends the digest and the backend refuses a body that does not match | nothing changes for the caller: the Disk verifies the digest itself on every driver before the write commits; the capability adds the backend's own check on its side of the wire |
| `ConditionalWrite` | none | `WriteOptions.IfNotExists` | `CodeUnsupported`, never a silent overwrite |

The two URL capabilities have configured alternatives on the Disk, which is why `SignedURL` works on local disk with a `signedurl.Signer` and `PublicURL` works on any disk with a CDN base. Only two drivers implement `PublicURL`: Vercel Blob on a public store and Cloudinary for `upload` resources; every other driver's public URLs come from `WithPublicURL` (or the `public_url` DSN parameter), and a configured base wins over a driver's own URL on any disk. See [Serving files](serving-files.md).

## Step 2 — Fallbacks behave like the native operation

The local driver has no `Copy` (a local copy would stream bytes exactly as the fallback does, so it does not claim one) and no `DeleteKeys`. Both calls work, and the copied object keeps its visibility:

<!-- ds:block id=fallbacks-njuba3a9 -->
**fallbacks** · [`examples/concepts-capabilities/main.go:68-82`](../../examples/concepts-capabilities/main.go#L68-L82)

```go
func fallbacks(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("public file"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	e, err := disk.Copy(ctx, "a.txt", "b.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "streamed copy: %s %d bytes, %s\n", e.Key, e.Size, e.Visibility)
	if err := disk.DeleteKeys(ctx, []string{"a.txt", "b.txt"}); err != nil {
		return err
	}
	fmt.Fprintln(out, "deleted two keys one by one: ok")
	return nil
}
```
<!-- /ds:block hash=5d7d4a -->

Check: `streamed copy: b.txt 11 bytes, public` and `deleted two keys one by one: ok`.

A fallback runs as the operations it is made of, so interceptors see a streamed Copy as a `stat`, a `read` and a `write`, and a Move fallback as those plus a `delete`. Metrics count each.

## Step 3 — Everything else is CodeUnsupported

Asking local disk to store metadata or HTTP headers, or to sign a URL without a signer, or to build a public URL without a base, fails before anything is written:

<!-- ds:block id=unsupported-kra8uej8 -->
**unsupported** · [`examples/concepts-capabilities/main.go:88-103`](../../examples/concepts-capabilities/main.go#L88-L103)

```go
func unsupported(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	_, err := disk.WriteBytes(ctx, "m.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"owner": "u1"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "h.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "m.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	fmt.Fprintf(out, "signed URL without a signer: %s\n", filesystem.CodeOf(err))
	_, err = disk.PublicURL(ctx, "m.txt")
	fmt.Fprintf(out, "public URL without a base: %s\n", filesystem.CodeOf(err))
	ok, err := disk.Exists(ctx, "m.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "m.txt stored anyway: %t\n", ok)
	return nil
}
```
<!-- /ds:block hash=f314a7 -->

Check: four lines ending in `unsupported`, then `m.txt stored anyway: false`.

## Step 4 — Check before calling

`disk.Capabilities()` lets code offer a feature only where the backend has it, or choose another path:

<!-- ds:block id=checkfirst-u4pgcann -->
**checkFirst** · [`examples/concepts-capabilities/main.go:110-117`](../../examples/concepts-capabilities/main.go#L110-L117)

```go
func checkFirst(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	opts := filesystem.WriteOptions{}
	if disk.Capabilities().Metadata {
		opts.Metadata = map[string]string{"owner": "u1"}
	}
	_, err := disk.WriteBytes(ctx, "report.txt", []byte("x"), opts)
	fmt.Fprintf(out, "write after checking Metadata: %v\n", err)
}
```
<!-- /ds:block hash=c8ec05 -->

Typical uses: show the "upload directly from the browser" option only when `Capabilities().PresignPart` is true; keep metadata in your own database when `Metadata` is false; hide the "make public" toggle when `SetVisibility` is false and explain that the bucket decides. The records module does the last one for you with two disks; see [Public and private files](public-and-private.md).

## Step 5 — A driver cannot lie

`NewDisk` compares the declaration with the interfaces the driver implements. A driver that declares a capability without implementing its interface is refused with `CodeInvalidArgument` at start-up, so the first `Copy` in production can never be the first time anyone notices. `PresignPart` without `Multipart` is refused the same way.

<!-- ds:block id=liardriver-gzywh4kk -->
**liarDriver** · [`examples/concepts-capabilities/main.go:122`](../../examples/concepts-capabilities/main.go#L122-L122)

```go
type liarDriver struct{ inner *memory.Driver }
```
<!-- /ds:block hash=44519c -->

<!-- ds:block id=refuseliar-wra6kbkq -->
**refuseLiar** · [`examples/concepts-capabilities/main.go:150-153`](../../examples/concepts-capabilities/main.go#L150-L153)

```go
func refuseLiar(out io.Writer) {
	_, err := filesystem.NewDisk("liar", liarDriver{inner: memory.New()})
	fmt.Fprintf(out, "NewDisk(liar): %v\n", err)
}
```
<!-- /ds:block hash=2549b2 -->

Check: `NewDisk(liar): filesystem: invalid_argument: driver main.liarDriver declares capability Copy but does not implement its interface`.

The opposite direction is allowed: implementing an interface while declaring it false. Wrapper drivers rely on it. A scoped disk's driver implements every optional interface and reports the capabilities of the driver underneath, so the Disk calls only what the real backend supports. This is also why the library uses interceptors rather than decorators: a decorator that wraps a driver hides the optional interfaces it forgot to forward, while an interceptor never touches the driver. See [Interceptors](interceptors.md).

The attribute capabilities (`Metadata`, `ContentType`, `Headers`, `ConditionalWrite`) have no interface to check. The conformance suite in `filesystemtest` covers them: it stores and reads back content types, metadata and headers, and checks that a second conditional write gets `exists` and leaves the first, on every disk that declares them, failing when the backend does not deliver; and on a disk that does not declare `Metadata` or `Headers` it checks that the option is refused. See [the filesystemtest reference](../../filesystemtest/README.md).

## Capabilities by backend

| Capability | local | memory | s3 | gcs | azure | sftp |
|---|---|---|---|---|---|---|
| Copy | fallback | yes | yes | yes | yes | fallback |
| Move | yes | yes | fallback | fallback | fallback | yes |
| SetVisibility | file mode | yes | with `acl=true` | with ACL mode | no | file mode |
| DeletePrefix | yes | yes | yes | fallback | fallback | yes |
| DeleteKeys | fallback | yes | yes (1,000 per request) | fallback | fallback | fallback |
| Presign | via signer | via signer | yes (SigV4) | with a signing key | with a shared key | via signer |
| PublicURL | via base | via base | via base | via base | via base | via base |
| Multipart | yes | yes | yes | no | yes | no |
| PresignPart | no | no | yes | no | no | no |
| Metadata | no | yes | yes | yes | yes | no |
| ContentType | no (from extension) | yes | yes | yes | yes | no |
| Headers | no | yes | yes | yes | yes | no |
| ConditionalWrite | yes | yes | with `conditional_write=true` | yes | yes | with hard links |

Some capabilities on S3 depend on the provider, so they are configuration, not guesses: per-object ACLs exist on AWS (when enabled on the bucket) but not on R2 or most self-hosted stores, and not every S3-compatible store honours `If-None-Match`. The driver claims those only when its DSN says the provider supports them. The provider pages list what to set: [S3-compatible stores](../adapters/s3-compatible.md). The per-adapter matrix with every detail is in [Adapters](../adapters/README.md) and the full reference in [reference/capabilities](../reference/capabilities.md).

## Pitfalls

- Testing only on the memory driver: it has nearly every capability, so code that writes `Metadata` passes its tests and fails with `CodeUnsupported` on local disk or SFTP. Run the tests that matter on the backend you deploy, or check `Capabilities()` in the code.
- Assuming visibility is per object everywhere: on Azure, on S3 without `acl=true`, and on GCS without the ACL mode, the disk's default is the visibility of every object, and a write asking for another one fails.
- Expecting `Move` to be atomic on object stores: S3, GCS and Azure have no rename, so `Move` is a copy and a delete.

## Next

- [Errors](errors.md): `CodeUnsupported` and every other code.
- [Public and private files](public-and-private.md): visibility per object or per disk.
- [Write a driver](../extending/write-a-driver.md): declaring capabilities and passing the conformance suite.
