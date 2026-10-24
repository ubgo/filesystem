// Command recipe-browser-direct-uploads lets a browser send files straight
// to an S3 bucket, so large uploads never pass through the application
// server: the server hands out a presigned PUT (or, for big files, signs
// the parts of a multipart upload for Uppy), the browser uploads, and the
// server confirms the stored bytes against the policy before recording the
// file. Unconfirmed uploads are swept by age.
//
//	go run ./recipe-browser-direct-uploads       (listens on localhost:8105)
//
// Without STORAGE_DSN it runs an in-process fake S3 (gofakes3) on a random
// local port, so it works offline; with STORAGE_DSN=s3://bucket?region=…
// it uses a real bucket (credentials from the AWS default chain).
//
// Routes (the demo trusts the X-User header; a real app uses its session):
//
//	POST /uploads/tickets      {"filename","content_type","size"} → a presigned PUT
//	POST /uploads/confirm      {"key","filename"} → checks, moves and records the file
//	GET  /uploads              the user's confirmed files
//	/uploads/s3/multipart…     Uppy's AWS S3 multipart protocol
//	/uploads/s3r/multipart…    the same, recording the file on complete
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8105"
	// envDSN names the variable holding a real bucket's DSN.
	envDSN = "STORAGE_DSN"
	// diskName names the bucket disk in the manager.
	diskName = "uploads"
	// fakeBucket is the bucket created in the in-process fake.
	fakeBucket = "uploads"
	// fakeRegion is the region the fake's client signs for.
	fakeRegion = "us-east-1"
	// pendingDir holds uploads the browser sent but the server has not yet
	// confirmed. Confirmed files move out of it, so Sweep can delete by age.
	pendingDir = "pending"
	// filesDir holds confirmed files.
	filesDir = "files"
	// ticketLifetime is how long a presigned PUT stays valid.
	ticketLifetime = 15 * time.Minute
	// pendingMaxAge is how old an unconfirmed upload may get before Sweep
	// deletes it; longer than any ticket, so no upload in flight is lost.
	pendingMaxAge = 24 * time.Hour
	// maxUploadBytes caps one file.
	maxUploadBytes = 100 << 20
	// maxJSONBytes caps the JSON bodies of the app's own routes.
	maxJSONBytes = 16 << 10
	// ownerUser and roleUploads are where confirmed files are attached.
	ownerUser   = "user"
	roleUploads = "uploads"
	// headerUser carries the demo's user; a real app reads its session.
	headerUser = "X-User"
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// allowedTypes are the types this app accepts, checked twice: from the
// client's declaration when a ticket is issued, and from the stored bytes
// when the upload is confirmed.
// ds:def id=allowedtypes-3jfr2jzm
var allowedTypes = []string{"image/jpeg", "image/png", "image/webp", "application/pdf", "video/mp4"}

// policyFor is the upload policy of one user. The key places the upload
// under pending/<user>/, which is how confirm and the Uppy routes know an
// upload belongs to the user asking about it: a KeyFunc sees no request,
// so the user is bound in when the policy is built.
// ds:def id=policyfor-4j7a9dpb
func policyFor(user string) upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadBytes,
		Allowed: allowedTypes,
		Key:     upload.RandomKey(path.Join(pendingDir, user)),
	}
}

// openFakeS3 starts an in-process S3 server with one bucket and returns a
// disk on it. The server listens on a random local port; presigned URLs
// point at it, so a browser or curl on this machine can upload.
// ds:def id=openfakes3-pyu59jvs
func openFakeS3(ctx context.Context) (*filesystem.Disk, func(), error) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Region:       fakeRegion,
		Credentials:  credentials.NewStaticCredentialsProvider("fake", "fake", ""),
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(fakeBucket)}); err != nil {
		srv.Close()
		return nil, nil, err
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: fakeBucket})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return disk, srv.Close, nil
}

// openDisk opens the real bucket named by STORAGE_DSN, or the fake.
// ds:def id=opendisk-ztr3zg6t
func openDisk(ctx context.Context) (*filesystem.Disk, func(), error) {
	dsn, ok := os.LookupEnv(envDSN)
	if !ok {
		return openFakeS3(ctx)
	}
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return nil, nil, err
	}
	disk, err := mux.Open(ctx, diskName, dsn)
	if err != nil {
		return nil, nil, err
	}
	return disk, func() { _ = disk.Close() }, nil
}

// App holds the wired dependencies.
type App struct {
	Disk    *filesystem.Disk
	Store   *records.Store
	Handler http.Handler
}

