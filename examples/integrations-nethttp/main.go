// Command integrations-nethttp mounts every HTTP handler the library ships
// on a standard library ServeMux, and the same handlers on a gorilla/mux
// router: form uploads (upload.Handler), direct-upload tickets
// (upload.PresignHandler), Uppy multipart (upload.Uppy), tus resumable
// uploads (contrib/tus), signed downloads and uploads (fshttp.Signed),
// public files (fshttp.Public) and the records endpoints (recordshttp).
//
//	go run ./integrations-nethttp            (listens on localhost:8090)
//	go run ./integrations-nethttp -gorilla   (same routes on gorilla/mux)
//
// Everything runs in-process: two memory disks, an S3 disk on a fake S3
// server (Uppy and presigned uploads need a backend that signs), and the
// records tables in an in-memory SQLite database. The X-User header stands
// in for the application's own authentication.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gorilla/mux"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/contrib/tus"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Where the server listens and the URL clients reach it at. Signed links
// are built from baseURL, so it must be the address browsers use.
const (
	addr    = "localhost:8090"
	baseURL = "http://" + addr
	// readHeaderTimeout stops a slow client from holding a connection open
	// before it has sent its headers.
	readHeaderTimeout = 5 * time.Second
)

// Disk names. They are the application's choice; signed URLs carry them.
const (
	diskMedia  = "media"  // private files, served through signed links
	diskPublic = "public" // public files, served by fshttp.Public
	diskBucket = "bucket" // the S3 disk direct and Uppy uploads land on
)

// Mount points. Each handler is mounted with its prefix stripped.
const (
	pathUpload  = "/upload"
	pathTicket  = "/uploads/ticket"
	pathUppy    = "/uploads/s3"
	pathTus     = "/tus/"
	pathFiles   = "/files"
	pathPublic  = "/public"
	pathRecords = "/media"
)

// Application decisions about uploads and identities.
const (
	maxUploadBytes = 10 << 20
	maxFormFiles   = 5
	ticketLifetime = 15 * time.Minute
	ownerUser      = "user"
	tenant         = "demo"
	headerUser     = "X-User"
	signingKeyID   = "k1"
	signingKeySize = signedurl.MinKeyBytes
	s3Bucket       = "uploads"
	s3Region       = "us-east-1"
	tusTempPattern = "tus-sessions-*"
)

// policy is the one upload policy every route enforces: images, PDFs and
// plain text up to 10 MiB, stored under incoming/ with random names.
// ds:def id=policy-9pjq8nta
var policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey("incoming"),
}

func main() {
	useGorilla := flag.Bool("gorilla", false, "serve the routes with gorilla/mux instead of http.ServeMux")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *useGorilla); err != nil {
		log.Fatal(err)
	}
}

// run wires the app and serves it until the process is stopped.
func run(ctx context.Context, useGorilla bool) error {
	s3URL, stopS3, err := startFakeS3()
	if err != nil {
		return err
	}
	defer stopS3()
	tusDir, err := os.MkdirTemp("", tusTempPattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tusDir) }()
	app, err := newApp(ctx, baseURL, s3URL, tusDir)
	if err != nil {
		return err
	}
	var h http.Handler = newServeMux(app.Handlers)
	if useGorilla {
		h = newGorilla(app.Handlers)
	}
	log.Printf("listening on %s", baseURL)
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	return serveUntilDone(ctx, srv)
}

// App is the wired application: its disks, its records store, and one
// http.Handler per library endpoint, ready to mount on any router.
type App struct {
	Disks    Disks
	Store    *records.Store
	Handlers Handlers
}

// Disks are the app's disks, by role, and the Manager holding them all
// (fshttp.Signed and records look disks up by name in it).
type Disks struct {
	Manager *filesystem.Manager
	Media   *filesystem.Disk
	Public  *filesystem.Disk
	Bucket  *filesystem.Disk
}

// Handlers are the library's endpoints. Each is a plain http.Handler, which
// is why the same values mount on net/http, gorilla, chi, gin, echo or fiber.
// ds:def id=handlers-85duqk3k
type Handlers struct {
	Upload  http.Handler // POST multipart form uploads
	Ticket  http.Handler // POST a TicketRequest, get a presigned PUT
	Uppy    http.Handler // Uppy's AWS S3 multipart protocol
	Tus     http.Handler // tus resumable uploads
	Files   http.Handler // signed downloads and uploads
	Public  http.Handler // public files by key
	Records http.Handler // upload, attach, list, reorder, read, trash
}

