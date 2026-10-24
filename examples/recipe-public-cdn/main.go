// Command recipe-public-cdn keeps public files on a bucket behind a CDN and
// private ones on another bucket reached only through signed URLs: a CMS or
// a site builder where an uploaded image is private until it is published,
// can be unpublished again, and the app's own build assets are cached by
// browsers and the CDN for a year.
//
//	go run ./recipe-public-cdn       (listens on localhost:8103)
//
// With no environment it runs offline: both "buckets" are memory disks and
// this server plays the CDN (/cdn/…) and the signed-URL endpoint (/files/…).
// With CDN_DSN, VAULT_DSN and CDN_BASE_URL set it uses real buckets, e.g.
//
//	CDN_DSN='s3://site-public?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com'
//	VAULT_DSN='s3://site-private?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com'
//	CDN_BASE_URL='https://cdn.example.com'
//
// Routes:
//
//	POST /assets                    multipart, field "file": a private upload
//	GET  /assets/{id}               302 to the public or a signed URL
//	POST /assets/{id}/publish       moves the file to the CDN bucket
//	POST /assets/{id}/unpublish     moves it back and purges the CDN
//	POST /site-assets?ext=.css      raw body: a content-addressed build asset
//
// Authentication is out of scope: every route here is an editor's.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8103"
	// baseURL is the server's origin in offline mode.
	baseURL = "http://" + listenAddr
	// cdnPrefix is where this server plays the CDN in offline mode.
	cdnPrefix = "/cdn"
	// signedPrefix is where signed URLs of the offline vault point.
	signedPrefix = "/files"
	// diskCDN and diskVault name the public and the private disk.
	diskCDN   = "cdn"
	diskVault = "vault"
	// ownerSite is the owner type uploads are attached to.
	ownerSite = "site"
	// immutable is the Cache-Control of an object whose key changes
	// whenever its content does: cache it for a year, never revalidate.
	immutable = "public, max-age=31536000, immutable"
	// assetDir is where content-addressed build assets live.
	assetDir = "assets"
	// maxUpload and maxAsset cap an editor's upload and a build asset.
	maxUpload = 20 << 20
	maxAsset  = 2 << 20
	// linkLifetime is the lifetime of signed links to private files.
	linkLifetime = 10 * time.Minute
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// Environment variables of the real-bucket mode.
const (
	envCDNDSN   = "CDN_DSN"
	envVaultDSN = "VAULT_DSN"
	envCDNBase  = "CDN_BASE_URL"
)

// assetTypes is the closed set of build-asset extensions and their types.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".png":   "image/png",
}

// uploadPolicy stores editors' uploads private: on the vault disk, because
// the store chooses the disk by visibility (WithVisibilityDisks).
// ds:def id=uploadpolicy-mdq86khj
var uploadPolicy = upload.Policy{
	MaxSize:    maxUpload,
	Allowed:    []string{"image/*", "application/pdf", "video/mp4"},
	Key:        upload.RandomKey("uploads"),
	Visibility: filesystem.VisibilityPrivate,
}

// Disks are the two buckets, and the handlers that stand in for the CDN
// and the signed-URL endpoint when they are memory disks (nil otherwise).
// ds:def id=disks-hq2zqbew
type Disks struct {
	CDN, Vault *filesystem.Disk
	Serve      map[string]http.Handler
}

// memoryDisks builds the offline pair. The memory driver stores headers
// and per-object visibility, as S3, GCS and Azure do; the local driver
// stores neither headers nor content types, so it cannot stand in here.
// ds:def id=memorydisks-fjqmsg48
func memoryDisks(origin string, secret []byte) (Disks, error) {
	cdn, err := filesystem.NewDisk(diskCDN, memory.New(),
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic),
		filesystem.WithPublicURL(origin+cdnPrefix))
	if err != nil {
		return Disks{}, err
	}
	signer, err := signedurl.New(origin+signedPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return Disks{}, err
	}
	vault, err := filesystem.NewDisk(diskVault, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return Disks{}, err
	}
	mgr, err := filesystem.NewManager(cdn, vault)
	if err != nil {
		return Disks{}, err
	}
	return Disks{CDN: cdn, Vault: vault, Serve: map[string]http.Handler{
		cdnPrefix:    fshttp.Public(cdn),
		signedPrefix: fshttp.Signed(mgr, signer),
	}}, nil
}