// New wires the app over a disk and a data directory for SQLite.
func New(ctx context.Context, disk *filesystem.Disk, dataDir string) (*App, error) {
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	app := &App{Disk: disk, Store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /uploads/tickets", app.ticket)
	mux.HandleFunc("POST /uploads/confirm", app.confirm)
	mux.HandleFunc("GET /uploads", app.list)
	mux.Handle("/uploads/s3/", http.StripPrefix("/uploads/s3", http.HandlerFunc(app.uppy)))
	mux.Handle(recordedPrefix+"/", http.StripPrefix(recordedPrefix, http.HandlerFunc(app.uppyRecorded)))
	app.Handler = mux
	return app, nil
}

// userOf returns the signed-in user. Demo only: it trusts a header.
func userOf(r *http.Request) (string, error) {
	if u := r.Header.Get(headerUser); u != "" {
		return u, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "sign in first")
}

// ticket issues a presigned PUT. Presign checks the declared type, name
// and size against the policy, but they are the client's word; the bytes
// are checked in confirm. The URL pins the declared Content-Type.
// ds:def id=app-ticket-ungdkkjy
func (a *App) ticket(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req upload.TicketRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := upload.Presign(r.Context(), a.Disk, req, policyFor(user), ticketLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// confirmRequest is what the browser sends once its PUT succeeded.
type confirmRequest struct {
	Key      string `json:"key"`
	Filename string `json:"filename"`
}

// fileResponse describes a recorded file.
type fileResponse struct {
	FileID      string `json:"file_id"`
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// confirm checks that the key is the user's own pending upload, then
// finishes it.
// ds:def id=app-confirm-tjbds98v
func (a *App) confirm(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req confirmRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !filesystem.KeyInDir(req.Key, path.Join(pendingDir, user)) {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "no pending upload %q", req.Key))
		return
	}
	f, err := a.finish(r.Context(), user, req.Key, req.Filename)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(f))
}

// finish runs upload.Confirm on the stored bytes (size, type detected
// from content; a refused object is deleted), moves the object out of
// pending/ so the sweep leaves it alone, and records and attaches it in
// one transaction. If recording fails the moved object is deleted, so a
// failure leaves neither bytes nor a row.
// ds:def id=app-finish-cxhgemb8
func (a *App) finish(ctx context.Context, user, key, filename string) (records.File, error) {
	res, err := upload.Confirm(ctx, a.Disk, key, filename, policyFor(user))
	if err != nil {
		return records.File{}, err
	}
	final := path.Join(filesDir, user, path.Base(res.Key))
	if _, err := a.Disk.Move(ctx, res.Key, final); err != nil {
		return records.File{}, err
	}
	var f records.File
	err = a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err = a.Store.CreateFile(ctx, records.NewFile{
			Disk: diskName, Key: final, Filename: res.Filename,
			ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256, UploadedBy: user,
		})
		if err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleUploads)
		return err
	})
	if err != nil {
		return records.File{}, errors.Join(err, a.Disk.Delete(context.WithoutCancel(ctx), final))
	}
	return f, nil
}

// uppy serves Uppy's multipart protocol for the signed-in user. The handler
// is built per request because the policy carries the user's key prefix;
// Authorize refuses any upload ID and key outside that prefix, so one user
// cannot sign parts for, complete or abort another's upload.
// ds:def id=app-uppy-p728nqjx
func (a *App) uppy(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	mine := path.Join(pendingDir, user)
	opts := upload.UppyOptions{Authorize: func(_ *http.Request, key string) error {
		if !filesystem.KeyInDir(key, mine) {
			return fmt.Errorf("key %q is not this user's upload", key)
		}
		return nil
	}}
	upload.Uppy(a.Disk, policyFor(user), opts).ServeHTTP(w, r)
}

// list returns the user's confirmed files.
func (a *App) list(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
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
		out = append(out, toResponse(af.File))
	}
	writeJSON(w, http.StatusOK, out)
}

// sweepPending deletes unconfirmed uploads older than pendingMaxAge: a
// browser that never called confirm, a refused tab, a crash. The library
// starts no background work; run it from a ticker or a cron job.
// ds:def id=sweeppending-675fbb6c
func sweepPending(ctx context.Context, disk *filesystem.Disk, now time.Time) (int, error) {
	return upload.Sweep(ctx, disk, pendingDir, now.Add(-pendingMaxAge))
}

// runSweeper sweeps once an hour until ctx ends, logging what it did.
// ds:def id=runsweeper-px4wnzxq
func runSweeper(ctx context.Context, disk *filesystem.Disk) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			n, err := sweepPending(ctx, disk, now)
			fmt.Fprintf(os.Stderr, "sweep: deleted %d unconfirmed uploads (err: %v)\n", n, err)
		}
	}
}

func toResponse(f records.File) fileResponse {
	return fileResponse{FileID: f.ID, Key: f.Key, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, SHA256: f.SHA256}
}

// decodeJSON reads a small JSON body into v.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, v *T) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body")
	}
	return nil
}

// clientVisible lists the codes whose messages describe the client's own
// input and may be shown to it. Any other error (a backend's message, a
// key that does not exist) is answered with the status text only.
var clientVisible = map[filesystem.Code]bool{
	filesystem.CodeTooLarge:        true,
	filesystem.CodeRejected:        true,
	filesystem.CodeInvalidArgument: true,
	filesystem.CodeInvalidKey:      true,
}

// writeError answers with the status for err's code and a JSON body that
// names the code.
// ds:def id=writeerror-56yg78mg
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

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	disk, closeDisk, err := openDisk(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer closeDisk()
	dir, err := os.MkdirTemp("", "recipe-browser-direct-uploads-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, disk, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go runSweeper(ctx, disk)
	fmt.Println("listening on http://" + listenAddr)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