// newApp builds the disks, the records store and the handlers. baseURL is
// where the server is reached, s3URL the S3 endpoint, tusDir where
// unfinished tus sessions wait.
func newApp(ctx context.Context, baseURL, s3URL, tusDir string) (*App, error) {
	signer, err := newSigner(baseURL + pathFiles)
	if err != nil {
		return nil, err
	}
	disks, err := newDisks(ctx, baseURL, s3URL, signer)
	if err != nil {
		return nil, err
	}
	store, err := newStore(ctx, disks.Manager)
	if err != nil {
		return nil, err
	}
	h, err := newHandlers(disks, store, signer, tusDir)
	if err != nil {
		return nil, err
	}
	return &App{Disks: disks, Store: store, Handlers: h}, nil
}

// newSigner returns an HMAC signer for the memory disks, whose backend
// cannot sign. The secret is random per start, so links die with the
// process; production loads it from a secret store and keeps old keys
// listed while links signed with them are alive.
// ds:def id=newsigner-hv3c3az5
func newSigner(base string) (*signedurl.Signer, error) {
	secret := make([]byte, signingKeySize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return signedurl.New(base, []signedurl.Key{{ID: signingKeyID, Secret: secret}})
}

// newDisks opens the three disks. The memory disks come from DSNs, so the
// same code takes "s3://…" or "file:///…" in production; the S3 disk is
// built from a client because its endpoint is the in-process fake.
// ds:def id=newdisks-2t595xpg
func newDisks(ctx context.Context, baseURL, s3URL string, signer *signedurl.Signer) (Disks, error) {
	m := filesystem.NewMux()
	if err := m.Register(memory.Scheme, memory.Opener); err != nil {
		return Disks{}, err
	}
	media, err := m.Open(ctx, diskMedia, "mem://media", filesystem.WithURLSigner(signer))
	if err != nil {
		return Disks{}, err
	}
	public, err := m.Open(ctx, diskPublic, "mem://public?visibility=public&public_url="+url.QueryEscape(baseURL+pathPublic))
	if err != nil {
		return Disks{}, err
	}
	bucket, err := newBucket(ctx, s3URL)
	if err != nil {
		return Disks{}, err
	}
	manager, err := filesystem.NewManager(media, public, bucket)
	if err != nil {
		return Disks{}, err
	}
	return Disks{Manager: manager, Media: media, Public: public, Bucket: bucket}, nil
}

// newBucket builds the S3 disk on an S3 client of the application's own,
// creating the bucket on the fake server.
func newBucket(ctx context.Context, endpoint string) (*filesystem.Disk, error) {
	client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Region:       s3Region,
		Credentials:  credentials.NewStaticCredentialsProvider("example", "example", ""),
	})
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(s3Bucket)}); err != nil {
		return nil, err
	}
	drv, err := s3.New(s3.Config{Client: client, Bucket: s3Bucket})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(diskBucket, drv)
}

// newStore opens the records tables in an in-memory SQLite database. One
// connection keeps the in-memory database alive and serialises writers.
func newStore(ctx context.Context, disks *filesystem.Manager) (*records.Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
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
	return store, nil
}

// newHandlers builds every endpoint once. Construction errors (a missing
// hook, an invalid policy) surface here, at start-up.
// ds:def id=newhandlers-y3r6cbah
func newHandlers(disks Disks, store *records.Store, signer *signedurl.Signer, tusDir string) (Handlers, error) {
	tusHandler, err := tus.New(tus.Config{Disk: disks.Media, Policy: policy, TempDir: tusDir, BasePath: pathTus})
	if err != nil {
		return Handlers{}, err
	}
	recordsHandler, err := newRecordsHandler(store)
	if err != nil {
		return Handlers{}, err
	}
	return Handlers{
		Upload:  upload.Handler(disks.Media, policy, upload.HandlerOptions{MaxFiles: maxFormFiles}),
		Ticket:  upload.PresignHandler(disks.Bucket, policy, ticketLifetime),
		Uppy:    upload.Uppy(disks.Bucket, policy, upload.UppyOptions{Authorize: allowUppy}),
		Tus:     tusHandler,
		Files:   fshttp.Signed(disks.Manager, signer),
		Public:  fshttp.Public(disks.Public),
		Records: withUser(recordsHandler),
	}, nil
}