// bucketDisks opens the real pair from DSNs. The CDN bucket's default
// visibility is public (its bucket policy or CDN makes it readable) and
// its public URLs are the CDN's; the vault is private and signs with SigV4.
// Credentials come from the AWS default chain, never from the DSN.
// ds:def id=bucketdisks-wqfyd4st
func bucketDisks(ctx context.Context, cdnDSN, vaultDSN, cdnBase string) (Disks, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(s3.Scheme, s3.Opener); err != nil {
		return Disks{}, err
	}
	cdn, err := mux.Open(ctx, diskCDN, cdnDSN,
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic),
		filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return Disks{}, err
	}
	vault, err := mux.Open(ctx, diskVault, vaultDSN)
	if err != nil {
		return Disks{}, err
	}
	return Disks{CDN: cdn, Vault: vault}, nil
}

// CDNPurger asks the CDN to drop cached copies of urls (Cloudflare's
// purge_cache, CloudFront's CreateInvalidation, …).
// ds:def id=cdnpurger-e54eggag
type CDNPurger func(ctx context.Context, urls []string) error

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	Disks   Disks
	Purge   CDNPurger
	Handler http.Handler
}

// openStore opens SQLite and the records Store. WithVisibilityDisks keeps
// public files on the CDN disk and private ones on the vault, and makes
// SetVisibility move a file between them.
// ds:def id=openstore-w3rxwyq4
func openStore(ctx context.Context, dataDir string, d Disks) (*records.Store, error) {
	mgr, err := filesystem.NewManager(d.CDN, d.Vault)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "site.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, mgr,
		records.WithOwnerTypes(ownerSite),
		records.WithVisibilityDisks(diskCDN, diskVault))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}

// New wires the app over disks; purge is told of every URL that stops
// being public.
func New(ctx context.Context, dataDir string, d Disks, purge CDNPurger) (*App, error) {
	store, err := openStore(ctx, dataDir, d)
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, Disks: d, Purge: purge}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /assets", app.uploadAsset)
	mux.HandleFunc("GET /assets/{id}", app.getAsset)
	mux.HandleFunc("POST /assets/{id}/publish", app.publish)
	mux.HandleFunc("POST /assets/{id}/unpublish", app.unpublish)
	mux.HandleFunc("POST /site-assets", app.siteAsset)
	for prefix, h := range d.Serve {
		mux.Handle(prefix+"/", http.StripPrefix(prefix, h))
	}
	app.Handler = mux
	return app, nil
}

// assetResponse describes a file and where it can be read now.
type assetResponse struct {
	FileID     string                `json:"file_id,omitempty"`
	Disk       string                `json:"disk,omitempty"`
	Visibility filesystem.Visibility `json:"visibility,omitempty"`
	URL        string                `json:"url"`
}

// respond answers with f's current disk, visibility and URL.
func (a *App) respond(w http.ResponseWriter, r *http.Request, f records.File) {
	u, err := a.Store.URL(r.Context(), f, linkLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assetResponse{FileID: f.ID, Disk: f.Disk, Visibility: f.Visibility, URL: u})
}

// uploadAsset stores an editor's upload privately. An empty disk name lets
// the store choose by the policy's visibility: the vault.
// ds:def id=app-uploadasset-tawk584g
func (a *App) uploadAsset(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the file in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.Store.Upload(r.Context(), "", "", file, header.Filename, uploadPolicy)
	if err != nil {
		writeError(w, err)
		return
	}
	a.respond(w, r, f)
}

