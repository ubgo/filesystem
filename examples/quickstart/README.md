# Quick start: a photo service on net/http

The program behind [Your first app](../../docs/start/first-app.md): a photo service where a signed-in user uploads photos, checked by their bytes and capped in size, lists them, opens one through a short-lived signed link, moves one to the trash, restores it, and deletes it for good. Bytes live on local disk (a `file:` DSN), rows in SQLite, and signed links are served by the program itself through `fshttp`. It is the smallest complete app on the library and the shape most SaaS, social and mobile backends start from. It uses `github.com/ubgo/filesystem` with the `local` and `memory` drivers, `upload`, `signedurl`, `fshttp` and `records`.

## Run it

```sh
cd examples
go run ./quickstart
go test ./quickstart
```

It listens on `http://localhost:8082` and creates `quickstart-data/` in the current directory: `app.db` (SQLite) and `files/` (the stored photos). Stop it with Ctrl-C and delete that directory to start over.

| Variable | Effect |
|---|---|
| `STORAGE_DSN` | Where the bytes go; `mem://` keeps them in memory. Unset, they go to `quickstart-data/files` |
| `QUICKSTART_SECRET` | The link signing secret, at least 32 bytes. Unset, a random one is made at start-up, so links stop working when the server restarts |

```text
2026/10/03 16:20:13 QUICKSTART_SECRET is not set: using a random secret, so links stop working when the server restarts
2026/10/03 16:20:13 listening on http://localhost:8082; files and database in quickstart-data
```

Authentication is out of scope: every route but `/files/` takes the user from the `X-User` header, standing in for a session lookup.

| Route | What it does |
|---|---|
| `POST /photos` | Upload a photo (multipart form, field `file`) |
| `GET /photos` | The user's photos |
| `GET /photos/{id}` | `302` to a signed link |
| `DELETE /photos/{id}` | Move to the trash |
| `POST /photos/{id}/restore` | Take out of the trash |
| `GET /trash` | The user's trashed photos |
| `DELETE /trash/{id}` | Delete for good, bytes included |
| `GET /files/...` | Signed links, served by `fshttp` |

## Walkthrough

The commands use `beach.png` (a tiny PNG), `notes.png` (a text file with an image name), `beach.jpg` (the PNG renamed) and `huge.png` (the PNG padded past 5 MiB: `cp beach.png huge.png && head -c 6000000 /dev/zero >> huge.png`). IDs, keys and signatures differ on every run.

1. Upload a photo as Alice:

```sh
curl -s -X POST localhost:8082/photos -H 'X-User: alice' -F file=@beach.png
```

```json
{"id":"fil_misvfjsjbahofoekvgneo7fd4z","filename":"beach.png","content_type":"image/png","size":69,"created_at":"2026-10-03T10:50:14.134347Z"}
```

2. The refusals, judged from the bytes on the server (statuses after each body):

```sh
curl -s -w ' %{http_code}\n' -X POST localhost:8082/photos -H 'X-User: alice' -F file=@notes.png
curl -s -w ' %{http_code}\n' -X POST localhost:8082/photos -H 'X-User: alice' -F file=@beach.jpg
curl -s -w ' %{http_code}\n' -X POST localhost:8082/photos -H 'X-User: alice' -F file=@huge.png
curl -s -w ' %{http_code}\n' localhost:8082/photos
```

```text
{"error":"content type \"text/plain; charset=utf-8\" is not allowed","code":"rejected"}
 415
{"error":"the name says image/jpeg but the content is image/png","code":"rejected"}
 415
{"error":"body exceeds 5242880 bytes","code":"too_large"}
 413
{"error":"sign in first: send the X-User header","code":"permission"}
 401
```

3. Each user sees only their own photos:

```sh
curl -s localhost:8082/photos -H 'X-User: alice'
curl -s localhost:8082/photos -H 'X-User: bob'
```

```json
[{"id":"fil_misvfjsjbahofoekvgneo7fd4z","filename":"beach.png","content_type":"image/png","size":69,"created_at":"2026-10-03T10:50:14.134347Z"}]
[]
```

4. Opening a photo redirects to a signed link that expires in five minutes (headers shown; the body is a short HTML link to the same URL):

```sh
curl -s -i localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: alice'
```

