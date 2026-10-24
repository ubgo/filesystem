# Team drive

A shared drive is the file feature of collaboration tools, agency and client portals, project management apps and internal tools: a workspace stores files in folders, a plan limits how much it may store, editing a file keeps its history, files can be sent to people outside the workspace, and an activity feed says who did what. This recipe wires those five pieces from the records plugins onto one Store, about 500 lines including the HTTP layer.

The program is [examples/recipe-team-drive](../../examples/recipe-team-drive). Every code block below is copied from it, and every transcript is from running it. A larger program with tags, image dimensions and `recordshttp` attachments is [examples/drive](../../examples/drive).

## What you get

- Workspaces as scopes: every query is filtered by the signed-in workspace, so another workspace's folder or file ID is a `404`.
- A storage quota per plan, enforced in the upload's transaction: two uploads racing for the last megabyte cannot both pass. A full workspace answers `507 Insufficient Storage` with code `quota_exceeded`.
- Folders, with unique names per parent.
- Replacing a file keeps up to five previous contents; any of them can be restored, and a restore can itself be undone.
- Share links that expire and stop after a number of downloads, for people without an account.
- An activity feed with the user behind every change; a download through a share link is anonymous, so its entry has an empty actor.

## The design

| Piece | Why |
|---|---|
| Scope = workspace | `records` filters every query by scope. Taking it from the session, never from the URL, is what isolates workspaces. |
| `quota` plugin with a `LimitFunc` | Limits come from the app (the plan); usage is charged with one conditional `UPDATE` in the transaction that inserts the file row, so a rolled-back upload un-charges itself. |
| `folders` plugin | Folder rows and file placement, with cycle and depth checks on moves. |
| `versions` plugin | `Store.Replace` keeps the file's ID, so its folder and share links stay; the plugin takes ownership of the old object instead of letting the Store delete it. |
| `share` plugin + `share.Handler` | Tokens are shown once and stored as SHA-256 hashes; every unusable link looks like the same `404`. |
| `audit` plugin + `records.WithActor` | Every change is logged in the change's own transaction with the actor the middleware put in the context, so a rolled-back change is never logged. |

## Step 1: Plans and the quota

