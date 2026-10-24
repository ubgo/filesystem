# Share links

The share plugin makes links that let anyone holding them download one file, until the link expires, is revoked, or has been used a set number of times: the "send a link" feature of drives, chat apps and client portals. The person receiving the link needs no account.

A link is a random token of 256 bits. It is shown once, when the link is created, and the database keeps only its SHA-256 hash, so a leaked backup cannot be turned into working links. Every failure a visitor can see is the same not-found, so links cannot be probed for "expired" versus "never existed".

## Before you start

- A Store whose disk can sign URLs, as in [Files](../records/files.md): the link handler redirects to `Store.DownloadURL`, a short-lived signed GET. A memory or local disk needs `filesystem.WithURLSigner` and the `fshttp.Signed` handler mounted where the signer points; S3, GCS and Azure presign by themselves. See [Serving files](../concepts/serving-files.md).
- The example is [`examples/records-share`](../../examples/records-share). `go run ./records-share` in `examples/` prints the Go walk-through; `go run ./records-share serve` serves on `localhost:8090` and prints a link; `go test ./records-share` runs both offline.
- Who may create a link is your decision: the plugin has no notion of users. Check that the caller may share the file before calling `Create`.

## Step 1 — Install the plugin

<!-- ds:block id=newstore-ndhmkyzh -->
**newStore** · [`examples/records-share/main.go:100-110`](../../examples/records-share/main.go#L100-L110)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, opts ...share.Option) (*records.Store, *share.Plugin, error) {
	p := share.New(opts...)
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, p, nil
}
```
<!-- /ds:block hash=54ee41 -->

`Migrate` creates `fs_shares`. `share.WithClock` sets the plugin's clock, for tests of expiry; without it the plugin reads the Store's clock (`records.WithClock`), and the handler always does when it checks a file's own expiry. Link and use-record IDs come from `Store.NewID`, so `records.WithIDs` covers them.

## Step 2 — Create a link

`Create` takes the scope, a live file's ID and `share.Options`:

| Field | Meaning |
|---|---|
| `TTL` | the link's lifetime; required, greater than 0 and at most `share.MaxLifetime` (365 days), or the lower cap of `share.WithMaxLifetime` |
| `MaxDownloads` | how often the link works; 0 is unlimited, negative is refused |

<!-- ds:block id=sharefile-s3tjks9n -->
**shareFile** · [`examples/records-share/main.go:121-123`](../../examples/records-share/main.go#L121-L123)

```go
func shareFile(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) (share.Share, error) {
	return p.Create(ctx, s, tenant, fileID, share.Options{TTL: linkTTL, MaxDownloads: linkDownloads})
}
```
<!-- /ds:block hash=b08c90 -->

The returned `share.Share` carries `Token`; build the URL from it and give it to the user now. No later call returns it.

```text
link shr_… token 43 chars, expires in 168h0m0s, 2 downloads
```

Check: the `token_hash` column of `fs_shares` holds the hex SHA-256 of the token, never the token (`TestOnlyTheHashIsStored`).

A permanent public link is not what this plugin is for (hence the one-year cap): make the file public instead, with `Store.SetVisibility`, see [Replace and visibility](../records/replace-and-visibility.md).

## Step 3 — Resolve a link

`Resolve` spends one download and returns the file. It loads the file first, so a link to a trashed or missing file fails without spending anything, and in the spending transaction it counts the file's view (`records.Store.Viewed`) before the download, so an expired file, or a view-once file already seen, is refused and spends nothing; the spend is then one conditional `UPDATE` (not revoked, not expired, under the limit), so two visitors racing for the last download cannot both get it. `Check(ctx, s, token)` answers the same question without spending a download, for a landing page shown before the download starts.

<!-- ds:block id=open-zpzbgxf2 -->
**open** · [`examples/records-share/main.go:131-133`](../../examples/records-share/main.go#L131-L133)

```go
func open(ctx context.Context, s *records.Store, p *share.Plugin, token string) (records.File, error) {
	return p.Resolve(ctx, s, token)
}
```
<!-- /ds:block hash=1c4257 -->

```text
download 1: "contract.txt"
download 2: "contract.txt"
download 3: not_found (filesystem: not_found: share: link not found or no longer valid)
```

Every unusable link (unknown token, expired, revoked, exhausted, or its file trashed or gone) fails with `CodeNotFound` and that same message. Log the token-free context on your side if you need to know which.

## Step 4 — List and revoke

`Shares` lists a file's links, newest first, with `ExpiresAt`, `MaxDownloads`, `Downloads` and `RevokedAt`, and an empty `Token`: the owner's "shared links" panel. `Revoke` ends a link at once; revoking twice, or a link that does not exist, does nothing.

<!-- ds:block id=linksof-czvcpu4u -->
**linksOf** · [`examples/records-share/main.go:138-140`](../../examples/records-share/main.go#L138-L140)

```go
func linksOf(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) ([]share.Share, error) {
	return p.Shares(ctx, s, tenant, fileID)
}
```
<!-- /ds:block hash=de139a -->

<!-- ds:block id=stopsharing-y4m2w3yj -->
**stopSharing** · [`examples/records-share/main.go:144-146`](../../examples/records-share/main.go#L144-L146)

```go
func stopSharing(ctx context.Context, s *records.Store, p *share.Plugin, shareID string) error {
	return p.Revoke(ctx, s, tenant, shareID)
}
```
<!-- /ds:block hash=b1d2b2 -->

The walk-through then shows every way a link becomes unusable, and what a purge does:

<!-- ds:block id=unusable-2jbw3fmt -->
**unusable** · [`examples/records-share/main.go:220-252`](../../examples/records-share/main.go#L220-L252)

```go
func unusable(ctx context.Context, w io.Writer, a *app, f records.File) error {
	sh, err := shareFile(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	if err := stopSharing(ctx, a.store, a.share, sh.ID); err != nil {
		return err
	}
	_, err = open(ctx, a.store, a.share, sh.Token)
	fmt.Fprintf(w, "revoked: %v\n", err)
	_, err = open(ctx, a.store, a.share, "no-such-token")
	fmt.Fprintf(w, "unknown: %v\n", err)
	live, err := shareFile(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	if err := a.store.Trash(ctx, tenant, f.ID); err != nil {
		return err
	}
	_, err = open(ctx, a.store, a.share, live.Token)
	fmt.Fprintf(w, "file trashed: %v\n", err)
	list, err := linksOf(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "links of the file: %d (revoked: %t, token listed: %q)\n", len(list), list[1].RevokedAt != nil, list[0].Token)
	if err := a.store.Purge(ctx, tenant, f.ID); err != nil {
		return err
	}
	list, err = linksOf(ctx, a.store, a.share, f.ID)
	fmt.Fprintf(w, "links after purge: %d\n", len(list))
	return err
}
```
<!-- /ds:block hash=db52bb -->

```text
revoked: filesystem: not_found: share: link not found or no longer valid
unknown: filesystem: not_found: share: link not found or no longer valid
file trashed: filesystem: not_found: share: link not found or no longer valid
links of the file: 3 (revoked: true, token listed: "")
links after purge: 0
```

Check: trashing the file stops its links without deleting them, so restoring the file brings them back (with whatever time and downloads they have left). `Resolve` looks at the file before it spends, so attempts on a trashed file's link use none of its downloads. Purging the file deletes its links in the purge transaction.

## Step 5 — Serve links over HTTP

`share.Handler(store, plugin)` serves `GET /{token}`: it spends one download and answers `302` to a signed URL that saves the file under its name and lives five minutes (`share.DefaultLinkLifetime`; see [your own link policy](#your-own-link-policy)), long enough to start the download and too short to become a second, unlimited link. Mount it with its prefix stripped, next to the handler the signed URL points at:

<!-- ds:block id=newmux-6ne3q9m6 -->
**newMux** · [`examples/records-share/main.go:151-156`](../../examples/records-share/main.go#L151-L156)

```go
func newMux(s *records.Store, p *share.Plugin, disks *filesystem.Manager, signer *signedurl.Signer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(sharePrefix, http.StripPrefix(sharePrefix, share.Handler(s, p)))
	mux.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(disks, signer)))
	return mux
}
```
<!-- /ds:block hash=016e41 -->

With `go run ./records-share serve` running (it printed the link, limit 2):

```sh
L=http://localhost:8090/s/AMnh2E6qiYTeo-bPyToYkGN6ZsaTBDIv0ymG2z0mZw8
curl -s -i "$L"
```

```text
HTTP/1.1 302 Found
Cache-Control: no-store
Content-Type: text/html; charset=utf-8
Location: http://localhost:8090/files/media/docs/g2kbi37e3tjgxekyzr5vbcznqd.txt?cd=attachment%3B+filename%3D%22contract.txt%22%3B+filename%2A%3DUTF-8%27%27contract.txt&ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791019191&kid=k1&m=GET&sig=5b3897d2db2d735db36c59e95f5b89c8b5bc4e7fc49d135ff6b86797af6e5698
Referrer-Policy: no-referrer
Date: Sat, 03 Oct 2026 09:14:51 GMT
Content-Length: 332
```

Following the `Location` downloads the file:

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Disposition: attachment; filename="contract.txt"; filename*=UTF-8''contract.txt
Content-Length: 22
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/plain; charset=utf-8
Etag: "6ae3daeb2b1f75fb41f74e1db0a8e377"
Last-Modified: Sat, 03 Oct 2026 09:14:49 GMT
X-Content-Type-Options: nosniff

signed by both parties
```

