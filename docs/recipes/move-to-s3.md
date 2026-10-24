# Move a local store to S3

Most products start by writing uploads to a directory on the server, and at some point that stops working: a second instance needs the same files, the disk fills up, backups of a directory with a million files take hours. This recipe moves an existing local store to S3 or any S3-compatible bucket (R2, B2, Wasabi, Spaces, SeaweedFS, Garage) without losing a file, and without the application changing anything but a DSN.

The program is [examples/recipe-move-to-s3](../../examples/recipe-move-to-s3). Every code block below is copied from it, and every transcript is from running it.

## What you get

- A copy tool that walks the local disk, copies every object to the same key on the bucket with a bounded pool of workers, and reports what it copied, skipped and failed.
- Resumable runs: a key already on the bucket with the same size is skipped, so an interrupted run is finished by running it again, and a run just before the switch copies only what arrived since the last one.
- A dry run that lists what would be copied.
- A verification pass that compares every object by SHA-256 on both sides.
- The source opened read-only, so a mistake in the tool cannot change it.
- The switch: one DSN change, and no database change when the disk keeps its name.

## The design

| Piece | Why |
|---|---|
| `Disk.List` with `ListOptions{Recursive: true}` | Every object under the root, page by page; each page is one intercepted, retryable call, so a million keys never sit in memory. |
| `filesystem.CopyAcross` | Streams the bytes from one disk to the other and keeps the content type, and the metadata and visibility where the target stores them. |
| `Disk.Stat` on the target | The skip check: a missing key (`CodeNotFound`) or a different size means copy. |
| A worker pool over a channel | Copies are network-bound; a fixed number of workers keeps the request rate and memory bounded. |
| `filesystem.ReadOnly()` interceptor on the source | Every mutating operation on the source fails with `CodeReadOnly`. |
| The same disk name on both sides | `fs_files.disk` (and the plugin tables) store the disk name, not the backend, so a backend change behind the same name touches no row. |

## Step 1: Decide what a run is

