# Google Drive

The Google Drive driver stores files in a user's My Drive or in a shared drive, for applications that save into the customer's own Google storage rather than a bucket the application owns. It speaks the Drive API v3 as plain JSON over `net/http`, with no dependency beyond the library.

## A user's drive or a bucket?

Choose a consumer drive (Google Drive, [Dropbox](dropbox.md) or [OneDrive](onedrive.md)) when the files belong to the user and they expect to find them in their own drive: exports and reports they asked to have saved, backups of their data, documents a sync feature keeps up to date. The user pays for the storage, sees and manages the files in Drive's own interface, and can revoke the application at any time. Choose a bucket ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md)) for files the application owns and serves: avatars, attachments, uploads that browsers download through signed or public URLs. A consumer drive has no signed URLs and no public URLs, needs one OAuth token per user, and is rate-limited per user and per app, so it is a destination for the user's copy, not the store behind a busy page.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/gdrive`, a module of its own. Its reference is [adapters/gdrive/README.md](../../adapters/gdrive/README.md).
- Enable the Drive API in a Google Cloud project and run the OAuth flow in your application. Prefer the `drive.file` scope, which lets the application see only files it created or the user opened with it: create one folder for the application, keep its ID, and pass it as `FolderID`. The full `drive` scope needs Google's verification.
- The runnable program is [`examples/adapters-consumer-drives`](../../examples/adapters-consumer-drives/). It runs the same steps on Google Drive, Dropbox and OneDrive against in-process fakes of their APIs, so it needs no account.
- Tested against an in-process fake of the documented API. What only a real account shows: Drive's acceptance of `*` as the total size of an upload chunk when the length is unknown (pass `WriteOptions.SizeHint` when you know it), and shared-drive permission errors.

## Step 1: Adapt your OAuth token source

The driver never runs the OAuth flow and never stores a token. It calls a `TokenSource` before each request and sends the access token it returns. `TokenSource` is a one-method interface of the driver's own, so the module does not depend on `golang.org/x/oauth2`; `TokenFunc` adapts any function, such as an `oauth2.TokenSource` built from the user's saved refresh token:

```go
src := oauthConfig.TokenSource(ctx, savedToken) // golang.org/x/oauth2; refreshes and caches
token := gdrive.TokenFunc(func(context.Context) (string, error) {
	t, err := src.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
})
```

The source is called once per request and must be safe for concurrent use. `oauth2.Config.TokenSource` returns one that caches the access token until it expires, which is what you want: a source that refreshes on every call would cost a token request per Drive request. A failing source (a revoked refresh token) and a token Drive refuses are both `CodePermission`.

## Step 2: Open a disk

`gdrive.New` checks the Config and makes no request, so building one disk per user, per request, costs nothing. `FolderID` is the application's folder (or a shared drive's ID, or `root` for the whole My Drive, the default). The example points the two endpoints at its fake and uses the smallest chunk so a large body spans several requests; in production leave `APIURL`, `UploadURL` and `ChunkSize` unset.

<!-- ds:block id=opengdrive-g6g8ay6b -->
**openGDrive** · [`examples/adapters-consumer-drives/main.go:109-127`](../../examples/adapters-consumer-drives/main.go#L109-L127)

```go
func openGDrive(f *fakes.GDrive, tok tokenFunc, client *http.Client) (*filesystem.Disk, error) {
	drv, err := gdrive.New(gdrive.Config{
		// The folder the application created for itself; with the
		// drive.file scope it sees nothing else.
		FolderID: fakes.GDriveFolderID,
		Token:    gdrive.TokenFunc(tok),
		// Small chunks so the large body below spans several requests;
		// leave ChunkSize at 0 (8 MiB) in production.
		ChunkSize: gdrive.ChunkUnit,
		Client:    client,
		// Point at the fake; omit both for the real API.
		APIURL:    f.APIURL(),
		UploadURL: f.UploadURL(),
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("gdrive", drv)
}
```
<!-- /ds:block hash=81893b -->

There is no DSN form, since a token source is code. `APIURL` and `UploadURL` must be https; plain http is accepted only on the loopback interface, for tests.

## Step 3: Know what the disk can do

```text
gdrive capabilities: [copy,move,delete-prefix,content-type]
```

`Copy` (`files.copy` on the server), `Move` (one update of the file's parent and name), `DeletePrefix` (Drive deletes a folder with its contents) and `ContentType` (Drive keeps the MIME type you write). There are no conditional writes, since Drive v3 dropped `If-Match`, and no signed URLs, since Drive has none; `IfNotExists` and `IfMatch` are refused with `CodeUnsupported` before any request. No visibility (sharing is Drive's permission model, reachable through `Native()`, Step 8), metadata or headers. Names are case-sensitive.

The example checks each refusal through the Disk:

<!-- ds:block id=refusals-jn55w6u7 -->
**refusals** · [`examples/adapters-consumer-drives/main.go:257-263`](../../examples/adapters-consumer-drives/main.go#L257-L263)

```go
func refusals(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	_, signed := disk.SignedURL(ctx, "notes/todo.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	_, public := disk.WriteBytes(ctx, "share.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	_, meta := disk.WriteBytes(ctx, "tagged.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"owner": "42"}})
	fmt.Fprintf(out, "%s: signed URL: %s, public write: %s, metadata: %s\n", name, outcome(signed), outcome(public), outcome(meta))
	return nil
}
```
<!-- /ds:block hash=131285 -->

```text
gdrive: signed URL: unsupported, public write: unsupported, metadata: unsupported
gdrive: IfNotExists on an existing key: unsupported
gdrive: IfMatch current etag: unsupported, same etag again: unsupported
```

Because there is no compare-and-swap, two writers of one key race and the last one wins; if your application edits a shared document from several places, serialise those writes yourself (for example with check-out [locks](../plugins/locks.md) when the files are records).

## Step 4: Write, read, stat and list

Keys work as on any disk. A write creates the missing folders on the way, `Stat` returns the size, the MD5 checksum as the ETag and the content type you wrote, and `List` walks the folders below the one listed.

<!-- ds:block id=basics-bcgfabev -->
**basics** · [`examples/adapters-consumer-drives/main.go:196-218`](../../examples/adapters-consumer-drives/main.go#L196-L218)

```go
func basics(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "notes/todo.txt", []byte("buy milk\n"), filesystem.WriteOptions{ContentType: "text/plain"})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "notes/todo.txt")
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "notes/todo.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: read %q, stat size=%d etag set=%t type=%q\n", name, got, e.Size, e.ETag != "", e.ContentType)
	_, err = disk.Stat(ctx, "NOTES/TODO.TXT")
	fmt.Fprintf(out, "%s: stat NOTES/TODO.TXT: %s\n", name, outcome(err))
	listed, err := keys(ctx, disk, "notes")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: list notes: %v\n", name, listed)
	return nil
}
```
<!-- /ds:block hash=e7f02f -->

```text
gdrive: read "buy milk\n", stat size=9 etag set=true type="text/plain"
gdrive: stat NOTES/TODO.TXT: not_found
gdrive: list notes: [notes/todo.txt]
```

### Drive has IDs, not paths

Every item in Drive is an ID with a name and a parent, and one folder may hold several items with the same name. The driver maps a key to a path of names under `FolderID` and resolves it one segment at a time, so each operation costs a request per path segment; nothing is cached, so nothing goes stale when someone renames a folder in the Drive web interface. Where names repeat:

- A key that names two or more files is `CodeConflict` for `Stat`, `Read` and `Write`: the driver does not guess which one you meant. `Delete` removes all of them, which is how to clear the conflict.
- Folders of a repeated name resolve through the oldest. Listings walk every folder below the listed one, so files in a younger twin still appear.
- The driver serialises folder creation within a process, so it never makes twins itself; two processes creating the same new folder at the same moment can, and the rules above keep that safe.

Google Docs, Sheets, Slides and other native files have no bytes to read: listings skip them and they are `CodeNotFound`.

## Step 5: Copy, move and delete

<!-- ds:block id=copyandmove-ydqujfaq -->
**copyAndMove** · [`examples/adapters-consumer-drives/main.go:240-253`](../../examples/adapters-consumer-drives/main.go#L240-L253)

```go
func copyAndMove(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	if _, err := disk.Copy(ctx, "notes/todo.txt", "archive/todo.txt"); err != nil {
		return err
	}
	if _, err := disk.Move(ctx, "archive/todo.txt", "archive/2026/todo.txt"); err != nil {
		return err
	}
	listed, err := keys(ctx, disk, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: after copy and move: %v\n", name, listed)
	return nil
}
```
<!-- /ds:block hash=024998 -->

```text
gdrive: after copy and move: [archive/2026/todo.txt notes/todo.txt]
```

- `Copy` and `Move` delete the files the destination named before only after the new one is in place, so the destination is never missing (its name briefly repeats).
- Replacing a key with `Write` uploads a new revision of the same file ID, so Drive's version history and sharing settings stay with it.
- `Delete` and `DeletePrefix` delete permanently, not to the trash. A file named like the prefix is not inside it and is left alone.

Set `Config.Trash` to send every removal to Drive's trash instead: `Delete`, `DeletePrefix`, and the older files a `Write`, `Copy` or `Move` replaces. A shared drive needs it when the account is a Content manager, which may trash files but not delete them (every permanent delete fails there with `CodePermission`), and it gives users a 30-day window to restore what the application removed:

```go
drv, err := gdrive.New(gdrive.Config{
	Token:    gdrive.TokenFunc(tok),
	FolderID: sharedDriveID,
	Trash:    true, // files.update {trashed: true}, never files.delete
})
```

Trashed files still count against the owner's storage until Drive empties the trash, and a listing never shows them, so a trashed key reads as missing.

## Step 6: Large files go through a resumable upload

Every write is a resumable upload in `ChunkSize` pieces (8 MiB by default, a multiple of `gdrive.ChunkUnit`, 256 KiB), so a body of any size streams and the file appears only when the last piece arrives. A body that fails part-way cancels the session and stores nothing.

<!-- ds:block id=largebody-ktqgtqdb -->
**largeBody** · [`examples/adapters-consumer-drives/main.go:269-286`](../../examples/adapters-consumer-drives/main.go#L269-L286)

```go
func largeBody(ctx context.Context, out io.Writer, d drive, disk *filesystem.Disk) error {
	body := bytes.Repeat([]byte("0123456789abcdef"), largeSize/16)
	// io.MultiReader hides the length, as an HTTP request body does, so
	// the driver cannot know the size up front.
	before := d.chunks()
	e, err := disk.Write(ctx, "backups/large.bin", io.MultiReader(bytes.NewReader(body)), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "backups/large.bin")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: large body %d bytes in %d session requests, read back identical=%t\n", d.name, e.Size, d.chunks()-before, bytes.Equal(got, body))
	_, err = disk.Write(ctx, "backups/large.bin", io.MultiReader(bytes.NewReader(body)), filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "%s: IfMatch on a large body: %s\n", d.name, outcome(err))
	return nil
}
```
<!-- /ds:block hash=20d6d8 -->

```text
gdrive: large body 819200 bytes in 4 session requests, read back identical=true
gdrive: IfMatch on a large body: unsupported
```

With an unknown length (here a stream, as an HTTP request body is) each chunk states the total as `*` until the last one; pass `WriteOptions.SizeHint` when you know the size, so every chunk states it.

## Step 7: Handle token failures and rate limits

<!-- ds:block id=failures-bq6t3tw6 -->
**failures** · [`examples/adapters-consumer-drives/main.go:291-312`](../../examples/adapters-consumer-drives/main.go#L291-L312)

```go
func failures(ctx context.Context, out io.Writer, d drive) error {
	revoked := func(context.Context) (string, error) { return "", errors.New("refresh token revoked") }
	wrong := func(context.Context) (string, error) { return "expired-token", nil }
	results := make([]string, 0, 2)
	for _, tok := range []tokenFunc{revoked, wrong} {
		disk, err := d.open(tok, nil)
		if err != nil {
			return err
		}
		_, err = disk.Stat(ctx, "notes/todo.txt")
		results = append(results, outcome(err))
	}
	fmt.Fprintf(out, "%s: failing token source: %s, refused token: %s\n", d.name, results[0], results[1])
	disk, err := d.open(validToken, &http.Client{Transport: throttled{}})
	if err != nil {
		return err
	}
	_, err = disk.Stat(ctx, "notes/todo.txt")
	wait, _ := filesystem.RetryAfterOf(err)
	fmt.Fprintf(out, "%s: rate limited: %s, retry after %s\n", d.name, outcome(err), wait)
	return nil
}
```
<!-- /ds:block hash=c43037 -->

```text
gdrive: failing token source: permission, refused token: permission
gdrive: rate limited: unavailable, retry after 7s
```

The example's throttled transport answers `429` with `Retry-After: 7`. The full mapping: 404 is `CodeNotFound`; 403 with `storageQuotaExceeded` (the user's Drive is full) is `CodeQuotaExceeded`; 403 with `userRateLimitExceeded` or `rateLimitExceeded`, 429 and 5xx are `CodeUnavailable` with the `Retry-After` the API sent (`filesystem.RetryAfterOf`); other 401 and 403 are `CodePermission`; 413 is `CodeTooLarge`; 400 is `CodeInvalidArgument`. Treat `CodePermission` as "ask the user to connect their Drive again", and wrap the disk in `layer.Retry` for `CodeUnavailable` (see [Errors](../concepts/errors.md)).

The token is added by the driver's transport only to requests for the two API origins, and transport errors never carry a URL's query, where an upload session's ID is.

## Step 8: Reach Drive features the driver does not cover

`filesystem.As[*http.Client](disk)` returns the driver's client, whose transport adds the token for the API origins only. Use it for permissions (sharing a file with someone), revisions and comments, which the Disk does not model.

## Try it

```sh
cd examples
go run ./adapters-consumer-drives
```

The Google Drive part of the output:

```text
gdrive capabilities: [copy,move,delete-prefix,content-type]
gdrive: read "buy milk\n", stat size=9 etag set=true type="text/plain"
gdrive: stat NOTES/TODO.TXT: not_found
gdrive: list notes: [notes/todo.txt]
gdrive: IfNotExists on an existing key: unsupported
gdrive: IfMatch current etag: unsupported, same etag again: unsupported
gdrive: after copy and move: [archive/2026/todo.txt notes/todo.txt]
gdrive: signed URL: unsupported, public write: unsupported, metadata: unsupported
gdrive: large body 819200 bytes in 4 session requests, read back identical=true
gdrive: IfMatch on a large body: unsupported
gdrive: failing token source: permission, refused token: permission
gdrive: rate limited: unavailable, retry after 7s
```

`go test ./adapters-consumer-drives` checks every line. The driver's own tests (`adapters/gdrive`) run the conformance suite against a fuller fake, with the smallest chunk too.

## Pitfalls

- **Expecting conditional writes.** `IfNotExists` and `IfMatch` are `CodeUnsupported`. Code written for S3 or Dropbox that relies on create-only writes for idempotency must check `disk.Capabilities()` or do its own locking.
- **Repeated names.** A user or another app can create a second file of the same name in your folder; the key then reads as `CodeConflict` until one is deleted. `Delete` removes every file of the name.
- **Case-sensitive keys.** `notes/todo.txt` and `NOTES/TODO.TXT` are two keys here, while Dropbox and OneDrive treat them as one. Normalise keys yourself if the same code runs on all three.
- **Deep keys are slow.** Each operation lists one folder per path segment before acting, and each of those requests calls the token source. Keep keys shallow and give the driver a caching token source.
- **Files the user adds by hand.** Native Google files are skipped and `CodeNotFound`; a name holding `/`, or `.` or `..`, cannot be a key and is skipped by listings.
- **Rate limits are per user.** Drive throttles with 403 `userRateLimitExceeded` as well as 429; both arrive as `CodeUnavailable` with a wait. A streamed body cannot be replayed, so `layer.Retry` retries a write only when the body can seek (a file or `WriteBytes`); otherwise retry at the application level.
- **Memory per write.** A write reads one chunk ahead, so it holds up to two chunks (16 MiB at the default size). Many concurrent uploads multiply that.
- **Deletes are permanent by default.** They skip Drive's trash, so the user cannot restore them from there; set `Trash` to keep them restorable.

## Related

- [Dropbox](dropbox.md) and [OneDrive and SharePoint](onedrive.md), the other consumer drives, with the same steps.
- [Cloud picker imports](../plugins/pickers.md): import one file the user picks in Drive, rather than writing into their Drive.
- [Capabilities](../concepts/capabilities.md) and [Errors](../concepts/errors.md).
- [Comparison of every backend](README.md).
