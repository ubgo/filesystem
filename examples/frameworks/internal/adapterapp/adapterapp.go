// Package adapterapp is the storage side shared by the adapter examples
// (adapters/gin, adapters/echo, adapters/fiber, adapters/chi): one memory
// disk, a signer, the records tables in in-memory SQLite, and the
// library's handlers. Each framework's program adds only its routing and
// the adapter's middleware, which is what those examples are about.
//
// Authentication is out of scope: the X-User header stands in for the
// app's session, and every signed-in user belongs to one tenant.
package adapterapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Mount points, the same on every framework.
const (
	PathUpload = "/upload" // POST, upload.Handler
	PathFiles  = "/files"  // fshttp.Signed, where signed links point
	GroupMe    = "/me"     // the signed-in routes: Actor and Scope
	MountMedia = "/media"  // recordshttp, inside GroupMe
	// PathExport is GET /me/export/{role}: the user's files of a role as a
	// zip; ParamRole names the path parameter.
	PathExport = "/export"
	ParamRole  = "role"
)

// HeaderUser names the signed-in user. It stands in for real
// authentication (a session cookie, a bearer token).
const HeaderUser = "X-User"

// Decisions of this demo, not of the library.
const (
	diskMedia      = "media"
	ownerUser      = "user"
	tenant         = "demo"
	signingKeyID   = "k1"
	incomingDir    = "incoming"
	maxUploadBytes = 10 << 20
	// ShutdownTimeout is how long requests in flight get to finish when
	// the program is stopped.
	ShutdownTimeout = 5 * time.Second
	// ReadHeaderTimeout stops a slow client from holding a connection open
	// before it has sent its headers.
	ReadHeaderTimeout = 5 * time.Second
)

// MaxUploadBytes is the policy's size limit; fiber, which reads every
// body before a handler runs, must allow at least this plus form overhead.
const MaxUploadBytes = maxUploadBytes

// Policy is the one upload policy: images, PDFs and plain text, stored
// under incoming/ with random names.
var Policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey(incomingDir),
}

// App is the wired storage side.
type App struct {
	Media   *filesystem.Disk
	Store   *records.Store
	Upload  http.Handler // POST multipart form uploads, no records
	Files   http.Handler // signed downloads
	Records http.Handler // upload, attach, list, read, trash, …
}

// New wires the app for a server reached at baseURL (signed links are
// built from it). scope is the adapter's ScopeOf: recordshttp reads the
// tenant the adapter's Scope middleware resolved.
func New(ctx context.Context, baseURL string, scope func(*http.Request) (string, error)) (*App, error) {
	secret := make([]byte, signedurl.MinKeyBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	signer, err := signedurl.New(baseURL+PathFiles, []signedurl.Key{{ID: signingKeyID, Secret: secret}})
	if err != nil {
		return nil, err
	}
	media, err := filesystem.NewDisk(diskMedia, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(media)
	if err != nil {
		return nil, err
	}
	store, err := newStore(ctx, disks)
	if err != nil {
		return nil, err
	}
	rec, err := recordshttp.Handler(recordshttp.Config{
		Store: store, Disk: diskMedia, Policy: Policy, Scope: scope,
		Authorize: func(r *http.Request, _ recordshttp.Action, t recordshttp.Target) error {
			return authorize(store.Actor(r.Context()), t)
		},
	})
	if err != nil {
		return nil, err
	}
	return &App{
		Media: media, Store: store,
		Upload:  upload.Handler(media, Policy, upload.HandlerOptions{}),
		Files:   fshttp.Signed(disks, signer),
		Records: rec,
	}, nil
}

// newStore opens the records tables in an in-memory SQLite database.
func newStore(ctx context.Context, disks *filesystem.Manager) (*records.Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// errNotYours refuses a user acting on someone else's owner or file.
var errNotYours = errors.New("not yours")

// authorize lets the actor (the user the adapter's Actor middleware
// recorded) act on their own owner routes and on the files they uploaded.
func authorize(actor string, t recordshttp.Target) error {
	if t.File != nil {
		if t.File.UploadedBy == actor {
			return nil
		}
		return errNotYours
	}
	if t.Owner.Type == ownerUser && t.Owner.ID == actor {
		return nil
	}
	return errNotYours
}

// User is the signed-in user of r: the X-User header, "" when absent.
// The adapters' Actor middleware calls it.
func User(r *http.Request) (string, error) { return r.Header.Get(HeaderUser), nil }

// errSignedOut refuses a request without a user a tenant.
var errSignedOut = errors.New("sign in first")

// Tenant is the tenant of r: one for every signed-in user, an error
// otherwise. The adapters' Scope middleware calls it.
func Tenant(r *http.Request) (string, error) { return TenantOf(r.Header.Get(HeaderUser)) }

// TenantOf is the tenant of user, for frameworks without an
// *http.Request (fiber).
func TenantOf(user string) (string, error) {
	if user == "" {
		return "", errSignedOut
	}
	return tenant, nil
}

// Export lists the zip entries of the actor's files of role in scope,
// numbered by position so two files of the same name stay apart. A role
// without files is CodeNotFound, which the adapters' Error answers 404.
// ds:def id=app-export-2axc7z6k
func (a *App) Export(ctx context.Context, scope, role string) ([]fshttp.ZipFile, error) {
	attached, err := a.Store.Attachments(ctx, scope, records.Owner{Type: ownerUser, ID: a.Store.Actor(ctx)}, role)
	if err != nil {
		return nil, err
	}
	if len(attached) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "no %s files", role)
	}
	files := make([]fshttp.ZipFile, 0, len(attached))
	for _, af := range attached {
		files = append(files, fshttp.ZipFile{Key: af.File.Key, Name: fmt.Sprintf("%d-%s", af.Attachment.Position, upload.SafeFilename(af.File.Filename))})
	}
	return files, nil
}

// ExportName is the zip's file name for role.
func ExportName(role string) string { return role + ".zip" }

// ServeUntilDone serves srv until ctx is cancelled (Ctrl+C or SIGTERM
// through signal.NotifyContext), then shuts it down gracefully, giving
// requests in flight ShutdownTimeout to finish.
func ServeUntilDone(ctx context.Context, srv *http.Server) error {
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
