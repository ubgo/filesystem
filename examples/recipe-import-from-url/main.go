// Command recipe-import-from-url lets a user add a file by pasting a link:
// "import from URL", "set avatar from Gravatar", "attach from Dropbox
// link", a CMS pulling an image from another site. The server fetches the
// URL itself, which is exactly what an attacker wants it to do with
// http://169.254.169.254/ or http://localhost:6379/, so every connection
// goes through the SSRF guard of upload.Fetch, and the fetched bytes go
// through the same upload policy as any other upload.
//
//	go run ./recipe-import-from-url       (listens on localhost:8108)
//
// IMPORT_ALLOW_ADDRS=127.0.0.1 permits that address despite the guard, so
// the demo can import from its own /samples routes.
//
// Routes (the demo trusts the X-User header; a real app uses its session):
//
//	POST /imports            {"url": "https://…"} → the recorded file
//	GET  /samples/photo.jpg  a sample image, with a Content-Disposition name
//	GET  /samples/huge.jpg   announces 64 MiB, over the limit
//	GET  /samples/metadata   redirects to the cloud metadata address
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

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
	listenAddr = "localhost:8108"
	// envAllowAddrs lists, comma-separated, internal addresses imports
	// may reach despite the guard.
	envAllowAddrs = "IMPORT_ALLOW_ADDRS"
	// diskName names the one disk in the manager.
	diskName = "media"
	// maxImportBytes caps one imported file.
	maxImportBytes = 10 << 20
	// fetchTimeout bounds a whole import, body included, so a slow server
	// cannot hold a request open.
	fetchTimeout = 30 * time.Second
	// maxRedirects is how many redirects an import follows; each one is
	// checked by the guard like the first connection.
	maxRedirects = 3
	// maxJSONBytes caps the request body.
	maxJSONBytes = 4 << 10
	// ownerUser and roleImports are where imported files are attached.
	ownerUser   = "user"
	roleImports = "imports"
	// headerUser carries the demo's user; a real app reads its session.
	headerUser = "X-User"
	// metadataURL is the cloud metadata service, the classic SSRF target.
	metadataURL = "http://169.254.169.254/latest/meta-data/"
	// hugeLength is what /samples/huge.jpg announces.
	hugeLength = 64 << 20
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// importPolicy accepts images and PDFs. The type is detected from the
// fetched bytes; the remote server's Content-Type is never trusted.
// ds:def id=importpolicy-6yfetmpk
var importPolicy = upload.Policy{
	MaxSize: maxImportBytes,
	Allowed: []string{"image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf"},
	Key:     upload.RandomKey("imports"),
}

// fetchOptions bounds every import. FetchOptions.MaxSize is set from the
// policy, so a response announcing a larger Content-Length is refused
// before its body is read; allow is nil unless the operator listed
// internal addresses.
// ds:def id=fetchoptions-f7b2zwau
func fetchOptions(allow func(netip.Addr) bool) upload.FetchOptions {
	return upload.FetchOptions{
		Timeout:      fetchTimeout,
		MaxRedirects: maxRedirects,
		MaxSize:      importPolicy.MaxSize,
		AllowAddr:    allow,
	}
}

// allowList parses IMPORT_ALLOW_ADDRS into an AllowAddr function. Listing
// exact addresses, not "any private address", keeps the guard closed for
// everything else on the network.
// ds:def id=allowlist-qudb84q4
func allowList(raw string) (func(netip.Addr) bool, error) {
	if raw == "" {
		return nil, nil
	}
	allowed := map[netip.Addr]bool{}
	for _, s := range strings.Split(raw, ",") {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envAllowAddrs, err)
		}
		allowed[a] = true
	}
	return func(a netip.Addr) bool { return allowed[a] }, nil
}

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	Fetch   upload.FetchOptions
	Handler http.Handler
}

// New wires the app over a data directory with the given fetch options.
func New(ctx context.Context, dataDir string, fetch upload.FetchOptions) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, err
	}
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
	app := &App{Store: store, Fetch: fetch}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /imports", app.importURL)
	mux.HandleFunc("GET /samples/photo.jpg", samplePhoto)
	mux.HandleFunc("GET /samples/huge.jpg", hugeFile)
	mux.HandleFunc("GET /samples/metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, metadataURL, http.StatusFound)
	})
	app.Handler = mux
	return app, nil
}

// importFrom fetches rawURL through the guard and stores and records it
// in one transaction. Store.Upload streams the body under the policy (type
// from the bytes, size counted) and deletes the object if the transaction
// rolls back. The file name comes from the response's Content-Disposition,
// else the URL's last path segment, made safe.
// ds:def id=app-importfrom-a59m22gc
func (a *App) importFrom(ctx context.Context, user, rawURL string) (records.File, error) {
	remote, err := upload.Fetch(ctx, rawURL, a.Fetch)
	if err != nil {
		return records.File{}, err
	}
	defer func() { _ = remote.Body.Close() }()
	var f records.File
	err = a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if f, err = a.Store.Upload(ctx, "", diskName, remote.Body, remote.Filename, importPolicy); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleImports)
		return err
	})
	return f, err
}

// importRequest is the body of POST /imports.
type importRequest struct {
	URL string `json:"url"`
}

// fileResponse describes an imported file.
type fileResponse struct {
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// importURL handles POST /imports.
// ds:def id=app-importurl-6t52qvb4
func (a *App) importURL(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes)).Decode(&req); err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body"))
		return
	}
	f, err := a.importFrom(r.Context(), user, req.URL)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fileResponse{FileID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size})
}

// samplePhoto serves a small JPEG named by Content-Disposition, standing in
// for an image on another site.
func samplePhoto(w http.ResponseWriter, _ *http.Request) {
	img := image.NewNRGBA(image.Rect(0, 0, 320, 240))
	for y := range 240 {
		for x := range 320 {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 150, 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, nil) // encoding into a buffer cannot fail
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="sunset.jpg"`)
	_, _ = w.Write(buf.Bytes())
}

// hugeFile announces a body over the import limit; the import refuses it
// from the header without reading the body.
func hugeFile(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(hugeLength))
	w.WriteHeader(http.StatusOK)
}

// writeError answers with the status for err's code and a JSON body.
// Codes about the client's input carry the message (which never includes
// a resolved internal address); anything else, the status text.
// ds:def id=writeerror-dwktbxnt
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
	allow, err := allowList(os.Getenv(envAllowAddrs))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "recipe-import-from-url-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, fetchOptions(allow))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on http://"+listenAddr, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
