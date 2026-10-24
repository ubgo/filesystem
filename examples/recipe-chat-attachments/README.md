# Chat attachments: files on messages, visible to members only

The file side of a chat or messaging app: the author of a message attaches files to it, every member of the conversation can list and open them through short-lived signed links, nobody else can, and deleting the message deletes its files in the same transaction as the message row. It mounts `records/recordshttp` with the app's own `Scope` and `Authorize` hooks, keeps files private on a local disk signed with `signedurl` and served by `fshttp.Signed`, stores rows with `records` on SQLite, and uses `Store.Tx` with `PurgeOwner` so the message and its files go together. The guide is [Chat attachments](../../docs/recipes/chat-attachments.md).

## Run it

```sh
cd examples
go run ./recipe-chat-attachments
```

It creates a new temporary directory (printed at start-up) holding `chat.db` (SQLite: the app's `conversation_members` and `messages` tables plus the `fs_` tables) and `files/`, seeds two conversations (`general` with alice and bob, `leads` with carol) and listens on `http://localhost:8109`. The signed-in user is the `X-User` header, a demo shortcut: a real app takes it from its session. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Run the tests with `go test ./recipe-chat-attachments`.

## Walkthrough

The files used below: a small PDF named `Angebot Müller.pdf`, a 4×4 `photo.png`, and `page.html`.

1. alice posts a message in `general`; carol, who is not a member, cannot:

```sh
curl -s -X POST localhost:8109/conversations/general/messages -H 'X-User: alice' -d '{"body":"the offer and a photo"}'
curl -s -X POST localhost:8109/conversations/general/messages -H 'X-User: carol' -d '{"body":"hi"}'
```

```text
{"id":"msg_1c8e6422d80edb3cccb5"}
{"error":"Forbidden","code":"permission"}
```

2. The author attaches two files; bob, a member but not the author, cannot attach:

```sh
curl -s -X POST localhost:8109/media/owners/message/msg_1c8e6422d80edb3cccb5/attachments -H 'X-User: alice' -F 'file=@Angebot Müller.pdf' -F file=@photo.png
curl -s -X POST localhost:8109/media/owners/message/msg_1c8e6422d80edb3cccb5/attachments -H 'X-User: bob' -F file=@photo.png
```

```text
{"files":[{"attachment_id":"att_z6bvj2hkrv5dlmgxfst4jaxzfp","file_id":"fil_dfgsc4mxxeteoahtyjz57xhdul","filename":"Angebot Müller.pdf","content_type":"application/pdf","size":14,"position":1,"status":"ready"},{"attachment_id":"att_eqagzdt6uf2lg36g44zox4xdzs","file_id":"fil_nktfqvsgsovhrkdnr4jbzeyahk","filename":"photo.png","content_type":"image/png","size":83,"position":2,"status":"ready"}]}
{"error":"forbidden","code":"permission"}
```

3. One refused file sinks the whole request, the good one with it (HTML is never allowed):

```sh
curl -s -X POST localhost:8109/media/owners/message/msg_1c8e6422d80edb3cccb5/attachments -H 'X-User: alice' -F file=@photo.png -F file=@page.html
```

```json
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
```

4. A member lists the attachments with signed links that work for five minutes; carol is refused:

```sh
curl -s localhost:8109/media/owners/message/msg_1c8e6422d80edb3cccb5/attachments -H 'X-User: bob'
curl -s localhost:8109/media/owners/message/msg_1c8e6422d80edb3cccb5/attachments -H 'X-User: carol'
```

```text
{"files":[{"attachment_id":"att_z6bvj2hkrv5dlmgxfst4jaxzfp","file_id":"fil_dfgsc4mxxeteoahtyjz57xhdul","filename":"Angebot Müller.pdf","content_type":"application/pdf","size":14,"position":1,"url":"http://localhost:8109/signed/chat/chat/7mhi5rpjj6grxjn2j4k7hqvjet.pdf?ct=application%2Fpdf\u0026exp=1791268121\u0026kid=k1\u0026m=GET\u0026sig=7709090079fe70436b6b00424617e4b905a5b8d76938e5c4a5c4b41549e1c9e0","status":"ready"},{"attachment_id":"att_eqagzdt6uf2lg36g44zox4xdzs","file_id":"fil_nktfqvsgsovhrkdnr4jbzeyahk","filename":"photo.png","content_type":"image/png","size":83,"position":2,"url":"http://localhost:8109/signed/chat/chat/glo3ugf2d4o4ywjhwbdyi4m7yi.png?ct=image%2Fpng\u0026exp=1791268121\u0026kid=k1\u0026m=GET\u0026sig=54e407a9ca565c16871afefb262443da728b6d7dec77c6aca9952b331b4f9b97","status":"ready"}]}
{"error":"forbidden","code":"permission"}
```

5. Open one file (a redirect to a signed link), and save it under its original name through `/downloads`:

```sh
curl -s -i localhost:8109/media/files/fil_dfgsc4mxxeteoahtyjz57xhdul -H 'X-User: bob' | grep -E '^(HTTP|Location)'
curl -s localhost:8109/media/files/fil_dfgsc4mxxeteoahtyjz57xhdul -H 'X-User: carol'
D=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8109/downloads/fil_dfgsc4mxxeteoahtyjz57xhdul -H 'X-User: bob')
curl -s -D - -o /dev/null "$D" | grep -iE '^(HTTP|Content-Disposition|Content-Type)'
```

```text
HTTP/1.1 302 Found
Location: http://localhost:8109/signed/chat/chat/7mhi5rpjj6grxjn2j4k7hqvjet.pdf?ct=application%2Fpdf&exp=1791268121&kid=k1&m=GET&sig=7709090079fe70436b6b00424617e4b905a5b8d76938e5c4a5c4b41549e1c9e0
{"error":"forbidden","code":"permission"}
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="Angebot M_ller.pdf"; filename*=UTF-8''Angebot%20M%C3%BCller.pdf
Content-Type: application/pdf
```

6. A single file cannot be trashed; the message can be deleted, by its author only, and the download link signed a moment earlier now answers `404` because the object is gone:

```sh
curl -s -X DELETE localhost:8109/media/files/fil_dfgsc4mxxeteoahtyjz57xhdul -H 'X-User: alice'
curl -s -X DELETE localhost:8109/messages/msg_1c8e6422d80edb3cccb5 -H 'X-User: bob'
curl -s -i -X DELETE localhost:8109/messages/msg_1c8e6422d80edb3cccb5 -H 'X-User: alice' | head -1
curl -s -o /dev/null -w '%{http_code}\n' "$D"
```

```text
{"error":"forbidden","code":"permission"}
{"error":"Forbidden","code":"permission"}
HTTP/1.1 204 No Content
404
```

Afterwards the data directory holds only `chat.db`: `files/` is empty.

## The code

`appSchema` is the app's own tables, in the same database as the `fs_` tables so one transaction covers a message and its files.

<!-- ds:block id=appschema-u3atbtrs -->
**appSchema** · [`examples/recipe-chat-attachments/main.go:100-103`](main.go#L100-L103)

```go
var appSchema = []string{
	`CREATE TABLE IF NOT EXISTS conversation_members (conversation_id TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY (conversation_id, user_id))`,
	`CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, author_id TEXT NOT NULL, body TEXT NOT NULL)`,
}
```
<!-- /ds:block hash=7909f7 -->

`attachmentPolicy` accepts images, PDFs, office documents, text and archives, never HTML, SVG or executables, and stores everything private.

<!-- ds:block id=attachmentpolicy-nfhy2kc2 -->
**attachmentPolicy** · [`examples/recipe-chat-attachments/main.go:86-95`](main.go#L86-L95)

```go
var attachmentPolicy = upload.Policy{
	MaxSize: maxAttachmentBytes,
	Allowed: []string{
		"image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf", "text/plain", "text/csv", "application/zip",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	},
	Key:        upload.RandomKey("chat"),
	Visibility: filesystem.VisibilityPrivate,
}
```
<!-- /ds:block hash=7c82fe -->

`openDisk` opens a private local disk whose links are signed for the `fshttp.Signed` route. On S3 the driver signs instead and neither is needed.

<!-- ds:block id=opendisk-eevty284 -->
**openDisk** · [`examples/recipe-chat-attachments/main.go:115-130`](main.go#L115-L130)

```go
func openDisk(dataDir, base string, secret []byte) (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(base+signedPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, nil, err
	}
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer), filesystem.WithDefaultVisibility(filesystem.VisibilityPrivate))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, signer, err
}
```
<!-- /ds:block hash=c7af5b -->

`openStore` opens SQLite, creates the app's tables and runs `records` migrations, with `message` as the only owner type.

<!-- ds:block id=openstore-jbmv6rbq -->
**openStore** · [`examples/recipe-chat-attachments/main.go:136-153`](main.go#L136-L153)

```go
func openStore(ctx context.Context, dataDir string, disks *filesystem.Manager) (*sql.DB, *records.Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "chat.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range appSchema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, nil, err
		}
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerMessage))
	if err != nil {
		return nil, nil, err
	}
	_, err = store.Migrate(ctx)
	return db, store, err
}
```
<!-- /ds:block hash=b17325 -->

`New` mounts `recordshttp` under `/media` with the app's hooks, plus the app's own message, download and signed-link routes.

<!-- ds:block id=new-zkdac58q -->
**New** · [`examples/recipe-chat-attachments/main.go:158-184`](main.go#L158-L184)

```go
func New(ctx context.Context, dataDir, base string, secret []byte) (*App, error) {
	disks, signer, err := openDisk(dataDir, base, secret)
	if err != nil {
		return nil, err
	}
	db, store, err := openStore(ctx, dataDir, disks)
	if err != nil {
		return nil, err
	}
	app := &App{DB: db, Store: store}
	media, err := recordshttp.Handler(recordshttp.Config{
		Store: store, Disk: diskName, Policy: attachmentPolicy,
		MaxFiles: maxAttachmentsPerMessage, LinkLifetime: linkLifetime,
		Scope: app.scope, Authorize: app.authorize,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(mediaPrefix+"/", http.StripPrefix(mediaPrefix, media))
	mux.HandleFunc("POST /conversations/{id}/messages", app.postMessage)
	mux.HandleFunc("DELETE /messages/{id}", app.deleteMessage)
	mux.HandleFunc("GET /downloads/{id}", app.download)
	mux.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	app.Handler = mux
	return app, nil
}
```
<!-- /ds:block hash=c4ebf8 -->

`scope` is the `Scope` hook: the signed-in user's workspace, never taken from the URL.

<!-- ds:block id=app-scope-2dj96agp -->
**App.scope** · [`examples/recipe-chat-attachments/main.go:203-208`](main.go#L203-L208)

```go
func (a *App) scope(r *http.Request) (string, error) {
	if _, err := userOf(r); err != nil {
		return "", err
	}
	return workspace, nil
}
```
<!-- /ds:block hash=91e6a1 -->

`canReadFile` joins `fs_attachments` with the app's messages and memberships: the `fs_` tables may be read freely, only written through `records`.

<!-- ds:block id=app-canreadfile-rj7ndtf9 -->
**App.canReadFile** · [`examples/recipe-chat-attachments/main.go:236-246`](main.go#L236-L246)

```go
func (a *App) canReadFile(ctx context.Context, scope, user, fileID string) (bool, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s fa
		JOIN messages m ON m.id = fa.%s
		JOIN conversation_members cm ON cm.conversation_id = m.conversation_id
		WHERE fa.%s = ? AND fa.%s = ? AND fa.%s = ? AND cm.user_id = ?`,
		records.TableNameAttachments, records.ColAttachmentOwnerID,
		records.ColAttachmentScope, records.ColAttachmentFileID, records.ColAttachmentOwnerType)
	var n int
	err := a.DB.QueryRowContext(ctx, q, scope, fileID, ownerMessage, user).Scan(&n)
	return n > 0, err
}
```
<!-- /ds:block hash=80176b -->

`authorize` is the `Authorize` hook: members list and read, only the author attaches, and replacing or trashing a single file is off because a sent message does not change under its readers.

<!-- ds:block id=app-authorize-5tzr78db -->
**App.authorize** · [`examples/recipe-chat-attachments/main.go:253-277`](main.go#L253-L277)

```go
func (a *App) authorize(r *http.Request, action recordshttp.Action, t recordshttp.Target) error {
	user, err := userOf(r)
	if err != nil {
		return err
	}
	switch action {
	case recordshttp.ActionRead:
		ok, err := a.canReadFile(r.Context(), t.Scope, user, t.FileID)
		return allow(ok, err)
	case recordshttp.ActionReplace, recordshttp.ActionTrash:
		return errForbidden
	}
	if t.Owner.Type != ownerMessage || t.Role != roleAttachments {
		return errForbidden
	}
	m, err := a.message(r.Context(), t.Owner.ID)
	if err != nil {
		return err
	}
	if action == recordshttp.ActionList {
		ok, err := a.isMember(r.Context(), m.Conversation, user)
		return allow(ok, err)
	}
	return allow(m.Author == user, nil)
}
```
<!-- /ds:block hash=a636aa -->

`postMessage` creates a message in a conversation the user belongs to; files are attached afterwards to its ID.

<!-- ds:block id=app-postmessage-gzz55veb -->
**App.postMessage** · [`examples/recipe-chat-attachments/main.go:309-331`](main.go#L309-L331)

```go
func (a *App) postMessage(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	conv := r.PathValue("id")
	if err := allow(a.isMember(r.Context(), conv, user)); err != nil {
		writeError(w, err)
		return
	}
	var in messageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body"))
		return
	}
	id := newID()
	if _, err := a.DB.ExecContext(r.Context(), `INSERT INTO messages (id, conversation_id, author_id, body) VALUES (?, ?, ?, ?)`, id, conv, user, in.Body); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, messageResponse{ID: id})
}
```
<!-- /ds:block hash=2ac69d -->

`deleteMessage` deletes the message row and calls `PurgeOwner` inside one `Store.Tx`: either both happen or neither, and objects are deleted only after the commit.

<!-- ds:block id=app-deletemessage-kttunujv -->
**App.deleteMessage** · [`examples/recipe-chat-attachments/main.go:338-364`](main.go#L338-L364)

```go
func (a *App) deleteMessage(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	m, err := a.message(r.Context(), r.PathValue("id"))
	if err == nil && m.Author != user {
		err = errForbidden
	}
	if err != nil {
		writeError(w, err)
		return
	}
	err = a.Store.Tx(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, m.ID); err != nil {
			return err
		}
		_, err := a.Store.PurgeOwner(ctx, workspace, records.Owner{Type: ownerMessage, ID: m.ID})
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=a70d00 -->

`download` redirects a member to a `Store.DownloadURL` link, which saves the file under its original name instead of opening it.

<!-- ds:block id=app-download-ag9j9tnn -->
**App.download** · [`examples/recipe-chat-attachments/main.go:369-392`](main.go#L369-L392)

```go
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	if err := allow(a.canReadFile(r.Context(), workspace, user, id)); err != nil {
		writeError(w, err)
		return
	}
	f, err := a.Store.File(r.Context(), workspace, id)
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := a.Store.DownloadURL(r.Context(), f, linkLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, link, http.StatusFound)
}
```
<!-- /ds:block hash=567533 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The schema, policy, disk and store, the `recordshttp` hooks, the message and download routes, and the server with its seeded conversations. |
| `main_test.go` | Members-only access, the all-or-nothing upload, and message deletion (including a failing delete that rolls the purge back). |

## Next

- The guide: [Chat attachments](../../docs/recipes/chat-attachments.md), with the edge cases it handles and production notes.
- [Attachments](../../docs/records/attachments.md), [HTTP endpoints](../../docs/records/http-endpoints.md) and [Lifecycle](../../docs/records/lifecycle.md) for the records pieces used here.
- Related recipes: [recipe-account-deletion](../recipe-account-deletion) (the same pattern for a whole user) and [recipe-team-drive](../recipe-team-drive) (files that belong to a workspace rather than a message).
