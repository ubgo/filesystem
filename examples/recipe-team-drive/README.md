# Team drive: folders, versions, share links, quota and activity

The core of a team drive (the shape of Dropbox, Google Drive or a project tool's file tab): each workspace has a storage quota from its plan, files live in folders, replacing a file keeps its previous contents as versions that can be restored, files are shared outside the workspace through expiring links, and every change lands in an activity feed with who made it. It is `records` with five plugins installed together, `quota`, `folders`, `versions`, `share` and `audit`, over a local disk whose share downloads go through `signedurl` and `fshttp.Signed`. The guide is [Team drive](../../docs/recipes/team-drive.md).

## Run it

```sh
cd examples
go run ./recipe-team-drive
```

It creates a new temporary directory (printed at start-up) holding `files/` (the disk) and `drive.db` (SQLite with the `fs_` tables and each plugin's tables), and listens on `http://localhost:8110`. The signed-in workspace and user are the `X-Workspace` and `X-User` headers (demo only). The workspace `acme` is on the team plan (100 GiB); every other workspace is on the free plan, deliberately tiny at 1 MiB so the demo can hit it. Each file keeps five versions. The signing secret is a fixed demo value in `main`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory.

Run the tests with `go test ./recipe-team-drive`.

## Walkthrough

`offer.txt` holds `price: 100` (11 bytes) and `offer-v2.txt` holds `price: 120, payable in 30 days` (31 bytes).

1. Alice creates a folder in `studio` and uploads into it. Another workspace asking for that folder gets `404`, and a second folder with the same name in the same parent is `409`:

```sh
curl -s -X POST localhost:8110/folders -H 'X-Workspace: studio' -H 'X-User: alice' -d '{"parent":"","name":"Contracts"}'
curl -s -X POST 'localhost:8110/files?folder=fld_ecay3a2uz3wx3ipkq5u5q67ggf' -H 'X-Workspace: studio' -H 'X-User: alice' -F file=@offer.txt
curl -s localhost:8110/folders/fld_ecay3a2uz3wx3ipkq5u5q67ggf/files -H 'X-Workspace: studio'
curl -s localhost:8110/folders/fld_ecay3a2uz3wx3ipkq5u5q67ggf/files -H 'X-Workspace: other'
curl -s -w '%{http_code}\n' -X POST localhost:8110/folders -H 'X-Workspace: studio' -H 'X-User: alice' -d '{"parent":"","name":"Contracts"}'
```

```text
{"id":"fld_ecay3a2uz3wx3ipkq5u5q67ggf","parent":"","name":"Contracts"}
{"id":"fil_szmo2iutqwix76wvdcll7wvlpe","filename":"offer.txt","size":11,"uploaded_by":"user:alice"}
[{"id":"fil_szmo2iutqwix76wvdcll7wvlpe","filename":"offer.txt","size":11,"uploaded_by":"user:alice"}]
{"error":"Not Found","code":"not_found"}
{"error":"Conflict","code":"exists"}
409
```

2. Bob replaces the file; the original becomes version 1. Alice restores it, and the content bob wrote becomes version 2, so the restore can be undone:

```sh
curl -s -X PUT localhost:8110/files/fil_szmo2iutqwix76wvdcll7wvlpe -H 'X-Workspace: studio' -H 'X-User: bob' -F file=@offer-v2.txt
curl -s localhost:8110/files/fil_szmo2iutqwix76wvdcll7wvlpe/versions -H 'X-Workspace: studio'
curl -s -X POST localhost:8110/versions/ver_wtkisc4gkobtu5aqsmsl7mrg2w/restore -H 'X-Workspace: studio' -H 'X-User: alice'
curl -s localhost:8110/files/fil_szmo2iutqwix76wvdcll7wvlpe/versions -H 'X-Workspace: studio'
```

```text
{"id":"fil_szmo2iutqwix76wvdcll7wvlpe","filename":"offer-v2.txt","size":31,"uploaded_by":"user:alice"}
[{"id":"ver_wtkisc4gkobtu5aqsmsl7mrg2w","number":1,"filename":"offer.txt","size":11,"replaced_at":"2026-10-06T03:16:22.141924Z"}]
{"id":"fil_szmo2iutqwix76wvdcll7wvlpe","filename":"offer.txt","size":11,"uploaded_by":"user:alice"}
[{"id":"ver_ibuj4maqbycwv7sgupg7e54r5q","number":2,"filename":"offer-v2.txt","size":31,"replaced_at":"2026-10-06T03:16:22.172499Z"},{"id":"ver_wtkisc4gkobtu5aqsmsl7mrg2w","number":1,"filename":"offer.txt","size":11,"replaced_at":"2026-10-06T03:16:22.141924Z"}]
```

The file ID never changes, and `uploaded_by` stays alice after bob's edit: a replacement keeps it. Who replaced the file is in the activity feed (step 5).

3. Alice shares the file for one download. The link redirects to a signed URL that downloads it; the second visit is `404`:

```sh
curl -s -X POST localhost:8110/files/fil_szmo2iutqwix76wvdcll7wvlpe/share -H 'X-Workspace: studio' -H 'X-User: alice' -d '{"ttl_seconds":3600,"max_downloads":1}'
curl -s -L -D - http://localhost:8110/s/akddrs5j3pOMbGQlO1jxbhF8T0zV_K2eFTMfy4pWTnQ | grep -iE '^(HTTP|Content-Disposition)|price'
curl -s -i http://localhost:8110/s/akddrs5j3pOMbGQlO1jxbhF8T0zV_K2eFTMfy4pWTnQ | head -1
```

```text
{"url":"http://localhost:8110/s/akddrs5j3pOMbGQlO1jxbhF8T0zV_K2eFTMfy4pWTnQ","expires_at":"2026-10-06T04:16:22.197472Z"}
HTTP/1.1 302 Found
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="offer.txt"; filename*=UTF-8''offer.txt
price: 100
HTTP/1.1 404 Not Found
```

A download is counted when `/s/{token}` redirects, so a chat app's link preview that follows the link spends one.

4. Fill the free plan. The second 700,000-byte upload would pass 1 MiB and is refused with `507`; it charges nothing and leaves no object. The same two uploads in `acme` (team plan) both answer `201`:

```sh
head -c 700000 /dev/zero | tr '\0' a > big.txt
curl -s -w '%{http_code}\n' -X POST localhost:8110/files -H 'X-Workspace: studio' -H 'X-User: alice' -F file=@big.txt
curl -s -w '%{http_code}\n' -X POST localhost:8110/files -H 'X-Workspace: studio' -H 'X-User: alice' -F file=@big.txt
curl -s localhost:8110/usage -H 'X-Workspace: studio'
```

```text
{"id":"fil_bvyejrbyivprhmyankg46epbyj","filename":"big.txt","size":700000,"uploaded_by":"user:alice"}
201
{"error":"quota: scope \"studio\" would exceed its limit of 1048576 bytes","code":"quota_exceeded"}
507
{"used_bytes":700011,"used_files":2,"limit_bytes":1048576,"limit_files":100}
```

Usage counts current contents only (11 + 700,000 bytes): kept versions are not charged, as the [quota plugin](../../docs/plugins/quota.md) documents.

5. The activity feed, newest first. The refused upload is not in it: its transaction rolled back, and the audit row with it:

```sh
curl -s localhost:8110/activity -H 'X-Workspace: studio'
```

```json
[{"at":"2026-10-06T03:16:22.274197Z","actor":"user:alice","kind":"file.created","file_id":"fil_bvyejrbyivprhmyankg46epbyj"},{"at":"2026-10-06T03:16:22.209911Z","actor":"","kind":"share.downloaded","file_id":"fil_szmo2iutqwix76wvdcll7wvlpe","subject":"shr_wen5c6awjxb23zgt5wqa4h2job","detail":{"downloads":"1"}},{"at":"2026-10-06T03:16:22.197755Z","actor":"user:alice","kind":"share.created","file_id":"fil_szmo2iutqwix76wvdcll7wvlpe","subject":"shr_wen5c6awjxb23zgt5wqa4h2job","detail":{"expires_at":"2026-10-06T04:16:22.197472Z","max_downloads":"1"}},{"at":"2026-10-06T03:16:22.172573Z","actor":"user:alice","kind":"file.replaced","file_id":"fil_szmo2iutqwix76wvdcll7wvlpe"},{"at":"2026-10-06T03:16:22.141991Z","actor":"user:bob","kind":"file.replaced","file_id":"fil_szmo2iutqwix76wvdcll7wvlpe"},{"at":"2026-10-06T03:16:22.092814Z","actor":"user:alice","kind":"folders.file_put","file_id":"fil_szmo2iutqwix76wvdcll7wvlpe","detail":{"folder":"fld_ecay3a2uz3wx3ipkq5u5q67ggf","from":""}},{"at":"2026-10-06T03:16:22.092534Z","actor":"user:alice","kind":"file.created","file_id":"fil_szmo2iutqwix76wvdcll7wvlpe"},{"at":"2026-10-06T03:16:22.070087Z","actor":"user:alice","kind":"folders.created","subject":"fld_ecay3a2uz3wx3ipkq5u5q67ggf","detail":{"name":"Contracts","parent":""}}]
```

## The code

`plans` and `planLimit` give each workspace its plan's limit. The limit is asked on every upload, so a plan change applies at once.

<!-- ds:block id=plans-mjbvbvf8 -->
**plans** · [`examples/recipe-team-drive/main.go:83-86`](main.go#L83-L86)

```go
var plans = map[string]quota.Limit{
	"free": {Bytes: 1 << 20, Files: 100},
	"team": {Bytes: 100 << 30, Files: 1_000_000},
}
```
<!-- /ds:block hash=a8d3b9 -->

<!-- ds:block id=planlimit-cfcrukp2 -->
**planLimit** · [`examples/recipe-team-drive/main.go:95-100`](main.go#L95-L100)

```go
func planLimit(_ context.Context, workspace string) (quota.Limit, error) {
	if teamWorkspaces[workspace] {
		return plans["team"], nil
	}
	return plans["free"], nil
}
```
<!-- /ds:block hash=f078eb -->

`policy` accepts documents and images, never active content.

<!-- ds:block id=policy-vhwdjca2 -->
**policy** · [`examples/recipe-team-drive/main.go:104-108`](main.go#L104-L108)

```go
var policy = upload.Policy{
	MaxSize: maxFileBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain", "text/csv", "application/zip"},
	Key:     upload.RandomKey("files"),
}
```
<!-- /ds:block hash=3981b3 -->

`newPlugins` builds the five plugins (versions keeps five per file), and `openStore` installs them on one `records.Store`; `Migrate` creates each plugin's tables.

<!-- ds:block id=newplugins-99f3mrb9 -->
**newPlugins** · [`examples/recipe-team-drive/main.go:124-134`](main.go#L124-L134)

```go
func newPlugins() (*App, error) {
	q, err := quota.New(planLimit)
	if err != nil {
		return nil, err
	}
	v, err := versions.New(versions.WithKeep(keptVersions))
	if err != nil {
		return nil, err
	}
	return &App{Quota: q, Folders: folders.New(), Versions: v, Shares: share.New(), Audit: audit.New()}, nil
}
```
<!-- /ds:block hash=4c122c -->

<!-- ds:block id=app-openstore-uzs4x4gh -->
**App.openStore** · [`examples/recipe-team-drive/main.go:139-168`](main.go#L139-L168)

```go
func (a *App) openStore(ctx context.Context, dataDir, base string, secret []byte) (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(base+signedPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, nil, err
	}
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "drive.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	a.Store, err = records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithPlugins(a.Quota, a.Versions, a.Folders, a.Shares, a.Audit))
	if err != nil {
		return nil, nil, err
	}
	_, err = a.Store.Migrate(ctx)
	return disks, signer, err
}
```
<!-- /ds:block hash=dfe6cf -->

`withActor` records the signed-in user as the actor of every change the request makes, which `File.UploadedBy` and the audit log read. `handle` runs a JSON handler in the request's workspace.

<!-- ds:block id=withactor-r6bu5mf8 -->
**withActor** · [`examples/recipe-team-drive/main.go:200-207`](main.go#L200-L207)

```go
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), "user:"+user))
		}
		next.ServeHTTP(w, r)
	})
}
```
<!-- /ds:block hash=bd6688 -->

<!-- ds:block id=handle-ttrqu2ur -->
**handle** · [`examples/recipe-team-drive/main.go:228-247`](main.go#L228-L247)

```go
func handle[In, Out any](w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, ws string, in In) (Out, error)) {
	ws, err := workspaceOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var in In
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
			writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body"))
			return
		}
	}
	out, err := fn(r.Context(), ws, in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
```
<!-- /ds:block hash=c9284a -->

`store` uploads into a folder in one transaction: the file row, its quota charge and its audit row, and the folder placement commit together. A full workspace or an unknown folder rolls back and deletes the stored bytes.

<!-- ds:block id=app-store-yfye774s -->
**App.store** · [`examples/recipe-team-drive/main.go:274-287`](main.go#L274-L287)

```go
func (a *App) store(ctx context.Context, ws, folder string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.Upload(ctx, ws, diskName, body, filename, policy); err != nil {
			return err
		}
		if folder == folders.RootID {
			return nil
		}
		return a.Folders.Put(ctx, a.Store, ws, f.ID, folder)
	})
	return f, err
}
```
<!-- /ds:block hash=5d0936 -->

`replaceFile` puts new content under the same file ID, and the versions plugin keeps the old content; `restoreVersion` makes a version current again.

<!-- ds:block id=app-replacefile-3fex6my6 -->
**App.replaceFile** · [`examples/recipe-team-drive/main.go:336-354`](main.go#L336-L354)

```go
func (a *App) replaceFile(w http.ResponseWriter, r *http.Request) {
	ws, err := workspaceOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	file, name, err := formFile(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.Store.Replace(r.Context(), ws, r.PathValue("id"), file, name, policy)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(f))
}
```
<!-- /ds:block hash=4d200f -->

<!-- ds:block id=app-restoreversion-czm9ymqs -->
**App.restoreVersion** · [`examples/recipe-team-drive/main.go:397-402`](main.go#L397-L402)

```go
func (a *App) restoreVersion(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) (fileResponse, error) {
		f, err := a.Versions.Restore(ctx, a.Store, ws, r.PathValue("id"))
		return toResponse(f), err
	})
}
```
<!-- /ds:block hash=aa4b4c -->

`shareFile` creates an expiring link with an optional download limit; the lifetime must be between one second and seven days. The token appears in the answer once; only its hash is stored.

<!-- ds:block id=app-sharefile-fgqxyp5e -->
**App.shareFile** · [`examples/recipe-team-drive/main.go:418-427`](main.go#L418-L427)

```go
func (a *App) shareFile(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, in shareRequest) (shareResponse, error) {
		ttl := time.Duration(in.TTLSeconds) * time.Second
		if ttl <= 0 || ttl > maxShareTTL {
			return shareResponse{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "ttl_seconds must be in (0, %d]", int64(maxShareTTL/time.Second))
		}
		sh, err := a.Shares.Create(ctx, a.Store, ws, r.PathValue("id"), share.Options{TTL: ttl, MaxDownloads: in.MaxDownloads})
		return shareResponse{URL: baseURL + "/s/" + sh.Token, ExpiresAt: sh.ExpiresAt}, err
	})
}
```
<!-- /ds:block hash=67a7fb -->

`activity` reads the workspace's audit log and `usage` its quota.

<!-- ds:block id=app-activity-sjmebe8n -->
**App.activity** · [`examples/recipe-team-drive/main.go:443-452`](main.go#L443-L452)

```go
func (a *App) activity(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) ([]activityEntry, error) {
		page, err := a.Audit.Entries(ctx, a.Store, audit.Query{Scope: ws, Cursor: r.URL.Query().Get("cursor")})
		out := make([]activityEntry, 0, len(page.Entries))
		for _, e := range page.Entries {
			out = append(out, activityEntry{At: e.At, Actor: e.Actor, Kind: e.Kind, FileID: e.FileID, Subject: e.Subject, Detail: e.Detail})
		}
		return out, err
	})
}
```
<!-- /ds:block hash=ef3d12 -->

<!-- ds:block id=app-usage-amdzfg2p -->
**App.usage** · [`examples/recipe-team-drive/main.go:463-472`](main.go#L463-L472)

```go
func (a *App) usage(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) (usageResponse, error) {
		used, err := a.Quota.Usage(ctx, a.Store, ws)
		if err != nil {
			return usageResponse{}, err
		}
		limit, err := planLimit(ctx, ws)
		return usageResponse{UsedBytes: used.Bytes, UsedFiles: used.Files, LimitBytes: limit.Bytes, LimitFiles: limit.Files}, err
	})
}
```
<!-- /ds:block hash=58d21b -->

`writeError` maps the error code to an HTTP status (`404`, `409`, `507`) with a JSON body.

<!-- ds:block id=writeerror-3zdpmjfy -->
**writeError** · [`examples/recipe-team-drive/main.go:483-486`](main.go#L483-L486)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, errorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=c0bee3 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | Plans, the policy, the plugins and store, the demo session middleware, and the folder, file, version, share, activity and usage handlers. |
| `main_test.go` | Folders, versions and share links end to end (including another workspace's `404`), and the quota per plan. |

## Next

- The guide: [Team drive](../../docs/recipes/team-drive.md), with edge cases and production notes (roles, scheduled maintenance, moving to S3, revoking share links).
- Each plugin in full: [quota](../../docs/plugins/quota.md), [folders](../../docs/plugins/folders.md), [versions](../../docs/plugins/versions.md), [share](../../docs/plugins/share.md), [audit](../../docs/plugins/audit.md); and [Replace and visibility](../../docs/records/replace-and-visibility.md).
- The [records-quota](../records-quota), [records-folders](../records-folders), [records-versions](../records-versions), [records-share](../records-share) and [records-audit](../records-audit) examples.
- Related recipes: [recipe-saas-tenants](../recipe-saas-tenants), [recipe-chat-attachments](../recipe-chat-attachments), [recipe-virus-scanning](../recipe-virus-scanning).
