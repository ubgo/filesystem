// Command mediaserver is the shape of a social or SaaS backend's file
// handling: users upload an avatar, the server checks it against a policy,
// stores it on disk, records it in SQLite, attaches it to the user, and
// hands out short-lived signed links to it.
//
//	go run ./mediaserver [-addr localhost:8080]
//
// Routes:
//
//	POST /users/{id}/avatar   multipart form, field "file"
//	GET  /users/{id}/avatar   302 to a signed link
//	GET  /files/…             signed links, served by fshttp
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
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
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Names this app chose. They are decisions of the application, not of the
// library, which is why they live here.
const (
	diskName      = "media"
	ownerUser     = "user"
	roleAvatar    = "avatar"
	linkLifetime  = 5 * time.Minute
	maxAvatarSize = 2 << 20
)

// avatars is the upload policy: images only, 2 MiB, stored under avatars/
// with random names.
var avatars = upload.Policy{
	MaxSize: maxAvatarSize,
	Allowed: []string{"image/png", "image/jpeg", "image/webp", "image/gif"},
	Key:     upload.RandomKey("avatars"),
}

// App holds the wired dependencies.
type App struct {
	Store *records.Store
	Disks *filesystem.Manager
	Mux   *http.ServeMux
}

// New wires the app over a data directory and the public base URL of the
// server (used in signed links).
// ds:def id=new-e4n8hw3h
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

// uploadAvatar replaces the user's avatar in one transaction: upload and
// record the new file, then SetAttachment makes it the user's only avatar
// and trashes the old one. If anything fails, the transaction rolls back
// and the uploaded bytes are deleted (records.Upload registers that on
// rollback). "avatar" is a single role, so two concurrent uploads cannot
// leave a user with two avatars.
// ds:def id=app-uploadavatar-jqtqcs3t
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

// avatar redirects to a short-lived signed link to the user's avatar.
// ds:def id=app-avatar-u35yeadu
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

// defaultAddr is where the server listens unless -addr says otherwise.
const defaultAddr = "localhost:8080"

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	addr := flag.String("addr", defaultAddr, "listen address; the public URL is http://<addr>")
	flag.Parse()
	dir, err := os.MkdirTemp("", "mediaserver-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, "http://"+*addr, bytes.Repeat([]byte("change-me"), 4))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", *addr, "; data in", dir)
	srv := &http.Server{Addr: *addr, Handler: app.Mux, ReadHeaderTimeout: 10 * time.Second}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
