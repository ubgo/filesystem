# OneDrive and SharePoint

The OneDrive driver stores files in a Microsoft Graph drive: a user's OneDrive (personal or for Business) or a SharePoint document library. Use it when files belong in the customer's Microsoft 365 storage rather than in a bucket the application owns. It speaks Graph REST as plain JSON over `net/http`, with no dependency beyond the library.

## A user's OneDrive or a bucket?

Choose a consumer drive (OneDrive, [Google Drive](gdrive.md) or [Dropbox](dropbox.md)) when the files belong to the user or their organisation and they expect to find them there: reports saved into a team's SharePoint library, exports a user asked for, documents a sync feature keeps current. The customer pays for the storage, their retention and compliance policies apply, and they can revoke the application at any time. Choose a bucket ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md)) for files the application owns and serves to browsers: a consumer drive has no signed or public URLs through the Disk, needs one OAuth token per user (or an application token for a SharePoint site), and is throttled per user and per tenant.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/onedrive`, a module of its own. Its reference is [adapters/onedrive/README.md](../../adapters/onedrive/README.md).
- Register the application in Microsoft Entra ID with the delegated `Files.ReadWrite` permission (or `Files.ReadWrite.AppFolder` to confine it to `Apps/<app name>`, or `Sites.Selected` for one SharePoint site). Your application runs the OAuth flow and keeps the refresh token; the driver only asks for an access token before each request.
- The runnable program is [`examples/adapters-consumer-drives`](../../examples/adapters-consumer-drives/). It runs the same steps on OneDrive, Google Drive and Dropbox against in-process fakes of their APIs, so it needs no account.
- Tested against an in-process fake of the documented Graph requests. What only a real tenant shows: Graph's acceptance of `*` as the total size in an upload fragment of a stream whose length is unknown (pass `WriteOptions.SizeHint` when you know it), and the names OneDrive reserves (`.lock`, `CON`, `desktop.ini`, `~$` prefixes).

## Step 1: Adapt your OAuth token source

`TokenSource` is a one-method interface of the driver's own, so the module does not depend on `golang.org/x/oauth2` or MSAL; `TokenFunc` adapts any function:

```go
src := oauthConfig.TokenSource(ctx, savedToken) // golang.org/x/oauth2; refreshes and caches
token := onedrive.TokenFunc(func(context.Context) (string, error) {
	t, err := src.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
})
```

The source is called once per request to the Graph origin and must be safe for concurrent use; Graph access tokens are short-lived, so it must refresh, and should cache the token until it expires. A failing source and a refused token are both `CodePermission`.

## Step 2: Open a disk

`onedrive.New` checks the Config and makes no request, so one disk per user, built per request, costs nothing. `Drive` names any Graph drive: `/me/drive` (the default), `/drives/{drive-id}`, `/users/{user-id}/drive`, `/sites/{site-id}/drive`. `Root` is the folder holding the disk; empty means the drive's root. The example points `BaseURL` at its fake and sets the smallest fragment so a large body uses an upload session; in production leave `BaseURL` (or set it for a national cloud) and `ChunkSize` unset.

