# Backups and mirrors: move a live store to S3 without downtime

A program that moves a live store from a local directory to S3 while the app keeps writing, then keeps a dated backup copy. It answers two needs every growing product meets: changing storage backend without a maintenance window, and having a backup that is more than a second copy of the same bucket. It uses `filesystem.Mirror` (write the primary, then the mirror, under the name the app already uses), `filesystem.CopyAcross` for the backfill and backups, `adapters/s3`, and a size and SHA-256 verification written in the example. The guide is [Backups and mirrors](../../docs/recipes/backups-and-mirrors.md).

## Run it

```sh
cd examples
go run ./recipe-backups-and-mirrors
```

It is not a server: it runs the five phases and exits. The old store is a local directory in a new temporary directory, deleted when the program ends; the S3 side is an in-process fake (gofakes3) and the backup disk is in memory, so nothing outlives the run. To move to a real bucket, name it in `MIRROR_DSN` (credentials come from the AWS default chain):

```sh
MIRROR_DSN='s3://my-bucket?region=eu-west-1' go run ./recipe-backups-and-mirrors
```

Run the tests with `go test ./recipe-backups-and-mirrors`.

## What it does

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

1. Mirror: the app's disk becomes a mirror of the local store (primary) and S3, so a write made during the move reaches both.
2. Backfill: the five objects stored before the mirror existed are copied; the one written through the mirror is skipped. A second backfill copies nothing, so a backfill that dies part-way resumes by running it again.
3. Verify: every object is compared by size and SHA-256 both ways; nothing differs, is missing or is extra.
4. Switch: S3 becomes the primary and the local store the mirror, still written, so going back is one more swap. A signed PUT on a mirror is refused (`unsupported`: a direct upload would reach only one side); a signed GET is signed by the primary.
5. Backup: a full run copies all seven objects; after one invoice changes, the incremental run copies only that one.

The failure paths are in the tests: `TestMirrorFailureIsReturnedAndRetryConverges` makes the mirror refuse writes and checks that the write returns `CodeUnavailable` after the primary stored it and converges on retry; `TestVerifyFindsEveryDifference` plants a changed, a missing and an extra object.

## The code

`openTarget` opens the S3 side: the bucket `MIRROR_DSN` names through a DSN mux, or `fakeS3`, an in-process gofakes3 server with one bucket.

<!-- ds:block id=opentarget-cs6qemev -->
**openTarget** · [`examples/recipe-backups-and-mirrors/main.go:93-112`](main.go#L93-L112)

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
**fakeS3** · [`examples/recipe-backups-and-mirrors/main.go:71-89`](main.go#L71-L89)

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

`startMirror` is phase 1: the app's disk, under the name it already uses, writes the old store first and S3 second. Reads, listings and URLs still come from the old store.

<!-- ds:block id=startmirror-f4by8yz3 -->
**startMirror** · [`examples/recipe-backups-and-mirrors/main.go:268-279`](main.go#L268-L279)

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

`backfill` copies every object the target lacks or holds with another size; rerunning it skips what is there. Size is a cheap first check; `verify` compares content with `digest`, because `Stat` on the built-in drivers reports no SHA-256.

<!-- ds:block id=backfill-4upds8sy -->
**backfill** · [`examples/recipe-backups-and-mirrors/main.go:130-149`](main.go#L130-L149)

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

<!-- ds:block id=digest-wfjmcum7 -->
**digest** · [`examples/recipe-backups-and-mirrors/main.go:154-163`](main.go#L154-L163)

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
**verify** · [`examples/recipe-backups-and-mirrors/main.go:179-211`](main.go#L179-L211)

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

`copyExisting` is phases 2 and 3 together, and refuses to go on when the copy does not match.

<!-- ds:block id=copyexisting-h3m6e8hd -->
**copyExisting** · [`examples/recipe-backups-and-mirrors/main.go:283-303`](main.go#L283-L303)

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

`switchPrimary` is phase 4: the mirror is rebuilt with S3 first. Once S3 is trusted, the app opens it alone under the same name and retires the old store.

<!-- ds:block id=switchprimary-cb2vhdjw -->
**switchPrimary** · [`examples/recipe-backups-and-mirrors/main.go:309-329`](main.go#L309-L329)

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

`backupSince` copies every object modified after a cutoff; run it on a schedule with the start time of the previous successful run. Deletions are not propagated, so a backup keeps what the app deleted until the backup's own retention removes it. `backup` shows a full run and an incremental one.

<!-- ds:block id=backupsince-5maq5zbp -->
**backupSince** · [`examples/recipe-backups-and-mirrors/main.go:219-234`](main.go#L219-L234)

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
**backup** · [`examples/recipe-backups-and-mirrors/main.go:334-351`](main.go#L334-L351)

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

`run` is the whole move, phase by phase.

<!-- ds:block id=run-8cmqhc4s -->
**run** · [`examples/recipe-backups-and-mirrors/main.go:355-375`](main.go#L355-L375)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The fake or real S3 target, the mirror, backfill, verification, the primary switch, incremental backups, and `run`. |
| `main_test.go` | The whole run's output, verification catching a changed, a missing and an extra object, and a failing mirror whose write is returned and converges on retry. |

## Next

- The guide: [Backups and mirrors](../../docs/recipes/backups-and-mirrors.md), with edge cases and production notes (retries on the mirror, doubled read traffic, backups in another account with deletion protection).
- [Disks and drivers](../../docs/concepts/disks-and-drivers.md), [Consistency](../../docs/concepts/consistency.md), [S3](../../docs/adapters/s3.md) and [S3-compatible stores](../../docs/adapters/s3-compatible.md).
- Related: [recipe-move-to-s3](../recipe-move-to-s3) (a one-off copy, no live writes), [recipe-account-deletion](../recipe-account-deletion) (deletions reach mirrors, not backups), and the [adapters-s3](../adapters-s3) example.