<!-- ds:block id=options-rgyqvp8b -->
**Options** · [`examples/recipe-move-to-s3/main.go:68-73`](../../examples/recipe-move-to-s3/main.go#L68-L73)

```go
type Options struct {
	// Workers is the number of concurrent copies; values below 1 mean 1.
	Workers int
	// DryRun lists what would be copied and copies nothing.
	DryRun bool
}
```
<!-- /ds:block hash=a76408 -->

<!-- ds:block id=report-4jzszwze -->
**Report** · [`examples/recipe-move-to-s3/main.go:78-83`](../../examples/recipe-move-to-s3/main.go#L78-L83)

```go
type Report struct {
	Copied  int64
	Skipped int64
	Bytes   int64
	Failed  map[string]error
}
```
<!-- /ds:block hash=d83e91 -->

A failure on one object is recorded and the run continues: one unreadable file should not stop a migration of a million. A failure to list stops the run, because what was not listed is unknown.

## Step 2: Skip what is already there

<!-- ds:block id=needscopy-9f8fz5c9 -->
**needsCopy** · [`examples/recipe-move-to-s3/main.go:89-98`](../../examples/recipe-move-to-s3/main.go#L89-L98)

```go
func needsCopy(ctx context.Context, dst *filesystem.Disk, e filesystem.Entry) (bool, error) {
	got, err := dst.Stat(ctx, e.Key)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return true, nil
	case err != nil:
		return false, err
	}
	return got.Size != e.Size, nil
}
```
<!-- /ds:block hash=63b939 -->

Size is the one attribute every backend reports on `Stat`. ETags are not comparable across backends (S3's multipart ETag is not an MD5 of the object, and the local driver's is its own), and none of the built-in drivers stores a SHA-256 to report on `Stat`. So the copy pass trusts sizes and the verification pass reads the bytes.

## Step 3: Copy one object

<!-- ds:block id=copyone-75r2dq2z -->
**copyOne** · [`examples/recipe-move-to-s3/main.go:105-124`](../../examples/recipe-move-to-s3/main.go#L105-L124)

```go
func copyOne(ctx context.Context, src, dst *filesystem.Disk, e filesystem.Entry, o Options, r *Report) error {
	need, err := needsCopy(ctx, dst, e)
	if err != nil || !need {
		if err == nil {
			atomic.AddInt64(&r.Skipped, 1)
		}
		return err
	}
	if o.DryRun {
		fmt.Printf("would copy %s (%d bytes)\n", e.Key, e.Size)
		atomic.AddInt64(&r.Copied, 1)
		return nil
	}
	if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
		return err
	}
	atomic.AddInt64(&r.Copied, 1)
	atomic.AddInt64(&r.Bytes, e.Size)
	return nil
}
```
<!-- /ds:block hash=e56201 -->

What `CopyAcross` carries over, from local disk to S3:

- Content type: the local driver stores none, so the target `Disk.Write` detects it from the bytes and the key's extension, exactly as an upload would. A PDF arrives as `application/pdf`.
- Metadata and stored headers: the local driver has neither, so there is nothing to carry.
- Visibility: carried only when the target has the `SetVisibility` capability. On S3 that is `acl=true` (AWS buckets with ACLs enabled); on R2 and most self-hosted stores the bucket policy decides who can read, so public files belong in a public bucket ([public and private](../concepts/public-and-private.md)).

## Step 4: Copy everything with a worker pool

<!-- ds:block id=copy-fx35x5fx -->
**Copy** · [`examples/recipe-move-to-s3/main.go:130-157`](../../examples/recipe-move-to-s3/main.go#L130-L157)

```go
func Copy(ctx context.Context, src, dst *filesystem.Disk, o Options) (Report, error) {
	r := Report{Failed: map[string]error{}}
	var mu sync.Mutex
	jobs := make(chan filesystem.Entry)
	var wg sync.WaitGroup
	for range max(o.Workers, 1) {
		wg.Go(func() {
			for e := range jobs {
				if err := copyOne(ctx, src, dst, e, o, &r); err != nil {
					mu.Lock()
					r.Failed[e.Key] = err
					mu.Unlock()
				}
			}
		})
	}
	var listErr error
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			listErr = err
			break
		}
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	return r, listErr
}
```
<!-- /ds:block hash=7b58a6 -->

The listing runs in the calling goroutine and feeds the workers, so listing and copying overlap. Objects added to the source during a run may or may not be listed; the next run picks them up.

## Step 5: Verify

<!-- ds:block id=digest-kqeqrzyz -->
**digest** · [`examples/recipe-move-to-s3/main.go:161-172`](../../examples/recipe-move-to-s3/main.go#L161-L172)

```go
func digest(ctx context.Context, d *filesystem.Disk, key string) (string, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```
<!-- /ds:block hash=d6529b -->

<!-- ds:block id=verify-u9yjwc9b -->
**Verify** · [`examples/recipe-move-to-s3/main.go:178-194`](../../examples/recipe-move-to-s3/main.go#L178-L194)

```go
func Verify(ctx context.Context, src, dst *filesystem.Disk) ([]string, error) {
	var bad []string
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return bad, err
		}
		want, err := digest(ctx, src, e.Key)
		if err != nil {
			return bad, err
		}
		got, err := digest(ctx, dst, e.Key)
		if err != nil || got != want {
			bad = append(bad, e.Key)
		}
	}
	return bad, nil
}
```
<!-- /ds:block hash=802560 -->

Verification reads every byte twice (once from each side). Run it once, after the last copy and before the switch, not after every run.

## Step 6: Open real disks from DSNs

<!-- ds:block id=openfromenv-ucj3efgh -->
**openFromEnv** · [`examples/recipe-move-to-s3/main.go:265-284`](../../examples/recipe-move-to-s3/main.go#L265-L284)

```go
func openFromEnv(ctx context.Context) (src, dst *filesystem.Disk, ok bool, err error) {
	srcDSN, dstDSN := os.Getenv(envSource), os.Getenv(envTarget)
	if srcDSN == "" && dstDSN == "" {
		return nil, nil, false, nil
	}
	if srcDSN == "" || dstDSN == "" {
		return nil, nil, true, fmt.Errorf("set both %s and %s", envSource, envTarget)
	}
	mux := filesystem.NewMux()
	if err := errors.Join(mux.Register(local.Scheme, local.Opener), mux.Register(fss3.Scheme, fss3.Opener)); err != nil {
		return nil, nil, true, err
	}
	if src, err = mux.Open(ctx, diskName, srcDSN, filesystem.WithInterceptors(filesystem.ReadOnly())); err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", envSource, err)
	}
	if dst, err = mux.Open(ctx, diskName, dstDSN); err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", envTarget, err)
	}
	return src, dst, true, nil
}
```
<!-- /ds:block hash=a99ac9 -->

S3 credentials come from the AWS default chain (`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, shared config files, instance roles), never from the DSN, so the DSN can be printed and logged. The DSN parameters for each provider are in [S3-compatible stores](../adapters/s3-compatible.md).

<!-- ds:block id=run-eqa8vn2g -->
**run** · [`examples/recipe-move-to-s3/main.go:288-311`](../../examples/recipe-move-to-s3/main.go#L288-L311)

```go
func run(ctx context.Context, src, dst *filesystem.Disk, o Options) error {
	start := time.Now()
	r, err := Copy(ctx, src, dst, o)
	verb := "copied"
	if o.DryRun {
		verb = "to copy"
	}
	fmt.Printf("%s %d, skipped %d, failed %d, %d bytes in %s\n", verb, r.Copied, r.Skipped, len(r.Failed), r.Bytes, time.Since(start).Round(time.Millisecond))
	for key, ferr := range r.Failed {
		fmt.Printf("  failed %s: %v\n", key, ferr)
	}
	if err != nil || len(r.Failed) > 0 || o.DryRun {
		return errors.Join(err, failedErr(len(r.Failed)))
	}
	bad, err := Verify(ctx, src, dst)
	if err != nil {
		return err
	}
	fmt.Printf("verified: %d objects differ\n", len(bad))
	if len(bad) > 0 {
		return fmt.Errorf("objects differ: %v", bad)
	}
	return nil
}
```
<!-- /ds:block hash=944cd0 -->

## Try it

The demo seeds a temporary directory, copies it to an in-process fake S3, runs again, then adds a file and does a dry run:

```sh
cd examples
go run ./recipe-move-to-s3
```

```text
first run:
copied 6, skipped 0, failed 0, 115 bytes in 18ms
verified: 0 objects differ
on the bucket: invoices/2025/0001.pdf application/pdf 23 bytes
second run (resumes, nothing to do):
copied 0, skipped 6, failed 0, 0 bytes in 8ms
verified: 0 objects differ
after a new upload, dry run:
would copy avatars/u3.png (22 bytes)
to copy 1, skipped 6, failed 0, 0 bytes in 1ms
```

Check: the second run copies nothing, and the PDF has a content type on the bucket although the local disk stored none.

To try it against a bucket by DSN, start the fake S3 as a server (it accepts any credentials):

<!-- ds:block id=servefakes3-7btu4b8k -->
**serveFakeS3** · [`examples/recipe-move-to-s3/main.go:362-370`](../../examples/recipe-move-to-s3/main.go#L362-L370)

```go
func serveFakeS3(ctx context.Context, addr string) error {
	backend := s3mem.New()
	if err := backend.CreateBucket(demoBucket); err != nil {
		return err
	}
	fmt.Printf("fake S3 on http://%s, bucket %q (any credentials)\n", addr, demoBucket)
	srv := &http.Server{Addr: addr, Handler: gofakes3.New(backend).Server(), ReadHeaderTimeout: readHeaderTimeout}
	return serve.UntilDone(ctx, srv)
}
```
<!-- /ds:block hash=b54323 -->

```sh
go run ./recipe-move-to-s3 -fake-s3 localhost:8116
```

```text
fake S3 on http://localhost:8116, bucket "media" (any credentials)
```

In another terminal, copy one file by hand with [the `fs` command](../integrations/cli.md), then let the tool copy the rest:

```sh
export AWS_ACCESS_KEY_ID=demo AWS_SECRET_ACCESS_KEY=demo
T='s3://media?region=us-east-1&path_style=true&endpoint=http://localhost:8116'
fs cp "file://$PWD/uploads#avatars/u1.jpg" "$T#avatars/u1.jpg"
```

```text
key:          avatars/u1.jpg
size:         51566
modified:     2026-10-03T09:18:46Z
content type: image/jpeg
etag:         e8df9db82fe45d80705ac2d6ecc69318
visibility:   
sha256:       eadc308ea1dfd2b91321aba4488a61fd10a2d593bd3837f0ce0c095c4de9bf9d
```

```sh
MOVE_SOURCE="file://$PWD/uploads" MOVE_TARGET="$T" go run ./recipe-move-to-s3 -workers 4
fs ls -r "$T"
```

```text
copied 1, skipped 1, failed 0, 23 bytes in 12ms
verified: 0 objects differ
  51566  2026-10-03 09:18:46     avatars/u1.jpg
     23  2026-10-03 09:18:47  invoices/0001.pdf
```

Check: the file `fs cp` already copied is skipped. `fs cp` copies one object per call (there is no recursive copy); it is the tool for a handful of keys, this program for a whole store.

## Step 7: Switch

The cut-over, for an application that reads its disks from the environment ([configuration](../concepts/configuration.md)):

1. Run the copy until a run copies little (each run copies only what arrived since the previous one).
2. Stop writes to the old store: put the application in maintenance mode for uploads, or mount the disk read-only with `filesystem.ReadOnly()`.
3. Run the copy once more, then `Verify`.
4. Change the DSN and restart: `STORAGE_MEDIA_DSN='s3://media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com'` instead of `file:///var/app/uploads`. Keep the disk name `media`.
5. Keep the old directory until you are sure, then delete it.

If uploads cannot pause even for a minute, use a mirror instead: wrap the old disk and the bucket in `filesystem.Mirror` so every new write lands on both, backfill with this tool, then make the bucket the primary ([backups and mirrors](backups-and-mirrors.md)).

### When the disk name has to change

Records store the disk's name in `fs_files.disk`, and two plugins keep their own copies: `fs_variants.disk` (image variants) and `fs_file_versions.disk` (kept versions). Keep the name and none of these change. If you must rename, register both names in the Manager during the change, pause uploads, and call `store.RenameDisk`:

```go
n, err := store.RenameDisk(ctx, "media", "media-s3")
```

It rewrites `fs_files.disk` in every scope, trashed files included, and asks every plugin that stores disk names of its own (variants and versions implement `records.DiskRenamer`) to rewrite theirs, all in one transaction: a plugin error rolls the whole rename back. It changes rows only and copies no bytes, so run it after the copy and `Verify`, when the objects already exist on the new disk under the same keys. `n` is the number of `fs_files` rows changed. It emits no events, so no audit entry records it; `media-s3` must be a disk of the Store's Manager, while `media` need not be any more ([replace and visibility](../records/replace-and-visibility.md) covers it).

## Edge cases this handles

- An interrupted run: run it again; copied objects are skipped by size.
- A failing object (unreadable file, a network error on one PUT): reported with its key, the others are copied, the exit code is 1, and the next run retries only what is missing.
- A file that changed size since the last run: copied again.
- A same-size object that differs (truncated and padded, a corrupted copy): not caught by the skip check, caught by `Verify`.
- A mistake that would write to the source: refused with `CodeReadOnly`.
- Large files: `CopyAcross` streams them, and the S3 driver uploads bodies over the part size as multipart, so nothing is held in memory whole.

## For production

- Run the copy close to the bucket (an instance in the same region) and raise `-workers` until the provider starts throttling; S3 handles thousands of requests per second per prefix, R2 and B2 have their own limits.
- Delete-aware sync is not part of this tool: an object deleted from the source after it was copied stays on the bucket. Between the first run and the switch that is usually harmless (the records no longer point at it); `Store.SweepOrphans` on the bucket's disk deletes such orphans after checking each object against `fs_files`.
- Empty directories are not objects and are not copied; nothing on S3 needs them.
- Local file modes map to visibility only on a target with `SetVisibility`. Decide visibility per bucket instead.
- Keep `-dry-run` output from the last run before the switch: it is the list of what changed during the cut-over.

## Related

- [Backups and mirrors](backups-and-mirrors.md): `filesystem.Mirror` for a migration without a write pause, and copies to a second backend.
- [S3](../adapters/s3.md) and [S3-compatible stores](../adapters/s3-compatible.md): DSNs and per-provider quirks.
- [Configuration](../concepts/configuration.md): `Mux`, DSNs and `OpenManager` from the environment.
- [The `fs` command](../integrations/cli.md): `ls`, `stat`, `cp` and `mv` across backends.
- [Consistency](../concepts/consistency.md): why objects and rows can be changed in this order.