The second request is the last download (`302`); the third finds the link exhausted:

```text
HTTP/1.1 404 Not Found
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Content-Length: 10

Not Found
```

Check: `Referrer-Policy: no-referrer` keeps the token out of the `Referer` the download page could send, and `Cache-Control: no-store` keeps the redirect (which spent a download) out of caches. Any method but GET and HEAD is `405` with `Allow: GET, HEAD`; a database failure is `500`, never a hint about the link. A `HEAD` request checks the link without spending a download: `204` with no `Location` for a usable link (a signed URL handed to a HEAD would be a free download), `404` otherwise.

The handler does not rate-limit. Put your rate limiter in front of the mount; 256-bit tokens cannot be guessed, but a limiter keeps scanners off the database.

### Your own link policy

What the handler answers, how it hands a file over and who may use a link are options; the defaults above stay as they are without them.

| Option | Replaces |
|---|---|
| `share.WithErrors(w)` | The plain-text answers: `w` is the `fshttp.ErrorWriter` every handler of the library takes. It receives `share.ErrPassword` for the `401`, where your password page goes, and a `not_found` error for every unusable link (keep those alike, so links cannot be probed). |
| `share.WithDeliver(fn)` | The redirect: `fn(w, r, link, file, url)` gets the signed URL after the download is spent, for a branded download page, a preview, or streaming. |
| `share.WithLinkLifetime(d)` | The five-minute signed URL, up to `share.MaxLinkLifetime` (one hour), for download managers that resume after a pause. |
| `share.WithAccessCheck(fn)` | "Anyone holding the link": `fn(r, link)` runs for a live link before the password and before anything is spent, and its error's code is the answer (`permission` `403`, `not_found` hides the link, an uncoded error `500`). |
| `share.WithUploadPolicy(fn)` | One policy for every request link: `fn(r, link, base)` returns the policy of this link's upload. |
| `share.WithUploadDisk(fn)` | `WithUploads`' one disk: `fn(r, link)` names the disk of this link's upload, a tenant's own bucket or a disk per collection, without a handler per tenant. Its error's code is the answer; an empty or unknown name is `500` (your function is wrong, not the visitor). Nothing of the file is read before it decides. |
| `share.WithRespond(fn)` | The JSON answers: a collection's listing (`share.ListResponse`, a bare array) and a request link's upload (`share.DepositResponse`, `{"id": …}`). `fn(w, r, status, body)` writes them in your envelope (`{"data": …}`, camelCase); `body` is a closed `share.SuccessBody`, so a type switch over the two covers it. Each `ListedFile` and the `DepositResponse` carry the `records.File` behind them in `File` (never encoded), for a thumbnail, a date or a caption without a lookup per row. Redirects, `WithDeliver` pages and a listing's `HEAD` do not pass here. |

