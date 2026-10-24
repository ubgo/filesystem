# Drive: a team drive built from the records plugins

The shape of a team drive such as Google Drive or Dropbox for a workspace: workspaces (scopes) with a 50 MiB storage quota, files attached to projects through `recordshttp`, folders, tags, version history with restore, expiring share links with a download limit, an activity feed, and image dimensions, all plugins of one records Store over a local disk and SQLite. It uses the core `filesystem` package with `local`, `signedurl`, `fshttp` and `upload`, the `records` module with `recordshttp` and the `quota`, `versions`, `folders`, `tags`, `share` and `audit` plugins, and `contrib/imaging/dimensions`. The [team drive recipe](../../docs/recipes/team-drive.md) builds the smaller version step by step and points here for the larger one; the [recipes index](../../docs/recipes/README.md) lists it.

Authentication is out of scope: the demo trusts the `X-Workspace` and `X-User` headers, and `identify` is the one function a real app replaces with its session lookup.

## Run it

```sh
cd examples
go run ./drive -addr localhost:18080
go test ./drive
```

`-addr` is the listen address (default `localhost:8080`), and the public URL the app builds its links from is `http://<addr>`. It creates a temporary data directory, printed at start-up, holding `files/` and `drive.db`; the signing secret is a constant in `main.go`. Stop it with Ctrl-C. Ctrl-C or SIGTERM shuts it down cleanly and removes the data directory. The test runs the same `New` behind an `httptest` server and walks a file through its whole life.

```text
listening on localhost:18080 ; data in /var/folders/sk/k6vkn27d2xbcxcj1ll6dvwjw0000gn/T/drive-3549251790
```

## Walkthrough

The commands below send each user's headers from a shell array (bash and zsh):

```sh
A=localhost:18080
ANA=(-H 'X-Workspace: acme' -H 'X-User: ana')
BEN=(-H 'X-Workspace: acme' -H 'X-User: ben')
```

1. Upload a 64×32 PNG and attach it to project `p1` in the role `assets`:

```sh
curl -s "${ANA[@]}" -F file=@logo.png $A/api/owners/project/p1/assets
```

```json
{"files":[{"attachment_id":"att_4t7eyof6ulox3au54n4b3behh2","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","filename":"logo.png","content_type":"image/png","size":123,"position":1,"uploaded_by":"user:ana","status":"ready"}]}
```

2. Create a folder, move the file into it, and tag it. Tags are normalised and sorted:

```sh
curl -s "${ANA[@]}" -d '{"name":"Brand"}' $A/api/folders
curl -s "${ANA[@]}" -X PUT -d '{"folder":"fld_2uvrozykivhcgcor2niclmvojx"}' $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u/folder
curl -s "${ANA[@]}" -X PUT -d '{"tags":["Logo","brand"]}' $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u/tags
```

```json
{"id":"fld_2uvrozykivhcgcor2niclmvojx","parent_id":"","name":"Brand","created_at":"2026-10-06T06:22:34.83018Z"}
{"folder":"fld_2uvrozykivhcgcor2niclmvojx"}
{"tags":["brand","logo"]}
```

3. Ben uploads a new version (128×128). The file keeps its id; the old bytes become version 1:

```sh
curl -s "${BEN[@]}" -X PUT -F file=@logo-v2.png $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u
curl -s "${ANA[@]}" $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u/versions
```

```json
{"file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","filename":"logo-v2.png","content_type":"image/png","size":311}
[{"id":"ver_viktbahnq6hv7mx7i4lklfaz7c","number":1,"filename":"logo.png","content_type":"image/png","size":123,"replaced_at":"2026-10-06T06:22:35.035604Z"}]
```

4. Ana restores version 1; the dimensions plugin's metadata follows the bytes back to 64×32:

```sh
curl -s "${ANA[@]}" -X POST $A/api/versions/ver_viktbahnq6hv7mx7i4lklfaz7c/restore
```

```json
{"file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","filename":"logo.png","content_type":"image/png","size":123,"width":64,"height":32,"uploaded_by":"user:ana"}
```

5. Share the file for an hour with at most two downloads. Anyone with the path gets a redirect to a signed download link, twice; the third request is `404`. A TTL outside (0, 7 days] is refused:

```sh
curl -s "${ANA[@]}" -d '{"ttl_seconds":3600,"max_downloads":2}' $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u/share
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' $A/s/hOkkGJcU4cINFy0b5IWPjnL5TI-qBQCIR-O9OGM1Fgo
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' $A/s/hOkkGJcU4cINFy0b5IWPjnL5TI-qBQCIR-O9OGM1Fgo
curl -s -w '%{http_code}\n' $A/s/hOkkGJcU4cINFy0b5IWPjnL5TI-qBQCIR-O9OGM1Fgo
curl -s "${ANA[@]}" -d '{"ttl_seconds":0}' $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u/share
```

