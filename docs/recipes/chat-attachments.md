# Chat attachments

Messaging apps, team chat, support desks, marketplaces with buyer-seller threads and comment sections all have the same file feature: people attach files to a message, everyone in the conversation can open them, nobody outside it can, and when the message goes the files go too. This recipe builds that on `recordshttp`, with the membership check written once in the app.

The program is [examples/recipe-chat-attachments](../../examples/recipe-chat-attachments). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `POST /media/owners/message/{id}/attachments` uploads up to 10 files and attaches them to a message, all or none. Only the message's author may do it.
- Members of the conversation list a message's files, each with a signed link that works for five minutes; anyone else gets `403`.
- `GET /downloads/{file}` redirects a member to a link that saves the file under its original name, `Angebot Müller.pdf` included.
- `DELETE /messages/{id}` deletes the message row and every file attached only to it in one transaction; the objects go after the commit.
- Files are private: no URL works without a fresh signature.

## The design

| Piece | Why |
|---|---|
| Owner type `message`, role `attachments` | Each file belongs to a message, so its lifetime and its readers follow the message. |
| `recordshttp.Handler` | Upload-and-attach, list, redirect and detach endpoints, written once. It has no notion of users: `Scope` names the workspace, `Authorize` decides each action. Both are required. |
| `Authorize` backed by the app's own tables | Who may read a file is a fact of the chat (membership), not of storage. The check joins `fs_attachments` with the app's `messages` and `conversation_members`; the fs_ tables are open for reading. |
| Private disk + `signedurl` + `fshttp.Signed` | List answers carry signed links; links expire, so a URL pasted outside the conversation stops working. On S3 the driver signs instead. |
| `Store.Tx` + `PurgeOwner` | The app's `DELETE FROM messages` and the purge share one transaction, so either both happen or neither. |

## Step 1: The app's tables and the store

<!-- ds:block id=appschema-u3atbtrs -->
**appSchema** · [`examples/recipe-chat-attachments/main.go:100-103`](../../examples/recipe-chat-attachments/main.go#L100-L103)

```go
var appSchema = []string{
	`CREATE TABLE IF NOT EXISTS conversation_members (conversation_id TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY (conversation_id, user_id))`,
	`CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, author_id TEXT NOT NULL, body TEXT NOT NULL)`,
}
```
<!-- /ds:block hash=7909f7 -->

