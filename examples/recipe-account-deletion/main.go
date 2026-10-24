// Command recipe-account-deletion is "delete my account" done so that it
// holds up to a data-protection request: the user's row and every file
// only they used go in one transaction, files shared with a team stay for
// the team, objects are deleted only after the commit, an object deletion
// that fails is queued and retried, and trashed files have a retention
// period.
//
//	go run ./recipe-account-deletion       (listens on localhost:8114)
//
// Routes:
//
//	POST   /users/{id}                       create a user (JSON {"email": …})
//	POST   /users/{id}/files                 multipart form, field "file"
//	GET    /users/{id}/files                 the user's files
//	DELETE /users/{id}/files/{file}          move one file to the trash
//	PUT    /teams/{team}/files/{file}        share a file with a team
//	GET    /teams/{team}/files               the team's shared files
//	POST   /users/{id}/invoices              add an unpaid invoice (blocks deletion)
//	DELETE /users/{id}                       delete the account
//
// Authentication is out of scope: a real app checks that the signed-in
// user is {id} (or an admin acting on a request) before these run.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ubgo/filesystem"
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
	listenAddr = "localhost:8114"
	// diskName names the one disk in the manager.
	diskName = "media"
	// ownerUser and ownerTeam are the owner types files attach to.
	ownerUser = "user"
	ownerTeam = "team"
	// roleUploads holds a user's own files; roleShared a team's.
	roleUploads = "uploads"
	roleShared  = "shared"
	// maxFileSize caps one upload.
	maxFileSize = 10 << 20
	// maxJSONBody caps a JSON request body.
	maxJSONBody = 4 << 10
	// trashRetention is how long a trashed file can be restored before the
	// sweep purges it. State it in the privacy policy.
	trashRetention = 30 * 24 * time.Hour
	// sweepEvery is how often main runs the maintenance pass.
	sweepEvery = time.Hour
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// policy accepts documents and images.
var policy = upload.Policy{
	MaxSize: maxFileSize,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey("files"),
}

// appSchema is the application's own tables, in the same database as the
// fs_ tables so one transaction covers both. invoices references users
// without ON DELETE: a user with invoices cannot be deleted, which is the
// failure this recipe shows rolling back.
// ds:def id=appschema-3nwqefjs
const appSchema = `
CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS invoices (id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id));
CREATE TABLE IF NOT EXISTS orphaned_objects (disk TEXT NOT NULL, storage_key TEXT NOT NULL, failed_at TEXT NOT NULL, PRIMARY KEY (disk, storage_key));
`

// App holds the wired dependencies.
type App struct {
	DB      *sql.DB
	Store   *records.Store
	Disks   *filesystem.Manager
	Handler http.Handler
}

// openDB opens SQLite with foreign keys enforced and creates the app's
// tables.
// ds:def id=opendb-vuhkjgvy
func openDB(ctx context.Context, dataDir string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, appSchema)
	return db, err
}

// New wires the app over a data directory. diskOpts are added to the disk,
// which is how the test injects storage failures.
// ds:def id=new-n64tswyy
func New(ctx context.Context, dataDir string, diskOpts ...filesystem.Option) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, diskOpts...)
	if err != nil {
		return nil, err
	}
	app := &App{}
	if app.Disks, err = filesystem.NewManager(disk); err != nil {
		return nil, err
	}
	if app.DB, err = openDB(ctx, dataDir); err != nil {
		return nil, err
	}
	app.Store, err = records.New(app.DB, sqlb.SQLiteDialect{}, app.Disks,
		records.WithOwnerTypes(ownerUser, ownerTeam),
		records.WithObjectErrorHandler(app.rememberOrphan))
	if err != nil {
		return nil, err
	}
	if _, err := app.Store.Migrate(ctx); err != nil {
		return nil, err
	}
	app.Handler = app.routes()
	return app, nil
}

// routes mounts the handlers.
func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users/{id}", a.createUser)
	mux.HandleFunc("POST /users/{id}/files", a.uploadFile)
	mux.HandleFunc("GET /users/{id}/files", a.listOwner(ownerUser, roleUploads))
	mux.HandleFunc("DELETE /users/{id}/files/{file}", a.trashFile)
	mux.HandleFunc("PUT /teams/{id}/files/{file}", a.shareWithTeam)
	mux.HandleFunc("GET /teams/{id}/files", a.listOwner(ownerTeam, roleShared))
	mux.HandleFunc("POST /users/{id}/invoices", a.addInvoice)
	mux.HandleFunc("DELETE /users/{id}", a.deleteAccount)
	return mux
}

// errHasInvoices is what a foreign key from invoices turns into.
var errHasInvoices = filesystem.Errorf(filesystem.CodeExists, "the account has invoices that must be kept; settle or anonymize them first")

// deleteUserRow deletes the app's own row on the transaction records
// uses. A foreign key failure becomes a 409 the client can act on.
// ds:def id=deleteuserrow-8n5kpwv5
func deleteUserRow(ctx context.Context, tx *sql.Tx, userID string) error {
	res, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID)
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		return errHasInvoices
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return filesystem.Errorf(filesystem.CodeNotFound, "no user %q", userID)
	}
	return nil
}

// deleteAccount is the whole deletion, in one transaction: PurgeOwner
// detaches every attachment of the user and purges each file nobody else
// uses, then the user's row goes on the same *sql.Tx. If anything fails,
// both roll back and every file is still there. Objects are deleted only
// after the commit, so a rollback never leaves a row without its bytes.
// ds:def id=app-deleteaccount-24xv49h8
func (a *App) deleteAccount(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var purged int
	err := a.Store.Tx(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if purged, err = a.Store.PurgeOwner(ctx, "", records.Owner{Type: ownerUser, ID: userID}); err != nil {
			return err
		}
		return deleteUserRow(ctx, tx, userID)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"files_purged": purged})
}

