# Integrity

A storage library has to prove two things about every file: that what it stored is what was sent, and that what it holds later is still what it stored. This page covers the checks that do it: the SHA-256 every write computes while it streams, a declared digest and size held against the body before it replaces anything, ETags for cheap change checks, checksums the backend verifies on its own side, content-addressed keys and deduplication, and scrubbing stored files against their records. Read it when files matter enough that a silent corruption, a truncated upload or a swapped file would be a bug report.

The snippets come from `examples/concepts-integrity`. Run it with `go run ./concepts-integrity` from the `examples` directory.

## Which check answers which question

| Question | Check | Where |
|---|---|---|
| What digest does the stored file have? | `Entry.SHA256`, computed while streaming | every `Write`, on every backend (step 1) |
| Did the bytes arrive as the sender sent them? | `WriteOptions.SHA256` | the Disk refuses a mismatch before the driver commits (step 2) |
| Did the whole body arrive? | `WriteOptions.SizeHint` | the Disk refuses a short or long body (step 3) |
| Did a direct upload store what the client declared? | `upload.ConfirmExpected` | after a presigned upload ([Uploads](uploads.md#declared-size-and-checksum)) |
| Has the object changed since I last looked? | `Entry.ETag`, `ReadIf`, `WriteOptions.IfMatch` | step 4 |
| Can the backend itself refuse a corrupted body? | the `ChecksumSHA256` capability and backend checksums | step 5 |
| Is this upload one I already store? | content-addressed keys, `records.Store.FileBySHA256` | step 6 |
| Is what I stored last year still intact? | `jobs.Scrub` | step 7 |

## Before you start

- `import "github.com/ubgo/filesystem"`, and `github.com/ubgo/filesystem/upload` for step 6. Scrubbing needs the records module and `github.com/ubgo/filesystem/jobs`.
- A digest is hex SHA-256, 64 characters, either case. The example computes one the way a client or an upstream store would:

<!-- ds:block id=digest-fqnk4769 -->
**digest** · [`examples/concepts-integrity/main.go:63-66`](../../examples/concepts-integrity/main.go#L63-L66)

```go
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
```
<!-- /ds:block hash=5cddbf -->

## Step 1 — Every write is hashed while it streams

The Disk counts and hashes every body as it passes to the driver, so every `Write` returns the stored size and SHA-256 without reading the object a second time. It does this on every backend, including those that have no checksum of their own (local disk, SFTP, FTP, WebDAV). Keep `Entry.SHA256` with your record of the file: it is what deduplication, scrubbing and an end-to-end check compare against. The records module stores it on every file row.

<!-- ds:block id=streameddigest-898w4g6t -->
**streamedDigest** · [`examples/concepts-integrity/main.go:72-79`](../../examples/concepts-integrity/main.go#L72-L79)

```go
func streamedDigest(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Write(ctx, reportKey, strings.NewReader(report), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "write: %d bytes, sha256 matches an independent hash: %t\n", e.Size, e.SHA256 == digest(report))
	return nil
}
```
<!-- /ds:block hash=281cf1 -->

Check: `write: 29 bytes, sha256 matches an independent hash: true`.

A multipart upload is assembled by the backend from parts the Disk saw one at a time, so its `Entry.SHA256` is empty unless the driver reports one; hash the parts yourself, or `Stat` and re-read the object, when you need it.

## Step 2 — Hold the body to a declared digest

When the digest is known before the bytes arrive (a client computed it, an import manifest lists it, a copy from another store reported it), pass it as `WriteOptions.SHA256`. The Disk hashes the body anyway and refuses a mismatch with `CodeInvalidArgument` before the driver commits, on every driver, so a corrupted body never replaces the version already stored:

<!-- ds:block id=declareddigest-74fh5emm -->
**declaredDigest** · [`examples/concepts-integrity/main.go:85-100`](../../examples/concepts-integrity/main.go#L85-L100)

```go
func declaredDigest(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	next := "region,revenue\neu,125\nus,340\n"
	corrupted := strings.Replace(next, "125", "12\x00", 1)
	_, err := disk.Write(ctx, reportKey, strings.NewReader(corrupted), filesystem.WriteOptions{SHA256: digest(next)})
	fmt.Fprintf(out, "corrupted body with a declared sha256: %s\n", filesystem.CodeOf(err))
	b, err := disk.ReadAll(ctx, reportKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored version is still the first: %t\n", string(b) == report)
	if _, err := disk.Write(ctx, reportKey, strings.NewReader(next), filesystem.WriteOptions{SHA256: digest(next)}); err != nil {
		return err
	}
	fmt.Fprintln(out, "good body with a declared sha256: stored")
	return nil
}
```
<!-- /ds:block hash=3be559 -->

Check: `corrupted body with a declared sha256: invalid_argument`, `stored version is still the first: true`, then `good body with a declared sha256: stored`. The atomic replace every driver guarantees is what makes this safe: the object at the key is the old one until the new one is complete and checked. `WriteOptions.SHA256` is refused with `CodeUnsupported` on a multipart upload, whose parts are checked one by one.

### Other digests beside SHA-256

SHA-256 is the digest the Disk always computes; `WriteOptions.Checksums` asks it for more in the same pass, and can hold the body to each. Bring any `hash.Hash`: MD5 for an existing checksum column, CRC32C to compare with what GCS reports, BLAKE3 or xxHash for content addressing. The library names no algorithm:

```go
crc32c := func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }
e, err := disk.Write(ctx, "exports/q3.csv", body, filesystem.WriteOptions{
	Checksums: []filesystem.Checksum{
		{Name: "crc32c", New: crc32c, Want: declaredCRC}, // verified like SHA256
		{Name: "md5", New: md5.New},                      // computed only
	},
})
// e.Checksums["crc32c"], e.Checksums["md5"]: lowercase hex; e.SHA256 as always
```

A `Want` that does not match fails the write with `invalid_argument` naming the digest, before the driver commits, so the stored version stays the old one. A name is 1 to 32 lowercase letters, digits or dashes and unique in the list; a missing `New` or a `Want` of the wrong length is `invalid_argument`; multipart uploads refuse `Checksums` with `unsupported`, because the Disk never sees the parts' whole body in order; an upload through package `upload` gets them anyway from `Policy.Checksums`, which `Confirm` computes from the assembled object ([Uploads](uploads.md#more-digests-than-sha-256)). `Entry.Checksums` is filled only by that `Write`: `Stat` and `List` do not report it, and records keeps them on the file row (`File.Checksums`, its `checksums` column) when they are passed as `NewFile.Checksums`, which `Store.Upload` does from an upload's `Result.Checksums`. Code that computes the same digests itself checks the list first with `filesystem.ValidateChecksums(list)`, so a bad list fails with the same `invalid_argument` on every path.

Try it: [`examples/concepts-integrity`](../../examples/concepts-integrity) runs this section in `digests.go` ([`otherDigests`](ds:block?id=otherdigests-x8p2w5rn)): `go run ./concepts-integrity` from `examples/`.

## Step 3 — Hold the body to a declared size

A dropped connection near the end of an upload leaves a body that is short but otherwise well formed, and often still detects as the right type. `WriteOptions.SizeHint` is the body's length when it is known in advance; a positive hint is also a declaration the Disk holds the body to. A body that ends early or runs past it fails the write with `CodeInvalidArgument`, and nothing is stored:

<!-- ds:block id=declaredsize-d6thjej9 -->
**declaredSize** · [`examples/concepts-integrity/main.go:106-112`](../../examples/concepts-integrity/main.go#L106-L112)

```go
func declaredSize(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	truncated := report[:len(report)-5]
	_, err := disk.Write(ctx, "reports/q4.csv", strings.NewReader(truncated), filesystem.WriteOptions{SizeHint: int64(len(report))})
	ok, _ := disk.Exists(ctx, "reports/q4.csv")
	fmt.Fprintf(out, "truncated body with a size hint: %s, stored: %t\n", filesystem.CodeOf(err), ok)
	return nil
}
```
<!-- /ds:block hash=4fa891 -->

Check: `truncated body with a size hint: invalid_argument, stored: false`. Pass the request's `Content-Length` as `SizeHint` when you stream a request body to a disk yourself; `upload.Receive` and the handlers do the equivalent for you with the policy's size cap.

## Step 4 — ETags for "has it changed?"

Every write returns the object's ETag and `Stat` reports it. It is the cheapest change check there is: one `Stat`, no bytes read. What an ETag is depends on the backend: S3 (for a single-part upload) and the memory driver use the MD5 of the content, so rewriting the same bytes keeps it; a multipart S3 object's ETag is not an MD5 of anything; the local driver's changes on every write. So use it only as "changed since I looked", never as a content digest:

<!-- ds:block id=etags-g9xa5g2g -->
**etags** · [`examples/concepts-integrity/main.go:121-141`](../../examples/concepts-integrity/main.go#L121-L141)

```go
func etags(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	first, err := disk.WriteBytes(ctx, "a.txt", []byte("same"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	again, err := disk.WriteBytes(ctx, "a.txt", []byte("same"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	changed, err := disk.WriteBytes(ctx, "a.txt", []byte("different"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	now, err := disk.Stat(ctx, "a.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "etag: same bytes keep it (memory): %t, new bytes change it: %t, stat reports the latest: %t\n",
		first.ETag == again.ETag, changed.ETag != again.ETag, now.ETag == changed.ETag)
	return nil
}
```
<!-- /ds:block hash=21b1b6 -->

Check: `etag: same bytes keep it (memory): true, new bytes change it: true, stat reports the latest: true`.

The two operations built on it are in [Disks and drivers](disks-and-drivers.md#step-9--append-conditional-reads-and-object-state): `ReadIf` with `IfNoneMatch` answers `not_modified` with no body while the object is unchanged, and a write with `IfMatch` succeeds only while the ETag is still the one you read, so two writers can never both win. `fshttp` sends the ETag on every download and answers `If-None-Match` with 304, so browsers and CDNs revalidate for free.

## Step 5 — Checksums the backend verifies

`WriteOptions.SHA256` is checked by the Disk on your side of the wire. Some backends can check a checksum on theirs too, which also covers corruption between your server and the storage:

| Backend | What it verifies | How |
|---|---|---|
| S3 | SHA-256 of the whole object, for a single-request write with a known size | the `ChecksumSHA256` capability: `WriteOptions.SHA256` is sent as `x-amz-checksum-sha256` and S3 refuses a body that does not match; `checksum=CRC32C` (or `CRC32`, `CRC64NVME`, `SHA1`) makes S3 store and verify that algorithm on every write instead ([S3: checksums](../adapters/s3.md#checksums-s3-verifies)) |
| GCS | CRC32C and MD5, which it stores with every object | `Driver.ObjectInfo` reads them, to compare a stored object with a checksum you hold without downloading it ([GCS: checksums](../adapters/gcs.md#checksums)) |
| Azure | CRC64 of each staged block, with `Config.ValidateCRC64` (off by default) | Azure refuses a corrupted block before accepting it; `Driver.ObjectInfo` reads the stored Content-MD5 of a blob uploaded in one request ([Azure: checksums](../adapters/azure.md#checksums)) |
| Local, SFTP, FTP, WebDAV, memory | nothing of their own | the Disk's check in step 2 is the whole check |

`disk.Capabilities().ChecksumSHA256` tells whether the backend checks `WriteOptions.SHA256` itself. Without it the write is still checked, by the Disk; with it, it is checked twice.

## Step 6 — Store identical content once

A content-addressed key is the content's SHA-256: two uploads of one file land on one key, and the key proves what the object holds, as Git and container registries store blobs. `upload.ContentAddressed(dir, ContentAddressing{Owner})` returns the `Key` and `Settle` steps of a policy that does this. Sharing stays within one owner (`<dir>/<owner digest>/<sha256>.<ext>`) unless `Shared` is set:

<!-- ds:block id=contentaddressed-fnuean6r -->
**contentAddressed** · [`examples/concepts-integrity/main.go:147-169`](../../examples/concepts-integrity/main.go#L147-L169)

```go
func contentAddressed(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	owner := func(context.Context) (string, error) { return tenant, nil }
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Owner: owner})
	var keys []string
	for _, name := range []string{"logo.png", "logo-copy.png"} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), name, p)
		if err != nil {
			return err
		}
		keys = append(keys, res.Key)
	}
	n := 0
	for _, err := range disk.List(ctx, "blobs", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "content-addressed: same key twice: %t, key is the sha256: %t, objects: %d\n",
		keys[0] == keys[1], strings.TrimSuffix(path.Base(keys[0]), ".png") == digest(pngFile), n)
	return nil
}
```
<!-- /ds:block hash=18dfaa -->

Check: `content-addressed: same key twice: true, key is the sha256: true, objects: 1`.

Sharing an object has a cost: deleting one file must not delete the object while another file still points at it. With the records module, `Store.FileBySHA256(ctx, scope, sum)` finds an existing file with the same content to attach again instead of recording a copy, and reference counting decides when the object can go. Both layouts, with and without records, are compared in [Files: deduplicated uploads and shared objects](../records/files.md#deduplicated-uploads-and-shared-objects). Deduplicating across tenants (`Shared: true`) tells one tenant that another holds identical content, through a `conflict` when settings differ; leave it off unless that is acceptable.

## Step 7 — Scrub stored files against their records

Bytes at rest can change behind your back: a disk that rots, a bucket edited by hand, a restore that brought back the wrong version. `jobs.Scrub(store, ScrubOptions{Disk, Cursor, Interval, OnMissing, OnMismatch, OnError})` re-reads the objects of the files recorded on one disk, a batch per run, re-hashes each and compares it with the SHA-256 and size on its row. It only reports: restoring from a backup, re-uploading or purging is your decision. The cursor keeps its place, so a pass over a large disk spreads over many runs, and `Interval` bounds the read rate, and with it the egress bill. From `examples/maintenance`:

<!-- ds:block id=scrub-p9w3re6t -->
**scrub** · [`examples/maintenance/main.go:70-81`](../../examples/maintenance/main.go#L70-L81)

```go
func scrub(ctx context.Context, out io.Writer, d deployment, now time.Time) error {
	job := jobs.Scrub(d.store, jobs.ScrubOptions{
		Disk:       "hot",
		Cursor:     jobs.ObjectCursor(d.cold, "_jobs/scrub-hot"),
		Interval:   10 * time.Millisecond, // at most 100 objects a second
		OnMissing:  func(f records.File) { fmt.Fprintf(out, "scrub: %s is missing\n", f.Filename) },
		OnMismatch: func(f records.File, _ int64, _ string) { fmt.Fprintf(out, "scrub: %s changed\n", f.Filename) },
	})
	res, err := job.Run(ctx, now)
	fmt.Fprintf(out, "scrub: checked %d file(s)\n", res.Count)
	return err
}
```
<!-- /ds:block hash=b4a5f8 -->

That program damages one object behind the Store's back and the scrub prints `scrub: report.txt changed`. Schedule it like the other jobs ([Maintenance jobs](maintenance-jobs.md)); a full pass a month is typical, faster for data you cannot recreate.

## Pitfalls

- Treating an ETag as a content hash. It is one on some backends for some uploads and not on others; compare `Entry.SHA256`, or a backend checksum you know the algorithm of.
- Passing the client's digest without keeping it server-side. A declared digest is only worth checking if it came from somewhere you trust: store it with the pending upload, never re-read it from the request that confirms it.
- `SizeHint` from a guess. A positive hint is enforced; pass 0 when the length is not known exactly.
- A scrub with no `Interval` on a metered bucket. It reads every object you have; bound the rate, and run it where egress is free when you can.
- Shared content-addressed keys without reference counting. Deleting one file deletes the object every identical file uses.
- Expecting `Entry.SHA256` after a multipart upload. The Disk saw the parts, not the whole; hash it yourself if you need it.

## Next

- [Uploads](uploads.md#declared-size-and-checksum): declared sizes and digests for direct uploads.
- [Security](security.md): the other defaults every upload passes.
- [Files](../records/files.md#deduplicated-uploads-and-shared-objects): deduplication with records.
- [Maintenance jobs](maintenance-jobs.md): scheduling the scrub.
