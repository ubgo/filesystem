# HTTP endpoints

`recordshttp` serves the records layer over `net/http`: upload files and attach them to an owner, list an owner's files with links, reorder and detach them, make an existing file an owner's only one (a new avatar), redirect to a file, replace its content, make it public or private, move it to the trash, restore it and purge it. It is the endpoint set most apps write by hand around an avatar, a gallery or a message's attachments, written once. Use it when your frontend uploads through your own server; skip it if you already have an API layer and call the [Store](README.md) from it instead.

The handler is a plain `http.Handler`, so it mounts under `net/http`, chi, gorilla, gin's `WrapH` or echo's `WrapHandler`.

## Before you start

- A Store with its owner types registered, as in [Files](files.md) and [Attachments](attachments.md). `recordshttp` adds no tables.
- The example is [`examples/records-http`](../../examples/records-http): a SQLite Store over a memory disk, the records endpoints under `/media`, and `fshttp.Signed` under `/files` so the redirect of `GET /media/files/{id}` leads to bytes. `go run ./records-http` in `examples/` serves it on `localhost:8094`; `go test ./records-http` runs the flows below against `httptest`.
- The library has no notion of users. You supply two hooks: `Scope` (which tenant the request acts in) and `Authorize` (whether it may do this). Both are required; `Handler` refuses a config without them, so there is no way to mount the endpoints unprotected by accident.

## Step 1 — Decide the upload policy

Uploads go through `upload.Receive`, so the type is decided from the bytes, the size is enforced while streaming, and active content (HTML, SVG, scripts) is refused unless the policy lifts it. The demo accepts images and plain text up to 64 KiB, stored under random keys:

