# Share links to one file

The share plugin of the records layer: a link to one file with a lifetime and a download limit, as "anyone with the link" sharing in a drive, a client portal or a support tool works. The program creates a link, spends its downloads, lists and revokes links, shows that every unusable link answers with the same not-found, and serves the HTTP handler that turns a link into a short-lived signed download. Only the token's SHA-256 is stored. It uses `github.com/ubgo/filesystem/records`, `records/share`, `fshttp` and `signedurl` on SQLite over an in-memory disk. The guide [Share links](../../docs/plugins/share.md) explains each step.

## Run it

```sh
cd examples
go run ./records-share          # the walk-through, then exit
go run ./records-share serve    # serve on localhost:8090 and print a link
go test ./records-share
```

Both modes need no network or environment variables. The SQLite database lives in a temporary directory and the bytes in memory. The walk-through deletes that directory when it exits. `serve` listens on `http://localhost:8090` until you stop it with Ctrl-C (or SIGTERM), then shuts down cleanly and removes its temporary directory (`records-share…` under your system temp directory).

## Walkthrough

The walk-through, in Go:

```sh
go run ./records-share
```

```text
link shr_… token 43 chars, expires in 168h0m0s, 2 downloads
download 1: "contract.txt"
download 2: "contract.txt"
download 3: not_found (filesystem: not_found: share: link not found or no longer valid)
revoked: filesystem: not_found: share: link not found or no longer valid
unknown: filesystem: not_found: share: link not found or no longer valid
file trashed: filesystem: not_found: share: link not found or no longer valid
links of the file: 3 (revoked: true, token listed: "")
links after purge: 0
password link without a password: permission (true)
with a wrong one: permission
with the right one: contract.txt
folder link lists 1 file(s)
request link: receipt.txt by share:… into Inbox true
second upload past its limit: not_found
listing through the request link: not_found
folder link stats: 1 use(s), last by "user:carol"
policy: link opened from another organisation: 403
policy: link opened by a member: 200 <h1>invoice.txt</h1>
policy: password link: 401 <form method="post"><input name="password" type="password"></form>
policy: 12-byte upload through the request link: 413
policy: 5-byte upload through the request link: 201
```

1. A link is made for `contract.txt` that works twice within seven days. The 43-character token is shown once; the database cannot give it back.
2. Two downloads succeed and the third is `not_found`.
3. A revoked link, an unknown token and a link to a trashed file all fail with the same `not_found`, so a caller cannot tell which.
4. The file's links are listed for its owner, with counts and expiry but never tokens. Purging the file deletes its links.
5. `collections.go` opens a second Store on the same database with the folders plugin as a share collection. A link with a password is refused (`share.ErrPassword`) without it or with a wrong one, and resolves with the right one.
6. A link to the `Trip` folder lists its file. A request link into `Inbox` accepts one upload, recorded as uploaded by `share:<link ID>` and placed in the folder; a second upload is past its limit, and the request link lists nothing.
7. `Stats` counts the folder link's use (a download by Carol, the actor of the request) and names her.
8. `policy.go` serves links through `share.Handler` with the app's own policy: `WithAccessCheck(membersOnly)` lets only visitors signed in to the link's organisation use it, `WithDeliver(brandedPage)` hands the file over on a page instead of a redirect, `WithErrors(linkErrors)` renders a password form for `share.ErrPassword`, and `WithUploadPolicy(portalPolicy)` caps uploads through request links at 10 bytes. A visitor from another organisation gets 403; a member gets the branded page (200) with the signed URL in it.
9. A password link answers 401 with the app's form rather than plain text.
10. Through a request link, a 12-byte upload is past the portal's cap (413) although `WithUploads`' base policy allows a megabyte; a 5-byte one is stored (201). The test `TestAccessCheckSpendsNothing` shows a refused visitor spends none of the link's downloads.

Over HTTP, `serve` records the same file and prints its link:

```sh
go run ./records-share serve
```

```text
http://localhost:8090/s/ppUfyp4izGwN5Q6ahCB55akxFyCd_wk0xE-CMImU33I
```

Opening the link spends a download and redirects to a signed link that is not cached or leaked through the referrer:

```sh
L=http://localhost:8090/s/ppUfyp4izGwN5Q6ahCB55akxFyCd_wk0xE-CMImU33I
curl -s -i "$L"
```

