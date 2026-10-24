// Command recipe-chat-attachments is the file side of a chat or messaging
// app: members of a conversation attach files to their messages, every
// member can list and open them through short-lived signed links, nobody
// else can, and deleting a message deletes its files in the same
// transaction as the message row.
//
//	go run ./recipe-chat-attachments       (listens on localhost:8109)
//
// Routes (the signed-in user is the X-User header, demo only):
//
//	POST   /conversations/{id}/messages                       {"body": "…"}
//	DELETE /messages/{id}                                     message and its files
//	POST   /media/owners/message/{id}/attachments             upload and attach (recordshttp)
//	GET    /media/owners/message/{id}/attachments             list with signed links
//	GET    /media/files/{id}                                  302 to a signed link
//	GET    /downloads/{id}                                    302 to a "save as" link
//	GET    /signed/…                                          signed links, served by fshttp
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8109"
	// baseURL is the server's public origin; signed links start with it.
	baseURL = "http://" + listenAddr
	// mediaPrefix is where recordshttp is mounted.
	mediaPrefix = "/media"
	// signedPrefix is where fshttp.Signed serves signed links.
	signedPrefix = "/signed"
	// diskName names the private disk attachments live on.
	diskName = "chat"
	// ownerMessage is the owner type attachments belong to.
	ownerMessage = "message"
	// roleAttachments is the only role a message has.
	roleAttachments = "attachments"
	// workspace is the scope of every file. This demo has one workspace;
	// a multi-workspace app returns the signed-in user's workspace instead.
	workspace = "acme"
	// headerUser carries the signed-in user. Demo only: a real app takes it
	// from its session.
	headerUser = "X-User"
	// maxAttachmentBytes caps one file.
	maxAttachmentBytes = 25 << 20
	// maxAttachmentsPerMessage caps the files of one upload request.
	maxAttachmentsPerMessage = 10
	// linkLifetime is how long the links in a listing work. Chat clients
	// re-list when a link has expired.
	linkLifetime = 5 * time.Minute
	// maxJSONBody caps a message body request.
	maxJSONBody = 64 << 10
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// attachmentPolicy accepts what people send in chat: images, PDFs, office
// documents, text and archives. Never HTML, SVG or executables: the default
// denials stay on. Files are private, reachable only through signed links.
// ds:def id=attachmentpolicy-nfhy2kc2
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

// appSchema is the app's own tables: who is in which conversation, and the
// messages. The fs_ tables are created by records.Migrate.
// ds:def id=appschema-u3atbtrs
var appSchema = []string{
	`CREATE TABLE IF NOT EXISTS conversation_members (conversation_id TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY (conversation_id, user_id))`,
	`CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, author_id TEXT NOT NULL, body TEXT NOT NULL)`,
}

// App holds the wired dependencies.
type App struct {
	DB      *sql.DB
	Store   *records.Store
	Handler http.Handler
}

// openDisk opens a private local disk whose signed links point at the
// fshttp.Signed route. On S3 the driver signs and neither is needed.
// ds:def id=opendisk-eevty284
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

// openStore opens SQLite, creates the app's tables and the fs_ tables.
// The app and the Store share one database, so a message and its files
// can change in one transaction.
// ds:def id=openstore-jbmv6rbq
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

// New wires the app over a data directory, its public origin and the
// signing secret (at least 32 bytes).
// ds:def id=new-zkdac58q
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

// errSignedOut and errForbidden are the app's refusals.
var (
	errSignedOut = filesystem.Errorf(filesystem.CodePermission, "not signed in")
	errForbidden = filesystem.Errorf(filesystem.CodePermission, "forbidden")
)

// userOf returns the signed-in user.
func userOf(r *http.Request) (string, error) {
	if u := r.Header.Get(headerUser); u != "" {
		return u, nil
	}
	return "", errSignedOut
}

// scope is recordshttp's Scope hook: the workspace of the signed-in user.
// It never comes from the URL.
// ds:def id=app-scope-2dj96agp
func (a *App) scope(r *http.Request) (string, error) {
	if _, err := userOf(r); err != nil {
		return "", err
	}
	return workspace, nil
}

// message is a row of the app's messages table.
type message struct {
	ID, Conversation, Author string
}

// message loads one message, or CodeNotFound.
func (a *App) message(ctx context.Context, id string) (message, error) {
	m := message{ID: id}
	err := a.DB.QueryRowContext(ctx, `SELECT conversation_id, author_id FROM messages WHERE id = ?`, id).Scan(&m.Conversation, &m.Author)
	if errors.Is(err, sql.ErrNoRows) {
		return m, filesystem.Errorf(filesystem.CodeNotFound, "no message %s", id)
	}
	return m, err
}

// isMember reports whether user is in conversation.
func (a *App) isMember(ctx context.Context, conversation, user string) (bool, error) {
	var n int
	err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, conversation, user).Scan(&n)
	return n > 0, err
}

// canReadFile reports whether user is a member of a conversation holding a
// message the file is attached to. It reads fs_attachments directly: the
// fs_ tables may be read and joined freely, only written through records.
// ds:def id=app-canreadfile-rj7ndtf9
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

// authorize is recordshttp's Authorize hook. Members may list and read;
// only a message's author may attach, reorder or detach; replacing and
// trashing single files is off, because a sent message does not change
// under its readers (deleting the message deletes its files).
// ds:def id=app-authorize-5tzr78db
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

// allow turns a check's outcome into Authorize's answer.
func allow(ok bool, err error) error {
	if err != nil {
		return err
	}
	if !ok {
		return errForbidden
	}
	return nil
}

// newID returns a random message ID.
func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return "msg_" + hex.EncodeToString(b)
}

// messageRequest and messageResponse are the JSON of postMessage.
type messageRequest struct {
	Body string `json:"body"`
}

type messageResponse struct {
	ID string `json:"id"`
}

// postMessage creates a message in a conversation the user is a member
// of. Its files are attached afterwards, to the message's ID.
// ds:def id=app-postmessage-gzz55veb
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

// deleteMessage deletes the message row and its files in one transaction:
// the app's DELETE runs on the transaction Store.Tx opened, and PurgeOwner
// joins it through ctx. If either fails, both roll back. Objects are
// deleted only after the commit.
// ds:def id=app-deletemessage-kttunujv
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

// download redirects a member to a signed link that saves the file under
// its original name (Store.DownloadURL), instead of opening it.
// ds:def id=app-download-ag9j9tnn
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

// errorResponse is the JSON body of a failed request, the same shape
// recordshttp answers with.
type errorResponse struct {
	Error string          `json:"error"`
	Code  filesystem.Code `json:"code"`
}

// writeError answers with the status for err's code; server-side failures
// say only their status text.
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, errorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

// Seed puts users in conversations, for the demo and the tests.
func (a *App) Seed(ctx context.Context, members map[string][]string) error {
	for conv, users := range members {
		for _, u := range users {
			if _, err := a.DB.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_members (conversation_id, user_id) VALUES (?, ?)`, conv, u); err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-chat-attachments-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, baseURL, bytes.Repeat([]byte("change-me"), 4))
	if err == nil {
		err = app.Seed(ctx, map[string][]string{"general": {"alice", "bob"}, "leads": {"carol"}})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", baseURL, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
