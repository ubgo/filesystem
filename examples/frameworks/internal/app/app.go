// Package app wires the storage side of the framework examples once: the
// disks, the records store and one http.Handler per library endpoint. Each
// framework's program only mounts these handlers, which is the point the
// guides make: the library's endpoints are plain net/http handlers, and the
// framework changes nothing but the routing.
//
// Everything runs in-process: two memory disks, an S3 disk on a fake S3
// server (Uppy and presigned uploads need a backend that signs), and the
// records tables in an in-memory SQLite database. The X-User header stands
// in for the application's own authentication.
package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
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

// Mount points. Every framework mounts each handler at its path with the
// prefix stripped; the tus handler expects its prefix stripped with the
// trailing slash, because tusd builds upload URLs from BasePath.
const (
	PathUpload  = "/upload"
	PathTicket  = "/uploads/ticket"
	PathUppy    = "/uploads/s3"
	PathTus     = "/tus/"
	PathFiles   = "/files"
	PathPublic  = "/public"
	PathRecords = "/media"
)

// HeaderUser names the signed-in user. It stands in for real
// authentication (a session cookie, a bearer token).
const HeaderUser = "X-User"

// MaxUploadBytes is the policy's size limit. Frameworks that buffer request
// bodies (fiber) must allow at least this much plus form overhead.
const MaxUploadBytes = 10 << 20

// Disk names, application decisions; signed URLs carry them.
const (
	DiskMedia  = "media"
	DiskPublic = "public"
	DiskBucket = "bucket"
)

// Application decisions about uploads and identities.
const (
	maxFormFiles      = 5
	ticketLifetime    = 15 * time.Minute
	ownerUser         = "user"
	tenant            = "demo"
	signingKeyID      = "k1"
	s3Bucket          = "uploads"
	s3Region          = "us-east-1"
	incomingDir       = "incoming"
	readHeaderTimeout = 5 * time.Second
)

// Policy is the one upload policy every route enforces: images, PDFs and
// plain text up to MaxUploadBytes, stored under incoming/ with random names.
var Policy = upload.Policy{
	MaxSize: MaxUploadBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey(incomingDir),
}

// App is the wired storage side of an application.
type App struct {
	Disks    Disks
	Store    *records.Store
	Handlers Handlers
}

// Disks are the app's disks by role, and the Manager holding them all.
type Disks struct {
	Manager *filesystem.Manager
	Media   *filesystem.Disk
	Public  *filesystem.Disk
	Bucket  *filesystem.Disk
}

// Handlers are the library's endpoints, each a plain http.Handler.
// ds:def id=handlers-4j27haag
type Handlers struct {
	Upload  http.Handler // POST multipart form uploads
	Ticket  http.Handler // POST a TicketRequest, get a presigned PUT
	Uppy    http.Handler // Uppy's AWS S3 multipart protocol
	Tus     http.Handler // tus resumable uploads
	Files   http.Handler // signed downloads and uploads
	Public  http.Handler // public files by key
	Records http.Handler // upload, attach, list, reorder, read, trash
}

// New wires the app. baseURL is where the server is reached (signed links
// are built from it), s3URL the S3 endpoint, tusDir where unfinished tus
// sessions wait.
// ds:def id=new-weem4qf2
func New(ctx context.Context, baseURL, s3URL, tusDir string) (*App, error) {
	signer, err := newSigner(baseURL + PathFiles)
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

// newSigner returns an HMAC signer for the memory disks, with a secret that
// is random per start; production loads it from a secret store.
func newSigner(base string) (*signedurl.Signer, error) {
	secret := make([]byte, signedurl.MinKeyBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return signedurl.New(base, []signedurl.Key{{ID: signingKeyID, Secret: secret}})
}

// newDisks opens the two memory disks from DSNs and the S3 disk from a
// client of the app's own.
func newDisks(ctx context.Context, baseURL, s3URL string, signer *signedurl.Signer) (Disks, error) {
	m := filesystem.NewMux()
	if err := m.Register(memory.Scheme, memory.Opener); err != nil {
		return Disks{}, err
	}
	media, err := m.Open(ctx, DiskMedia, "mem://media", filesystem.WithURLSigner(signer))
	if err != nil {
		return Disks{}, err
	}
	public, err := m.Open(ctx, DiskPublic, "mem://public?visibility=public&public_url="+url.QueryEscape(baseURL+PathPublic))
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

// newBucket builds the S3 disk and creates its bucket on the fake server.
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
	return filesystem.NewDisk(DiskBucket, drv)
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

// newHandlers builds every endpoint once, so configuration errors surface
// at start-up.
func newHandlers(disks Disks, store *records.Store, signer *signedurl.Signer, tusDir string) (Handlers, error) {
	tusHandler, err := tus.New(tus.Config{Disk: disks.Media, Policy: Policy, TempDir: tusDir, BasePath: PathTus})
	if err != nil {
		return Handlers{}, err
	}
	recordsHandler, err := recordshttp.Handler(recordshttp.Config{
		Store: store, Disk: DiskMedia, Policy: Policy,
		Scope: scope,
		Authorize: func(r *http.Request, _ recordshttp.Action, t recordshttp.Target) error {
			return authorize(r.Context(), store, r.Header.Get(HeaderUser), t)
		},
	})
	if err != nil {
		return Handlers{}, err
	}
	return Handlers{
		Upload:  upload.Handler(disks.Media, Policy, upload.HandlerOptions{MaxFiles: maxFormFiles}),
		Ticket:  upload.PresignHandler(disks.Bucket, Policy, ticketLifetime),
		Uppy:    upload.Uppy(disks.Bucket, Policy, upload.UppyOptions{Authorize: allowUppy}),
		Tus:     tusHandler,
		Files:   fshttp.Signed(disks.Manager, signer),
		Public:  fshttp.Public(disks.Public),
		Records: withUser(recordsHandler),
	}, nil
}

// Errors the authorization hooks return.
var (
	errSignedOut = errors.New("sign in first")
	errNotYours  = errors.New("not your file")
)

// scope maps a request to its tenant: one tenant here, and none without a
// user (recordshttp answers 401).
func scope(r *http.Request) (string, error) {
	if r.Header.Get(HeaderUser) == "" {
		return "", errSignedOut
	}
	return tenant, nil
}

// authorize lets a user act on their own owner routes and on the files
// they uploaded.
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

// withUser records the signed-in user as the actor of every change.
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(HeaderUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), user))
		}
		next.ServeHTTP(w, r)
	})
}

// allowUppy authorises continuing an Uppy upload: a signed-in user and a
// key under the upload area.
func allowUppy(r *http.Request, key string) error {
	if r.Header.Get(HeaderUser) == "" {
		return errSignedOut
	}
	if !filesystem.KeyInDir(key, incomingDir) {
		return errNotYours
	}
	return nil
}

// StartFakeS3 serves an in-memory S3 on a free local port and returns its
// URL and a function that stops it.
func StartFakeS3() (string, func(), error) {
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

// ServeUntilDone serves srv until ctx ends (main cancels it on Ctrl+C or
// SIGTERM), then shuts it down gracefully and returns, so the caller's
// deferred clean-up (the tus session directory) runs instead of being
// skipped by a killed process.
// ds:def id=serveuntildone-cgw5a9pa
func ServeUntilDone(ctx context.Context, srv *http.Server) error {
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