// newRecordsHandler configures recordshttp. Scope and Authorize are
// required: the library knows nothing about users, so these two hooks are
// where the application's authentication decides.
// ds:def id=newrecordshandler-365kkwzk
func newRecordsHandler(store *records.Store) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:  store,
		Disk:   diskMedia,
		Policy: policy,
		Scope: func(r *http.Request) (string, error) {
			if r.Header.Get(headerUser) == "" {
				return "", errSignedOut
			}
			return tenant, nil
		},
		Authorize: func(r *http.Request, _ recordshttp.Action, t recordshttp.Target) error {
			return authorize(r.Context(), store, r.Header.Get(headerUser), t)
		},
	})
}

// errSignedOut refuses a request without a user (recordshttp answers 401).
var errSignedOut = errors.New("sign in first")

// errNotYours refuses a request on someone else's files (403).
var errNotYours = errors.New("not your file")

// authorize lets a user act on their own owner routes, and on files they
// uploaded. A real app asks its own permission model here.
// ds:def id=authorize-u2cj265k
func authorize(ctx context.Context, store *records.Store, user string, t recordshttp.Target) error {
	if t.FileID == "" {
		if t.Owner.Type == ownerUser && t.Owner.ID == user {
			return nil
		}
		return errNotYours
	}
	f, err := store.File(ctx, t.Scope, t.FileID)
	if err != nil {
		return err
	}
	if f.UploadedBy != user {
		return errNotYours
	}
	return nil
}

// withUser records the signed-in user as the actor of every change, so
// uploads carry UploadedBy (and the audit plugin, when installed, logs it).
// ds:def id=withuser-gs93hupd
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), user))
		}
		next.ServeHTTP(w, r)
	})
}

// allowUppy authorises the continuation of an Uppy upload. Uploads land
// under the policy's random keys, so this demo accepts any key under
// incoming/; a real app checks the key belongs to the requesting user.
// ds:def id=allowuppy-peqz35h9
func allowUppy(r *http.Request, key string) error {
	if r.Header.Get(headerUser) == "" {
		return errSignedOut
	}
	if !filesystem.KeyInDir(key, "incoming") {
		return errNotYours
	}
	return nil
}

// newServeMux mounts every handler on the standard library router. The
// library's handlers route on the path below their mount point, so each is
// wrapped in http.StripPrefix; the tus handler expects its prefix stripped
// with the trailing slash.
// ds:def id=newservemux-usseuwm9
func newServeMux(h Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST "+pathUpload, h.Upload)
	mux.Handle("POST "+pathTicket, h.Ticket)
	mux.Handle(pathUppy+"/", http.StripPrefix(pathUppy, h.Uppy))
	mux.Handle(pathTus, http.StripPrefix(pathTus, h.Tus))
	mux.Handle(pathFiles+"/", http.StripPrefix(pathFiles, h.Files))
	mux.Handle(pathPublic+"/", http.StripPrefix(pathPublic, h.Public))
	mux.Handle(pathRecords+"/", http.StripPrefix(pathRecords, h.Records))
	return mux
}

// newGorilla mounts the same handlers on gorilla/mux. PathPrefix plays the
// part of a trailing-slash pattern; the prefix is stripped the same way.
// ds:def id=newgorilla-et5p2jev
func newGorilla(h Handlers) *mux.Router {
	r := mux.NewRouter()
	r.Handle(pathUpload, h.Upload).Methods(http.MethodPost)
	r.Handle(pathTicket, h.Ticket).Methods(http.MethodPost)
	r.PathPrefix(pathUppy + "/").Handler(http.StripPrefix(pathUppy, h.Uppy))
	r.PathPrefix(pathTus).Handler(http.StripPrefix(pathTus, h.Tus))
	r.PathPrefix(pathFiles + "/").Handler(http.StripPrefix(pathFiles, h.Files))
	r.PathPrefix(pathPublic + "/").Handler(http.StripPrefix(pathPublic, h.Public))
	r.PathPrefix(pathRecords + "/").Handler(http.StripPrefix(pathRecords, h.Records))
	return r
}

// startFakeS3 serves an in-memory S3 on a free local port, so the example
// needs no cloud account. Replace it with your provider's endpoint.
func startFakeS3() (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: gofakes3.New(s3mem.New()).Server(), ReadHeaderTimeout: readHeaderTimeout}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// shutdownTimeout bounds how long a stopping server waits for requests in
// flight.
const shutdownTimeout = 5 * time.Second

// serveUntilDone serves srv until ctx ends (main cancels it on Ctrl+C or
// SIGTERM), then shuts it down gracefully and returns, so the deferred
// clean-up (the fake S3 and the tus session directory) runs instead of
// being skipped by a killed process.
// ds:def id=serveuntildone-wvuc3w29
func serveUntilDone(ctx context.Context, srv *http.Server) error {
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
