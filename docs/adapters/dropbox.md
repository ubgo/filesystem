# Dropbox

The Dropbox driver stores files in a user's or a team member's Dropbox, for applications that save into the customer's own storage (exports, backups, synced documents) rather than into a bucket the application owns. It speaks the Dropbox HTTP API v2 as plain JSON over `net/http`, with no dependency beyond the library.

## A user's Dropbox or a bucket?

Choose a consumer drive (Dropbox, [Google Drive](gdrive.md) or [OneDrive](onedrive.md)) when the files belong to the user and they expect to find them in their own drive: exports they asked for, backups of their data, documents a sync feature keeps current. The user pays for the space, sees the files in the Dropbox apps on every device, and can disconnect the application at any time. Choose a bucket ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md)) for files the application owns and serves to browsers: a consumer drive has no signed or public URLs, needs one OAuth token per user, and is rate-limited per user. Of the three consumer drives, Dropbox is the one with atomic create-only writes and compare-and-swap for bodies of any size, so it suits documents that several devices edit.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/dropbox`, a module of its own. Its reference is [adapters/dropbox/README.md](../../adapters/dropbox/README.md).
- Your application runs the OAuth flow and stores the refresh token; the driver only asks for an access token before each request. Create the app in the Dropbox App Console with the `files.content.read` and `files.content.write` scopes (and `files.metadata.read`). An app with app-folder access sees only `/Apps/<app name>`, which is the safest choice for an application's own files.
- The runnable program is [`examples/adapters-consumer-drives`](../../examples/adapters-consumer-drives/). It runs the same steps on Dropbox, Google Drive and OneDrive against in-process fakes of their APIs, so it needs no account.
- Tested against an in-process fake of the documented API. What only a real account shows: which names Dropbox refuses, and how it normalises Unicode in names.

## Step 1: Adapt your OAuth token source

`TokenSource` is a one-method interface of the driver's own, so the module does not depend on `golang.org/x/oauth2`; `TokenFunc` adapts any function:

```go
src := oauthConfig.TokenSource(ctx, savedToken) // golang.org/x/oauth2; refreshes and caches
token := dropbox.TokenFunc(func(context.Context) (string, error) {
	t, err := src.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
})
```

The source is called once per request and must be safe for concurrent use. Dropbox access tokens are short-lived, so the source must refresh; `oauth2.Config.TokenSource` does, and caches the token until it expires. A source that fails (a revoked refresh token) and a token Dropbox refuses are both `CodePermission`.

## Step 2: Open a disk

`dropbox.New` checks the Config and makes no request, so one disk per user, built per request, costs nothing. `Root` is the folder holding the disk and must start with `/`; empty means the root of the account, or of the app folder for an app-folder app. The example points both hosts at its fake and sets a small chunk so a large body uses an upload session; in production leave `APIURL`, `ContentURL` and `ChunkSize` unset.

<!-- ds:block id=opendropbox-4bkc6hzd -->
**openDropbox** · [`examples/adapters-consumer-drives/main.go:131-150`](../../examples/adapters-consumer-drives/main.go#L131-L150)

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

There is no DSN form, since a token source is code. `APIURL` and `ContentURL` must be https (http only on loopback, for tests), and the driver follows no redirect, so the token goes only to the two API hosts.

## Step 3: Know what the disk can do

```text
dropbox capabilities: [copy,move,delete-prefix,conditional-write,compare-and-swap,case-insensitive]
```

`Copy`, `Move` and `DeletePrefix` on the server; `ConditionalWrite` (`IfNotExists` is write mode `add` without autorename) and `CompareAndSwap` (`IfMatch` is write mode `update` with the rev, with `strict_conflict`), both checked by Dropbox atomically at commit; and `CaseInsensitive`. There are no signed URLs: Dropbox's temporary links last four hours and cannot be given a shorter expiry, so they do not meet the library's rule that signed URLs expire when asked. No visibility, metadata, stored content type or headers.

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
dropbox: signed URL: unsupported, public write: unsupported, metadata: unsupported
```