```text
HTTP/1.1 302 Found
Content-Type: text/html; charset=utf-8
Location: http://localhost:8082/files/media/photos/2q5sqohd46bpziecd47yoxrzz5.png?ct=image%2Fpng&exp=1791024914&kid=k1&m=GET&sig=1ce9042fb0656adcbc3413ba0c9c9159595978b9448ffce3f4c382f923b884f8
Date: Sat, 03 Oct 2026 10:50:14 GMT
Content-Length: 222
```

5. The link serves the bytes with headers that stop a browser from running them:

```sh
LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: alice')
curl -s -D - -o got.png "$LINK"
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 69
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: image/png
Etag: "18daff65b4da2789-45"
Last-Modified: Sat, 03 Oct 2026 10:50:14 GMT
X-Content-Type-Options: nosniff
Date: Sat, 03 Oct 2026 10:50:14 GMT
```

6. Bob asking for Alice's photo gets not found, because each user is the scope of their own files:

```sh
curl -s -w ' %{http_code}\n' localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: bob'
```

```text
{"error":"records: no file \"fil_misvfjsjbahofoekvgneo7fd4z\"","code":"not_found"}
 404
```

7. Trash: the photo leaves the list, appears in the trash, and cannot be opened:

```sh
curl -s -i -X DELETE localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: alice' | head -1
curl -s localhost:8082/photos -H 'X-User: alice'
curl -s localhost:8082/trash -H 'X-User: alice'
curl -s -w ' %{http_code}\n' localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: alice'
```

```text
HTTP/1.1 204 No Content
[]
[{"id":"fil_misvfjsjbahofoekvgneo7fd4z","filename":"beach.png","content_type":"image/png","size":69,"created_at":"2026-10-03T10:50:14.134347Z"}]
{"error":"the photo is in the trash","code":"not_found"}
 404
```

8. Restore puts it back in the list:

```sh
curl -s -i -X POST localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z/restore -H 'X-User: alice' | head -1
curl -s localhost:8082/photos -H 'X-User: alice'
```

```text
HTTP/1.1 204 No Content
[{"id":"fil_misvfjsjbahofoekvgneo7fd4z","filename":"beach.png","content_type":"image/png","size":69,"created_at":"2026-10-03T10:50:14.134347Z"}]
```

9. Delete for good: trash it again, then purge it from the trash. The row goes in a transaction and the bytes once it commits:

```sh
curl -s -X DELETE localhost:8082/photos/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: alice'
curl -s -w ' %{http_code}\n' -X DELETE localhost:8082/trash/fil_misvfjsjbahofoekvgneo7fd4z -H 'X-User: alice'
curl -s localhost:8082/trash -H 'X-User: alice'
```

```text
 204
[]
```

## The code

The configuration and `main`:

<!-- ds:block id=quickstart-consts-5rhugm4x -->
**addr** · [`examples/quickstart/main.go:52`](main.go#L52-L52)

```go
"localhost:8082"
```
<!-- /ds:block hash=cf4a9d -->

<!-- ds:block id=quickstart-names-j55c2mjk -->
**diskName** · [`examples/quickstart/main.go:71`](main.go#L71-L71)

```go
"media"
```
<!-- /ds:block hash=5fbf5f -->

<!-- ds:block id=quickstart-config-web4nsmd -->
**config** · [`examples/quickstart/main.go:98-103`](main.go#L98-L103)

```go
type config struct {
	BaseURL    string
	DataDir    string
	StorageDSN string
	Secret     []byte
}
```
<!-- /ds:block hash=2062c6 -->

<!-- ds:block id=quickstart-main-q476eb9b -->
**main** · [`examples/quickstart/main.go:106-120`](main.go#L106-L120)

```go
func main() {
	cfg := config{BaseURL: baseURL, DataDir: dataDir, StorageDSN: os.Getenv(envStorageDSN), Secret: []byte(os.Getenv(envSecret))}
	if len(cfg.Secret) == 0 {
		cfg.Secret = []byte(rand.Text() + rand.Text())
		log.Printf("%s is not set: using a random secret, so links stop working when the server restarts", envSecret)
	}
	a, err := newApp(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	log.Printf("listening on %s; files and database in %s", cfg.BaseURL, cfg.DataDir)
	srv := &http.Server{Addr: addr, Handler: a.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	log.Fatal(srv.ListenAndServe())
}
```
<!-- /ds:block hash=4883fd -->

Wiring, where any mistake (a bad DSN, a short secret) fails at start-up rather than at the first upload:

<!-- ds:block id=quickstart-newapp-sbmrwauk -->
**newApp** · [`examples/quickstart/main.go:135-156`](main.go#L135-L156)

```go
func newApp(ctx context.Context, cfg config) (*app, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	signer, err := signedurl.New(cfg.BaseURL+filesPrefix, []signedurl.Key{{ID: signingKeyID, Secret: cfg.Secret}})
	if err != nil {
		return nil, err
	}
	disks, err := openDisks(ctx, cfg, signer)
	if err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "app.db"))
	if err != nil {
		return nil, errors.Join(err, disks.Close())
	}
	store, err := openStore(ctx, db, disks)
	if err != nil {
		return nil, errors.Join(err, db.Close(), disks.Close())
	}
	return &app{db: db, disks: disks, store: store, signer: signer}, nil
}
```
<!-- /ds:block hash=b28393 -->

<!-- ds:block id=quickstart-opendisks-8e548zhk -->
**openDisks** · [`examples/quickstart/main.go:162-183`](main.go#L162-L183)

```go
func openDisks(ctx context.Context, cfg config, signer *signedurl.Signer) (*filesystem.Manager, error) {
	mux := filesystem.NewMux()
	if err := errors.Join(mux.Register(local.Scheme, local.Opener), mux.Register(memory.Scheme, memory.Opener)); err != nil {
		return nil, err
	}
	dsn := cfg.StorageDSN
	if dsn == "" {
		dsn = "file:" + filepath.ToSlash(filepath.Join(cfg.DataDir, "files"))
	}
	disk, err := mux.Open(ctx, diskName, dsn, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, errors.Join(err, disk.Close())
	}
	if err := disks.Check(ctx); err != nil {
		return nil, errors.Join(err, disks.Close())
	}
	return disks, nil
}
```
<!-- /ds:block hash=efd17f -->

<!-- ds:block id=quickstart-opendb-hejp8gnt -->
**openDB** · [`examples/quickstart/main.go:188-190`](main.go#L188-L190)

```go
func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
}
```
<!-- /ds:block hash=5a8f70 -->

<!-- ds:block id=quickstart-openstore-nwxqwbhb -->
**openStore** · [`examples/quickstart/main.go:195-204`](main.go#L195-L204)

```go
func openStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	return store, nil
}
```
<!-- /ds:block hash=df0664 -->

What an upload must be to become a photo: an image by its bytes, at most 5 MiB, whose name does not claim another type, under a random key:

<!-- ds:block id=quickstart-policy-zu25btv2 -->
**photoPolicy** · [`examples/quickstart/main.go:215-222`](main.go#L215-L222)

```go
func photoPolicy() upload.Policy {
	return upload.Policy{
		MaxSize:        maxPhotoSize,
		Allowed:        []string{"image/png", "image/jpeg", "image/webp", "image/gif"},
		MatchExtension: true,
		Key:            upload.RandomKey(rolePhotos),
	}
}
```
<!-- /ds:block hash=75cee5 -->

The routes, and the user standing in for authentication:

<!-- ds:block id=quickstart-handler-ys6fdgek -->
**app.Handler** · [`examples/quickstart/main.go:227-241`](main.go#L227-L241)

```go
func (a *app) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /photos", a.uploadPhoto)
	api.HandleFunc("GET /photos", a.listPhotos)
	api.HandleFunc("GET /photos/{id}", a.openPhoto)
	api.HandleFunc("DELETE /photos/{id}", a.trashPhoto)
	api.HandleFunc("POST /photos/{id}/restore", a.restorePhoto)
	api.HandleFunc("GET /trash", a.listTrash)
	api.HandleFunc("DELETE /trash/{id}", a.purgePhoto)

	root := http.NewServeMux()
	root.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(a.disks, a.signer)))
	root.Handle("/", requireUser(api))
	return root
}
```
<!-- /ds:block hash=1fbbcf -->

<!-- ds:block id=quickstart-requireuser-yutht6zb -->
**requireUser** · [`examples/quickstart/main.go:250-260`](main.go#L250-L260)

```go
func requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get(userHeader)
		if user == "" {
			writeJSON(w, http.StatusUnauthorized, upload.ErrorResponse{Error: "sign in first: send the " + userHeader + " header", Code: filesystem.CodePermission})
			return
		}
		ctx := records.WithActor(context.WithValue(r.Context(), userKey{}, user), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```
<!-- /ds:block hash=c83ee8 -->

<!-- ds:block id=quickstart-currentuser-m487796w -->
**currentUser** · [`examples/quickstart/main.go:266-269`](main.go#L266-L269)

```go
func currentUser(r *http.Request) string {
	user, _ := r.Context().Value(userKey{}).(string) // set by requireUser for every route that calls this
	return user
}
```
<!-- /ds:block hash=0e978a -->

Upload, streamed from the connection to the disk and attached to the user in one transaction:

<!-- ds:block id=quickstart-uploadphoto-xwydn32c -->
**app.uploadPhoto** · [`examples/quickstart/main.go:273-287`](main.go#L273-L287)

```go
func (a *app) uploadPhoto(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoSize+formOverhead)
	part, err := formFile(r)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = part.Close() }()
	f, err := a.addPhoto(r.Context(), currentUser(r), part, part.FileName())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, photoOf(f))
}
```
<!-- /ds:block hash=9bac50 -->

<!-- ds:block id=quickstart-formfile-jwqeg25d -->
**formFile** · [`examples/quickstart/main.go:292-307`](main.go#L292-L307)

```go
func formFile(r *http.Request) (*multipart.Part, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "send a multipart form with the photo in the field %q", formField)
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "no form field %q", formField)
		}
		if part.FormName() == formField {
			return part, nil
		}
		_ = part.Close()
	}
}
```
<!-- /ds:block hash=7887c2 -->

<!-- ds:block id=quickstart-addphoto-rwxdsegc -->
**app.addPhoto** · [`examples/quickstart/main.go:313-324`](main.go#L313-L324)

```go
func (a *app) addPhoto(ctx context.Context, user string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := a.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.store.Upload(ctx, user, diskName, body, filename, photoPolicy()); err != nil {
			return err
		}
		_, err = a.store.Attach(ctx, user, f.ID, records.Owner{Type: ownerUser, ID: user}, rolePhotos)
		return err
	})
	return f, err
}
```
<!-- /ds:block hash=fa4507 -->

Listing and opening:

<!-- ds:block id=quickstart-photo-c3f4dmpd -->
**photo** · [`examples/quickstart/main.go:329-335`](main.go#L329-L335)

```go
type photo struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}
```
<!-- /ds:block hash=830006 -->

<!-- ds:block id=quickstart-listphotos-j8nb4nzd -->
**app.listPhotos** · [`examples/quickstart/main.go:345-357`](main.go#L345-L357)

```go
func (a *app) listPhotos(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	list, err := a.store.Attachments(r.Context(), user, records.Owner{Type: ownerUser, ID: user}, rolePhotos)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]photo, 0, len(list))
	for _, af := range list {
		out = append(out, photoOf(af.File))
	}
	writeJSON(w, http.StatusOK, out)
}
```
<!-- /ds:block hash=ed3709 -->

<!-- ds:block id=quickstart-openphoto-vs3hcttn -->
**app.openPhoto** · [`examples/quickstart/main.go:363-378`](main.go#L363-L378)

```go
func (a *app) openPhoto(w http.ResponseWriter, r *http.Request) {
	f, err := a.store.File(r.Context(), currentUser(r), r.PathValue("id"))
	if err == nil && f.DeletedAt != nil {
		err = filesystem.Errorf(filesystem.CodeNotFound, "the photo is in the trash")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := a.store.URL(r.Context(), f, linkLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, link, http.StatusFound)
}
```
<!-- /ds:block hash=98d4b0 -->

Trash, restore and purge:

<!-- ds:block id=quickstart-trashphoto-khwwnesx -->
**app.trashPhoto** · [`examples/quickstart/main.go:382-388`](main.go#L382-L388)

```go
func (a *app) trashPhoto(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Trash(r.Context(), currentUser(r), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=9c97fc -->

<!-- ds:block id=app-restorephoto-h7tavh77 -->
**app.restorePhoto** · [`examples/quickstart/main.go:393-399`](main.go#L393-L399)

```go
func (a *app) restorePhoto(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Restore(r.Context(), currentUser(r), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=451f92 -->

<!-- ds:block id=quickstart-listtrash-pzv93qp4 -->
**app.listTrash** · [`examples/quickstart/main.go:404-415`](main.go#L404-L415)

```go
func (a *app) listTrash(w http.ResponseWriter, r *http.Request) {
	page, err := a.store.Files(r.Context(), records.FileQuery{Scope: currentUser(r), Trashed: true})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]photo, 0, len(page.Files))
	for _, f := range page.Files {
		out = append(out, photoOf(f))
	}
	writeJSON(w, http.StatusOK, out)
}
```
<!-- /ds:block hash=a752ef -->

<!-- ds:block id=quickstart-purgephoto-8f9jkzp7 -->
**app.purgePhoto** · [`examples/quickstart/main.go:421-435`](main.go#L421-L435)

```go
func (a *app) purgePhoto(w http.ResponseWriter, r *http.Request) {
	user, id := currentUser(r), r.PathValue("id")
	f, err := a.store.File(r.Context(), user, id)
	if err == nil && f.DeletedAt == nil {
		err = filesystem.Errorf(filesystem.CodeInvalidArgument, "move the photo to the trash first")
	}
	if err == nil {
		err = a.store.Purge(r.Context(), user, id)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=b37358 -->

Errors: the status from the code, and no internal detail for a server-side failure:

<!-- ds:block id=quickstart-writeerror-ajcm3vft -->
**writeError** · [`examples/quickstart/main.go:441-449`](main.go#L441-L449)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	if status >= http.StatusInternalServerError {
		log.Printf("request failed: %v", err)
		writeJSON(w, status, upload.ErrorResponse{Error: http.StatusText(status), Code: filesystem.CodeOf(err)})
		return
	}
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=5f94b8 -->

The test server and the test that walks one photo through its whole life:

<!-- ds:block id=quickstart-testserver-6kckt4sx -->
**testServer** · [`examples/quickstart/main_test.go:28-40`](main_test.go#L28-L40)

```go
func testServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	a, err := newApp(t.Context(), config{BaseURL: srv.URL, DataDir: dir, Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	srv.Config.Handler = a.Handler()
	return srv, dir
}
```
<!-- /ds:block hash=e39512 -->

<!-- ds:block id=quickstart-lifecycle-test-murxdrz5 -->
**TestPhotoLifecycle** · [`examples/quickstart/main_test.go:110-141`](main_test.go#L110-L141)

```go
func TestPhotoLifecycle(t *testing.T) {
	srv, dir := testServer(t)
	img := pngBytes(t)

	status, body := uploadAs(t, srv, "alice", "beach.png", img)
	if status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, body)
	}
	p := decode[photo](t, body)
	if p.ContentType != "image/png" || p.Size != int64(len(img)) || p.Filename != "beach.png" {
		t.Fatalf("uploaded %+v", p)
	}
	stored, err := filepath.Glob(filepath.Join(dir, "files", rolePhotos, "*.png"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored files = %v, %v", stored, err)
	}

	if _, body = do(t, srv, http.MethodGet, "/photos", "alice", nil, ""); len(decode[[]photo](t, body)) != 1 {
		t.Fatalf("alice's photos = %s", body)
	}
	if _, body = do(t, srv, http.MethodGet, "/photos", "bob", nil, ""); len(decode[[]photo](t, body)) != 0 {
		t.Fatalf("bob sees %s", body)
	}
	if status, _ = do(t, srv, http.MethodGet, "/photos/"+p.ID, "bob", nil, ""); status != http.StatusNotFound {
		t.Fatalf("bob opening alice's photo = %d", status)
	}
	if status, body = do(t, srv, http.MethodGet, "/photos/"+p.ID, "alice", nil, ""); status != http.StatusOK || !bytes.Equal(body, img) {
		t.Fatalf("download through the signed link = %d, %d bytes", status, len(body))
	}

	trashAndPurge(t, srv, p.ID, stored[0])
}
```
<!-- /ds:block hash=157f73 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The app: configuration, wiring, upload policy, routes and handlers |
| `main_test.go` | The photo lifecycle through `httptest`, the refusals leaving nothing on disk, the same app on a `mem://` DSN, and bad wiring failing at start-up |

## Next

- The guide: [Your first app](../../docs/start/first-app.md), then [Test your app](../../docs/start/testing.md) and [Production](../../docs/start/production.md).
- Related examples: [start-install](../start-install) for a first disk, [start-testing](../start-testing), [start-production](../start-production), [records-http](../records-http) for a ready-made JSON API, [recipe-avatars](../recipe-avatars).
