// Command quickstart is the program behind docs/start/first-app.md: a photo
// service where a signed-in user uploads photos, lists them, opens one
// through a short-lived signed link, moves one to the trash, restores it,
// and deletes it for good.
//
// Bytes live on local disk (a "file:" DSN), rows in SQLite, and signed
// links are served by the program itself through fshttp.
//
//	go run ./quickstart        (from the examples directory)
//
// Routes (every one but /files/ needs the X-User header):
//
//	POST   /photos              multipart form, field "file"
//	GET    /photos              the user's photos
//	GET    /photos/{id}         302 to a signed link
//	DELETE /photos/{id}         move to the trash
//	POST   /photos/{id}/restore take out of the trash
//	GET    /trash               the user's trashed photos
//	DELETE /trash/{id}          delete for good, bytes included
//	GET    /files/...           signed links, served by fshttp
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

const (
	// addr is where the server listens: this machine only, for development.
	// ds:def id=quickstart-consts-5rhugm4x
	addr = "localhost:8082"
	// baseURL is the server's public URL. Signed links point at it.
	baseURL = "http://localhost:8082"
	// dataDir holds the stored files and the SQLite database, created next
	// to where you run the program.
	dataDir = "quickstart-data"
	// envStorageDSN overrides where the bytes go ("mem://" keeps them in
	// memory); unset, they go to dataDir/files.
	envStorageDSN = "STORAGE_DSN"
	// envSecret holds the signing secret for links, at least
	// signedurl.MinKeyBytes long. Unset, a random one is made at start-up.
	envSecret = "QUICKSTART_SECRET"
	// readHeaderTimeout bounds how long a client may take to send headers.
	readHeaderTimeout = 5 * time.Second
)

const (
	// diskName names the one disk; records store it with every file.
	// ds:def id=quickstart-names-j55c2mjk
	diskName = "media"
	// filesPrefix is where fshttp serves signed links.
	filesPrefix = "/files"
	// signingKeyID names the signing key inside each link, so a later key
	// can replace it without breaking links already handed out.
	signingKeyID = "k1"
	// ownerUser is the one owner type photos are attached to.
	ownerUser = "user"
	// rolePhotos is the role of a user's photos, and their key prefix.
	rolePhotos = "photos"
	// formField is the multipart field the photo arrives in.
	formField = "file"
	// maxPhotoSize is the largest photo accepted: 5 MiB.
	maxPhotoSize = 5 << 20
	// formOverhead is what a multipart body may add around the photo
	// (boundaries, part headers) before the request is cut off.
	formOverhead = 64 << 10
	// linkLifetime is how long a signed link to a photo works.
	linkLifetime = 5 * time.Minute
	// userHeader carries the signed-in user. It stands in for your
	// authentication: a real app reads the user from its session.
	userHeader = "X-User"
)

// config is what the app needs from outside: main fills it from the
// constants and the environment, tests from a temporary directory.
// ds:def id=quickstart-config-web4nsmd
type config struct {
	BaseURL    string
	DataDir    string
	StorageDSN string
	Secret     []byte
}

// ds:def id=quickstart-main-q476eb9b
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

// app holds what the handlers use: the records store (rows and bytes),
// the disks it reaches the bytes through, and the link signer.
type app struct {
	db     *sql.DB
	disks  *filesystem.Manager
	store  *records.Store
	signer *signedurl.Signer
}

// newApp wires the storage, the database and the store, and migrates.
// Any mistake in the wiring (a bad DSN, a short secret) fails here, at
// start-up, not at the first upload.
// ds:def id=quickstart-newapp-sbmrwauk
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

// openDisks opens the "media" disk from a DSN and checks it end to end.
// The Mux knows only the drivers registered here; moving to S3 later is
// registering its opener and changing the DSN.
// ds:def id=quickstart-opendisks-8e548zhk
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

// openDB opens the SQLite file with a busy timeout, so a writer waits for
// another instead of failing with "database is locked".
// ds:def id=quickstart-opendb-hejp8gnt
func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
}

// openStore builds the records store and creates its tables. Photos can
// be attached to users only: any other owner type is refused.
// ds:def id=quickstart-openstore-nwxqwbhb
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