To let a browser download a file, stream it through your application ([Serving files](../concepts/serving-files.md)).

## Step 4: Write, read, stat and list

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
dropbox: read "buy milk\n", stat size=9 etag set=true type=""
dropbox: stat NOTES/TODO.TXT: ok
dropbox: list notes: [notes/todo.txt]
```

- Keys are paths under `Root`. Dropbox paths are case-insensitive, so `a.txt` and `A.txt` are one file, and listings return the casing Dropbox reports, where a parent folder keeps the casing it was first created with.
- A segment ending in `.` or a space is stored percent-escaped and decoded back; so is a `%` itself.
- The ETag is the file's `rev`. Dropbox stores no content type, so `Stat` returns none.
- `List` is one recursive `list_folder` following its cursor, O(n) in the files under the directory.

## Step 5: Use conditional writes

Create-only writes and compare-and-swap are what make Dropbox safe for documents several devices edit: read the file with its ETag, change it, and write it back with `IfMatch`, so a write based on an old copy fails instead of overwriting newer work.

<!-- ds:block id=conditional-n68fgurx -->
**conditional** · [`examples/adapters-consumer-drives/main.go:223-234`](../../examples/adapters-consumer-drives/main.go#L223-L234)

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

```text
dropbox: IfNotExists on an existing key: exists
dropbox: IfMatch current etag: ok, same etag again: conflict
```

`IfNotExists` fails against an existing file even when the new bytes are identical (the driver sets `strict_conflict`, without which Dropbox would accept the identical upload). A failed `IfMatch` is `CodeConflict`, also when the file was deleted meanwhile.

## Step 6: Copy, move and delete

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
dropbox: after copy and move: [archive/2026/todo.txt notes/todo.txt]
```

Dropbox refuses to copy or move onto an existing file, so `Copy` and `Move` replace one in steps that never lose it: a copy goes to a transient file beside the destination first, then the old destination is moved aside, the new file moved in and the old one deleted; if moving in fails, the old destination is moved back (see Pitfalls). Transient files are named with `dropbox.SwapPrefix` (`.fs-swap-`). A destination that does not exist costs one call. A move to the same key in another letter case renames it. `DeletePrefix` is one `delete_v2` of the folder; a file named like the prefix is not inside it and is left alone.

## Step 7: Large files go through an upload session

A body shorter than `ChunkSize` (8 MiB, at most 150 MiB, the most Dropbox accepts per request) is one upload request. A longer one goes through an upload session: each chunk is appended, then one `finish` commits the file, so a reader never sees a partial file and a body that fails part-way stores nothing (Dropbox expires the abandoned session). A write holds one chunk in memory.

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
dropbox: large body 819200 bytes in 4 session requests, read back identical=true
dropbox: IfMatch on a large body: ok
```

The four requests are `start`, two `append_v2` and `finish`. The write mode goes with the `finish`, so `IfNotExists` and `IfMatch` hold for a body of any size.

## Step 8: Handle token failures and rate limits

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
dropbox: failing token source: permission, refused token: permission
dropbox: rate limited: unavailable, retry after 7s
```

The full mapping of Dropbox's 409 error summaries: `not_found` is `CodeNotFound`, `conflict` is `CodeExists` (or `CodeConflict` for a failed `IfMatch`), `insufficient_space` is `CodeQuotaExceeded`, `malformed_path` and `disallowed_name` are `CodeInvalidKey`, and `too_many_*` (`too_many_write_operations` when several writes contend for one namespace) is `CodeUnavailable`. 401 and 403 are `CodePermission`; 429 and 5xx are `CodeUnavailable` with the `Retry-After` the API sent (`filesystem.RetryAfterOf`). Treat `CodePermission` as "ask the user to connect Dropbox again", and wrap the disk in `layer.Retry` for `CodeUnavailable` (see [Errors](../concepts/errors.md)).