// getAsset redirects to the file: its CDN URL when public, a signed URL
// valid for linkLifetime when private.
func (a *App) getAsset(w http.ResponseWriter, r *http.Request) {
	f, err := a.Store.File(r.Context(), "", r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	u, err := a.Store.URL(r.Context(), f, linkLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// publish moves the file to the CDN disk: copied there first, the row
// updated, the vault copy deleted after the commit.
// ds:def id=app-publish-vzs5yhrh
func (a *App) publish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.Store.SetVisibility(r.Context(), "", id, filesystem.VisibilityPublic); err != nil {
		writeError(w, err)
		return
	}
	f, err := a.Store.File(r.Context(), "", id)
	if err != nil {
		writeError(w, err)
		return
	}
	a.respond(w, r, f)
}

// makePrivate moves a public file back to the vault and, once that has
// committed, asks the CDN to forget its public URL: the bucket no longer
// has the object, but edge caches keep it until they are purged.
// ds:def id=app-makeprivate-tu66zhch
func (a *App) makePrivate(ctx context.Context, id string) (records.File, error) {
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.File(ctx, "", id)
		if err != nil || f.Visibility != filesystem.VisibilityPublic {
			return err
		}
		public, err := a.Store.URL(ctx, f, linkLifetime)
		if err != nil {
			return err
		}
		if err := a.Store.SetVisibility(ctx, "", id, filesystem.VisibilityPrivate); err != nil {
			return err
		}
		a.Store.AfterCommit(ctx, func(ctx context.Context) {
			if err := a.Purge(ctx, []string{public}); err != nil {
				fmt.Fprintf(os.Stderr, "cdn purge of %s: %v\n", public, err)
			}
		})
		return nil
	})
	if err != nil {
		return records.File{}, err
	}
	return a.Store.File(ctx, "", id)
}

// unpublish handles POST /assets/{id}/unpublish.
func (a *App) unpublish(w http.ResponseWriter, r *http.Request) {
	f, err := a.makePrivate(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	a.respond(w, r, f)
}

// publishAsset stores a build asset under a key derived from its content,
// so a changed file gets a new URL and an unchanged one keeps its URL and
// its cached copies. That is what makes "immutable" safe. Writing the same
// bytes twice is a no-op.
// ds:def id=publishasset-vwgjvveb
func publishAsset(ctx context.Context, disk *filesystem.Disk, ext string, body []byte) (string, error) {
	sum := sha256.Sum256(body)
	key := filesystem.JoinKey(assetDir, hex.EncodeToString(sum[:16])+ext)
	exists, err := disk.Exists(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists {
		opts := filesystem.WriteOptions{ContentType: assetTypes[ext], MaxSize: maxAsset}
		if disk.Capabilities().Headers {
			opts.Headers = filesystem.HTTPHeaders{CacheControl: immutable}
		}
		if _, err := disk.WriteBytes(ctx, key, body, opts); err != nil {
			return "", err
		}
	}
	return disk.PublicURL(ctx, key)
}

// siteAsset handles POST /site-assets?ext=…, as a deploy step would call
// publishAsset for every file of a build.
func (a *App) siteAsset(w http.ResponseWriter, r *http.Request) {
	ext := r.URL.Query().Get("ext")
	if _, ok := assetTypes[ext]; !ok {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "ext must be one of .css, .js, .woff2, .png"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAsset))
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeTooLarge, "asset over %d bytes", maxAsset))
		return
	}
	u, err := publishAsset(r.Context(), a.Disks.CDN, ext, body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assetResponse{URL: u})
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
	enc := json.NewEncoder(w)
	// URLs are answered as written: HTML escaping would turn the & of a
	// signed URL's query into \u0026, which is valid JSON but hard to read.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // a client that hung up cannot be told
}

// logPurge stands in for the CDN's purge API.
func logPurge(_ context.Context, urls []string) error {
	fmt.Println("cdn purge:", urls)
	return nil
}

// openDisks chooses the real buckets when their DSNs are set, else the
// offline memory pair.
// ds:def id=opendisks-j4hsr23q
func openDisks(ctx context.Context) (Disks, error) {
	cdnDSN, vaultDSN := os.Getenv(envCDNDSN), os.Getenv(envVaultDSN)
	if cdnDSN == "" || vaultDSN == "" {
		return memoryDisks(baseURL, bytes.Repeat([]byte("change-me"), 4))
	}
	return bucketDisks(ctx, cdnDSN, vaultDSN, os.Getenv(envCDNBase))
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-public-cdn-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	disks, err := openDisks(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	app, err := New(ctx, dir, disks, logPurge)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", baseURL)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
