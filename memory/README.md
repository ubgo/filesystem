# memory

`github.com/ubgo/filesystem/memory` is an in-process `filesystem.Driver` that keeps objects in a map. It supports every capability that needs no network, which makes it the reference implementation the conformance suite is written against, the base of `filesystemtest.Fake`, and a fit for caches that need no persistence. Nothing survives the process.

The full guide is in [Memory](../docs/adapters/memory.md); the concepts are in [Disks and drivers](../docs/concepts/disks-and-drivers.md).

```sh
STORAGE_CACHE_DSN=mem://cache
```

<!-- ds:block id=newmemorydisk-7us7uxyf -->
**newMemoryDisk** · [`examples/concepts-disks/main.go:95-97`](../examples/concepts-disks/main.go#L95-L97)

```go
func newMemoryDisk() (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New())
}
```
<!-- /ds:block hash=63e54c -->

## API

| Name | What it is |
|---|---|
| `New(options...)` | an empty memory driver; safe for concurrent use |
| `Driver` | the backend |
| `Option` | configures a `Driver` |
| `WithVersions()` | keeps every version of every key, like a versioned bucket: each write, copy, move, append and completed multipart upload adds a version, each delete a delete marker, and the driver declares `Versions` and `VersionRead`; memory grows with every write, so use it in tests |
| `WithClock(now)` | replaces `time.Now` for modification times, so tests can assert exact timestamps or make objects look old (for `upload.Sweep`) |
| `Scheme` | `"mem"`, for `Mux.Register`; the DSN's host is ignored, so `mem://cache` names it for readers |
| `Opener` | opens a fresh, empty driver for every DSN; accepts no parameters beyond the Mux's own |

The `Driver` methods (`Stat`, `Read`, `Write`, `Delete`, `List`, `Copy`, `Move`, `SetVisibility`, `DeletePrefix`, `DeleteKeys`, `CreateMultipart`, `UploadPart`, `ListParts`, `CompleteMultipart`, `AbortMultipart`, `SweepAbandoned`, `FindAbandoned`, `MakeDir`, `DirExists`, `Capabilities`) implement the `filesystem.Driver` contract and its optional interfaces; applications call them through a `*filesystem.Disk`.

## Behaviour

- `Read` returns a snapshot: a concurrent overwrite does not change bytes already being read.
- `Move` is atomic; `DeleteKeys` deletes in one locked pass.
- `CompleteMultipart` requires the parts in ascending order, each with the ETag `UploadPart` returned, as S3 does; anything else is `CodeInvalidArgument` and the upload stays open. Parts have no minimum size.
- Content types, metadata, HTTP headers and visibility are stored and returned by `Stat`.
- Every method honours its context: one that has ended fails the call with the context's error (`unavailable` once the Disk classifies it), and `Write` and `UploadPart` stop reading the body when the context ends mid-stream, storing nothing.

## Capabilities

`Copy`, `Move`, `SetVisibility`, `DeletePrefix`, `DeleteKeys`, `Multipart`, `SweepAbandoned` (drops uploads created or last given a part before the cutoff, by the driver's clock, so `WithClock` controls it in tests), `FindAbandoned` (the uploads that sweep would drop, left in place), `Dirs` (`MakeDir` records a directory that `DirExists` reports while it is empty, until `DeletePrefix` removes it or a parent; an object at its key is `CodeExists`), `Metadata`, `ContentType`, `Headers`, `ConditionalWrite`, `Append`, `SetModTime`, `Tags`, `StorageClass` (any class name, stored and reported by `Stat`), `Versions` and `VersionRead` (with `WithVersions`), `CompareAndSwap` (against the MD5 ETag, under the driver's lock). Not declared: `Presign` (use a `signedurl.Signer`), `PublicURL` (use `WithPublicURL`) and `PresignPart`. Because it has more capabilities than most production backends, code tested only against it can rely on features your backend lacks; see [Capabilities](../docs/concepts/capabilities.md).

## Limits

Everything lives in the process's memory: objects count against its heap, and two `Opener` calls (two DSNs, two processes, a restart) never share data.