<!-- ds:block id=policy-wvgc7kq3 -->
**policy** · [`examples/records-http/main.go:67-71`](../../examples/records-http/main.go#L67-L71)

```go
var policy = upload.Policy{
	MaxSize: maxFileSize,
	Allowed: []string{"image/*", "text/plain"},
	Key:     upload.RandomKey("uploads"),
}
```
<!-- /ds:block hash=523b37 -->

`Handler` calls `Policy.Validate`, so a policy without `MaxSize` or `Allowed` fails at start-up, not at the first upload.

The disk must be able to hand out a link, because the list route returns one per file and `GET /files/{id}` redirects to one. A bucket presigns by itself; a local or memory disk needs a URL signer and the `fshttp.Signed` handler mounted where the signer's base URL points:

<!-- ds:block id=newdisks-y73hs4nf -->
**newDisks** · [`examples/records-http/main.go:76-87`](../../examples/records-http/main.go#L76-L87)

```go
func newDisks() (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(baseURL+"/files", []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, signer, err
}
```
<!-- /ds:block hash=568c80 -->

<!-- ds:block id=newstore-peccqhpr -->
**newStore** · [`examples/records-http/main.go:103-112`](../../examples/records-http/main.go#L103-L112)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=7d9526 -->

## Step 2 — Write the two hooks

`Scope` maps a request to the scope it may act in. Take it from the session, never from the URL or a parameter the client chooses: every query the handler makes filters by it, so a scope from the client is a cross-tenant leak. An error answers `401`.

<!-- ds:block id=scopeof-kgn4xeh9 -->
**scopeOf** · [`examples/records-http/main.go:123-128`](../../examples/records-http/main.go#L123-L128)

```go
func scopeOf(r *http.Request) (string, error) {
	if t := r.Header.Get(headerTenant); t != "" {
		return t, nil
	}
	return "", errNoSession
}
```
<!-- /ds:block hash=c333dd -->

When your authentication middleware already knows the scope, it does not need a lookup of its own: the middleware puts it on the request's context with `recordshttp.WithScope(ctx, scope)`, and `Config.Scope` is the ready `recordshttp.ScopeFromRequest`, which reads it back. A request that reaches the handler without one (a route mounted outside the middleware) is `401`, never the `""` scope; `""` is a scope like any other only when set on purpose. `recordshttp.ScopeFromContext(ctx)` returns the scope and whether one was set, for your own handlers. The framework adapters (gin, echo, fiber, chi) use this same key, so a scope set once works for all of them.

```go
// In the authentication middleware, once the session is known:
r = r.WithContext(recordshttp.WithScope(r.Context(), session.TenantID))
// In the config:
cfg.Scope = recordshttp.ScopeFromRequest
```

`Authorize(r, action, target)` decides whether the request may perform `action` on `target`. An error answers `403`; its message is never sent to the client, so a refusal cannot leak policy details. On every route that names a file, `target.File` is the file the handler loaded, so the rule can look at `UploadedBy`, `Visibility` or `DeletedAt` without a query of its own. The demo's rule: a user may touch only their own owner row (`user/<their id>`) and only files they uploaded. It reads `t.File.UploadedBy`, so it makes no query of its own.

The file in `target.File` is what the handler read before calling your rule, not a lock on it. Base a rule on fields that cannot change under you without that being caught: the purge route re-checks the trash state you saw (see Step 7), and every other route acts on the current row through the Store, which applies its own checks.

<!-- ds:block id=authorizer-uejc9mcn -->
**authorizer** · [`examples/records-http/main.go:138-157`](../../examples/records-http/main.go#L138-L157)

```go
func authorizer() func(*http.Request, recordshttp.Action, recordshttp.Target) error {
	return func(r *http.Request, action recordshttp.Action, t recordshttp.Target) error {
		user := r.Header.Get(headerUser)
		if user == "" {
			return errNotYours
		}
		if t.Owner.Type != "" {
			if t.Owner.Type != ownerUser || t.Owner.ID != user {
				return errNotYours
			}
			return nil
		}
		// recordshttp loads the file in the request's scope before calling
		// this, so another tenant's file never reaches here (it is a 404).
		if t.File == nil || t.File.UploadedBy != actorPrefix+user {
			return errNotYours
		}
		return nil
	}
}
```
<!-- /ds:block hash=340a13 -->

What the hook receives per route:

| Route | `Action` | `Target` fields set |
|---|---|---|
| `POST /owners/{type}/{id}/{role}` | `upload` | `Scope`, `Owner`, `Role` |
| `GET /owners/{type}/{id}/{role}` | `list` | `Scope`, `Owner`, `Role` |
| `PUT /owners/{type}/{id}/{role}/order` | `reorder` | `Scope`, `Owner`, `Role` |
| `PUT /owners/{type}/{id}/{role}` | `set_attachment`, then `read` | `Scope`, `Owner`, `Role`, `FileID`, `File`, `Change.Previous`; then `Scope`, `FileID`, `File` |
| `DELETE /attachments/{id}` | `detach` | `Scope`, `Owner`, `Role`, `FileID`, all loaded from the attachment |
| `GET /files/{id}` | `read` | `Scope`, `FileID`, `File` |
| `PUT /files/{id}` | `replace` | `Scope`, `FileID`, `File` |
| `DELETE /files/{id}` | `trash` | `Scope`, `FileID`, `File` |
| `POST /files/{id}/restore` | `restore` | `Scope`, `FileID`, `File` |
| `POST /files/{id}/purge` | `purge` | `Scope`, `FileID`, `File` |
| `PUT /files/{id}/visibility` | `set_visibility` | `Scope`, `FileID`, `File`, `Change.Visibility` |
| `PUT /files/{id}/text` | `set_text` | `Scope`, `FileID`, `File`, `Change.Text` |
| `GET /files/{id}/status` | `read` | `Scope`, `FileID`, `File` |

The owner and role in the URL are passed to the hook before anything is checked against the Store: an unregistered owner type or a malformed role reaches `Authorize` first, and the Store refuses it afterwards with `400`. A file is loaded before the hook runs, in the request's scope: a file of another scope, or one that does not exist, is `404` and never reaches `Authorize`, so another tenant cannot tell the two apart. Inside the scope, a file that exists but is not yours is `403`; IDs are random 128-bit values, so that tells a probe nothing it can enumerate. The detach route loads the attachment the same way (a `404` when it is not in the scope) and passes its owner, role and file ID.

`Target.Change` is the change the request asks for, read from its body (capped at 1 MB) before `Authorize`, so a rule can tell publishing from unpublishing: "only admins publish" is a check of `t.Change.Visibility`. It is nil on every other route, and nil on the visibility and text routes when the body cannot be read; such a request is still `403` when the rule refuses it, and `400` only once the rule allows it.

```go
cfg.Authorize = func(r *http.Request, a recordshttp.Action, t recordshttp.Target) error {
	if a == recordshttp.ActionSetVisibility && t.Change != nil && t.Change.Visibility == filesystem.VisibilityPublic && !isAdmin(r) {
		return errors.New("only admins publish") // your own role check
	}
	return allowOwnFiles(r, a, t) // the rest of your rule
}
```

The set-attachment route asks twice. Attaching a file hands out a link to it through the owner's list, so being allowed to change the owner is not enough: the request must also be allowed to `read` the file, asked with a target that carries only the file. A rule written for the owner routes alone, like the demo's, therefore cannot let a user attach someone else's file to their own profile and read it from there.

Set the actor once per request, so the Store records who uploaded each file as `UploadedBy` and plugins such as [audit](../plugins/audit.md) log it:

<!-- ds:block id=withactor-5rp58uu3 -->
**withActor** · [`examples/records-http/main.go:162-169`](../../examples/records-http/main.go#L162-L169)

```go
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), actorPrefix+user))
		}
		next.ServeHTTP(w, r)
	})
}
```
<!-- /ds:block hash=398966 -->

## Step 3 — Mount the handler

<!-- ds:block id=newhandler-ytttpkep -->
**newHandler** · [`examples/records-http/main.go:174-191`](../../examples/records-http/main.go#L174-L191)

```go
func newHandler(s *records.Store, disks *filesystem.Manager, signer *signedurl.Signer) (http.Handler, error) {
	media, err := recordshttp.Handler(recordshttp.Config{
		Store:        s,
		Disk:         diskName,
		Policy:       policy,
		MaxFiles:     maxFilesPerRequest,
		Scope:        scopeOf,
		Authorize:    authorizer(),
		LinkLifetime: linkLifetime,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/media/", http.StripPrefix("/media", withActor(media)))
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	return mux, nil
}
```
<!-- /ds:block hash=17e177 -->

The routes are registered without a prefix, so mount the handler with `http.StripPrefix`. `Config` fields:

| Field | Meaning | Default |
|---|---|---|
| `Store` | The records Store | required |
| `Disk` | Name of the disk uploads are stored on (a disk of the Store's manager). Leave it empty to choose per request: with `DiskFor`, or by the policy's visibility when the Store has `WithVisibilityDisks` | required unless `DiskFor` or `WithVisibilityDisks` |
| `DiskFor` | `func(*http.Request, Target) (string, error)`: the disk of each upload request, chosen after `Authorize` and `Resolve` and before any byte of the body is read; `Target` carries the scope, owner and role. Its error refuses the request with the code's status (`500` when uncoded) and nothing is stored; an empty or unknown disk name is a bug in your `DiskFor`: `500` `unknown`. Setting it with `Disk` is refused by `Handler`. The replace route keeps a file on its own disk | `nil` |
| `Policy` | `upload.Policy` for every upload and replacement | required (validated) |
| `Scope` | `func(*http.Request) (string, error)`; error is `401` | required |
| `Authorize` | `func(*http.Request, Action, Target) error`; error is `403` | required |
| `MaxFiles` | Files per upload request | `10` |
| `LinkLifetime` | Lifetime of the signed URLs returned by the list route and redirected to by the file route | `recordshttp.DefaultLinkLifetime`, 10 minutes |
| `Partial` | Uploads store every file they can and report the others in `failed` (see Step 4) | `false`: all files or none |
| `Resolve` | `upload.Resolver`, `func(r, base upload.Policy) (upload.Policy, error)`: the policy of each upload and replace request, chosen after `Authorize` and before any byte of the body is read; the result is validated like `Policy`. Its error refuses the request with the code's status; `unavailable` with a retry delay answers `503` and `Retry-After`. A returned policy that fails validation is a bug in your `Resolve`, not the request's fault: `500` `unknown` with the status text only | `nil`: `Policy` for every request |
| `MaxTotal` | Bytes of all the files of one request together, on top of the per-file `MaxSize`: each file may use only what the earlier ones left, and one that does not fit is `too_large` ("the files of this request exceed N bytes in total"), a failed file under `Partial`. The request body is capped at `MaxTotal` plus form overhead when that is lower than `MaxFiles` × `MaxSize`. Negative is refused by `Handler` | `0`: no total |

Check: `go run ./records-http` prints `listening on http://localhost:8094`.

### Choose the disk per request

`Disk` puts every upload on one disk. Two other models need no second handler. With a Store built `records.WithVisibilityDisks(public, private)`, leave `Disk` empty: each upload goes to the disk of its policy's visibility, so a `Resolve` that makes avatars public sends them to the CDN bucket while documents stay private. For a choice of your own, such as tenants who bring their own bucket, set `DiskFor`:

```go
cfg := recordshttp.Config{
	Store: store, Policy: policy, Scope: scope, Authorize: authorize,
	// Each tenant's uploads go to the disk mounted for its bucket; a
	// tenant without one is refused before any byte is read.
	DiskFor: func(r *http.Request, t recordshttp.Target) (string, error) {
		name, ok := tenantDisks[t.Scope]
		if !ok {
			return "", filesystem.Errorf(filesystem.CodePermission, "no storage configured for this workspace")
		}
		return name, nil
	},
}
```

Every disk `DiskFor` names must be in the Store's manager. Replacing a file keeps it on the disk it is on.

Try it: [`examples/records-http`](../../examples/records-http) runs this section in `perdisk.go` ([`diskFor`](ds:block?id=diskfor-h3v8q2nw)): `go run ./records-http` from `examples/`, then POST a file under `/byo/`.

## Step 4 — Upload and list

`POST /owners/{type}/{id}/{role}` takes `multipart/form-data` with one or more files in the field `file` (`recordshttp.FormField`). Other fields and parts without a file name are skipped. Every file is stored and attached inside one transaction: all of them, or none, with the objects of a rolled-back request deleted again. The answer is `201` with the attachments, in the order they were sent.

```sh
H='-H X-Tenant:acme -H X-User:u1'
curl -s -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@photo.png -F file=@notes.txt
```

```json
{"files":[{"attachment_id":"att_4l7gfbnefutcwvcwnllhhwqbds","file_id":"fil_oundea2i5z7edbyvvtuzgdi4wc","filename":"photo.png","content_type":"image/png","size":74,"position":1,"uploaded_by":"user:u1","status":"ready"},{"attachment_id":"att_ueeuzr36dsajoz3lnp4dzctkwk","file_id":"fil_ilayzw7eqnhojtyzkh3kylgnqz","filename":"notes.txt","content_type":"text/plain; charset=utf-8","size":14,"position":2,"uploaded_by":"user:u1","status":"ready"}]}
```

`GET /owners/{type}/{id}/{role}` lists the live (not trashed) files in position order, each with a `url`: the public URL for a public file on a disk that has one, otherwise a signed GET valid for `LinkLifetime`.

```sh
curl -s localhost:8094/media/owners/user/u1/gallery $H
```

```json
{"files":[{"attachment_id":"att_4l7gfbnefutcwvcwnllhhwqbds","file_id":"fil_oundea2i5z7edbyvvtuzgdi4wc","filename":"photo.png","content_type":"image/png","size":74,"position":1,"uploaded_by":"user:u1","url":"http://localhost:8094/files/media/uploads/7mnpmbnq62ff66boj2ls73ujgg.png?ct=image%2Fpng\u0026exp=1791268032\u0026kid=k1\u0026m=GET\u0026sig=4b455376070231f9fafbd9b48353eb930289d129f1636438e2b58b3f6544c00f","status":"ready"},{"attachment_id":"att_ueeuzr36dsajoz3lnp4dzctkwk","file_id":"fil_ilayzw7eqnhojtyzkh3kylgnqz","filename":"notes.txt","content_type":"text/plain; charset=utf-8","size":14,"position":2,"uploaded_by":"user:u1","url":"http://localhost:8094/files/media/uploads/l3446ye55azocwybeemv4bhqkb.txt?ct=text%2Fplain%3B+charset%3Dutf-8\u0026exp=1791268032\u0026kid=k1\u0026m=GET\u0026sig=4c20176422c38570da53f7fe956e9c3cbcacb7be8e06b3454811e1278585ec20","status":"ready"}]}
```

A gallery of thousands is better read a page at a time: add `?limit=` (`recordshttp.QueryLimit`; 0 or absent is 50, at most 1000) and `?cursor=` (`QueryCursor`) and the answer carries `next_cursor` while more files follow (`records.Store.AttachmentsPage`, keyset on position and ID). Without either parameter the route lists every file, as above. A page leaves out expired files after reading them, so it may hold fewer than `limit` and still have a `next_cursor`.

```sh
curl -s 'localhost:8094/media/owners/user/u1/gallery?limit=1' $H
```

```text
{"files":[{"attachment_id":"att_…","file_id":"fil_…","filename":"photo.png", …}],"next_cursor":"…"}
```

With `Config.Partial` the upload route stores every file it can instead, each in its own transaction, and lists the ones it refused under `failed` (`upload.FileFailure`: `filename`, `error`, `code`), the same contract as `upload.HandlerOptions.Partial`. Use it for bulk uploads where one bad file should not sink the rest; leave it off for a form whose files belong together. Use `Resolve` for limits that depend on who is asking (a 5 MB free plan and a 100 MB paid one, a stricter type list for one role) or to refuse early (a used-up quota, a rate limit), and `MaxTotal` to bound a whole bulk upload; `MinSize` applies to every file as it does through `upload.Receive`. The status is `201` when every file was attached, `200` when some were, and the first failure's status when none was (with `files` empty and every failure listed):

```json
{"files":[{"attachment_id":"att_…","file_id":"fil_…","filename":"a.txt","content_type":"text/plain; charset=utf-8","size":1,"position":1}],"failed":[{"filename":"page.html","error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}]}
```

A failure of the request itself (not multipart, malformed, more than `MaxFiles` files, over the request cap) still fails it, and the files it already attached are purged first, so a client that only sees the error is not left with files it does not know about. When a refused file failed with a backend that asked for a wait (`unavailable` with a retry delay), the answer carries the longest such wait as `Retry-After`, so a client knows when to retry the files in `failed`.

Check: the storage key is `uploads/<random>.png`, never the client's name; the client's name is kept only as `filename`. `uploaded_by` is omitted when no actor was set.

The JSON shape of one file (`recordshttp.AttachedFile`):

| Field | From |
|---|---|
| `attachment_id` | `Attachment.ID` (`att_…`) |
| `file_id` | `File.ID` (`fil_…`) |
| `filename` | `File.Filename`, the cleaned client name |
| `content_type` | detected from the bytes |
| `size` | bytes |
| `position` | `Attachment.Position`, from 1 |
| `uploaded_by` | `File.UploadedBy`; omitted when empty |
| `url` | list route only; omitted on upload |
| `status`, `status_detail` | `File.Status` and `File.StatusDetail` ([Processing status](processing-status.md)); the detail is omitted when empty |

## Step 5 — Reorder, detach and set the only file

`PUT /owners/{type}/{id}/{role}/order` takes `{"ids": [...]}`: every attachment ID of that owner and role exactly once, in the new order. Anything else changes nothing and is `400`, so a client working from a stale list cannot drop an item. The body is capped at 1 MiB and unknown fields are refused.

```sh
curl -s -i -X PUT localhost:8094/media/owners/user/u1/gallery/order $H -d '{"ids":["att_ueeuzr36dsajoz3lnp4dzctkwk","att_4l7gfbnefutcwvcwnllhhwqbds"]}' | head -1
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/owners/user/u1/gallery/order $H -d '{"ids":["att_ueeuzr36dsajoz3lnp4dzctkwk"]}'
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/owners/user/u1/gallery/order $H -d '{"order":[]}'
```

```
HTTP/1.1 204 No Content
{"error":"records: reorder must list each of the 2 attachments exactly once","code":"invalid_argument"}
 400
{"error":"invalid JSON body","code":"invalid_argument"}
 400
```

`DELETE /attachments/{id}` removes the attachment and keeps the file (it may be attached elsewhere; [Lifecycle](lifecycle.md) covers deleting files). An attachment of another scope is `404`, the same as an unknown one.

```sh
curl -s -i -X DELETE localhost:8094/media/attachments/att_4l7gfbnefutcwvcwnllhhwqbds $H | head -1
curl -s -w ' %{http_code}\n' -X DELETE localhost:8094/media/attachments/att_ueeuzr36dsajoz3lnp4dzctkwk -H X-Tenant:globex -H X-User:u1
```

```
HTTP/1.1 204 No Content
{"error":"Not Found","code":"not_found"}
 404
```

`PUT /owners/{type}/{id}/{role}` makes an existing file the only one of that owner and role with `Store.SetAttachment`: the user's new avatar replacing the old, in one transaction. The body is `{"file_id": "fil_…", "previous": "keep"}`; `previous` says what happens to the files it detaches (`keep`, `trash` or `purge`, applied only to files no other attachment uses) and defaults to `keep`, so a client that does not say loses nothing. The answer is `200` with the attachment (`recordshttp.AttachedFile`, without a URL). The usual flow is an upload to a scratch role or another owner, then this call; on a [single role](attachments.md) it is the only way to change the file, since the upload route refuses a second one with `409`.

```sh
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/owners/user/u1/avatar $H -d '{"file_id":"fil_fbiolgouc4gp4gq636rlbg6ttp","previous":"trash"}'
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/owners/user/u2/avatar -H X-Tenant:acme -H X-User:u2 -d '{"file_id":"fil_fbiolgouc4gp4gq636rlbg6ttp"}'
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/owners/user/u1/avatar -H X-Tenant:globex -H X-User:u1 -d '{"file_id":"fil_fbiolgouc4gp4gq636rlbg6ttp"}'
```

```
{"attachment_id":"att_zfgm5j6pkhxo3ga2aenpmfjaad","file_id":"fil_fbiolgouc4gp4gq636rlbg6ttp","filename":"avatar.png","content_type":"image/png","size":25,"position":1,"uploaded_by":"user:u1","status":"ready"}
 200
{"error":"forbidden","code":"permission"}
 403
{"error":"Not Found","code":"not_found"}
 404
```

Check: `u2` owns `user/u2`, so the demo's rule allows `set_attachment`; the `read` check on a file `u1` uploaded refuses it. In another tenant the file does not exist.

## Step 6 — Read, replace and trash a file

`GET /files/{id}` answers `302` to the file's URL (the same choice as the list route) with `Cache-Control: private, no-store`, so a browser or CDN never keeps a redirect to a link that expires. Use it as an `<img src>` or a download link that stays valid as long as the page does.

```sh
curl -s -i localhost:8094/media/files/fil_ilayzw7eqnhojtyzkh3kylgnqz $H
```

```
HTTP/1.1 302 Found
Cache-Control: private, no-store
Content-Type: text/html; charset=utf-8
Location: http://localhost:8094/files/media/uploads/l3446ye55azocwybeemv4bhqkb.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791268032&kid=k1&m=GET&sig=4c20176422c38570da53f7fe956e9c3cbcacb7be8e06b3454811e1278585ec20
Date: Tue, 06 Oct 2026 06:22:12 GMT
Content-Length: 243

<a href="http://localhost:8094/files/media/uploads/l3446ye55azocwybeemv4bhqkb.txt?ct=text%2Fplain%3B+charset%3Dutf-8&amp;exp=1791268032&amp;kid=k1&amp;m=GET&amp;sig=4c20176422c38570da53f7fe956e9c3cbcacb7be8e06b3454811e1278585ec20">Found</a>.
```

Check: `curl -s -L` on the same URL prints `meeting notes`, served by `fshttp.Signed`.

`PUT /files/{id}` takes one file in the field `file` and makes it the file's new content with `Store.Replace`: the file keeps its ID and attachments, gets the uploaded name, and keeps its visibility whatever the policy says. The old object is deleted after the commit, unless the [versions](../plugins/versions.md) plugin keeps it. Only the first file part is used. The answer is `200` with `recordshttp.FileResponse`:

```sh
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/files/fil_ilayzw7eqnhojtyzkh3kylgnqz $H -F file=@notes-v2.txt
```

```
{"file_id":"fil_ilayzw7eqnhojtyzkh3kylgnqz","filename":"notes-v2.txt","content_type":"text/plain; charset=utf-8","size":18}
 200
```

Check: following the redirect again prints `meeting notes, v2`.

`DELETE /files/{id}` moves the file to the trash with `Store.Trash` (`204`). It drops out of every list at once, and `GET /files/{id}` answers `404` for it (after `Authorize`, so only a request allowed to read the file learns it is trashed); it stays restorable until it is purged (see [Lifecycle](lifecycle.md)).

```sh
curl -s -i -X DELETE localhost:8094/media/files/fil_ilayzw7eqnhojtyzkh3kylgnqz $H | head -1
curl -s -w ' %{http_code}\n' localhost:8094/media/files/fil_ilayzw7eqnhojtyzkh3kylgnqz $H
```

```
HTTP/1.1 204 No Content
{"error":"Not Found","code":"not_found"}
 404
```

## Save only what you read: If-Match

The file's ETag is its SHA-256 in quotes; `PUT /files/{id}` answers the new one in its `ETag` header. Send it back in `If-Match` on the next `PUT /files/{id}`, `PUT /files/{id}/visibility`, `DELETE /files/{id}`, `restore` or `purge`. When the content has changed since, the route answers `412` with code `conflict` and changes nothing, so two editors cannot silently overwrite each other's save. Every one of these routes passes it to the Store as `records.IfUnchanged`, which checks it again under the file's row lock, so a change landing between the route's check and the write is refused too (`412`). `If-Match: *` or no header means no condition.

## Step 7 — Restore, purge and change visibility

`POST /files/{id}/restore` takes the file out of the trash with `Store.Restore`, and `POST /files/{id}/purge` deletes it for good with `Store.Purge`: its attachments and row in one transaction, its object after the commit. Both answer `204`; restoring a live file does nothing. Purge works on a live file too: whether that is allowed is your rule's call, and `target.File.DeletedAt` tells it whether the file is in the trash. The file is loaded before `Authorize` runs, so a `Restore` or `Trash` could land between your rule's decision and the purge; the route purges with `records.IfTrashed` set to the state your rule saw, and if that state changed it answers `503` `unavailable` and purges nothing. Most apps allow `restore` to the uploader and `purge` only to admins, or not at all over HTTP, leaving it to `SweepTrash`.

`PUT /files/{id}/visibility` takes `{"visibility": "public"}` or `"private"` and calls `Store.SetVisibility` (`204`): the object changes access or moves disk first, then the row (see [Replace and visibility](replace-and-visibility.md)). The body is read only after `Authorize` allowed the request.

```sh
curl -s -i -X PUT localhost:8094/media/files/fil_agiosc3lbwdqxsct4qjhba3yjv/visibility $H -d '{"visibility":"public"}' | head -1
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/files/fil_agiosc3lbwdqxsct4qjhba3yjv/visibility $H -d '{"visibility":"secret"}'
curl -s -i -X DELETE localhost:8094/media/files/fil_agiosc3lbwdqxsct4qjhba3yjv $H | head -1
curl -s -i -X POST localhost:8094/media/files/fil_agiosc3lbwdqxsct4qjhba3yjv/restore $H | head -1
curl -s -i -X POST localhost:8094/media/files/fil_agiosc3lbwdqxsct4qjhba3yjv/purge $H | head -1
curl -s -w ' %{http_code}\n' localhost:8094/media/files/fil_agiosc3lbwdqxsct4qjhba3yjv $H
```

```
HTTP/1.1 204 No Content
{"error":"records: unknown visibility \"secret\"","code":"invalid_argument"}
 400
HTTP/1.1 204 No Content
HTTP/1.1 204 No Content
HTTP/1.1 204 No Content
{"error":"Not Found","code":"not_found"}
 404
```

`PUT /files/{id}/text` takes `recordshttp.TextRequest`, `{"alt": "…", "caption": "…", "title": "…"}`, and calls `Store.SetText` (`204`). The body is the file's whole describing text: a field that is absent or `""` is removed. Text over its limit, invalid UTF-8 or with control characters (a caption may hold line breaks) is `400` `invalid_argument`; a concurrent change to the file's metadata is `503` `unavailable`, to retry. The list route returns the text as `alt`, `caption` and `title` on each `AttachedFile`, omitted when empty. Like the visibility route, the body is read only after `Authorize` allowed the request, for the action `set_text`.

`GET /files/{id}/status` answers `recordshttp.StatusResponse`, `{"file_id": "fil_…", "status": "processing", "status_detail": "thumbnails 1/2"}`, with `Cache-Control: no-store`: the route a client polls while a file is processed (thumbnails, a scan) until `status` is `ready` or `failed`. It is authorized as `read`, since a client allowed to fetch the file may watch it, and a trashed file is `404` as on the read route. The list route returns `status` (and `status_detail` when set) on each `AttachedFile` too. See [Processing status](processing-status.md).

With `Config.Quarantine` every file the upload route stores, and every new content the replace route stores, is recorded `quarantined` until a worker scans it (`records.Store.Scan`): meanwhile `GET /files/{id}` answers `409` with code `conflict`, the list route returns the file without a `url`, setting it public is `409`, and the status route reports `quarantined`. `Config.Policy` must then not be public. See [Quarantine until scanned](processing-status.md#quarantine-until-scanned).

## Step 8 — Errors

Every failure is JSON (`recordshttp.ErrorResponse`): `error` is a message and `code` the `filesystem.Code`. The status comes from `fshttp.StatusOf`. The message is `fshttp.ClientMessage(err)`, the rule every handler in the library shares: the real message only for codes that describe the client's own input (`too_large`, `rejected`, `quota_exceeded`, `invalid_argument`, `invalid_key`). Every other code (a database or backend failure, `not_found`, `exists`) gets the status text, so internals never leak; a `409` answers `Conflict` with the code `exists`. When a visible error wraps another `filesystem.Error` (a Disk error, whose text names the disk and key), the inner one is judged by the same rule, so a storage key never reaches the client; a request cap tripped while a file is being written answers `http: request body too large`. When the backend asked for a wait (`filesystem.Error.RetryAfter`, typically on a `503` `unavailable`), the answer carries it as a `Retry-After` header in whole seconds.

```sh
curl -s -w ' %{http_code}\n' localhost:8094/media/owners/user/u1/gallery
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery -H X-Tenant:acme -H X-User:u2 -F file=@notes.txt
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/files/fil_oundea2i5z7edbyvvtuzgdi4wc -H X-Tenant:acme -H X-User:u2 -F file=@notes.txt
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@page.html
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@big.txt
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@notes.txt -F file=@notes.txt -F file=@notes.txt -F file=@notes.txt
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/Gallery $H -F file=@notes.txt
```

```
{"error":"not signed in","code":"permission"}
 401
{"error":"forbidden","code":"permission"}
 403
{"error":"forbidden","code":"permission"}
 403
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
 415
{"error":"body exceeds 65536 bytes","code":"too_large"}
 413
{"error":"more than 3 files","code":"invalid_argument"}
 400
{"error":"records: role \"Gallery\" must match ^[a-z][a-z0-9_.-]{0,63}$","code":"invalid_argument"}
 400
```

Check: after the four-file request the gallery is still empty (`{"files":[]}`): the three files stored before the refusal were rolled back with their objects.

| Status | Code | When |
|---|---|---|
| 400 | `invalid_argument` | Not multipart, no file in `file`, more than `MaxFiles` files, a malformed JSON body or ID list, an unregistered owner type, a malformed role, a missing `file_id`, an unknown `previous` or visibility |
| 401 | `permission` | `Scope` returned an error |
| 403 | `permission` | `Authorize` returned an error |
| 404 | `not_found` | Unknown file, a trashed file on the read and set-attachment routes, unknown attachment, anything in another scope |
| 409 | `exists` | A [single role](attachments.md) (`WithSingleRoles`) already holds a file |
| 409 | `conflict` | The file is quarantined until its scan passes (read, or set public) |
| 413 | `too_large` | A file over `Policy.MaxSize`, or a request over `MaxFiles × MaxSize + 64 KiB` |
| 415 | `rejected` | A type the policy refuses, active content, a failed `Inspect` (virus scan) |
| 507 | `quota_exceeded` | The [quota](../plugins/quota.md) plugin refused the upload or the growth of a replacement |
| 503 | `unavailable` | The backend is unavailable or throttling; carries `Retry-After` when it asked for a wait |
| 501 | `unsupported` | The disk cannot build a link (no presign, no URL signer), or cannot change one object's visibility |
| 500 | `unknown` | Anything else, such as a database failure |

### Your own error envelope

The JSON above is the default. `Config.Errors` takes an `fshttp.ErrorWriter`, the type every handler of the library accepts, and answers every failure on this table in your API's shape instead: RFC 9457 problem+json, a `{data, errors}` envelope, an HTML page. It receives the status the handler would send and the error, which is for your logs (the 5xx's cause, the `Scope` or `Authorize` error behind a `401` or `403`); text for the client comes from `fshttp.ClientMessage(err)`, which keeps internals out. `Retry-After` is already set when the backend asked for one. The writer may change the status, for instance to answer a quota with `402` instead of `507`.

```go
cfg.Errors = func(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status >= 500 {
		slog.ErrorContext(r.Context(), "media request failed", "status", status, "err", err)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "https://example.com/problems/" + string(filesystem.CodeOf(err)), "status": status,
		"title": http.StatusText(status), "detail": fshttp.ClientMessage(err),
	})
}
```

The answer of a `Partial` upload that refused some files is a `ListResponse`, not an error, and does not pass through it.

### Your own success envelope

The bodies of successful requests are the JSON shapes above by default. `Config.Respond` takes a `recordshttp.SuccessWriter` and writes them in your API's shape instead: a `{"data": …}` envelope, camelCase keys, or fields of your own beside them. It receives the status the handler would send and the body, a `recordshttp.SuccessBody`: one of `ListResponse` (upload, list, and a `Partial` upload's answer), `AttachedFile` (set the only file), `FileResponse` (replace) and `StatusResponse` (status). The interface is closed, so a type switch over those four covers every body. Each `AttachedFile`, `FileResponse` and `StatusResponse` carries the `records.File` behind it in `File`, never encoded, so a gallery can add image dimensions, metadata or variant URLs without one `Store.File` lookup per row. Headers the handler sets (`ETag` on replace, `Retry-After` on a partial upload) are already on `w`. A `204` has no body and does not pass here; errors go to `Config.Errors`.

```go
cfg.Respond = func(w http.ResponseWriter, r *http.Request, status int, body recordshttp.SuccessBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
}
```

A gallery listing with each image's size, from the metadata the [dimensions plugin](../plugins/dimensions.md) keeps:

```go
type galleryItem struct {
	recordshttp.AttachedFile
	Width  string `json:"width,omitempty"`
	Height string `json:"height,omitempty"`
}

cfg.Respond = func(w http.ResponseWriter, r *http.Request, status int, body recordshttp.SuccessBody) {
	list, ok := body.(recordshttp.ListResponse)
	if !ok {
		recordshttp.JSONSuccess(w, r, status, body)
		return
	}
	items := make([]galleryItem, 0, len(list.Files))
	for _, af := range list.Files {
		items = append(items, galleryItem{AttachedFile: af, Width: af.File.Metadata[dimensions.MetaWidth], Height: af.File.Metadata[dimensions.MetaHeight]})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"files": items, "next_cursor": list.NextCursor})
}
```

Without it the answers are `recordshttp.JSONSuccess`'s, byte for byte the shapes documented on this page.

Try it: [`examples/concepts-uploads`](../../examples/concepts-uploads) runs this section in `envelope.go` ([`envelopeHandlers`](ds:block?id=envelopehandlers-h2m8v4xc)): `go run ./concepts-uploads` from `examples/`.

## What can go wrong

- `recordshttp: Store, Scope and Authorize are required` from `Handler` — one of them is nil. There is no "allow everything" default; pass a hook that returns nil if you really mean it.
- `recordshttp: Disk is required unless DiskFor is set or the Store has WithVisibilityDisks` from `Handler` — nothing says where uploads go. `recordshttp: set Disk or DiskFor, not both` — one would silently override the other.
- `404` on every route — the handler is mounted without `http.StripPrefix`, so it sees `/media/owners/...` instead of `/owners/...`.
- `501` with code `unsupported` on the list route or `GET /files/{id}` — the disk cannot sign URLs: give a local or memory disk `filesystem.WithURLSigner` and mount `fshttp.Signed`.
- Redirects to a host the browser cannot reach — the signer's base URL is wrong for the environment (it is the public origin where `fshttp.Signed` is mounted).
- A single role refuses a second upload with `409` — the upload route only attaches. Upload the new file elsewhere and make it the role's file with `PUT /owners/{type}/{id}/{role}` (Step 5), or change the content of the current one with `PUT /files/{id}`.
- `403` on set-attachment for a user who may change the owner — the request must also be allowed to `read` the file (Step 2); a rule that knows only owner targets and refuses anything else refuses that second call.

## Next

- [Attachments](attachments.md): what the upload, reorder and set-attachment routes call, and single roles.
- [Replace and visibility](replace-and-visibility.md): what `PUT /files/{id}` does to versions, variants and quotas.
- [Serving files](../concepts/serving-files.md): signed URLs, public URLs and `fshttp.Signed`.
- [Uploads](../concepts/uploads.md): the policy fields and how types are detected.
- The [recordshttp README](../../records/recordshttp/README.md) is the reference for the package.
