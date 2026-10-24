// Command recipe-resumable-uploads accepts uploads that survive dropped
// connections, over the tus protocol (https://tus.io) that Uppy, tus-js-client
// and the official iOS and Android clients speak. The client sends the file
// in chunks; after a network failure it asks how much arrived and continues
// from there. When the last byte arrives the file goes through the upload
// policy into the disk and is recorded and attached to the user, all before
// the client is told it succeeded.
//
//	go run ./recipe-resumable-uploads       (listens on localhost:8106)
//
// Routes (the demo trusts the X-User header; a real app uses its session):
//
//	POST/HEAD/PATCH/DELETE /uploads/…   the tus protocol
//	GET /files                          the user's uploaded files
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/tus"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8106"
	// tusPath is where the tus handler is mounted; tus builds upload URLs
	// from it, so it must match the mount.
	tusPath = "/uploads/"
	// diskName names the one disk in the manager.
	diskName = "media"
	// maxUploadBytes caps one file. tusd refuses a larger Upload-Length
	// at creation, before any byte is sent.
	maxUploadBytes = 2 << 30
	// sessionMaxAge is how long an unfinished session may sit in the temp
	// directory before it is swept: a client that never comes back.
	sessionMaxAge = 24 * time.Hour
	// ownerUser and roleUploads are where finished uploads are attached.
	ownerUser   = "user"
	roleUploads = "uploads"
	// headerUser carries the demo's user; a real app reads its session.
	headerUser = "X-User"
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// policy is enforced on the finished bytes, exactly as for a form upload:
// the type detected from content, the size counted while storing.
// ds:def id=policy-cf24neuq
var policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"video/mp4", "video/quicktime", "image/jpeg", "image/png", "application/pdf"},
	Key:     upload.RandomKey("uploads"),
}

// userKey is the context key of the signed-in user.
type userKey struct{}

// withUser is the authentication middleware: it puts the user in the
// request context, where tus's Authorize and OnStored hooks read it.
// Demo only: it trusts a header.
// ds:def id=withuser-cqj5cewu
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get(headerUser); u != "" {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		}
		next.ServeHTTP(w, r)
	})
}

// userFrom returns the user withUser stored, or an error.
// ds:def id=userfrom-wgd9ge2c
func userFrom(ctx context.Context) (string, error) {
	if u, ok := ctx.Value(userKey{}).(string); ok {
		return u, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "sign in first")
}

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	TempDir string
	Handler http.Handler
}

// openStore opens the local disk and the records Store over SQLite.
func openStore(ctx context.Context, dataDir string) (*records.Store, *filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, nil, err
	}
	_, err = store.Migrate(ctx)
	return store, disk, err
}

// newTus configures the tus handler. Authorize refuses anonymous uploads
// when they are created; OnStored records and attaches the finished file.
// ds:def id=app-newtus-g2wn6ycj
func (a *App) newTus(disk *filesystem.Disk) (http.Handler, error) {
	return tus.New(tus.Config{
		Disk:     disk,
		Policy:   policy,
		TempDir:  a.TempDir,
		BasePath: tusPath,
		Authorize: func(ctx context.Context, _ http.Header) error {
			_, err := userFrom(ctx)
			return err
		},
		OnStored: a.record,
	})
}

// record runs once the finished upload is on the disk: it records and
// attaches the file to the user who sent the final chunk. An error here
// makes tus delete the object and fail the client's last request, so the
// client never hears "done" for a file that has no row.
// ds:def id=app-record-gyqamstd
func (a *App) record(ctx context.Context, res upload.Result, meta map[string]string) error {
	user, err := userFrom(ctx)
	if err != nil {
		return err
	}
	return a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.CreateFile(ctx, records.NewFile{
			Disk: diskName, Key: res.Key, Filename: res.Filename,
			ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256, UploadedBy: user,
		})
		if err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleUploads)
		return err
	})
}

// New wires the app over a data directory.
// ds:def id=new-sgs7z583
func New(ctx context.Context, dataDir string) (*App, error) {
	store, disk, err := openStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, TempDir: filepath.Join(dataDir, "tus")}
	h, err := app.newTus(disk)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(tusPath, http.StripPrefix(tusPath, h))
	mux.HandleFunc("GET /files", app.list)
	app.Handler = withUser(mux)
	return app, nil
}

// fileResponse describes a recorded file.
type fileResponse struct {
	FileID      string `json:"file_id"`
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// list returns the user's finished uploads.
func (a *App) list(w http.ResponseWriter, r *http.Request) {
	user, err := userFrom(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	attached, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerUser, ID: user}, roleUploads)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]fileResponse, 0, len(attached))
	for _, af := range attached {
		f := af.File
		out = append(out, fileResponse{FileID: f.ID, Key: f.Key, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size})
	}
	writeJSON(w, http.StatusOK, out)
}

// sweepSessions deletes the unfinished tus sessions nobody touched since
// cutoff and returns how many it deleted. tusd keeps each session in the
// temp directory as <id> (the bytes so far) and <id>.info (its state) and
// never expires them; finished and refused sessions are deleted by the
// handler itself. tus.Sweep judges a session by the later of its two files'
// modification times, because tusd writes .info only at creation: a
// per-file age would delete the .info of an upload still receiving bytes.
// ds:def id=sweepsessions-g7wfbt4s
func sweepSessions(ctx context.Context, dir string, cutoff time.Time) (int, error) {
	return tus.Sweep(ctx, dir, cutoff)
}

// runSweeper sweeps the temp directory once an hour until ctx ends.
// ds:def id=runsweeper-z2xq8a3z
func runSweeper(ctx context.Context, dir string) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			n, err := sweepSessions(ctx, dir, now.Add(-sessionMaxAge))
			fmt.Fprintf(os.Stderr, "sweep: removed %d abandoned sessions (err: %v)\n", n, err)
		}
	}
}

// writeError answers with the status for err's code and a JSON body.
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: http.StatusText(status), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-resumable-uploads-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if len(os.Args) > 1 && os.Args[1] == sharedCommand {
		if err := sharedInstances(ctx, os.Stdout, dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	app, err := New(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go runSweeper(ctx, app.TempDir)
	fmt.Println("listening on http://"+listenAddr, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
