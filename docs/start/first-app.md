# Your first app

You will have a running Go program where a signed-in user uploads photos, checked by their bytes and capped in size; each photo is stored on local disk, recorded as a row in SQLite and attached to the user; the user lists their photos, opens one through a signed link that expires, moves one to the trash, restores it, and deletes it for good. The program is [examples/quickstart](../../examples/quickstart) in the repository; every code block below is copied from it, and every transcript is from running it.

## Before you start

- [Install](install.md): Go 1.26 (the `records` module needs it), the core module, `records`, `sqlb` and the SQLite driver.
- `curl`, and any PNG or JPEG photo. The transcripts use a small PNG called `beach.png`.
- Nothing else: the bytes go to a directory, the rows to a SQLite file, and the program serves its own signed links.

## Step 1 — Start a module

```sh
mkdir photos && cd photos
go mod init example.com/photos
go get github.com/ubgo/filesystem@latest github.com/ubgo/filesystem/records@latest github.com/ubgo/sqlb modernc.org/sqlite
```

Check: `go.mod` lists the four modules. The core is in the same command as `records` on purpose; [install](install.md) explains why.

## Step 2 — Decide the constants

Two kinds of fixed values. The first are about where the program runs; in production they come from the environment ([production](production.md)):

<!-- ds:block id=quickstart-consts-5rhugm4x -->
**addr** · [`examples/quickstart/main.go:52`](../../examples/quickstart/main.go#L52-L52)

```go
"localhost:8082"
```
<!-- /ds:block hash=cf4a9d -->

The second are the app's own decisions: the disk's name, where links are served, what a photo may be. The library has no opinion on any of them, which is why they are yours:

<!-- ds:block id=quickstart-names-j55c2mjk -->
**diskName** · [`examples/quickstart/main.go:71`](../../examples/quickstart/main.go#L71-L71)

```go
"media"
```
<!-- /ds:block hash=5fbf5f -->

`userHeader` deserves a warning now: this tutorial takes the signed-in user from an `X-User` header so it can focus on files. A real app gets the user from its session (for example from [ubgo/auth](https://github.com/ubgo/auth)); a header any client can set is not authentication.

## Step 3 — Open the disk

The disk is opened from a DSN through a `Mux`, so moving to S3 later is a configuration change ([production](production.md), step 1). The `Mux` knows only the drivers you register: here the local directory driver and the memory driver.

<!-- ds:block id=quickstart-opendisks-8e548zhk -->
**openDisks** · [`examples/quickstart/main.go:162-183`](../../examples/quickstart/main.go#L162-L183)

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

Three things happen here that matter later:

- `filesystem.WithURLSigner(signer)` gives the disk a way to sign links. S3 signs its own URLs; a local directory cannot, so the `signedurl` package signs them with an HMAC key and the program serves them itself (step 7).
- The disk goes into a `Manager`, the app's named set of disks. `records` reaches bytes through it, and `fshttp` finds a link's disk by name in it.
- `Check` writes a small object, reads it back, compares and deletes it. A wrong path, a read-only directory or a bad credential fails the start-up instead of the first user's upload.

## Step 4 — Open the database and the records store

`records` keeps one row per file (`fs_files`) and one per attachment of a file to something of yours (`fs_attachments`). It runs on SQLite, Postgres and MySQL and never imports a driver; the app opens the `*sql.DB`.

<!-- ds:block id=quickstart-opendb-hejp8gnt -->
**openDB** · [`examples/quickstart/main.go:188-190`](../../examples/quickstart/main.go#L188-L190)

```go
func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
}
```
<!-- /ds:block hash=5a8f70 -->

<!-- ds:block id=quickstart-openstore-nwxqwbhb -->
**openStore** · [`examples/quickstart/main.go:195-204`](../../examples/quickstart/main.go#L195-L204)

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

`records.WithOwnerTypes(ownerUser)` is the list of things a file may be attached to. Attaching to any other type, such as a typo like `"usr"`, is an error instead of an attachment to nothing. `Migrate` creates the tables and records each migration in `fs_migrations`; running it again does nothing. Migrating at start-up suits one process; [production](production.md) moves it into the deploy.

## Step 5 — Wire it together

Everything the app needs from outside goes in one struct, so `main` fills it from constants and the environment and a test fills it from a temporary directory:

<!-- ds:block id=quickstart-config-web4nsmd -->
**config** · [`examples/quickstart/main.go:98-103`](../../examples/quickstart/main.go#L98-L103)

```go
type config struct {
	BaseURL    string
	DataDir    string
	StorageDSN string
	Secret     []byte
}
```
<!-- /ds:block hash=2062c6 -->

<!-- ds:block id=quickstart-newapp-sbmrwauk -->
**newApp** · [`examples/quickstart/main.go:135-156`](../../examples/quickstart/main.go#L135-L156)

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

<!-- ds:block id=quickstart-main-q476eb9b -->
**main** · [`examples/quickstart/main.go:106-120`](../../examples/quickstart/main.go#L106-L120)

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

The signing secret must be at least 32 bytes (`signedurl.MinKeyBytes`); a shorter one fails `newApp`. Without `QUICKSTART_SECRET` the program makes a random one, which is fine for a tutorial but means every link it handed out stops working when it restarts. Production keeps the secret in a secret store and can rotate it ([production](production.md), step 2).

## Step 6 — Say what a photo is

An upload policy is the whole answer to "what may this endpoint store". Every upload path in the library enforces it on the server, against the bytes actually received:

<!-- ds:block id=quickstart-policy-zu25btv2 -->
**photoPolicy** · [`examples/quickstart/main.go:215-222`](../../examples/quickstart/main.go#L215-L222)

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

- **The type comes from the content.** The first bytes are sniffed; the client's `Content-Type` header and the file name are never trusted. A text file named `cat.png` is `text/plain`.
- **`MatchExtension`** refuses a file whose name claims another type than its bytes, such as a PNG named `beach.jpg`, so a download never arrives with a misleading extension.
- **`MaxSize`** is counted while the bytes stream. Nothing larger than the limit is ever stored, whatever the request's `Content-Length` says.
- **Active content is refused by default.** HTML, SVG, XML, executables and script names (`.php`, also hidden as `photo.php.jpg`) are refused even if `Allowed` would match them, unless the policy names the type in `AllowActive` (as `contrib/svg` does for SVG) or sets `AllowActiveContent`, which lifts every one of these rules.
- **`RandomKey(rolePhotos)`** stores each photo as `photos/<random>.<extension of the detected type>`. The client's file name is kept in the row for display, made safe with `upload.SafeFilename`, and never reaches the key, so two users uploading `IMG_0001.jpg` cannot collide and a name like `../../etc/passwd` cannot travel anywhere.

## Step 7 — Routes and the signed-in user

<!-- ds:block id=quickstart-handler-ys6fdgek -->
**app.Handler** · [`examples/quickstart/main.go:227-241`](../../examples/quickstart/main.go#L227-L241)

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

The photo routes sit behind `requireUser`. The signed links under `/files/` do not: a signed link is its own authorisation, checked by `fshttp.Signed`, which refuses a missing, altered or expired signature with a bare `403`.

<!-- ds:block id=quickstart-requireuser-yutht6zb -->
**requireUser** · [`examples/quickstart/main.go:250-260`](../../examples/quickstart/main.go#L250-L260)

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

`records.WithActor` puts the user in the context, and the store records it as `uploaded_by` on every file created with that context; the `audit` plugin logs it too. Set it once, in the middleware that authenticates.

<!-- ds:block id=quickstart-currentuser-m487796w -->
**currentUser** · [`examples/quickstart/main.go:266-269`](../../examples/quickstart/main.go#L266-L269)

```go
func currentUser(r *http.Request) string {
	user, _ := r.Context().Value(userKey{}).(string) // set by requireUser for every route that calls this
	return user
}
```
<!-- /ds:block hash=0e978a -->

The user is also the **scope** of every call below. A scope is the tenant, workspace or user a file belongs to; every query of the store filters by it. Passing the user as the scope is what keeps users apart: when Bob asks for Alice's file ID, the store looks only among Bob's files and answers not found. In a multi-tenant product the scope is usually the workspace and the user is the actor ([multi-tenancy](../concepts/multi-tenancy.md)).

## Step 8 — Accept an upload

The handler caps the request body, finds the file in the form, and hands the stream to `addPhoto`:

<!-- ds:block id=quickstart-uploadphoto-xwydn32c -->
**app.uploadPhoto** · [`examples/quickstart/main.go:273-287`](../../examples/quickstart/main.go#L273-L287)

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

`r.MultipartReader` reads the form as a stream. `r.FormFile` would be shorter, but it first copies the whole upload into memory or a temporary file; with `MultipartReader` the bytes go from the connection to the disk with no copy in between.

<!-- ds:block id=quickstart-formfile-jwqeg25d -->
**formFile** · [`examples/quickstart/main.go:292-307`](../../examples/quickstart/main.go#L292-L307)

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

The upload and the attachment happen in one transaction:

<!-- ds:block id=quickstart-addphoto-rwxdsegc -->
**app.addPhoto** · [`examples/quickstart/main.go:313-324`](../../examples/quickstart/main.go#L313-L324)

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

`store.Upload` runs the policy (sniff, check, choose the key, stream with the size cap), writes the object, then inserts the row. Bytes and rows cannot share a transaction, so the store keeps them consistent with an order: object first, row second. If anything after the write fails, here the `Attach`, the transaction rolls back and the store deletes the object it wrote, so a failed upload leaves neither a row nor bytes. The [consistency](../concepts/consistency.md) page has every case.

The API never shows a storage key; clients address photos by ID:

<!-- ds:block id=quickstart-photo-c3f4dmpd -->
**photo** · [`examples/quickstart/main.go:329-335`](../../examples/quickstart/main.go#L329-L335)

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

Errors carry a code, and one function turns any of them into an HTTP answer. `fshttp.StatusOf` maps each code to its status (`rejected` is 415, `too_large` 413, `not_found` 404); a server-side failure is logged and the client sees only the status text:

<!-- ds:block id=quickstart-writeerror-ajcm3vft -->
**writeError** · [`examples/quickstart/main.go:441-449`](../../examples/quickstart/main.go#L441-L449)

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

Run it:

```sh
go run .
```

```text
2026/10/03 14:45:10 QUICKSTART_SECRET is not set: using a random secret, so links stop working when the server restarts
2026/10/03 14:45:10 listening on http://localhost:8082; files and database in quickstart-data
```

Upload a photo as Alice:

```sh
curl -s -X POST localhost:8082/photos -H 'X-User: alice' -F file=@beach.png
```

```json
{"id":"fil_ptarabviehp5vztzd56i4rqrtc","filename":"beach.png","content_type":"image/png","size":141,"created_at":"2026-10-03T09:15:20.29651Z"}
```

Now the refusals. A text file named `notes.png`, the PNG renamed `beach.jpg`, and a PNG with 6 MB of padding (`cp beach.png huge.png && head -c 6000000 /dev/zero >> huge.png`):

```sh
curl -s -X POST localhost:8082/photos -H 'X-User: alice' -F file=@notes.png
curl -s -X POST localhost:8082/photos -H 'X-User: alice' -F file=@beach.jpg
curl -s -X POST localhost:8082/photos -H 'X-User: alice' -F file=@huge.png
```

```json
{"error":"content type \"text/plain; charset=utf-8\" is not allowed","code":"rejected"}
{"error":"the name says image/jpeg but the content is image/png","code":"rejected"}
{"error":"body exceeds 5242880 bytes","code":"too_large"}
```

The statuses are `415`, `415` and `413`. Without the header:

```sh
curl -s -i localhost:8082/photos
```

```text
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Date: Sat, 03 Oct 2026 09:15:20 GMT
Content-Length: 70

{"error":"sign in first: send the X-User header","code":"permission"}
```

Check: one file is stored, and it is the accepted one. The refused uploads left nothing, not even the first 5 MiB of the large one:

```sh
ls quickstart-data/files/photos
sqlite3 -header quickstart-data/app.db "select id, scope, disk, storage_key, filename, content_type, size, uploaded_by from fs_files"
sqlite3 -header quickstart-data/app.db "select file_id, owner_type, owner_id, role, position from fs_attachments"
```

```text
vpvw7hqz2k63zneao5523asqkn.png
id|scope|disk|storage_key|filename|content_type|size|uploaded_by
fil_ptarabviehp5vztzd56i4rqrtc|alice|media|photos/vpvw7hqz2k63zneao5523asqkn.png|beach.png|image/png|141|alice
file_id|owner_type|owner_id|role|position
fil_ptarabviehp5vztzd56i4rqrtc|user|alice|photos|1
```

Reading the `fs_` tables from your own queries is fine; change them only through the store, where the invariants live.

## Step 9 — List a user's photos

<!-- ds:block id=quickstart-listphotos-j8nb4nzd -->
**app.listPhotos** · [`examples/quickstart/main.go:345-357`](../../examples/quickstart/main.go#L345-L357)

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

`Attachments` returns the files attached to an owner under a role, in position order, each with its file row. Trashed files are left out.

```sh
curl -s localhost:8082/photos -H 'X-User: alice'
curl -s localhost:8082/photos -H 'X-User: bob'
```

```json
[{"id":"fil_ptarabviehp5vztzd56i4rqrtc","filename":"beach.png","content_type":"image/png","size":141,"created_at":"2026-10-03T09:15:20.29651Z"}]
[]
```

## Step 10 — Open a photo through a signed link

The file is private: there is no URL that serves it to anyone. To show it, the app hands out a link signed for this one object that works for five minutes:

<!-- ds:block id=quickstart-openphoto-vs3hcttn -->
**app.openPhoto** · [`examples/quickstart/main.go:363-378`](../../examples/quickstart/main.go#L363-L378)

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

`store.URL` returns the public URL for a public file and a signed one otherwise; `store.DownloadURL` signs a link that makes the browser save the file under its original name instead of showing it.

```sh
curl -s -i localhost:8082/photos/fil_ptarabviehp5vztzd56i4rqrtc -H 'X-User: alice'
```

```text
HTTP/1.1 302 Found
Content-Type: text/html; charset=utf-8
Location: http://localhost:8082/files/media/photos/vpvw7hqz2k63zneao5523asqkn.png?ct=image%2Fpng&exp=1791019225&kid=k1&m=GET&sig=c62e928fdd42c880999afe4d3bc9239cf65bf01b44bd2a4bdff62715f4f042d2
Date: Sat, 03 Oct 2026 09:15:25 GMT
Content-Length: 222
```

The link names the disk and the key, the method (`m`), the expiry as a Unix time (`exp`), the content type to answer with (`ct`) and the signing key's ID (`kid`). The signature covers all of them, so none can be changed. Following it:

```sh
LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8082/photos/fil_ptarabviehp5vztzd56i4rqrtc -H 'X-User: alice')
curl -s -D - -o got.png "$LINK"
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 141
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: image/png
Etag: "18dafa3801bc482a-8d"
Last-Modified: Sat, 03 Oct 2026 09:15:20 GMT
X-Content-Type-Options: nosniff
Date: Sat, 03 Oct 2026 09:15:31 GMT
```

`fshttp` serves ranges (a `Range: bytes=0-7` request answers `206 Partial Content`), ETags and conditional requests. Every response carries `nosniff` and a sandboxing `Content-Security-Policy`, because these are user files served from your own origin; an HTML or SVG file, if a policy ever let one in, is served as a download and cannot run script.

A link with its method changed to `PUT`, or one character of the signature changed, is refused with nothing to learn from:

```text
HTTP/1.1 403 Forbidden
```

Bob asking for Alice's photo gets the same answer as for a photo that does not exist:

```sh
curl -s localhost:8082/photos/fil_ptarabviehp5vztzd56i4rqrtc -H 'X-User: bob'
```

```json
{"error":"records: no file \"fil_ptarabviehp5vztzd56i4rqrtc\"","code":"not_found"}
```

Check: `got.png` holds exactly the bytes of `beach.png` (`cmp got.png beach.png` prints nothing).

## Step 11 — Trash, restore and delete for good

Deleting is two steps, as in most products: the trash, from which a file can come back, then the purge, which is final.

<!-- ds:block id=quickstart-trashphoto-khwwnesx -->
**app.trashPhoto** · [`examples/quickstart/main.go:382-388`](../../examples/quickstart/main.go#L382-L388)

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

A trashed file keeps its row, its bytes and its attachments; `Attachments` and `Files` stop listing it until `Restore`. The trash itself is a `Files` query with `Trashed: true`:

<!-- ds:block id=quickstart-listtrash-pzv93qp4 -->
**app.listTrash** · [`examples/quickstart/main.go:404-415`](../../examples/quickstart/main.go#L404-L415)

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

```sh
curl -s -i -X DELETE localhost:8082/photos/fil_ptarabviehp5vztzd56i4rqrtc -H 'X-User: alice'
curl -s localhost:8082/photos -H 'X-User: alice'
curl -s localhost:8082/trash -H 'X-User: alice'
curl -s localhost:8082/photos/fil_ptarabviehp5vztzd56i4rqrtc -H 'X-User: alice'
```

```text
HTTP/1.1 204 No Content
[]
[{"id":"fil_ptarabviehp5vztzd56i4rqrtc","filename":"beach.png","content_type":"image/png","size":141,"created_at":"2026-10-03T09:15:20.29651Z"}]
{"error":"the photo is in the trash","code":"not_found"}
```

`POST /photos/{id}/restore` answers `204` and the photo is listed again, in its old position. Purging deletes the attachment and the row in one transaction, then the object once that transaction has committed, so a rollback can never leave a row pointing at deleted bytes:

<!-- ds:block id=quickstart-purgephoto-8f9jkzp7 -->
**app.purgePhoto** · [`examples/quickstart/main.go:421-435`](../../examples/quickstart/main.go#L421-L435)

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

```sh
curl -s -X DELETE localhost:8082/trash/fil_ptarabviehp5vztzd56i4rqrtc -H 'X-User: alice'
```

```json
{"error":"move the photo to the trash first","code":"invalid_argument"}
```

After moving it to the trash again, the same request answers `204 No Content`, and:

```sh
curl -s localhost:8082/trash -H 'X-User: alice'
ls quickstart-data/files/
sqlite3 quickstart-data/app.db "select count(*) from fs_files; select count(*) from fs_attachments"
```

```text
[]

0
0
```

Check: the directory is empty. The local driver removes directories a delete leaves empty, so the tree mirrors the keys. In production nobody purges by hand: `store.SweepTrash` purges everything trashed before a cutoff, on a schedule ([production](production.md), step 5).

## Step 12 — Test it

The repository's test for this program starts it on a temporary directory behind `httptest` and walks a photo through the same life, so none of the above can silently break:

<!-- ds:block id=quickstart-testserver-6kckt4sx -->
**testServer** · [`examples/quickstart/main_test.go:28-40`](../../examples/quickstart/main_test.go#L28-L40)

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
**TestPhotoLifecycle** · [`examples/quickstart/main_test.go:110-141`](../../examples/quickstart/main_test.go#L110-L141)

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

```sh
cd examples && go test ./quickstart
```

[Test your app](testing.md) shows the other tools: a fake disk with assertions, injected failures, a movable clock.

## What can go wrong

| Symptom | Fix |
|---|---|
| A link that worked stops working after a restart | The secret was random. Set `QUICKSTART_SECRET` to 32 bytes or more; in production use a fixed key from a secret store. |
| A link still works after the photo was trashed | A signed link authorises the object until it expires; trashing does not touch the bytes. Keep link lifetimes short. After a purge the link answers `404`. |
| `{"error":"…is not allowed","code":"rejected"}` for a real photo | The detected type is not in `Allowed`. HEIC photos from iPhones are `image/heic`; add it if you want them (browsers cannot display them). |
| `{"error":"the name says image/jpeg but the content is image/png",…}` | `MatchExtension` at work: the file's name lies about its content. Rename it, or drop `MatchExtension` if you accept that. |
| `413` with `too_large` for a file under the limit | The request body is capped at `maxPhotoSize` plus `formOverhead`; a form carrying other large fields before the file can pass it. |
| `database is locked` | The SQLite DSN lacks `busy_timeout`. Use the one from `openDB`. |
| `records: owner type "…" is not registered (WithOwnerTypes)` | Attach only to types passed to `records.WithOwnerTypes`. |

## Next

- [Production](production.md): S3 or R2 from the environment, secrets and rotation, migrations in deploys, scheduled sweeps, quotas, scanning, health.
- [Test your app](testing.md): fakes, faults and clocks.
- [Uploads](../concepts/uploads.md) for direct-to-bucket and resumable uploads, [serving files](../concepts/serving-files.md) for public files and CDNs, and [records](../records/README.md) for roles, single roles such as avatars, replacing a file and the plugins.
