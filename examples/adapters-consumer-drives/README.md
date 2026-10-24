# Consumer drives: Google Drive, Dropbox and OneDrive

The three consumer-drive drivers store files in the customer's own cloud storage rather than in a bucket the application owns: exports a user asked to have saved in their Dropbox, documents synced into a OneDrive or SharePoint library, backups written into a folder of the user's Google Drive. This program opens a `filesystem.Disk` on each of them and walks what an application does with a user's drive: write, read, stat and list a file, a create-only write and a compare-and-swap where the drive declares them, copy and move, a large body streamed through the drive's upload session, the features no consumer drive offers (signed URLs, visibility, metadata), and how a failing OAuth token source and a rate limit surface as error codes. It uses the core `filesystem` package and `github.com/ubgo/filesystem/adapters/gdrive`, `adapters/dropbox` and `adapters/onedrive`. The guides [Google Drive](../../docs/adapters/gdrive.md), [Dropbox](../../docs/adapters/dropbox.md) and [OneDrive and SharePoint](../../docs/adapters/onedrive.md) explain each step.

## Run it

```sh
cd examples
go run ./adapters-consumer-drives
go test ./adapters-consumer-drives
```

It runs against small in-process fakes of the Drive API v3, the Dropbox API v2 and Microsoft Graph (`internal/fakes`, trimmed from the fakes each driver's own tests use), each on a random local port, so it needs no account, no OAuth flow, no Docker and no network. The drivers accept plain `http` endpoints only on the loopback interface, which is what lets them talk to the fakes. Against the real services, drop the `APIURL`, `UploadURL`, `ContentURL` and `BaseURL` fields and pass a token source built from your OAuth client, as the guides show.

## What it does

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
dropbox capabilities: [copy,move,delete-prefix,conditional-write,compare-and-swap,case-insensitive]
dropbox: read "buy milk\n", stat size=9 etag set=true type=""
dropbox: stat NOTES/TODO.TXT: ok
dropbox: list notes: [notes/todo.txt]
dropbox: IfNotExists on an existing key: exists
dropbox: IfMatch current etag: ok, same etag again: conflict
dropbox: after copy and move: [archive/2026/todo.txt notes/todo.txt]
dropbox: signed URL: unsupported, public write: unsupported, metadata: unsupported
dropbox: large body 819200 bytes in 4 session requests, read back identical=true
dropbox: IfMatch on a large body: ok
dropbox: failing token source: permission, refused token: permission
dropbox: rate limited: unavailable, retry after 7s
onedrive capabilities: [move,delete-prefix,conditional-write,compare-and-swap,case-insensitive]
onedrive: read "buy milk\n", stat size=9 etag set=true type=""
onedrive: stat NOTES/TODO.TXT: ok
onedrive: list notes: [notes/todo.txt]
onedrive: IfNotExists on an existing key: exists
onedrive: IfMatch current etag: ok, same etag again: conflict
onedrive: after copy and move: [archive/2026/todo.txt notes/todo.txt]
onedrive: signed URL: unsupported, public write: unsupported, metadata: unsupported
onedrive: large body 819200 bytes in 3 session requests, read back identical=true
onedrive: IfMatch on a large body: unsupported
onedrive: failing token source: permission, refused token: permission
onedrive: rate limited: unavailable, retry after 7s
```

1. The capabilities differ. Google Drive copies on the server and keeps the content type you write, but has no conditional writes and is case-sensitive. Dropbox has server-side copy, create-only writes and compare-and-swap, and is case-insensitive. OneDrive has the conditional writes and is case-insensitive, but no server-side copy.
2. A file is written, read, stated and listed the same way on all three. Only Google Drive returns the content type written (`text/plain`); the other two store none.
3. `NOTES/TODO.TXT` finds `notes/todo.txt` on the two case-insensitive drives and is `not_found` on Google Drive.
4. `IfNotExists` on an existing key is `exists` on Dropbox and OneDrive. `IfMatch` with the current ETag succeeds and the same ETag a second time, now stale, is `conflict`. Google Drive refuses both with `unsupported` before sending anything.
5. A copy into `archive/` and a rename of the copy work everywhere. On OneDrive the Disk streams the copy through the application, because Graph copies asynchronously and the driver does not declare `Copy`.
6. Signed URLs, a public write and metadata are `unsupported` on all three.
7. An 800 KiB body of unknown length, with small chunk sizes set on purpose (256 KiB for Google Drive and Dropbox, 320 KiB for OneDrive), goes up in several session requests and reads back identical. A compare-and-swap of a body that long works on Dropbox, where the rev is checked when the session finishes, and is `unsupported` on OneDrive, because Graph checks `If-Match` only when a session starts.
8. A token source that returns an error and a token the API refuses are both `permission`. A `429` with `Retry-After: 7` is `unavailable` carrying a 7 second wait, which `layer.Retry` honours.

## The code

Opening each drive. The token source is any `func(context.Context) (string, error)`; in production it returns the current access token of your `oauth2.TokenSource`. `Client` is nil except in the rate-limit step.

<!-- ds:block id=opengdrive-g6g8ay6b -->
**openGDrive** · [`examples/adapters-consumer-drives/main.go:109-127`](main.go#L109-L127)

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

<!-- ds:block id=opendropbox-4bkc6hzd -->
**openDropbox** · [`examples/adapters-consumer-drives/main.go:131-150`](main.go#L131-L150)

```go
func openDropbox(f *fakes.Dropbox, tok tokenFunc, client *http.Client) (*filesystem.Disk, error) {
	drv, err := dropbox.New(dropbox.Config{
		// Keys live under this folder; empty means the account root (or the
		// app folder, for an app-folder app).
		Root:  "/Apps/Invoices",
		Token: dropbox.TokenFunc(tok),
		// Bodies longer than one chunk go through an upload session; leave
		// ChunkSize at 0 (8 MiB) in production.
		ChunkSize: gdrive.ChunkUnit,
		Client:    client,
		// Point at the fake; omit both for api.dropboxapi.com and
		// content.dropboxapi.com.
		APIURL:     f.APIURL(),
		ContentURL: f.APIURL(),
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("dropbox", drv)
}
```
<!-- /ds:block hash=f7c425 -->

<!-- ds:block id=openonedrive-4xmy77th -->
**openOneDrive** · [`examples/adapters-consumer-drives/main.go:154-171`](main.go#L154-L171)

```go
func openOneDrive(f *fakes.OneDrive, tok tokenFunc, client *http.Client) (*filesystem.Disk, error) {
	drv, err := onedrive.New(onedrive.Config{
		// Any Graph drive: /me/drive, /drives/{id}, /sites/{id}/drive.
		Drive: "/me/drive",
		Root:  "Apps/Invoices",
		Token: onedrive.TokenFunc(tok),
		// Fragments must be multiples of FragmentUnit; leave ChunkSize at
		// 0 (10 MiB) in production.
		ChunkSize: onedrive.FragmentUnit,
		Client:    client,
		// Point at the fake; omit for graph.microsoft.com.
		BaseURL: f.BaseURL(),
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("onedrive", drv)
}
```
<!-- /ds:block hash=1c49af -->

Write, read, stat and list, then a stat in another letter case.

<!-- ds:block id=basics-bcgfabev -->
**basics** · [`examples/adapters-consumer-drives/main.go:196-218`](main.go#L196-L218)

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

Create-only and compare-and-swap writes.

<!-- ds:block id=conditional-n68fgurx -->
**conditional** · [`examples/adapters-consumer-drives/main.go:223-234`](main.go#L223-L234)

```go
func conditional(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "notes/todo.txt", []byte("overwrite?"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "%s: IfNotExists on an existing key: %s\n", name, outcome(err))
	e, err := disk.Stat(ctx, "notes/todo.txt")
	if err != nil {
		return err
	}
	_, current := disk.WriteBytes(ctx, "notes/todo.txt", []byte("buy milk and eggs\n"), filesystem.WriteOptions{IfMatch: e.ETag})
	_, stale := disk.WriteBytes(ctx, "notes/todo.txt", []byte("lost update\n"), filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "%s: IfMatch current etag: %s, same etag again: %s\n", name, outcome(current), outcome(stale))
	return nil
}
```
<!-- /ds:block hash=857740 -->

Copy and move.

<!-- ds:block id=copyandmove-ydqujfaq -->
**copyAndMove** · [`examples/adapters-consumer-drives/main.go:240-253`](main.go#L240-L253)

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

What no consumer drive offers.

<!-- ds:block id=refusals-jn55w6u7 -->
**refusals** · [`examples/adapters-consumer-drives/main.go:257-263`](main.go#L257-L263)

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

A large body through the upload session, and compare-and-swap of a body that long.

<!-- ds:block id=largebody-ktqgtqdb -->
**largeBody** · [`examples/adapters-consumer-drives/main.go:269-286`](main.go#L269-L286)

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

Token failures and a rate limit. The throttled transport stands in for an API that answers `429`.

<!-- ds:block id=failures-bq6t3tw6 -->
**failures** · [`examples/adapters-consumer-drives/main.go:291-312`](main.go#L291-L312)

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

<!-- ds:block id=throttled-roundtrip-jfxbpmmb -->
**throttled.RoundTrip** · [`examples/adapters-consumer-drives/main.go:321-332`](main.go#L321-L332)

```go
func (throttled) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Header:     http.Header{"Retry-After": {fmt.Sprint(rateLimitWait)}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    r,
	}, nil
}
```
<!-- /ds:block hash=92ea62 -->

## What the example cannot show

The fakes model the requests each driver makes, not the services. These need a real account and are described in the guides from the drivers' source and tests:

- **The OAuth flow itself**: consent screens, scopes (`drive.file`, Dropbox app-folder access, `Files.ReadWrite.AppFolder`) and refresh tokens are the application's job; the drivers only call the token source before each request.
- **Real quotas and rate limits**: the example forces a `429`; how often each service throttles depends on the account and the app.
- **Names the services refuse**, Unicode normalisation in names, and Drive's acceptance of `*` as the total of an upload of unknown length.
- **Google Docs, Sheets and Slides**, which have no bytes; the driver skips them in listings and reports them as `not_found` (pinned in `adapters/gdrive/gdrive_test.go`).
- **Repeated names on Google Drive**, where two files of one name make the key `conflict` (pinned in the same tests).

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: one opener per drive and the steps every drive runs |
| `internal/fakes/gdrive.go` | The Drive API v3 fake: name queries, folders, copy, update, delete, resumable uploads |
| `internal/fakes/dropbox.go` | The Dropbox API v2 fake: write modes, upload sessions, copy, move, list_folder |
| `internal/fakes/onedrive.go` | The Graph fake: two hosts (Graph and a pre-authenticated download and upload host), conflictBehavior, If-Match, upload sessions |
| `internal/fakes/fakes.go` | The token the fakes accept and the JSON helper |
| `main_test.go` | Runs the program against fresh fakes and checks every line above |

## Next

- The guides: [Google Drive](../../docs/adapters/gdrive.md), [Dropbox](../../docs/adapters/dropbox.md), [OneDrive and SharePoint](../../docs/adapters/onedrive.md), and the driver references [adapters/gdrive](../../adapters/gdrive/README.md), [adapters/dropbox](../../adapters/dropbox/README.md), [adapters/onedrive](../../adapters/onedrive/README.md).
- Importing a single file the user picks in one of these drives: [pickers-import](../pickers-import).
- Other backends: [S3](../adapters-s3), [GCS](../adapters-gcs), [Azure](../adapters-azure), [WebDAV](../adapters-webdav), [SFTP](../adapters-sftp).
- [Capabilities](../concepts-capabilities) and [errors](../concepts-errors) across backends.