Links only members of the visitor's organisation can open, a branded page, and a 20 MB cap on the client portal's request links:

```go
h := share.Handler(store, plugin,
	share.WithAccessCheck(func(r *http.Request, link share.Share) error {
		if orgOf(r.Context()) != link.Scope { // your session's organisation
			return filesystem.Errorf(filesystem.CodePermission, "sign in to %s", link.Scope)
		}
		return nil
	}),
	share.WithDeliver(func(w http.ResponseWriter, r *http.Request, link share.Share, f records.File, url string) {
		_ = downloadPage.Execute(w, map[string]string{"Name": f.Filename, "URL": url}) // your html/template
	}),
	share.WithErrors(func(w http.ResponseWriter, r *http.Request, status int, err error) {
		if errors.Is(err, share.ErrPassword) {
			w.WriteHeader(status)
			_ = passwordPage.Execute(w, nil)
			return
		}
		log.Printf("share link: %d: %v", status, err)
		http.Error(w, http.StatusText(status), status)
	}),
	share.WithUploads("media", portalPolicy),
	share.WithUploadPolicy(func(r *http.Request, link share.Share, base upload.Policy) (upload.Policy, error) {
		base.MaxSize = 20 << 20
		return base, nil
	}),
	share.WithUploadDisk(func(r *http.Request, link share.Share) (string, error) {
		return bucketOf(link.Scope), nil // your tenant's own disk, registered in the Store's manager
	}),
)
```