// Close releases the database and the disks.
func (a *app) Close() error {
	return errors.Join(a.db.Close(), a.disks.Close())
}

// photoPolicy is what an upload must be to become a photo: an image, by
// its bytes, of at most 5 MiB, whose name does not claim another type. The
// key is random under photos/, so the client's file name never reaches it.
// ds:def id=quickstart-policy-zu25btv2
func photoPolicy() upload.Policy {
	return upload.Policy{
		MaxSize:        maxPhotoSize,
		Allowed:        []string{"image/png", "image/jpeg", "image/webp", "image/gif"},
		MatchExtension: true,
		Key:            upload.RandomKey(rolePhotos),
	}
}

// Handler routes the photo API behind requireUser, and the signed links
// without it: a link is its own authorisation.
// ds:def id=quickstart-handler-ys6fdgek
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

// userKey is the context key of the signed-in user.
type userKey struct{}

// requireUser refuses a request without a user, and otherwise records the
// user both for the handlers and as the actor of every change the store
// makes (File.UploadedBy).
// ds:def id=quickstart-requireuser-yutht6zb
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

// currentUser returns the user requireUser let in. Each user is the scope
// of their own files, so every query below is limited to that user's
// files by the store itself: another user's file ID is not found.
// ds:def id=quickstart-currentuser-m487796w
func currentUser(r *http.Request) string {
	user, _ := r.Context().Value(userKey{}).(string) // set by requireUser for every route that calls this
	return user
}

// uploadPhoto streams the photo from the form into storage and records it.
// ds:def id=quickstart-uploadphoto-xwydn32c
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

// formFile returns the form's file part without buffering the body: the
// photo streams from the connection to the disk.
// ds:def id=quickstart-formfile-jwqeg25d
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

// addPhoto uploads under the photo policy and attaches the file to the
// user, in one transaction: if the attachment fails, the row is rolled
// back and the store deletes the stored bytes, so nothing is left behind.
// ds:def id=quickstart-addphoto-rwxdsegc
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

// photo is a file as the API shows it. Clients address a photo by its ID;
// the storage key is the server's business.
// ds:def id=quickstart-photo-c3f4dmpd
type photo struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

// photoOf converts a stored file to its API form.
func photoOf(f records.File) photo {
	return photo{ID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, CreatedAt: f.CreatedAt}
}

// listPhotos lists the user's photos in upload order. Trashed ones are
// left out by the store.
// ds:def id=quickstart-listphotos-j8nb4nzd
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

// openPhoto redirects to a signed link that works for linkLifetime. The
// file stays private: the link is the only way to its bytes, and it
// expires.
// ds:def id=quickstart-openphoto-vs3hcttn
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

// trashPhoto moves a photo to the trash: still stored, no longer listed.
// ds:def id=quickstart-trashphoto-khwwnesx
func (a *app) trashPhoto(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Trash(r.Context(), currentUser(r), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restorePhoto takes a photo out of the trash; it is listed again, in its
// old place.
// ds:def id=app-restorephoto-h7tavh77
func (a *app) restorePhoto(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Restore(r.Context(), currentUser(r), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listTrash lists the user's trashed files, newest first (one page; a
// longer list continues with FilePage.NextCursor).
// ds:def id=quickstart-listtrash-pzv93qp4
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

// purgePhoto deletes a trashed photo for good: its row and attachment in
// one transaction, then its bytes once that commits. Only trashed photos
// can be purged, so one request can never destroy a live photo.
// ds:def id=quickstart-purgephoto-8f9jkzp7
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

// writeError answers with the HTTP status of the error's code and a JSON
// body naming the code. A server-side failure is logged, and the client
// gets the status text only, so no internal detail leaks.
// ds:def id=quickstart-writeerror-ajcm3vft
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	if status >= http.StatusInternalServerError {
		log.Printf("request failed: %v", err)
		writeJSON(w, status, upload.ErrorResponse{Error: http.StatusText(status), Code: filesystem.CodeOf(err)})
		return
	}
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON response body.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}