```text
HTTP/1.1 302 Found
Cache-Control: no-store
Content-Type: text/html; charset=utf-8
Location: http://localhost:8090/files/media/docs/4e6kamed3dpwmd2tytsw7hwwxu.txt?cd=attachment%3B+filename%3D%22contract.txt%22%3B+filename%2A%3DUTF-8%27%27contract.txt&ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791024840&kid=k1&m=GET&sig=68e63ae01e51e4b25af7ea7e356ea32e3282cc09becd08dae3d122494da0fa77
Referrer-Policy: no-referrer
Date: Sat, 03 Oct 2026 10:49:00 GMT
Content-Length: 332
```

Following the `Location` downloads the file through `fshttp.Signed`, as an attachment, sandboxed:

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Disposition: attachment; filename="contract.txt"; filename*=UTF-8''contract.txt
Content-Length: 22
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/plain; charset=utf-8
Etag: "6ae3daeb2b1f75fb41f74e1db0a8e377"
Last-Modified: Sat, 03 Oct 2026 10:49:00 GMT
X-Content-Type-Options: nosniff
Date: Sat, 03 Oct 2026 10:49:00 GMT

signed by both parties
```

A second request to `$L` spends the second download. After that the link is used up:

```sh
curl -s -i "$L"
```

```text
HTTP/1.1 404 Not Found
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Date: Sat, 03 Oct 2026 10:49:01 GMT
Content-Length: 10

Not Found
```

## The code

Installing the plugin; `Migrate` creates `fs_shares`:

<!-- ds:block id=newstore-ndhmkyzh -->
**newStore** · [`examples/records-share/main.go:100-110`](main.go#L100-L110)

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

Making a link, whose token is shown now or never:

<!-- ds:block id=sharefile-s3tjks9n -->
**shareFile** · [`examples/records-share/main.go:121-123`](main.go#L121-L123)

```go
func shareFile(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) (share.Share, error) {
	return p.Create(ctx, s, tenant, fileID, share.Options{TTL: linkTTL, MaxDownloads: linkDownloads})
}
```
<!-- /ds:block hash=b08c90 -->

Spending a download, listing a file's links, and revoking one:

<!-- ds:block id=open-zpzbgxf2 -->
**open** · [`examples/records-share/main.go:131-133`](main.go#L131-L133)

```go
func open(ctx context.Context, s *records.Store, p *share.Plugin, token string) (records.File, error) {
	return p.Resolve(ctx, s, token)
}
```
<!-- /ds:block hash=1c4257 -->

<!-- ds:block id=linksof-czvcpu4u -->
**linksOf** · [`examples/records-share/main.go:138-140`](main.go#L138-L140)

```go
func linksOf(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) ([]share.Share, error) {
	return p.Shares(ctx, s, tenant, fileID)
}
```
<!-- /ds:block hash=de139a -->

<!-- ds:block id=stopsharing-y4m2w3yj -->
**stopSharing** · [`examples/records-share/main.go:144-146`](main.go#L144-L146)

```go
func stopSharing(ctx context.Context, s *records.Store, p *share.Plugin, shareID string) error {
	return p.Revoke(ctx, s, tenant, shareID)
}
```
<!-- /ds:block hash=b1d2b2 -->

Every unusable link fails the same way, and a purge deletes a file's links:

<!-- ds:block id=unusable-2jbw3fmt -->
**unusable** · [`examples/records-share/main.go:220-252`](main.go#L220-L252)

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

The share handler and the signed-download handler its redirects point at:

<!-- ds:block id=newmux-6ne3q9m6 -->
**newMux** · [`examples/records-share/main.go:151-156`](main.go#L151-L156)

```go
func newMux(s *records.Store, p *share.Plugin, disks *filesystem.Manager, signer *signedurl.Signer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(sharePrefix, http.StripPrefix(sharePrefix, share.Handler(s, p)))
	mux.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(disks, signer)))
	return mux
}
```
<!-- /ds:block hash=016e41 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the plugin, links, downloads, revocation, the walk-through and the `serve` mode |
| `collections.go` | Passwords, folder links, file request links and link statistics, on a second Store with the folders plugin |
| `policy.go` | The handler with an access check, a branded delivery page, the app's own error pages and a per-link upload policy; code: [`membersOnly`](ds:block?id=membersonly-z5h8c3qr), [`brandedPage`](ds:block?id=brandedpage-k2w9t6vn), [`linkErrors`](ds:block?id=linkerrors-q8m3x7bd), [`portalPolicy`](ds:block?id=portalpolicy-n7v4c2xs) |
| `main_test.go` | Runs the walk-through, plus the handler redirecting until the link is used up, an expired link, only the hash being stored, the lifetime cap, and a refused visitor spending nothing |

## Next

- The guide: [Share links](../../docs/plugins/share.md).
- Related examples: [recipe-private-downloads](../recipe-private-downloads) for signed downloads, [recipe-team-drive](../recipe-team-drive), [records-folders](../records-folders).