Two limits are floors and cannot be loosened: tokens stay 256 random bits or more (`share.WithTokenBytes(n)` takes 32 to 64 bytes, so short codes and vanity slugs are not possible), and a link lives at most `share.MaxLifetime` (`share.WithMaxLifetime(d)` can only lower the cap, for a policy such as "links last at most 30 days"). A link that should never expire is a public file.

Try it: [`examples/records-share`](../../examples/records-share) runs this section in `policy.go` ([`newPolicyHandler`](ds:block?id=newpolicyhandler-c6r2p9wf)): `go run ./records-share` from `examples/`.

## Passwords

A password makes a link need two things: the token and what the owner told the recipient separately. The second half of [`examples/records-share`](../../examples/records-share) (`collections.go`) runs this section and the next three: `go run ./records-share` prints every check.

<!-- ds:block id=protect-c3w7t9nk -->
**protect** · [`examples/records-share/collections.go:41-43`](../../examples/records-share/collections.go#L41-L43)

```go
func protect(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) (share.Share, error) {
	return p.Create(ctx, s, tenant, fileID, share.Options{TTL: linkTTL, Password: linkPassword})
}
```
<!-- /ds:block hash=0321a2 -->

Resolve it with `share.WithPassword(pw)`:

<!-- ds:block id=openprotected-r9n4x2hv -->
**openProtected** · [`examples/records-share/collections.go:48-50`](../../examples/records-share/collections.go#L48-L50)

```go
func openProtected(ctx context.Context, s *records.Store, p *share.Plugin, token, password string) (records.File, error) {
	return p.Resolve(ctx, s, token, share.WithPassword(password))
}
```
<!-- /ds:block hash=a0030f -->

Check: `password link without a password: permission (true)` (it is `share.ErrPassword`), `with a wrong one: permission`, `with the right one: contract.txt`.

`share.Options{Password: "…"}` protects a link. Only a PBKDF2-SHA256 hash is stored (600,000 iterations, each hash carrying its own count). `Resolve`, `Check` and the collection calls need `share.WithPassword(pw)`; without it, or with a wrong one, they return `share.ErrPassword` and spend nothing. The Handler answers `401` until a password arrives, as the `password` field of a POST to the same path (a landing page's form, answered with `303` to the download) or the `Share-Password` header. It looks the link up first, so a token that names no link never reaches the limiter, then takes an attempt from an `AttemptLimiter` keyed by the link's ID before checking a password (`Allow`, one atomic step, so guesses sent in parallel cannot overrun it) and gives it back when the password was right (`Release`), so only wrong ones stay counted: the right password is never limited until the link has seen too many wrong ones, after which every password, the right one included, is answered `429` until the window passes (answering the right one would let guessing go on). The default allows 10 wrong passwords per link per 15 minutes in this process and tracks at most `share.MaxTrackedKeys` links, dropping the oldest window first; give `share.WithAttemptLimiter` one backed by shared storage when several instances serve the links. The default limiter counts its window on the Store's clock (`records.WithClock`); `share.NewAttemptLimiterClock(max, window, now)` builds the same in-process limiter on a clock of your own.

## Share a folder or a collection

A link can share a set of files instead of one: a folder, an owner's role (a post's gallery), or any `share.Collection` of your own. Install the collections the links may name:

<!-- ds:block id=newfolderstore-v8m2k5wc -->
**newFolderStore** · [`examples/records-share/collections.go:25-36`](../../examples/records-share/collections.go#L25-L36)

```go
func newFolderStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *folders.Plugin, *share.Plugin, error) {
	fp := folders.New()
	sp := share.New(share.WithCollections(fp.Collection(), share.Owners{}))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(fp, sp))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, err
	}
	return s, fp, sp, nil
}
```
<!-- /ds:block hash=6e81d6 -->

`CreateFor` makes the link. The target is a folder (`Target{Kind: folders.KindFolder, Ref: folderID}`, its own files, not its subfolders'), an owner's role (`Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "gallery")}`), or your collection's kind:

<!-- ds:block id=sharefolder-p5k8w3tm -->
**shareFolder** · [`examples/records-share/collections.go:55-57`](../../examples/records-share/collections.go#L55-L57)

```go
func shareFolder(ctx context.Context, s *records.Store, p *share.Plugin, folderID string) (share.Share, error) {
	return p.CreateFor(ctx, s, tenant, share.Target{Kind: folders.KindFolder, Ref: folderID}, share.Options{TTL: linkTTL})
}
```
<!-- /ds:block hash=6eaee6 -->

`List(ctx, store, token)` returns the live files and the link; `ResolveFile(ctx, store, token, fileID)` resolves one of them for download, counting toward `MaxDownloads`. Over HTTP, the Handler's `GET /{token}` lists the files as JSON and `GET /{token}/{fileID}` downloads one. The set is read at each request, so files added later are included, and trashed files and files outside the collection are the same `404` as an unknown link. A kind no installed collection serves is `invalid_argument`. Check: `folder link lists 1 file(s)`.

## Request files: upload-only links

`CreateRequest` makes a link that accepts uploads into a collection that is also a `Receiver` (a folder, an owner's role) and shows nothing. `MaxDownloads` caps the uploads:

<!-- ds:block id=requestfiles-h6c2v8qn -->
**requestFiles** · [`examples/records-share/collections.go:62-64`](../../examples/records-share/collections.go#L62-L64)

```go
func requestFiles(ctx context.Context, s *records.Store, p *share.Plugin, folderID string, maxFiles int64) (share.Share, error) {
	return p.CreateRequest(ctx, s, tenant, share.Target{Kind: folders.KindFolder, Ref: folderID}, share.Options{TTL: linkTTL, MaxDownloads: maxFiles})
}
```
<!-- /ds:block hash=398ef4 -->

`Deposit` is one upload through it: it stores the body under your policy, records it in the link's scope (quota and every hook apply, so a full scope answers `507` over HTTP), places it in the target and counts one use, all in one transaction. The body is received before that transaction begins, so a slow client holds no database connection. Its `UploadedBy` is the request's actor, else `share:<link ID>`:

<!-- ds:block id=deposit-t2q7m4wk -->
**deposit** · [`examples/records-share/collections.go:70-72`](../../examples/records-share/collections.go#L70-L72)

```go
func deposit(ctx context.Context, s *records.Store, p *share.Plugin, token, name, body string) (records.File, error) {
	return p.Deposit(ctx, s, token, diskName, strings.NewReader(body), name, docPolicy)
}
```
<!-- /ds:block hash=8e93a4 -->

Over HTTP, mount the Handler with `share.WithUploads(disk, policy)`, and a POST of a multipart `file` to the link does the same. `share.WithQuarantinedDeposits()` records deposits [quarantined](../records/processing-status.md#quarantine-until-scanned), for uploads from strangers. Check: `request link: receipt.txt by share:… into Inbox true`, then `second upload past its limit: not_found` and `listing through the request link: not_found`: a request link never reveals what the folder holds.

## Link statistics

Every use (a download through a file or collection link, an upload through a request link) is recorded in `fs_share_events` with its time and, when your middleware set one with `records.WithActor`, the visitor. Listing a collection is not a use. `Stats(ctx, s, scope, shareID, recent)` returns the count and up to `recent` newest uses (at most `share.MaxRecentUses`, 1000), newest first:

```go
st, err := sp.Stats(ctx, s, tenant, link.ID, 10) // st.Uses, st.Recent[0].At, st.Recent[0].Viewer
```

Check: after Carol downloads the beach photo through the folder link, the example prints `folder link stats: 1 use(s), last by "user:carol"`. The records go with their link when `Prune` deletes it, when its file is purged, and with the scope.

Bytes per link: every download is reported to `records.Store.Served` as `records.ServeShare` with the link's ID as the subject, so the [egress](egress.md#per-share-link-a-breakdown-by-subject) plugin built with `egress.WithPerSubject(records.ServeShare)` meters each link's bytes per day (`SubjectDays`), for a "this link used 4 GB" panel or a cap of your own in `WithAccessCheck`.

## The table

`fs_shares`, one row per link:

| Column | Type | Holds |
|---|---|---|
| `id` | key, primary key | `shr_` plus 128 random bits |
| `scope` | key | the file's scope |
| `file_id` | key, indexed (`fs_shares_file`) | the shared file |
| `token_hash` | short, unique (`fs_shares_token`) | hex SHA-256 of the token |
| `expires_at` | timestamp (UTC) | when the link stops working |
| `max_downloads` | 64-bit integer | the limit; 0 is unlimited |
| `downloads` | 64-bit integer | downloads spent |
| `revoked_at` | timestamp, nullable | set by `Revoke` |
| `created_at` | timestamp (UTC) | creation |

Expired and revoked rows stay until their file is purged, so `Shares` can still show them. `Prune(ctx, s, cutoff)` deletes the links, in every scope, that expired or were revoked before `cutoff`, and returns how many; schedule it with your retention (`time.Now().Add(-30 * 24 * time.Hour)`). A link exhausted by `MaxDownloads` goes once it expires, within `MaxLifetime`.

## How it works with the rest

- **Trash.** A trashed file's links fail as not-found and come back with `Restore`. `Create` refuses a trashed file with `CodeNotFound`.
- **Quarantine.** `Create` refuses a quarantined file (`records.ErrQuarantined`, `conflict`). A link whose file was quarantined after the link was made answers its visitors the uniform 404 (`Resolve`, `Check`, GET and HEAD) and spends nothing, until the scan passes; a folder or collection link leaves quarantined files out of its listing and refuses to serve them. `share.WithQuarantinedDeposits()` records every upload through a request link quarantined, the natural default for files from whoever holds a link. See [Quarantine until scanned](../records/processing-status.md#quarantine-until-scanned).
- **Purge.** `BeforePurge` deletes the file's links in the purge transaction, whichever way the purge came (`Purge`, `SweepTrash`, `PurgeOwner`, `SetAttachment`).
- **Replace.** A link points at the file, not at a content: after `Replace` (or a [versions](versions.md) restore) the link downloads the new content.
- **Visibility.** The handler always redirects to a signed download, so a link works for private and public files alike, and keeps working when the file moves disks with `WithVisibilityDisks`.
- **Events.** `Create`, `Resolve` and `Revoke` emit `share.created`, `share.downloaded` and `share.revoked` inside their transactions, through `Store.Emit`, with the file's `FileID` and the link's ID as `Subject` (never the token). `Detail` has `share.DetailExpiresAt` and `DetailMaxDownloads` on creation and `DetailDownloads`, the count including this download, on each download; see [Events and hooks](../records/events-and-hooks.md#events-from-plugins). `Check`, a refused download, revoking a revoked link and `Prune` emit nothing. A hook's error rolls the change back, so a download is never counted without its event. The [audit plugin](audit.md) writes one entry per event with its kind, file, actor, time, `Subject` and `Detail`; a visitor following a link usually has no actor.

## What can go wrong

- `invalid_argument` from `Create` — `TTL` is zero, negative or longer than `MaxLifetime` (or the `WithMaxLifetime` cap), or `MaxDownloads` is negative. From `Migrate` and every `Create` too: a `WithTokenBytes` or `WithMaxLifetime` value out of range.
- `not_found` from `Create` — the file does not exist in that scope, or is in the trash.
- `conflict` from `Create` — the file is quarantined until its scan passes.
- Every visitor gets `404` — the link is spent, expired or revoked, or its file is trashed. `Shares` shows which, to the owner.
- `500` from the handler on a valid link — the disk cannot sign URLs (no `WithURLSigner` on a memory or local disk), or the database failed.
- A download whose redirect target answers `403` — the signed URL expired (five minutes, or `WithLinkLifetime`'s) or the `fshttp.Signed` mount does not match the signer's base URL.

## Next

- [Serving files](../concepts/serving-files.md): signed URLs and the `fshttp.Signed` handler the redirect lands on.
- [Lifecycle](../records/lifecycle.md): trash, restore and purge, which decide when links work.
- [Private downloads](../recipes/private-downloads.md): signed links for signed-in users rather than anyone.
- The plugin's [README](../../records/share/README.md) is the reference for its API and table.
