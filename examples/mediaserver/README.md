# Media server: user avatars on disk and in SQLite

The file handling of a social or SaaS backend, written by hand: a user uploads an avatar, the server checks it against a policy (images only, 2 MiB, type taken from the bytes), stores it on a local disk under a random name, records it in SQLite, attaches it to the user as their single avatar, and hands out five-minute signed links to it. It uses the core `filesystem` package with `local`, `signedurl`, `fshttp` and `upload`, plus the `records` module on `ubgo/sqlb`. It is listed in the [recipes index](../../docs/recipes/README.md); the [avatars recipe](../../docs/recipes/avatars.md) is the fuller version with variants and cropping.

## Run it

```sh
cd examples
go run ./mediaserver -addr localhost:18080
go test ./mediaserver
```

`-addr` is the listen address (default `localhost:8080`), and the public URL the signed links are built from is `http://<addr>`. It creates a temporary data directory, printed at start-up, holding `files/` and `app.db`; the signing secret is a constant in `main.go`. Stop it with Ctrl-C. Ctrl-C or SIGTERM shuts it down cleanly and removes the data directory. The test runs the same `New` behind an `httptest` server instead.

## Walkthrough

```text
listening on localhost:18080 ; data in /var/folders/sk/k6vkn27d2xbcxcj1ll6dvwjw0000gn/T/mediaserver-1198159115
```

1. Upload an avatar for user 42. The response names the new file and the content type sniffed from the bytes:

```sh
A=localhost:18080
curl -s -F file=@me.png $A/users/42/avatar
```

```json
{"content_type":"image/png","file":"fil_png4sbo6oqkajxditszppmvavg"}
```

2. A file that is not an image is refused before anything is stored, whatever its name claims:

```sh
curl -s -F file=@notes.txt $A/users/42/avatar
```

```text
Unsupported Media Type
```

3. Reading the avatar redirects to a short-lived signed link, served by `fshttp` under `/files/`:

```sh
curl -s -i $A/users/42/avatar
```

```text
HTTP/1.1 302 Found
Location: http://localhost:18080/files/media/avatars/ispcw7ms2hu4ycmprvzytldt43.png?ct=image%2Fpng&exp=1791025272&kid=k1&m=GET&sig=34a03ff03969f28dc5522c115ea7e6440d63daa612fd546b9e88261c628fa6e5
```

Following the link returns the image (`200 image/png`, 131 bytes); changing the last character of the signature returns `403 Forbidden`.

4. Uploading again replaces the avatar in one transaction: the new file becomes the only attachment in the `avatar` role and the old file is moved to the trash. A user with no avatar gets `404`.

```sh
curl -s -F file=@logo.png $A/users/42/avatar
curl -s -o /dev/null -w '%{http_code}\n' $A/users/7/avatar
```

```text
{"content_type":"image/png","file":"fil_hfzpncbbvwp5gh6krkmmsl64x6"}
404
```

## The code

`New` wires a local disk with a URL signer, a Manager, a SQLite records Store with `user` as an owner type and `avatar` as a single role, and the three routes.

<!-- ds:block id=new-e4n8hw3h -->
**New** · [`examples/mediaserver/main.go:67-101`](main.go#L67-L101)

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
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser), records.WithSingleRoles(roleAvatar))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	app := &App{Store: store, Disks: disks, Mux: http.NewServeMux()}
	app.Mux.HandleFunc("POST /users/{id}/avatar", app.uploadAvatar)
	app.Mux.HandleFunc("GET /users/{id}/avatar", app.avatar)
	app.Mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	return app, nil
}
```
<!-- /ds:block hash=3a3d74 -->

`uploadAvatar` uploads and attaches inside `Store.Tx`. If any step fails the transaction rolls back and the uploaded object is deleted, and because the role is single, two concurrent uploads cannot leave a user with two avatars.

<!-- ds:block id=app-uploadavatar-jqtqcs3t -->
**App.uploadAvatar** · [`examples/mediaserver/main.go:110-135`](main.go#L110-L135)

```go
func (a *App) uploadAvatar(w http.ResponseWriter, r *http.Request) {
	user := records.Owner{Type: ownerUser, ID: r.PathValue("id")}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarSize+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "send the image in the form field \"file\"", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	var stored records.File
	err = a.Store.Tx(r.Context(), func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if stored, err = a.Store.Upload(ctx, "", diskName, file, header.Filename, avatars); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, "", stored.ID, user, roleAvatar, records.PreviousTrash)
		return err
	})
	if err != nil {
		http.Error(w, http.StatusText(fshttp.StatusOf(err)), fshttp.StatusOf(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"file": stored.ID, "content_type": stored.ContentType})
}
```
<!-- /ds:block hash=8dbd7c -->

`avatar` looks up the attachment and redirects to a signed URL that expires after five minutes.

<!-- ds:block id=app-avatar-u35yeadu -->
**App.avatar** · [`examples/mediaserver/main.go:139-151`](main.go#L139-L151)

```go
func (a *App) avatar(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerUser, ID: r.PathValue("id")}, roleAvatar)
	if err != nil || len(list) == 0 {
		http.NotFound(w, r)
		return
	}
	link, err := a.Store.URL(r.Context(), list[0].File, linkLifetime)
	if err != nil {
		http.Error(w, http.StatusText(fshttp.StatusOf(err)), fshttp.StatusOf(err))
		return
	}
	http.Redirect(w, r, link, http.StatusFound)
}
```
<!-- /ds:block hash=1f3396 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The upload policy, `New`, the two handlers and `main` |
| `main_test.go` | The avatar flow over HTTP: upload, signed download, replace, and the old file in the trash |

## Next

- The fuller flow with variants and cropping: [avatars recipe](../../docs/recipes/avatars.md) and [recipe-avatars](../recipe-avatars).
- The pieces it uses: [uploads](../../docs/concepts/uploads.md), [serving files](../../docs/concepts/serving-files.md), [attachments](../../docs/records/attachments.md).
- Every records plugin on one Store: [drive](../drive).