<!-- ds:block id=openonedrive-4xmy77th -->
**openOneDrive** · [`examples/adapters-consumer-drives/main.go:154-171`](../../examples/adapters-consumer-drives/main.go#L154-L171)

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

There is no DSN form, since a token source is code. `BaseURL` must be https (http only on loopback, for tests).

## Step 3: Know what the disk can do

```text
onedrive capabilities: [move,delete-prefix,conditional-write,compare-and-swap,case-insensitive]
```

`Move` (a `PATCH` of parent and name, replacing an existing destination) and `DeletePrefix` (one `DELETE` of the folder) on the server; `ConditionalWrite` (`IfNotExists` is `conflictBehavior=fail`) and `CompareAndSwap` (`IfMatch` is an `If-Match` header on the item's eTag), both checked by Graph; and `CaseInsensitive`. `Copy` is not declared, because Graph copies asynchronously behind a monitor URL; the Disk streams the bytes instead. There are no signed URLs (Graph's pre-authenticated download URLs take no expiry), and no visibility, metadata, stored content type or headers.

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
onedrive: signed URL: unsupported, public write: unsupported, metadata: unsupported
```

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
onedrive: read "buy milk\n", stat size=9 etag set=true type=""
onedrive: stat NOTES/TODO.TXT: ok
onedrive: list notes: [notes/todo.txt]
```

- Keys are paths under `Root`. Paths are case-insensitive. Characters OneDrive refuses in a name (`" * : < > ? \ | #`) and a segment ending in `.` or a space are stored percent-escaped and decoded back, so the conformance suite's odd-character key round-trips. Graph creates missing parent folders on upload, and `Move` creates them before moving.
- Reads ask Graph for `/content`, which redirects to a pre-authenticated download host; the `Range` header travels with the redirect and the token does not, as Graph requires.
- `List` walks folders with `children` requests (1,000 per page), following `@odata.nextLink` only within the Graph origin.

## Step 5: Use conditional writes

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
onedrive: IfNotExists on an existing key: exists
onedrive: IfMatch current etag: ok, same etag again: conflict
```

`IfNotExists` holds for upload sessions too, since Graph checks the conflict again when the last fragment commits. `IfMatch` does not: Graph checks it only when a session starts, so a swap through a session would not be atomic, and a body longer than `ChunkSize` with `IfMatch` is refused with `CodeUnsupported` before anything is written (Step 7). Raise `ChunkSize` (up to 60 MiB) for larger compare-and-swap documents. A failed `IfMatch` is `CodeConflict`, also when the item was deleted meanwhile.

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
onedrive: after copy and move: [archive/2026/todo.txt notes/todo.txt]
```

The copy above worked although the driver does not declare `Copy`: the Disk read the file and wrote it back, so its bytes went through the application. `Move` is one `PATCH` that replaces an existing destination. `DeletePrefix` is one `DELETE` of the folder; a file named like the prefix is not inside it and is left alone.

## Step 7: Large files go through an upload session

A body shorter than `ChunkSize` (10 MiB, a multiple of `onedrive.FragmentUnit`, 320 KiB, at most 60 MiB) is one `PUT`. A longer one goes through an upload session: fragments of `ChunkSize`, the last of which commits the file, so a reader never sees a partial one. A body that fails part-way cancels the session and stores nothing. Fragments go to a pre-authenticated session URL that never receives the token.

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
onedrive: large body 819200 bytes in 3 session requests, read back identical=true
onedrive: IfMatch on a large body: unsupported
```

## Step 8: Handle token failures and throttling

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
onedrive: failing token source: permission, refused token: permission
onedrive: rate limited: unavailable, retry after 7s
```

The full mapping: 404 and 410 are `CodeNotFound`, 409 `CodeExists`, 412 `CodeConflict`, 401 and 403 `CodePermission`, 413 `CodeTooLarge`, 507 `CodeQuotaExceeded` (the drive is full), 400 `CodeInvalidArgument`, and 429 and 5xx `CodeUnavailable` with the `Retry-After` Graph sent (`filesystem.RetryAfterOf`). Treat `CodePermission` as "ask the user to sign in again", and wrap the disk in `layer.Retry` for `CodeUnavailable` (see [Errors](../concepts/errors.md)).

A failed request's error never carries a pre-authenticated URL: the query of every URL and the path of any URL off the Graph origin are removed from transport errors, so logs cannot replay a download or an upload session. For Graph endpoints the driver does not call (sharing links, permissions), `filesystem.As[*http.Client](disk)` returns the driver's client, whose transport adds the token for the Graph origin only.

## Try it

```sh
cd examples
go run ./adapters-consumer-drives
```

The OneDrive part of the output:

```text
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

`go test ./adapters-consumer-drives` checks every line. The driver's own tests (`adapters/onedrive`) run the conformance suite against a fuller fake, with the smallest fragment too.

## Pitfalls

- **Compare-and-swap stops at one chunk.** `IfMatch` on a body longer than `ChunkSize` is `CodeUnsupported`, with nothing written. Keep such documents under `ChunkSize`, or raise it (up to 60 MiB, at the cost of memory per write).
- **Copy is not server-side.** Every `Copy` downloads and uploads the file through your application, using its bandwidth and memory for one chunk at a time, and the destination is a new upload of those bytes.
- **Case-insensitive keys.** `Report.pdf` and `report.pdf` are one item. Listings show the casing OneDrive keeps, not necessarily the key you wrote.
- **Escaped names show in the user's OneDrive.** A key holding `%`, `#`, `:` or another refused character appears percent-escaped in OneDrive and SharePoint (`a:b` is stored as `a%3Ab`). A file the user adds by hand whose name is not one the driver could have written (a stray `%`) is skipped by listings.
- **Reserved names.** OneDrive refuses names such as `.lock`, `CON`, `desktop.ini` and names starting `~$`; the driver does not escape them, so a key that uses one fails on a real tenant.
- **Token refresh and throttling.** Graph access tokens are short-lived; pass a source that refreshes. Graph throttles per user and per tenant with 429; it arrives as `CodeUnavailable` with a wait. A streamed body cannot be replayed, so `layer.Retry` retries a write only when the body can seek (a file or `WriteBytes`).
- **Memory per write.** A session write reads one fragment ahead, so it holds up to two chunks (20 MiB at the default size).
- **ChunkSize rules.** `New` refuses a `ChunkSize` that is not a multiple of 320 KiB or is above 60 MiB.

## Related

- [Google Drive](gdrive.md) and [Dropbox](dropbox.md), the other consumer drives, with the same steps.
- [Cloud picker imports](../plugins/pickers.md): import one file the user picks with the OneDrive and SharePoint file picker, rather than writing into their drive.
- [Consistency](../concepts/consistency.md) for conditional writes across backends, [Capabilities](../concepts/capabilities.md) and [Errors](../concepts/errors.md).
- [Comparison of every backend](README.md).
