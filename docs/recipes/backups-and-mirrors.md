# Backups and mirrors

Sooner or later a product outgrows where its files live: the local directory on the first server has to move to S3, one provider is swapped for a cheaper one, or a second copy in another region becomes a requirement. This recipe is for teams that need to change storage backend while the app keeps taking uploads, and for anyone who wants a scheduled backup copy of their files.

The program is [examples/recipe-backups-and-mirrors](../../examples/recipe-backups-and-mirrors). It is a command, not a server: it runs the whole move against a local directory and an in-process S3, and prints each phase. Every code block below is copied from it, and the transcript is from running it.

## What you get

- A zero-downtime move from one backend to another in five phases: mirror, backfill, verify, switch, retire.
- Every write the app makes during the move reaches both backends, under the disk name the app already uses.
- A backfill that resumes after a failure and skips what is already copied.
- A verification that compares content (SHA-256), and finds objects missing on either side.
- A rollback that is a configuration change until the old backend is retired.
- An incremental backup to a third disk, copying only what changed since the last run.

## The design

| Piece | Why |
|---|---|
| `filesystem.Mirror(name, primary, mirrors...)` | A disk that writes the primary, then reads the object back from the primary and writes it to each mirror. Reads, listings and URLs come from the primary. It takes the app's disk name, so handlers, records rows and signed URLs keep working. |
| `Disk.List` + `filesystem.CopyAcross` | Copies objects that existed before the mirror, streaming each one and keeping its content type (and metadata and visibility where the target stores them). |
| A SHA-256 per object, read from both sides | `Stat` on the built-in drivers does not report a SHA-256, and ETags differ between backends (S3's multipart ETag is not a digest), so verification reads the bytes. |
| `Entry.ModTime` with a cutoff | An incremental backup copies objects modified after the previous run started. |

What the mirror guarantees, and what it does not:

- It is not atomic across disks. The primary write succeeds first; if a mirror then fails, the call returns `CodeUnavailable` naming the mirror, and the object exists on the primary only. Writes are overwrites, so retrying the same write converges.
- Mirrors get the bytes by reading the object back from the primary, because the caller's body can be read only once. Each write therefore costs one extra read of the primary.
- Signed PUT URLs and presigned multipart parts are refused with `CodeUnsupported` when the primary signs its own URLs: a client uploading straight to the primary's bucket would bypass the mirrors. Move browser direct uploads to a server-side upload for the duration, or confirm them and copy afterwards. Server-side multipart is allowed and replicated when it completes. A primary that cannot sign (local disk, memory) uses the disk's `WithURLSigner` instead, and that path is not refused: the link names the mirror disk, so `fshttp.Signed` writes through the mirror as long as the Manager holds the mirror under that name.
- The mirror disk's capabilities are the primary's, minus presigned parts.
- Deletes (`Delete`, `DeleteKeys`, `DeletePrefix`), copies, moves and visibility changes are applied to the primary and then to every mirror.

## Step 1: The S3 side

The demo starts an in-process S3 server, so it runs offline. With `MIRROR_DSN` set, it opens a real bucket through the Mux instead:

```sh
MIRROR_DSN='s3://my-bucket?region=eu-west-1' go run ./recipe-backups-and-mirrors
```

Credentials come from the AWS default chain (environment, shared files, instance role), never from the DSN. For R2, B2 or a self-hosted store, add `endpoint=` and, where needed, `path_style=true` ([S3-compatible stores](../adapters/s3-compatible.md)).

<!-- ds:block id=opentarget-cs6qemev -->
**openTarget** · [`examples/recipe-backups-and-mirrors/main.go:93-112`](../../examples/recipe-backups-and-mirrors/main.go#L93-L112)

```go
func openTarget(ctx context.Context) (*filesystem.Disk, func(), error) {
	if dsn, ok := os.LookupEnv(envMirrorDSN); ok {
		mux := filesystem.NewMux()
		if err := mux.Register(s3.Scheme, s3.Opener); err != nil {
			return nil, nil, err
		}
		d, err := mux.Open(ctx, newDisk, dsn)
		return d, func() {}, err
	}
	drv, stop, err := fakeS3(ctx)
	if err != nil {
		return nil, nil, err
	}
	d, err := filesystem.NewDisk(newDisk, drv)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return d, stop, nil
}
```
<!-- /ds:block hash=fb51c3 -->

<!-- ds:block id=fakes3-6jrsmx9s -->
**fakeS3** · [`examples/recipe-backups-and-mirrors/main.go:71-89`](../../examples/recipe-backups-and-mirrors/main.go#L71-L89)

```go
func fakeS3(ctx context.Context) (*s3.Driver, func(), error) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Region:       fakeRegion,
		Credentials:  credentials.NewStaticCredentialsProvider("fake", "fake", ""),
	})
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(fakeBucket)}); err != nil {
		srv.Close()
		return nil, nil, err
	}
	drv, err := s3.New(s3.Config{Client: client, Bucket: fakeBucket})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return drv, srv.Close, nil
}
```
<!-- /ds:block hash=f99279 -->

## Step 2: Mirror the app's writes

<!-- ds:block id=startmirror-f4by8yz3 -->
**startMirror** · [`examples/recipe-backups-and-mirrors/main.go:268-279`](../../examples/recipe-backups-and-mirrors/main.go#L268-L279)

```go
func startMirror(ctx context.Context, w io.Writer, d Disks) (*filesystem.Disk, error) {
	app, err := filesystem.Mirror(appDisk, d.Old, d.New)
	if err != nil {
		return nil, err
	}
	if _, err := app.WriteBytes(ctx, "invoices/2026/new.txt", []byte("written during the move\n"), filesystem.WriteOptions{}); err != nil {
		return nil, err
	}
	onNew, err := d.New.Exists(ctx, "invoices/2026/new.txt")
	fmt.Fprintf(w, "mirror: a new write reached %s=true %s=%v\n", oldDisk, newDisk, onNew)
	return app, err
}
```
<!-- /ds:block hash=c94805 -->

In an app, this is where the Manager is built: register the mirror under the name the app uses (`media`) instead of the old disk. Nothing else changes. From this moment every new or changed object reaches S3; only what existed before still has to be copied.

## Step 3: Backfill

<!-- ds:block id=backfill-4upds8sy -->
**backfill** · [`examples/recipe-backups-and-mirrors/main.go:130-149`](../../examples/recipe-backups-and-mirrors/main.go#L130-L149)

```go
func backfill(ctx context.Context, src, dst *filesystem.Disk) (copied, skipped int, err error) {
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return copied, skipped, err
		}
		have, err := dst.Stat(ctx, e.Key)
		if err == nil && have.Size == e.Size {
			skipped++
			continue
		}
		if err != nil && filesystem.CodeOf(err) != filesystem.CodeNotFound {
			return copied, skipped, err
		}
		if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
			return copied, skipped, err
		}
		copied++
	}
	return copied, skipped, nil
}
```
<!-- /ds:block hash=298063 -->

The size check makes the backfill cheap to rerun: after a crash or a timeout, the next run skips everything already copied. It is also why the backfill alone is not enough. An object the app rewrites with the same size between the backfill's read and its write can leave the old content on S3, and an object the app deletes while the backfill is copying it can reappear on S3 after the mirror deleted it. The verification finds both.

## Step 4: Verify

<!-- ds:block id=digest-wfjmcum7 -->
**digest** · [`examples/recipe-backups-and-mirrors/main.go:154-163`](../../examples/recipe-backups-and-mirrors/main.go#L154-L163)

```go
func digest(ctx context.Context, d *filesystem.Disk, key string) (string, int64, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
```
<!-- /ds:block hash=efcef3 -->

<!-- ds:block id=verify-4ca52aq5 -->
**verify** · [`examples/recipe-backups-and-mirrors/main.go:179-211`](../../examples/recipe-backups-and-mirrors/main.go#L179-L211)

```go
func verify(ctx context.Context, src, dst *filesystem.Disk) (Report, error) {
	var r Report
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return r, err
		}
		r.Checked++
		want, wantSize, err := digest(ctx, src, e.Key)
		if err != nil {
			return r, err
		}
		got, gotSize, err := digest(ctx, dst, e.Key)
		switch {
		case filesystem.CodeOf(err) == filesystem.CodeNotFound:
			r.Missing = append(r.Missing, e.Key)
		case err != nil:
			return r, err
		case got != want || gotSize != wantSize:
			r.Different = append(r.Different, e.Key)
		}
	}
	for e, err := range dst.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return r, err
		}
		if ok, err := src.Exists(ctx, e.Key); err != nil {
			return r, err
		} else if !ok {
			r.Extra = append(r.Extra, e.Key)
		}
	}
	return r, nil
}
```
<!-- /ds:block hash=778b31 -->

`Different` and `Missing` are fixed by copying those keys again with `CopyAcross`; `Extra` keys are deleted from the target after checking that the app really deleted them. Run the verification until it is clean, then switch.

<!-- ds:block id=copyexisting-h3m6e8hd -->
**copyExisting** · [`examples/recipe-backups-and-mirrors/main.go:283-303`](../../examples/recipe-backups-and-mirrors/main.go#L283-L303)

```go
func copyExisting(ctx context.Context, w io.Writer, d Disks) error {
	copied, skipped, err := backfill(ctx, d.Old, d.New)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "backfill: copied %d, skipped %d already there\n", copied, skipped)
	copied, skipped, err = backfill(ctx, d.Old, d.New)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "backfill again: copied %d, skipped %d\n", copied, skipped)
	r, err := verify(ctx, d.Old, d.New)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "verify: %d checked, %d different, %d missing, %d extra, ok=%v\n", r.Checked, len(r.Different), len(r.Missing), len(r.Extra), r.OK())
	if !r.OK() {
		return filesystem.Errorf(filesystem.CodeUnavailable, "the copy does not match: %+v", r)
	}
	return nil
}
```
<!-- /ds:block hash=74476e -->

## Step 5: Switch the primary

<!-- ds:block id=switchprimary-cb2vhdjw -->
**switchPrimary** · [`examples/recipe-backups-and-mirrors/main.go:309-329`](../../examples/recipe-backups-and-mirrors/main.go#L309-L329)

```go
func switchPrimary(ctx context.Context, w io.Writer, d Disks) (*filesystem.Disk, error) {
	app, err := filesystem.Mirror(appDisk, d.New, d.Old)
	if err != nil {
		return nil, err
	}
	if _, err := app.WriteBytes(ctx, "invoices/2026/after-switch.txt", []byte("written after the switch\n"), filesystem.WriteOptions{}); err != nil {
		return nil, err
	}
	onOld, err := d.Old.Exists(ctx, "invoices/2026/after-switch.txt")
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "switch: primary %s, a new write also reached %s=%v\n", newDisk, oldDisk, onOld)
	_, err = app.SignedURL(ctx, "invoices/2026/upload.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: signedLifetime})
	fmt.Fprintf(w, "switch: signed PUT on the mirror: %s\n", filesystem.CodeOf(err))
	get, err := app.SignedURL(ctx, "invoices/2026/001.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: signedLifetime})
	if err == nil {
		fmt.Fprintf(w, "switch: signed GET is signed by %s: %v\n", newDisk, get.URL != "")
	}
	return app, err
}
```
<!-- /ds:block hash=50214b -->

Swapping the arguments makes S3 the primary: reads, listings and signed URLs now come from S3, and the old store still receives every write, so going back is the same swap. Records rows need no change, because they name the disk (`media`), not the backend. When you trust the new backend, open it alone under the app's name and retire the old store. The switch is also when signed PUT URLs stop working, because the S3 primary signs its own URLs and the mirror refuses to issue an upload link that would bypass the old store; once the mirror is removed they work again.

## Step 6: Backups

A mirror is not a backup: a delete or an overwrite reaches every mirror at once. A backup is a copy taken on a schedule and kept for a while.

<!-- ds:block id=backupsince-5maq5zbp -->
**backupSince** · [`examples/recipe-backups-and-mirrors/main.go:219-234`](../../examples/recipe-backups-and-mirrors/main.go#L219-L234)

```go
func backupSince(ctx context.Context, src, dst *filesystem.Disk, since time.Time) (int, error) {
	n := 0
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return n, err
		}
		if !e.ModTime.After(since) {
			continue
		}
		if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
```
<!-- /ds:block hash=c1d429 -->

<!-- ds:block id=backup-vv56hxqn -->
**backup** · [`examples/recipe-backups-and-mirrors/main.go:334-351`](../../examples/recipe-backups-and-mirrors/main.go#L334-L351)

```go
func backup(ctx context.Context, w io.Writer, app *filesystem.Disk, d Disks) error {
	first := time.Time{}
	n, err := backupSince(ctx, app, d.Backup, first)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "backup: full run copied %d\n", n)
	second := time.Now()
	time.Sleep(time.Second) // S3 reports modification times in whole seconds
	if _, err := app.WriteBytes(ctx, "invoices/2026/001.txt", []byte("invoice 1, corrected\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if n, err = backupSince(ctx, app, d.Backup, second); err != nil {
		return err
	}
	fmt.Fprintf(w, "backup: incremental run copied %d\n", n)
	return nil
}
```
<!-- /ds:block hash=479e8b -->

Pass the start time of the previous successful run, not its end, so an object written while that run was listing is copied by the next one. The backup copies changes but never deletes: what the app deleted stays in the backup until the backup's own retention removes it (an S3 lifecycle rule on the backup bucket, or bucket versioning for point-in-time copies).

The whole move, phase by phase:

<!-- ds:block id=run-8cmqhc4s -->
**run** · [`examples/recipe-backups-and-mirrors/main.go:355-375`](../../examples/recipe-backups-and-mirrors/main.go#L355-L375)

```go
func run(ctx context.Context, w io.Writer, dataDir string) error {
	d, stop, err := openDisks(ctx, dataDir)
	if err != nil {
		return err
	}
	defer stop()
	if err := seed(ctx, d.Old); err != nil {
		return err
	}
	if _, err := startMirror(ctx, w, d); err != nil {
		return err
	}
	if err := copyExisting(ctx, w, d); err != nil {
		return err
	}
	app, err := switchPrimary(ctx, w, d)
	if err != nil {
		return err
	}
	return backup(ctx, w, app, d)
}
```
<!-- /ds:block hash=65867a -->

## Try it

```sh
cd examples
go run ./recipe-backups-and-mirrors
```

```text
mirror: a new write reached media-local=true media-s3=true
backfill: copied 5, skipped 1 already there
backfill again: copied 0, skipped 6
verify: 6 checked, 0 different, 0 missing, 0 extra, ok=true
switch: primary media-s3, a new write also reached media-local=true
switch: signed PUT on the mirror: unsupported
switch: signed GET is signed by media-s3: true
backup: full run copied 7
backup: incremental run copied 1
```

Check: the backfill skipped one object, the one the app wrote through the mirror; the second backfill copied nothing; the incremental backup copied only the invoice changed after the full run.

The tests also cover the failure paths: `TestMirrorFailureIsReturnedAndRetryConverges` makes the mirror refuse writes and checks that the write returns `CodeUnavailable`, the object is on the primary, and the same write succeeds on both once the mirror is back; `TestVerifyFindsEveryDifference` plants a changed, a missing and an extra object.

## Edge cases this handles

- A mirror that is down: the app's write fails with `503` (`CodeUnavailable`) after the primary stored it. The app or its retry interceptor retries; the object converges.
- A backfill that dies part-way: rerun it; it skips what is there.
- An object overwritten with the same size during the backfill: caught by the SHA-256 comparison.
- An object deleted during the backfill: reported as `Extra`.
- Objects modified during a backup run: copied by the next run, because the cutoff is the previous run's start.

## For production

- Put `layer.Retry` on the mirror disk so a transient failure on the new backend is retried rather than surfaced to users ([interceptors](../concepts/interceptors.md)).
- Run the backfill and verification as a job that reports progress, with a bounded number of concurrent copies; for millions of objects, prefer the provider's own bulk copy for the backfill and use this verification afterwards.
- The bytes of every write are read back from the primary, so expect roughly double the primary read traffic while mirroring.
- For a copy that does not need to stay live, `fs cp` copies between any two DSNs from the command line ([move to S3](move-to-s3.md), [CLI](../integrations/cli.md)).
- Backups belong in another account or region, with deletion protection (object lock or versioning), so the credentials that can delete production cannot delete the backup.
- Record when each backup run started in durable storage (a database row), not in memory, so a restart does not copy everything again or, worse, skip a window.

## Related

- [Move to S3](move-to-s3.md): a one-off copy of a local store, with `fs cp`.
- [Disks and drivers](../concepts/disks-and-drivers.md): how wrapper disks keep capabilities honest.
- [Consistency](../concepts/consistency.md): what can be left behind when two systems cannot share a transaction.
- [S3](../adapters/s3.md) and [S3-compatible stores](../adapters/s3-compatible.md): DSNs for the target.
- [Account deletion](account-deletion.md): deletions reach mirrors, but not backups.