```text
{"path":"/s/hOkkGJcU4cINFy0b5IWPjnL5TI-qBQCIR-O9OGM1Fgo","expires_at":"2026-10-06T07:22:35.134104Z"}
302 http://localhost:18080/files/drive/versions/qhjz43atfvpw5sze6hyi2rln23?cd=attachment%3B+filename%3D%22logo.png%22%3B+filename%2A%3DUTF-8%27%27logo.png&ct=image%2Fpng&exp=1791268055&kid=k1&m=GET&sig=176310354fdc02efc23d4b923d47a0a3271aaa839bb088ec4e369041e0c3c644
302 http://localhost:18080/files/drive/versions/qhjz43atfvpw5sze6hyi2rln23?cd=attachment%3B+filename%3D%22logo.png%22%3B+filename%2A%3DUTF-8%27%27logo.png&ct=image%2Fpng&exp=1791268055&kid=k1&m=GET&sig=176310354fdc02efc23d4b923d47a0a3271aaa839bb088ec4e369041e0c3c644
Not Found
404
Bad Request
```

Following the signed link returns the restored image (`200 image/png`, 123 bytes) with `Content-Disposition: attachment; filename="logo.png"; filename*=UTF-8''logo.png`.

6. Another workspace sees nothing, and a request with no workspace is refused:

```sh
curl -s -w '%{http_code}\n' -H 'X-Workspace: evil-corp' -H 'X-User: eve' $A/api/files/fil_dovyhqgbxxbuxaqtxkdzogsy6u/versions
curl -s -w '%{http_code}\n' $A/api/usage
```

```text
Not Found
404
Unauthorized
401
```

7. The workspace's usage counts the current bytes, and the activity feed records who did what, newest first, including the folder, tag and share-link events with the folder or link in `subject` and the particulars in `detail`. Downloads through a share link are anonymous, so their `actor` is empty:

```sh
curl -s "${ANA[@]}" $A/api/usage
curl -s "${ANA[@]}" $A/api/activity
```

```json
{"bytes":123,"files":1}
{"entries":[{"actor":"","kind":"share.downloaded","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","subject":"shr_ecp4ejy3qylm4o2jbpgzuk3d2a","detail":{"downloads":"2"},"at":"2026-10-06T06:22:35.185455Z"},{"actor":"","kind":"share.downloaded","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","subject":"shr_ecp4ejy3qylm4o2jbpgzuk3d2a","detail":{"downloads":"1"},"at":"2026-10-06T06:22:35.159157Z"},{"actor":"user:ana","kind":"share.created","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","subject":"shr_ecp4ejy3qylm4o2jbpgzuk3d2a","detail":{"expires_at":"2026-10-06T07:22:35.134104Z","max_downloads":"2"},"at":"2026-10-06T06:22:35.134361Z"},{"actor":"user:ana","kind":"file.replaced","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","at":"2026-10-06T06:22:35.108455Z"},{"actor":"user:ben","kind":"file.replaced","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","at":"2026-10-06T06:22:35.035869Z"},{"actor":"user:ana","kind":"tags.added","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","detail":{"tag":"brand"},"at":"2026-10-06T06:22:34.932794Z"},{"actor":"user:ana","kind":"tags.added","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","detail":{"tag":"logo"},"at":"2026-10-06T06:22:34.932591Z"},{"actor":"user:ana","kind":"folders.file_put","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","detail":{"folder":"fld_2uvrozykivhcgcor2niclmvojx","from":""},"at":"2026-10-06T06:22:34.875284Z"},{"actor":"user:ana","kind":"folders.created","subject":"fld_2uvrozykivhcgcor2niclmvojx","detail":{"name":"Brand","parent":""},"at":"2026-10-06T06:22:34.830254Z"},{"actor":"user:ana","kind":"attachment.created","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","at":"2026-10-06T06:22:34.780077Z"},{"actor":"user:ana","kind":"file.created","file_id":"fil_dovyhqgbxxbuxaqtxkdzogsy6u","at":"2026-10-06T06:22:34.777879Z"}]}
```

`next_cursor` is left out when there is no further page.

## The code

`New` is the whole wiring: a local disk with a signer, one records Store with every plugin, `recordshttp` mounted under `/api/` for uploads and attachments, the app's own routes for folders, tags, versions, shares, activity and usage, the public share handler under `/s/`, and `fshttp.Signed` under `/files/`.

The first half opens the local disk, the database and the records Store with its plugins, then migrates:

<!-- ds:block id=new-pd9xke6p lines=1-40 -->
**New** · [`examples/drive/main.go:80-144`](main.go#L80-L144)

```go
func New(ctx context.Context, dataDir, baseURL string, secret []byte) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	signer, err := signedurl.New(baseURL+"/files", []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "drive.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	q, err := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Bytes: workspaceBytes}, nil })
	if err != nil {
		return nil, err
	}
	v, err := versions.New(versions.WithKeep(keptVersions))
	if err != nil {
		return nil, err
	}
	app := &App{Folders: folders.New(), Tags: tags.New(), Versions: v, Shares: share.New(), Audit: audit.New(), Quota: q}
	app.Store, err = records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerProject),
		records.WithPlugins(app.Quota, app.Versions, app.Folders, app.Tags, app.Shares, app.Audit, dimensions.New()))
	if err != nil {
		return nil, err
	}
	if _, err := app.Store.Migrate(ctx); err != nil {
		return nil, err
	}
```
<!-- /ds:block hash=6d7eb4 -->

The second half mounts the HTTP routes:

<!-- ds:block id=new-pd9xke6p lines=41-65 -->
**New** · [`examples/drive/main.go:80-144`](main.go#L80-L144)

```go
	files, err := recordshttp.Handler(recordshttp.Config{
		Store: app.Store, Disk: diskName, Policy: policy,
		Scope: func(r *http.Request) (string, error) { return identify(r) },
		// Every member of a workspace may do everything in it; a real app
		// checks roles or project membership here.
		Authorize: func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil },
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", files))
	mux.HandleFunc("POST /api/folders", app.createFolder)
	mux.HandleFunc("PUT /api/files/{id}/folder", app.moveFile)
	mux.HandleFunc("PUT /api/files/{id}/tags", app.setTags)
	mux.HandleFunc("GET /api/files/{id}/versions", app.listVersions)
	mux.HandleFunc("POST /api/versions/{id}/restore", app.restoreVersion)
	mux.HandleFunc("POST /api/files/{id}/share", app.shareFile)
	mux.HandleFunc("GET /api/activity", app.activity)
	mux.HandleFunc("GET /api/usage", app.usage)
	mux.Handle("/s/", http.StripPrefix("/s/", share.Handler(app.Store, app.Shares)))
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	app.Handler = withActor(mux)
	return app, nil
}
```
<!-- /ds:block hash=6d7eb4 -->

`identify` returns the request's workspace; it is the demo's only stand-in for authentication.

<!-- ds:block id=identify-6g4cctkk -->
**identify** · [`examples/drive/main.go:150-155`](main.go#L150-L155)

```go
func identify(r *http.Request) (string, error) {
	if ws := r.Header.Get(headerSpace); ws != "" {
		return ws, nil
	}
	return "", errNoSession
}
```
<!-- /ds:block hash=4380b1 -->

`withActor` puts the user in the context, where the audit plugin reads it for every change the request makes.

<!-- ds:block id=withactor-jp5agfxx -->
**withActor** · [`examples/drive/main.go:160-167`](main.go#L160-L167)

```go
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(audit.WithActor(r.Context(), "user:"+user))
		}
		next.ServeHTTP(w, r)
	})
}
```
<!-- /ds:block hash=ef0f45 -->

`handle` is the one generic helper behind every app route: it resolves the workspace, decodes a size-limited JSON body, runs the plugin call and maps its error to a status with `fshttp.StatusOf`.

<!-- ds:block id=handle-6uxq3fmz -->
**handle** · [`examples/drive/main.go:172-193`](main.go#L172-L193)

```go
func handle[In, Out any](w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, scope string, in In) (Out, error)) {
	scope, err := identify(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	var in In
	if r.ContentLength != 0 && r.Method != http.MethodGet {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	out, err := fn(r.Context(), scope, in)
	if err != nil {
		status := fshttp.StatusOf(err)
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
```
<!-- /ds:block hash=cb2343 -->

`shareFile` shows the pattern for a route: the app's own bound (TTL at most seven days) checked before the plugin is called.

<!-- ds:block id=app-sharefile-rn3zppyb -->
**App.shareFile** · [`examples/drive/main.go:298-307`](main.go#L298-L307)

```go
func (a *App) shareFile(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, in shareRequest) (shareResponse, error) {
		ttl := time.Duration(in.TTLSeconds) * time.Second
		if ttl <= 0 || ttl > maxShareTTL {
			return shareResponse{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "ttl_seconds must be in (0, %d]", int64(maxShareTTL/time.Second))
		}
		sh, err := a.Shares.Create(ctx, a.Store, scope, r.PathValue("id"), share.Options{TTL: ttl, MaxDownloads: in.MaxDownloads})
		return shareResponse{Path: "/s/" + sh.Token, ExpiresAt: sh.ExpiresAt}, err
	})
}
```
<!-- /ds:block hash=6ffda5 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The app's decisions (quota, size, policy), `New`, the handlers and `main` |
| `main_test.go` | One workspace through a file's whole life over HTTP: upload, folder, tags, new version, restore, share limits, activity, usage, and another workspace seeing none of it |

## Next

- The smaller version built step by step: [team drive recipe](../../docs/recipes/team-drive.md) and [recipe-team-drive](../recipe-team-drive).
- Each plugin: [quota](../../docs/plugins/quota.md), [versions](../../docs/plugins/versions.md), [folders](../../docs/plugins/folders.md), [tags](../../docs/plugins/tags.md), [share](../../docs/plugins/share.md), [audit](../../docs/plugins/audit.md), [dimensions](../../docs/plugins/dimensions.md); the endpoints: [records HTTP](../../docs/records/http-endpoints.md).
- The avatar flow written by hand: [mediaserver](../mediaserver).