<!-- ds:block id=openstore-jbmv6rbq -->
**openStore** · [`examples/recipe-chat-attachments/main.go:136-153`](../../examples/recipe-chat-attachments/main.go#L136-L153)

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

The app's tables and the fs_ tables live in one database. That is what lets a message and its files change in one transaction; with separate databases you would delete the message first and purge its files in a retried job.

<!-- ds:block id=opendisk-eevty284 -->
**openDisk** · [`examples/recipe-chat-attachments/main.go:115-130`](../../examples/recipe-chat-attachments/main.go#L115-L130)

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

## Step 2: The policy

<!-- ds:block id=attachmentpolicy-nfhy2kc2 -->
**attachmentPolicy** · [`examples/recipe-chat-attachments/main.go:86-95`](../../examples/recipe-chat-attachments/main.go#L86-L95)

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

The list is explicit: chat files are images, PDFs, office documents, text and archives. HTML and SVG stay refused (the default denials), because a file served from your origin that runs script there is stored XSS. Files are stored under random keys; the user's file name is kept in the row for display and downloads.

## Step 3: Scope and Authorize

<!-- ds:block id=app-scope-2dj96agp -->
**App.scope** · [`examples/recipe-chat-attachments/main.go:203-208`](../../examples/recipe-chat-attachments/main.go#L203-L208)

```go
func (a *App) scope(r *http.Request) (string, error) {
	if _, err := userOf(r); err != nil {
		return "", err
	}
	return workspace, nil
}
```
<!-- /ds:block hash=91e6a1 -->

`Scope` returns the workspace of the signed-in user and is the scope of every records call the handler makes. It never comes from the URL, so a user cannot name another workspace's files. Answering an error makes the handler reply `401`.

<!-- ds:block id=app-authorize-5tzr78db -->
**App.authorize** · [`examples/recipe-chat-attachments/main.go:253-277`](../../examples/recipe-chat-attachments/main.go#L253-L277)

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

Every route calls `Authorize` with an action and a target before touching anything:

| Action | Target | Rule here |
|---|---|---|
| `list` | owner and role | a member of the message's conversation |
| `upload`, `reorder`, `detach` | owner and role (detach: loaded from the attachment) | the message's author |
| `read` | file ID | a member of a conversation that holds a message the file is attached to |
| `replace`, `trash` | file ID | nobody: a sent message does not change under its readers |

The owner type and role are checked too: without that, a user could attach files to `/owners/message/{id}/anything`.

The read check is one query:

<!-- ds:block id=app-canreadfile-rj7ndtf9 -->
**App.canReadFile** · [`examples/recipe-chat-attachments/main.go:236-246`](../../examples/recipe-chat-attachments/main.go#L236-L246)

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

The handler answers a refused hook with `403` and the code `permission`, never with the hook's own message, so the reason (not a member, not the author) does not leak.

## Step 4: Mount it

<!-- ds:block id=new-zkdac58q -->
**New** · [`examples/recipe-chat-attachments/main.go:158-184`](../../examples/recipe-chat-attachments/main.go#L158-L184)

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

`MaxFiles` caps one request (10 here, the handler's default when 0); the request body is capped at `MaxFiles × Policy.MaxSize` plus form overhead before anything is read. `LinkLifetime` is how long the links in listings and redirects live.

## Step 5: Messages, downloads, deletion

<!-- ds:block id=app-postmessage-gzz55veb -->
**App.postMessage** · [`examples/recipe-chat-attachments/main.go:309-331`](../../examples/recipe-chat-attachments/main.go#L309-L331)

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

The client creates the message first and attaches files to its ID. A client that wants "upload while typing" can upload to a draft message the same way and publish it later.

<!-- ds:block id=app-download-ag9j9tnn -->
**App.download** · [`examples/recipe-chat-attachments/main.go:369-392`](../../examples/recipe-chat-attachments/main.go#L369-L392)

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

<!-- ds:block id=app-deletemessage-kttunujv -->
**App.deleteMessage** · [`examples/recipe-chat-attachments/main.go:338-364`](../../examples/recipe-chat-attachments/main.go#L338-L364)

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

`Store.Tx` opens the transaction and hands the app the `*sql.Tx`; the app's `DELETE` runs on it, and `PurgeOwner` joins it through `ctx`. `PurgeOwner` removes every attachment of the message and purges each file no other owner still uses (a file forwarded to another message is only detached). If the app's delete fails, the purge rolls back with it; the example's test proves this with a trigger that blocks the delete.

## Try it

```sh
cd examples
go run ./recipe-chat-attachments
```

```text
listening on http://localhost:8109 data in /var/folders/sk/.../T/recipe-chat-attachments-1285384397
```

The demo seeds two conversations: `general` with alice and bob, `leads` with carol.

```sh
curl -s -X POST localhost:8109/conversations/general/messages -H 'X-User: alice' -d '{"body":"the offer and a photo"}'
curl -s -X POST localhost:8109/conversations/general/messages -H 'X-User: carol' -d '{"body":"hi"}'
```

```text
{"id":"msg_f4afb3ab2a60daa2484d"}
{"error":"Forbidden","code":"permission"}
```

Attach two files as the author, then try as someone else:

```sh
curl -s -X POST localhost:8109/media/owners/message/msg_f4afb3ab2a60daa2484d/attachments -H 'X-User: alice' -F 'file=@Angebot Müller.pdf' -F file=@photo.png
curl -s -X POST localhost:8109/media/owners/message/msg_f4afb3ab2a60daa2484d/attachments -H 'X-User: bob' -F file=@photo.png
```

```text
{"files":[{"attachment_id":"att_k4mcbm2rhlqtjg762v5yuojmko","file_id":"fil_7mtn3flto5of25qnlck7g7lirx","filename":"Angebot Müller.pdf","content_type":"application/pdf","size":15,"position":1,"status":"ready"},{"attachment_id":"att_tzjv2mw5rpoprdh34nygubxmlr","file_id":"fil_ycgelfv4ukwwo2ilmusjpck6t7","filename":"photo.png","content_type":"image/png","size":312,"position":2,"status":"ready"}]}
{"error":"forbidden","code":"permission"}
```

One bad file sinks the whole request, and the good one with it:

```sh
printf '<html><script>x</script>' > page.html
curl -s -X POST localhost:8109/media/owners/message/msg_f4afb3ab2a60daa2484d/attachments -H 'X-User: alice' -F file=@photo.png -F file=@page.html
```

```json
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
```

List as a member, and as carol, who is not in `general`:

```sh
curl -s localhost:8109/media/owners/message/msg_f4afb3ab2a60daa2484d/attachments -H 'X-User: bob'
curl -s localhost:8109/media/owners/message/msg_f4afb3ab2a60daa2484d/attachments -H 'X-User: carol'
```

```text
{"files":[{"attachment_id":"att_k4mcbm2rhlqtjg762v5yuojmko","file_id":"fil_7mtn3flto5of25qnlck7g7lirx","filename":"Angebot Müller.pdf","content_type":"application/pdf","size":15,"position":1,"url":"http://localhost:8109/signed/chat/chat/7pcsk6bl6pspy5gtxglrzg3tkf.pdf?ct=application%2Fpdf\u0026exp=1791268120\u0026kid=k1\u0026m=GET\u0026sig=0ebd870b…","status":"ready"},{"attachment_id":"att_tzjv2mw5rpoprdh34nygubxmlr","file_id":"fil_ycgelfv4ukwwo2ilmusjpck6t7","filename":"photo.png","content_type":"image/png","size":312,"position":2,"url":"http://localhost:8109/signed/chat/chat/rkytjxjgk7mmxhko6pfkl4lhjy.png?ct=image%2Fpng\u0026exp=1791268120\u0026kid=k1\u0026m=GET\u0026sig=3ec1facb…","status":"ready"}]}
{"error":"forbidden","code":"permission"}
```

Open and save one file:

```sh
curl -s -i localhost:8109/media/files/fil_7mtn3flto5of25qnlck7g7lirx -H 'X-User: bob' | grep -E '^(HTTP|Location)'
curl -s localhost:8109/media/files/fil_7mtn3flto5of25qnlck7g7lirx -H 'X-User: carol'
D=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8109/downloads/fil_7mtn3flto5of25qnlck7g7lirx -H 'X-User: bob')
curl -s -D - -o /dev/null "$D" | grep -iE '^(HTTP|Content-Disposition|Content-Type)'
```

```text
HTTP/1.1 302 Found
Location: http://localhost:8109/signed/chat/chat/7pcsk6bl6pspy5gtxglrzg3tkf.pdf?ct=application%2Fpdf&exp=1791268120&kid=k1&m=GET&sig=0ebd870be6de5c4e8ea449bea147cb9a7593c773475ceb1c2f7e0d5717b66a75
{"error":"forbidden","code":"permission"}
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="Angebot M_ller.pdf"; filename*=UTF-8''Angebot%20M%C3%BCller.pdf
Content-Type: application/pdf
```

Single files cannot be trashed; the message can be deleted, by its author only:

```sh
curl -s -X DELETE localhost:8109/media/files/fil_7mtn3flto5of25qnlck7g7lirx -H 'X-User: alice'
curl -s -X DELETE localhost:8109/messages/msg_f4afb3ab2a60daa2484d -H 'X-User: bob'
curl -s -i -X DELETE localhost:8109/messages/msg_f4afb3ab2a60daa2484d -H 'X-User: alice' | head -1
curl -s -o /dev/null -w '%{http_code}\n' "$D"
```

```text
{"error":"forbidden","code":"permission"}
{"error":"Forbidden","code":"permission"}
HTTP/1.1 204 No Content
404
```

Check: the download link signed a minute earlier is still within its lifetime, but answers `404`: the object is gone. The data directory's `files/` holds nothing.

## Edge cases this handles

- A user outside the conversation guessing message or file IDs: `403` on every route, with no reason given.
- A member who is not the author attaching to someone else's message: `403`.
- One refused file in a multi-file upload: nothing is stored and nothing attached. `recordshttp` uploads are always all-or-nothing; when a bulk upload should keep the good files, use `upload.Handler` with `HandlerOptions{Partial: true}` and attach in `OnStored` ([mobile uploads](mobile-uploads.md)).
- A message deleted while a member holds a link: the link answers `404` once the purge commits.
- A delete that fails half-way: the message row and the files roll back together; no message without files, no files without a message.
- A file forwarded to another message (attached twice): deleting one message only detaches it.
- Non-ASCII file names: kept exactly in the row and in the download's `filename*`.

## For production

- Replace `userOf` with your session lookup, and let `scope` return the user's real workspace.
- Index the app's tables for the read check (`messages(id)`, `conversation_members(conversation_id, user_id)`); `fs_attachments` is indexed on file and owner by the library's migrations.
- Scan uploads before they are attached ([virus scanning](virus-scanning.md)): chat is how malware moves between colleagues.
- Show image previews with the [variants plugin](../plugins/variants.md): declare a `preview` size and request its URL in the list response. Variants of a purged file are deleted with it.
- Put the disk on S3 and drop `signedurl` and `fshttp.Signed`; links become SigV4 URLs and nothing else changes ([private downloads](private-downloads.md)).
- Leaving a conversation does not revoke links already issued; keep `LinkLifetime` short.

## Related

- [Attachments](../records/attachments.md): owners, roles, positions and detaching.
- [HTTP endpoints](../records/http-endpoints.md): every `recordshttp` route and its answers.
- [Lifecycle](../records/lifecycle.md): trash, purge and `PurgeOwner`.
- [Account deletion](account-deletion.md): the same pattern for a whole user.
- [Team drive](team-drive.md): files that live in a workspace rather than a message.