For endpoints the driver does not call (sharing links, file requests), `filesystem.As[*http.Client](disk)` returns the driver's client; it follows no redirect, and you add the token yourself. `(*dropbox.Driver).AuthorizedClient()` returns a copy that adds the token (and the path-root and select-user headers below) to requests for the API and content hosts only, so it never leaks the token to another host and keeps an `Authorization` you set yourself.

Dropbox Business accounts keep shared work in team spaces, which a member's own paths do not reach. Three `Config` fields cover them, each sent on every request:

```go
drv, err := dropbox.New(dropbox.Config{
	Token: dropbox.TokenFunc(tok),
	Root:  "/Acme Projects",
	// The team space's root, from users/get_current_account's root_info.root_namespace_id.
	RootNamespaceID: account.RootInfo.RootNamespaceID,
	// For a team-scoped token: act as this member.
	SelectUser: "dbmid:AAH4f99T0taONIb-OurWxbNQ6ywGRopQngc",
})
```

`RootNamespaceID` sends `Dropbox-API-Path-Root {".tag": "root", "root": ID}`, which Dropbox refuses unless the ID is the user's root namespace; `NamespaceID` (exclusive with it) sends `{".tag": "namespace_id", ...}` for any team or shared folder the user can reach. The fake checks the headers are sent; their effect needs a Business account.

## Try it

```sh
cd examples
go run ./adapters-consumer-drives
```

The Dropbox part of the output:

```text
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
```

`go test ./adapters-consumer-drives` checks every line. The driver's own tests (`adapters/dropbox`) run the conformance suite against a fuller fake, with a chunk far smaller than its bodies too.

## Pitfalls

- **Copy and Move onto an existing key are not atomic, but never lose the old file.** A failed step leaves the old destination in place. What the API cannot make atomic: a reader can find the destination missing for the moment between moving the old file aside and moving the new one in, and a listing taken during the operation can show the `.fs-swap-` files. If a rollback itself fails, the error names the `.fs-swap-` file that still holds the old content, and if deleting the replaced file fails after a successful copy, one `.fs-swap-` file is left; an application can sweep files with `dropbox.SwapPrefix` in their name.
- **Case-insensitive keys.** `Report.pdf` and `report.pdf` are one file; writing both overwrites the first. Listings show Dropbox's casing, which for a folder is the casing it was first created with, not necessarily the key you wrote.
- **Escaped names show in the user's Dropbox.** A key holding `%`, or a segment ending in `.` or a space, appears percent-escaped in the Dropbox apps (`50%.txt` is stored as `50%25.txt`). A file the user adds by hand with a `%` in its name that is not an escape the driver writes is skipped by listings, since it cannot be decoded to a key.
- **Token refresh.** Dropbox access tokens are short-lived; pass a source that refreshes. A revoked app or refresh token is `CodePermission` on every call, not `CodeUnavailable`, so retrying does not help.
- **Rate limits and contention.** Concurrent writes into one Dropbox namespace can fail with `too_many_write_operations`; it is `CodeUnavailable`, so it can be retried. A streamed body cannot be replayed, so `layer.Retry` retries a write only when the body can seek (a file or `WriteBytes`).
- **Size limits.** `ChunkSize` above 150 MiB is refused by `New`. Each write holds one chunk in memory.
- **No temporary links through the Disk.** `SignedURL` is `CodeUnsupported`; Dropbox's own four-hour links are reachable through the native client, but they cannot expire sooner.

## Related

- [Google Drive](gdrive.md) and [OneDrive and SharePoint](onedrive.md), the other consumer drives, with the same steps.
- [Cloud picker imports](../plugins/pickers.md): import one file the user picks with the Dropbox Chooser, rather than writing into their Dropbox.
- [Consistency](../concepts/consistency.md) for conditional writes across backends, [Capabilities](../concepts/capabilities.md) and [Errors](../concepts/errors.md).
- [Comparison of every backend](README.md).
