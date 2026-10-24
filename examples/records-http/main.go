// Command records-http serves the records layer over HTTP with
// recordshttp: upload files and attach them to a user, list them, reorder
// and detach them, redirect to a file, replace its content and trash it.
// Downloads are served by fshttp.Signed on the same server, so the
// redirect of GET /media/files/{id} leads to bytes.
//
//	go run ./records-http      (listens on localhost:8094)
//
// Authentication is out of scope: the demo trusts the X-Tenant and X-User
// headers, which a real app replaces with its session lookup in
// scopeOf and userOf. The authorization rule is real: a user may change
// only their own attachments and only the files they uploaded.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// listenAddr is where main serves.
	listenAddr = "localhost:8094"
	// baseURL is the server's public origin; signed URLs point under it.
	baseURL = "http://" + listenAddr
	// diskName is the one disk uploads are stored on.
	diskName = "media"
	// ownerUser is the one owner type the app attaches files to.
	ownerUser = "user"
	// headerTenant and headerUser stand in for a session lookup.
	headerTenant = "X-Tenant"
	headerUser   = "X-User"
	// actorPrefix marks a user ID as an actor, as stored in UploadedBy.
	actorPrefix = "user:"
	// maxFileSize caps one uploaded file.
	maxFileSize = 64 << 10
	// maxFilesPerRequest caps the files of one upload request.
	maxFilesPerRequest = 3
	// linkLifetime is how long the signed URLs the file route redirects to
	// stay valid.
	linkLifetime = 5 * time.Minute
)

// signingSecret signs URLs; a real app loads it from its secret store.
var signingSecret = []byte("0123456789abcdef0123456789abcdef")

// policy accepts images and plain text, with random keys under uploads/.
// ds:def id=policy-wvgc7kq3
var policy = upload.Policy{
	MaxSize: maxFileSize,
	Allowed: []string{"image/*", "text/plain"},
	Key:     upload.RandomKey("uploads"),
}

// newDisks builds the manager and the signer: one memory disk whose
// signed URLs point at the fshttp.Signed handler mounted under /files.
// ds:def id=newdisks-y73hs4nf
func newDisks() (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(baseURL+"/files", []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, signer, err
}

// openSQLite opens a SQLite database file: one connection (SQLite has one
// writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newStore wires a Store that attaches files to users and creates its
// tables.
// ds:def id=newstore-peccqhpr
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// errNoSession refuses a request without a tenant (answered 401).
var errNoSession = errors.New("no session")

// errNotYours refuses a request on someone else's files (answered 403).
var errNotYours = errors.New("not yours")

// scopeOf returns the tenant the request acts in. Demo only: it trusts a
// header; a real app reads it from the session, never from the URL.
// ds:def id=scopeof-kgn4xeh9
func scopeOf(r *http.Request) (string, error) {
	if t := r.Header.Get(headerTenant); t != "" {
		return t, nil
	}
	return "", errNoSession
}

// authorizer returns the Authorize hook: owner routes are allowed only on
// the signed-in user's own owner row, file routes only on files that user
// uploaded. A detach carries the attachment's owner, so the owner rule
// covers it. recordshttp loads the file first, so within one tenant an
// unknown ID answers 404 and another user's file 403: a caller can tell
// whether an ID exists in its own tenant (IDs are random 128-bit values,
// so it cannot enumerate them), and never across tenants.
// ds:def id=authorizer-uejc9mcn
func authorizer() func(*http.Request, recordshttp.Action, recordshttp.Target) error {
	return func(r *http.Request, action recordshttp.Action, t recordshttp.Target) error {
		user := r.Header.Get(headerUser)
		if user == "" {
			return errNotYours
		}
		if t.Owner.Type != "" {
			if t.Owner.Type != ownerUser || t.Owner.ID != user {
				return errNotYours
			}
			return nil
		}
		// recordshttp loads the file in the request's scope before calling
		// this, so another tenant's file never reaches here (it is a 404).
		if t.File == nil || t.File.UploadedBy != actorPrefix+user {
			return errNotYours
		}
		return nil
	}
}

// withActor puts the signed-in user in the context, so the Store records
// them as UploadedBy of every file the request uploads.
// ds:def id=withactor-5rp58uu3
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), actorPrefix+user))
		}
		next.ServeHTTP(w, r)
	})
}

// newHandler mounts the records endpoints under /media and the signed
// download handler under /files.
// ds:def id=newhandler-ytttpkep
func newHandler(s *records.Store, disks *filesystem.Manager, signer *signedurl.Signer) (http.Handler, error) {
	media, err := recordshttp.Handler(recordshttp.Config{
		Store:        s,
		Disk:         diskName,
		Policy:       policy,
		MaxFiles:     maxFilesPerRequest,
		Scope:        scopeOf,
		Authorize:    authorizer(),
		LinkLifetime: linkLifetime,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/media/", http.StripPrefix("/media", withActor(media)))
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	return mux, nil
}

// build wires everything over a SQLite file in dir and returns the
// handler and how to close the database.
func build(ctx context.Context, dir string) (http.Handler, func(), error) {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return nil, nil, err
	}
	h, err := wire(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return h, func() { _ = db.Close() }, nil
}

// wire builds the disks, the Store and the handler over db, plus the
// /byo routes that put each tenant's uploads on its own bucket.
func wire(ctx context.Context, db *sql.DB) (http.Handler, error) {
	disks, signer, err := newDisks()
	if err != nil {
		return nil, err
	}
	if err := addTenantDisks(disks, signer); err != nil {
		return nil, err
	}
	s, err := newStore(ctx, db, disks)
	if err != nil {
		return nil, err
	}
	h, err := newHandler(s, disks, signer)
	if err != nil {
		return nil, err
	}
	byo, err := newBucketHandler(s)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/byo/", http.StripPrefix("/byo", withActor(byo)))
	mux.Handle("/", h)
	return mux, nil
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "records-http")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	h, closeDB, err := build(ctx, dir)
	if err != nil {
		log.Fatal(err)
	}
	defer closeDB()
	log.Printf("listening on %s", baseURL)
	srv := &http.Server{Addr: listenAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	if err := serve.UntilDone(ctx, srv); err != nil {
		log.Print(err)
	}
}