<!-- ds:block id=plans-mjbvbvf8 -->
**plans** · [`examples/recipe-team-drive/main.go:83-86`](../../examples/recipe-team-drive/main.go#L83-L86)

```go
var plans = map[string]quota.Limit{
	"free": {Bytes: 1 << 20, Files: 100},
	"team": {Bytes: 100 << 30, Files: 1_000_000},
}
```
<!-- /ds:block hash=a8d3b9 -->

<!-- ds:block id=planlimit-cfcrukp2 -->
**planLimit** · [`examples/recipe-team-drive/main.go:95-100`](../../examples/recipe-team-drive/main.go#L95-L100)

```go
func planLimit(_ context.Context, workspace string) (quota.Limit, error) {
	if teamWorkspaces[workspace] {
		return plans["team"], nil
	}
	return plans["free"], nil
}
```
<!-- /ds:block hash=f078eb -->

The plugin asks for the limit on every upload, so an upgrade applies to the next upload without touching any stored counter. `quota.Limit{}` fields at 0 mean unlimited. Usage counts trashed files too, because their bytes are still stored; purging releases it.

## Step 2: The plugins and the store

<!-- ds:block id=newplugins-99f3mrb9 -->
**newPlugins** · [`examples/recipe-team-drive/main.go:124-134`](../../examples/recipe-team-drive/main.go#L124-L134)

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
**App.openStore** · [`examples/recipe-team-drive/main.go:139-168`](../../examples/recipe-team-drive/main.go#L139-L168)

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

`Migrate` creates the core fs_ tables and each plugin's own (`fs_quota_usage`, `fs_file_versions`, the folders, shares and audit tables), recording each in the `fs_migrations` ledger.

The policy is the usual one: documents and images, never active content.

<!-- ds:block id=policy-vhwdjca2 -->
**policy** · [`examples/recipe-team-drive/main.go:104-108`](../../examples/recipe-team-drive/main.go#L104-L108)

```go
var policy = upload.Policy{
	MaxSize: maxFileBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain", "text/csv", "application/zip"},
	Key:     upload.RandomKey("files"),
}
```
<!-- /ds:block hash=3981b3 -->

## Step 3: Who is acting

<!-- ds:block id=withactor-r6bu5mf8 -->
**withActor** · [`examples/recipe-team-drive/main.go:200-207`](../../examples/recipe-team-drive/main.go#L200-L207)

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

Set the actor once, in the middleware that authenticates. The Store records it as `File.UploadedBy` on new files and the audit plugin logs it with every change. Background jobs (a trash sweep) run without one and are logged with an empty actor.

Every JSON route goes through one helper that resolves the workspace first:

<!-- ds:block id=handle-ttrqu2ur -->
**handle** · [`examples/recipe-team-drive/main.go:228-247`](../../examples/recipe-team-drive/main.go#L228-L247)

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

## Step 4: Upload into a folder

<!-- ds:block id=app-store-yfye774s -->
**App.store** · [`examples/recipe-team-drive/main.go:274-287`](../../examples/recipe-team-drive/main.go#L274-L287)

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

The upload, the quota charge (in the quota plugin's `BeforeCreate` hook) and the folder placement are one transaction. A refused quota or an unknown folder rolls it back, and `Store.Upload`'s rollback hook deletes the bytes it stored: nothing is left behind and nothing is charged.

## Step 5: Replace, versions, restore

<!-- ds:block id=app-replacefile-3fex6my6 -->
**App.replaceFile** · [`examples/recipe-team-drive/main.go:336-354`](../../examples/recipe-team-drive/main.go#L336-L354)

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

`Store.Replace` runs the new content through the policy, then swaps it in. The quota plugin charges only the size difference (and credits a shrink); the versions plugin records the old content as version N and drops the oldest past the cap. Kept versions are not counted against the quota; `versions.WithKeep` bounds them per file.

<!-- ds:block id=app-restoreversion-czm9ymqs -->
**App.restoreVersion** · [`examples/recipe-team-drive/main.go:397-402`](../../examples/recipe-team-drive/main.go#L397-L402)

```go
func (a *App) restoreVersion(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) (fileResponse, error) {
		f, err := a.Versions.Restore(ctx, a.Store, ws, r.PathValue("id"))
		return toResponse(f), err
	})
}
```
<!-- /ds:block hash=aa4b4c -->

A restore copies the version's object into place as new content, so the content it replaces becomes the newest version, and the version restored stays listed.

## Step 6: Share links

<!-- ds:block id=app-sharefile-fgqxyp5e -->
**App.shareFile** · [`examples/recipe-team-drive/main.go:418-427`](../../examples/recipe-team-drive/main.go#L418-L427)

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

`share.Handler`, mounted at `/s/`, resolves the token, spends one download and redirects to a short-lived signed URL that saves the file under its name. The link's lifetime is capped by the app (seven days here); `share.Options.TTL` is required.

## Step 7: Activity and usage

The feed lists the audit log newest first: the Store's file events and the plugins' own (a folder created, a file put in a folder, a share link created or spent), which name their folder or link in `subject` and carry their particulars in `detail`.

<!-- ds:block id=app-activity-sjmebe8n -->
**App.activity** · [`examples/recipe-team-drive/main.go:443-452`](../../examples/recipe-team-drive/main.go#L443-L452)

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
**App.usage** · [`examples/recipe-team-drive/main.go:463-472`](../../examples/recipe-team-drive/main.go#L463-L472)

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

A full workspace is the one server-side status whose message is worth showing, and `fshttp.ClientMessage` passes a `quota_exceeded` message through while it hides the disk and key of every error:

<!-- ds:block id=writeerror-3zdpmjfy -->
**writeError** · [`examples/recipe-team-drive/main.go:483-486`](../../examples/recipe-team-drive/main.go#L483-L486)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, errorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=c0bee3 -->

## Try it

```sh
cd examples
go run ./recipe-team-drive
```

```text
listening on http://localhost:8110 data in /var/folders/sk/.../T/recipe-team-drive-3318950818
```

The workspace `studio` is on the free plan (1 MiB); `acme` is on the team plan.

```sh
curl -s -X POST localhost:8110/folders -H 'X-Workspace: studio' -H 'X-User: alice' -d '{"parent":"","name":"Contracts"}'
curl -s -X POST 'localhost:8110/files?folder=fld_nwke7eeaauneknydaw35u4clfi' -H 'X-Workspace: studio' -H 'X-User: alice' -F file=@offer.txt
curl -s localhost:8110/folders/fld_nwke7eeaauneknydaw35u4clfi/files -H 'X-Workspace: studio'
curl -s localhost:8110/folders/fld_nwke7eeaauneknydaw35u4clfi/files -H 'X-Workspace: other'
```

```text
{"id":"fld_nwke7eeaauneknydaw35u4clfi","parent":"","name":"Contracts"}
{"id":"fil_n7qilgwayag76g4xa5mialpbws","filename":"offer.txt","size":11,"uploaded_by":"user:alice"}
[{"id":"fil_n7qilgwayag76g4xa5mialpbws","filename":"offer.txt","size":11,"uploaded_by":"user:alice"}]
{"error":"Not Found","code":"not_found"}
```

Bob edits the file, then alice restores the original:

```sh
curl -s -X PUT localhost:8110/files/fil_n7qilgwayag76g4xa5mialpbws -H 'X-Workspace: studio' -H 'X-User: bob' -F file=@offer-v2.txt
curl -s localhost:8110/files/fil_n7qilgwayag76g4xa5mialpbws/versions -H 'X-Workspace: studio'
curl -s -X POST localhost:8110/versions/ver_hfq4an4ejcnbvd7hmabpsos7zb/restore -H 'X-Workspace: studio' -H 'X-User: alice'
curl -s localhost:8110/files/fil_n7qilgwayag76g4xa5mialpbws/versions -H 'X-Workspace: studio'
```

```text
{"id":"fil_n7qilgwayag76g4xa5mialpbws","filename":"offer-v2.txt","size":31,"uploaded_by":"user:alice"}
[{"id":"ver_hfq4an4ejcnbvd7hmabpsos7zb","number":1,"filename":"offer.txt","size":11,"replaced_at":"2026-10-06T03:16:21.627249Z"}]
{"id":"fil_n7qilgwayag76g4xa5mialpbws","filename":"offer.txt","size":11,"uploaded_by":"user:alice"}
[{"id":"ver_yo25pruosgawbe3jlf33ugdf7l","number":2,"filename":"offer-v2.txt","size":31,"replaced_at":"2026-10-06T03:16:21.666632Z"},{"id":"ver_hfq4an4ejcnbvd7hmabpsos7zb","number":1,"filename":"offer.txt","size":11,"replaced_at":"2026-10-06T03:16:21.627249Z"}]
```

Check: the file ID never changed, and `uploaded_by` is still alice after bob's edit: a replacement keeps it. Who replaced the file is in the activity feed below.

Share it for one download:

```sh
curl -s -X POST localhost:8110/files/fil_n7qilgwayag76g4xa5mialpbws/share -H 'X-Workspace: studio' -H 'X-User: alice' -d '{"ttl_seconds":3600,"max_downloads":1}'
curl -s -L -D - http://localhost:8110/s/eiarslFfLZiuf63xlfHeceIGOTzNK96uNsiNEm1n_7k | grep -iE '^(HTTP|Content-Disposition)|price'
curl -s -i http://localhost:8110/s/eiarslFfLZiuf63xlfHeceIGOTzNK96uNsiNEm1n_7k | head -1
```

```text
{"url":"http://localhost:8110/s/eiarslFfLZiuf63xlfHeceIGOTzNK96uNsiNEm1n_7k","expires_at":"2026-10-06T04:16:21.696084Z"}
HTTP/1.1 302 Found
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="offer.txt"; filename*=UTF-8''offer.txt
price: 100
HTTP/1.1 404 Not Found
```

Fill the free plan:

```sh
head -c 700000 /dev/zero | tr '\0' a > big.txt
curl -s -w '%{http_code}\n' -X POST localhost:8110/files -H 'X-Workspace: studio' -H 'X-User: alice' -F file=@big.txt
curl -s -w '%{http_code}\n' -X POST localhost:8110/files -H 'X-Workspace: studio' -H 'X-User: alice' -F file=@big.txt
curl -s localhost:8110/usage -H 'X-Workspace: studio'
```

```text
{"id":"fil_ycio3evpixw5nfqlgllyb7s7zu","filename":"big.txt","size":700000,"uploaded_by":"user:alice"}
201
{"error":"quota: scope \"studio\" would exceed its limit of 1048576 bytes","code":"quota_exceeded"}
507
{"used_bytes":700011,"used_files":2,"limit_bytes":1048576,"limit_files":100}
```

Check: the refused upload charged nothing and left no object. The same two uploads in `acme` both answer `201`.

```sh
curl -s localhost:8110/activity -H 'X-Workspace: studio'
```

```json
[{"at":"2026-10-06T03:16:21.786393Z","actor":"user:alice","kind":"file.created","file_id":"fil_ycio3evpixw5nfqlgllyb7s7zu"},{"at":"2026-10-06T03:16:21.710121Z","actor":"","kind":"share.downloaded","file_id":"fil_n7qilgwayag76g4xa5mialpbws","subject":"shr_qtjjv5zcpystikzhna6dxchkxy","detail":{"downloads":"1"}},{"at":"2026-10-06T03:16:21.696426Z","actor":"user:alice","kind":"share.created","file_id":"fil_n7qilgwayag76g4xa5mialpbws","subject":"shr_qtjjv5zcpystikzhna6dxchkxy","detail":{"expires_at":"2026-10-06T04:16:21.696084Z","max_downloads":"1"}},{"at":"2026-10-06T03:16:21.666742Z","actor":"user:alice","kind":"file.replaced","file_id":"fil_n7qilgwayag76g4xa5mialpbws"},{"at":"2026-10-06T03:16:21.62749Z","actor":"user:bob","kind":"file.replaced","file_id":"fil_n7qilgwayag76g4xa5mialpbws"},{"at":"2026-10-06T03:16:21.581879Z","actor":"user:alice","kind":"folders.file_put","file_id":"fil_n7qilgwayag76g4xa5mialpbws","detail":{"folder":"fld_nwke7eeaauneknydaw35u4clfi","from":""}},{"at":"2026-10-06T03:16:21.581464Z","actor":"user:alice","kind":"file.created","file_id":"fil_n7qilgwayag76g4xa5mialpbws"},{"at":"2026-10-06T03:16:21.550986Z","actor":"user:alice","kind":"folders.created","subject":"fld_nwke7eeaauneknydaw35u4clfi","detail":{"name":"Contracts","parent":""}}]
```

The refused upload is not in the feed: its transaction rolled back, and the audit row with it.

## Edge cases this handles

- Concurrent uploads near the limit: the conditional `UPDATE` admits only what fits; the loser gets `507` and its bytes are deleted.
- A replacement that grows a file past the limit: `507`, and the file keeps its current content.
- A folder name already used in the same parent: `409` (`exists`). Names are checked by `folders.CheckName` (no `/`, no control characters, not `.` or `..`).
- Another workspace's IDs: `404` for folders, files and versions alike.
- A share link that expired, was revoked, ran out of downloads, or whose file was trashed: the same `404`. A download is counted when `/s/{token}` redirects, so a chat app's link preview that follows the link spends one; set `max_downloads` with that in mind, or leave it at 0 (unlimited) and rely on the expiry.
- Restoring a version: undoable, because the replaced content becomes a version too.

## For production

- Replace the header-based session with your own; take the workspace and user from it, never from the request.
- Check roles in the handlers: here every member may do everything. Use `audit.Query{Actor: …}` for a per-user activity page.
- Schedule the maintenance the library never starts on its own: `Store.SweepTrash` for trash retention, `audit.Plugin.Prune` for log retention, and `quota.Plugin.Recount` after any manual repair of `fs_files`.
- Move the disk to S3 and drop the signer and `fshttp.Signed`; share links then redirect to SigV4 URLs.
- Add [virus scanning](virus-scanning.md): a drive is how files travel between people.
- Offer a list of a file's share links (`share.Plugin.Shares`) and a revoke button (`share.Plugin.Revoke`).

## Related

- [Quota](../plugins/quota.md), [folders](../plugins/folders.md), [versions](../plugins/versions.md), [share](../plugins/share.md), [audit](../plugins/audit.md): each plugin in full.
- [Replace and visibility](../records/replace-and-visibility.md): what `Replace` keeps and what it changes.
- [SaaS tenants](saas-tenants.md): scopes, per-tenant disks and offboarding.
- [Multi-tenancy](../concepts/multi-tenancy.md): how scope isolates tenants.
- [Chat attachments](chat-attachments.md): files owned by messages instead of a workspace.