// rememberOrphan is the WithObjectErrorHandler: it runs when an object
// could not be deleted after its rows were committed away. The row is gone
// and cannot come back, so the object is recorded for retryOrphans rather
// than lost: personal data must not linger because a backend blinked.
// ds:def id=app-rememberorphan-7sauv2xh
func (a *App) rememberOrphan(ctx context.Context, f records.File, err error) {
	fmt.Fprintf(os.Stderr, "delete %s/%s after commit: %v (queued for retry)\n", f.Disk, f.Key, err)
	_, qerr := a.DB.ExecContext(ctx, "INSERT OR IGNORE INTO orphaned_objects (disk, storage_key, failed_at) VALUES (?, ?, ?)",
		f.Disk, f.Key, time.Now().UTC().Format(time.RFC3339))
	if qerr != nil {
		fmt.Fprintf(os.Stderr, "queue %s/%s: %v\n", f.Disk, f.Key, qerr)
	}
}

// orphan is one queued object.
type orphan struct{ disk, key string }

// retryOrphans deletes the queued objects and forgets each one that is
// gone. Deleting a missing object succeeds, so a retry is always safe.
// ds:def id=app-retryorphans-jzfew86s
func (a *App) retryOrphans(ctx context.Context) (int, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT disk, storage_key FROM orphaned_objects")
	if err != nil {
		return 0, err
	}
	var queued []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.disk, &o.key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		queued = append(queued, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	done := 0
	for _, o := range queued {
		disk, err := a.Disks.Disk(o.disk)
		if err == nil {
			err = disk.Delete(ctx, o.key)
		}
		if err != nil {
			continue // still failing; the next pass tries again
		}
		if _, err := a.DB.ExecContext(ctx, "DELETE FROM orphaned_objects WHERE disk = ? AND storage_key = ?", o.disk, o.key); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// maintain is the scheduled pass: purge files trashed longer than the
// retention period, then retry queued object deletions. The library starts
// no background work; the app decides when this runs.
// ds:def id=app-maintain-yjfrqhgf
func (a *App) maintain(ctx context.Context, now time.Time) error {
	swept, err := a.Store.SweepTrash(ctx, now.Add(-trashRetention))
	if err != nil {
		return err
	}
	retried, err := a.retryOrphans(ctx)
	if swept+retried > 0 {
		fmt.Printf("maintenance: purged %d trashed files, deleted %d queued objects\n", swept, retried)
	}
	return err
}

// userRequest is the body of POST /users/{id}.
type userRequest struct {
	Email string `json:"email"`
}

// createUser inserts the app's own user row.
func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	var in userRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil || in.Email == "" {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send {\"email\": …}"))
		return
	}
	if _, err := a.DB.ExecContext(r.Context(), "INSERT INTO users (id, email) VALUES (?, ?)", r.PathValue("id"), in.Email); err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeExists, "user %q exists", r.PathValue("id")))
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// addInvoice records an invoice for the user, which the foreign key keeps.
func (a *App) addInvoice(w http.ResponseWriter, r *http.Request) {
	if _, err := a.DB.ExecContext(r.Context(), "INSERT INTO invoices (user_id) VALUES (?)", r.PathValue("id")); err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "no user %q", r.PathValue("id")))
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// fileResponse is the JSON shape of one file.
type fileResponse struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// uploadFile stores a file and attaches it to the user, in one transaction.
func (a *App) uploadFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the file in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	ctx := records.WithActor(r.Context(), r.PathValue("id"))
	var f records.File
	err = a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if f, err = a.Store.Upload(ctx, "", diskName, file, header.Filename, policy); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: r.PathValue("id")}, roleUploads)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fileResponse{ID: f.ID, Filename: f.Filename, Size: f.Size})
}

// listOwner lists an owner's live files under role.
func (a *App) listOwner(ownerType, role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerType, ID: r.PathValue("id")}, role)
		if err != nil {
			writeError(w, err)
			return
		}
		out := make([]fileResponse, 0, len(list))
		for _, af := range list {
			out = append(out, fileResponse{ID: af.File.ID, Filename: af.File.Filename, Size: af.File.Size})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// trashFile moves one file to the trash: restorable until the sweep purges
// it after trashRetention. A real app checks the file is the user's.
// ds:def id=app-trashfile-89ztszy8
func (a *App) trashFile(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.Trash(r.Context(), "", r.PathValue("file")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shareWithTeam attaches an existing file to a team as well. The file now
// has two owners, so deleting the user only detaches it.
// ds:def id=app-sharewithteam-52gjmkkp
func (a *App) shareWithTeam(w http.ResponseWriter, r *http.Request) {
	_, err := a.Store.Attach(r.Context(), "", r.PathValue("file"), records.Owner{Type: ownerTeam, ID: r.PathValue("id")}, roleShared)
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeError answers with the status for err's code and a JSON body that
// names the code.
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

// runMaintenance runs maintain every sweepEvery until ctx ends.
// ds:def id=runmaintenance-vhjstp3z
func runMaintenance(ctx context.Context, a *App) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := a.maintain(ctx, now); err != nil {
				fmt.Fprintln(os.Stderr, "maintenance:", err)
			}
		}
	}
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-account-deletion-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go runMaintenance(ctx, app)
	fmt.Println("listening on http://"+listenAddr, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
