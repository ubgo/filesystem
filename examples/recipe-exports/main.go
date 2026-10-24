// Command recipe-exports is the "download my data" feature: a user asks for
// an export, the server writes a ZIP (a CSV of their orders and a JSON
// profile) straight into storage without a temporary file, and answers with
// a short-lived signed link that saves the file under a readable name.
// One export per user per day: asking again the same day returns a fresh
// link to the same file. Old exports are swept by age.
//
//	go run ./recipe-exports       (listens on localhost:8117)
//
// Routes:
//
//	POST /users/{id}/exports   create today's export (or reuse it); JSON with the link
//	GET  /files/…              signed links, served by fshttp
//
// Authentication is out of scope: a real app checks that the signed-in
// user is {id} before the handler runs.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8117"
	// baseURL is the server's public origin; signed links start with it.
	baseURL = "http://" + listenAddr
	// filesPrefix is where fshttp.Signed serves signed links.
	filesPrefix = "/files"
	// diskName names the disk exports are written to.
	diskName = "media"
	// exportDir is the prefix every export lives under, so a sweep by age
	// can never touch anything else.
	exportDir = "exports"
	// maxExportBytes aborts an export that grows past this size; nothing
	// larger is stored.
	maxExportBytes = 512 << 20
	// linkLifetime is how long a download link works. Short, because a link
	// is a bearer credential: whoever has it can download the export.
	linkLifetime = 15 * time.Minute
	// retention is how long an export is kept before the sweep deletes it.
	retention = 7 * 24 * time.Hour
	// sweepEvery is how often the demo server sweeps.
	sweepEvery = time.Hour
	// dayLayout names each day's export.
	dayLayout = "2006-01-02"
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// userIDPattern is what a user ID may look like. The ID becomes part of a
// storage key, so it is checked before it is used, never trusted.
var userIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Order is one row of the user's data; a real app reads its own tables.
type Order struct {
	ID     string
	Placed time.Time
	Total  int64 // in cents
}

// Profile is the user's account data as exported.
type Profile struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// Source is where the export reads the user's data from.
// ds:def id=source-km4dyh5y
type Source interface {
	Profile(ctx context.Context, userID string) (Profile, error)
	Orders(ctx context.Context, userID string) ([]Order, error)
}

// App holds the wired dependencies.
type App struct {
	Disk    *filesystem.Disk
	Data    Source
	Now     func() time.Time
	Handler http.Handler
}

// New wires the app over a data directory; publicBase is the origin signed
// links start with.
func New(dataDir, publicBase string, secret []byte, data Source) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	signer, err := signedurl.New(publicBase+filesPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	app := &App{Disk: disk, Data: data, Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users/{id}/exports", app.createExport)
	mux.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(disks, signer)))
	app.Handler = mux
	return app, nil
}

// exportKey is the user's export for the day of now. Naming by day is what
// makes the export idempotent: the same request the same day names the
// same object.
// ds:def id=exportkey-nr5pxscq
func exportKey(userID string, now time.Time) string {
	return exportDir + "/" + userID + "/" + now.UTC().Format(dayLayout) + ".zip"
}

// writeOrders writes the orders as CSV into the archive.
// ds:def id=writeorders-sd6bsf75
func writeOrders(zw *zip.Writer, orders []Order) error {
	w, err := zw.Create("orders.csv")
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "placed_at", "total_cents"}); err != nil {
		return err
	}
	for _, o := range orders {
		if err := cw.Write([]string{o.ID, o.Placed.UTC().Format(time.RFC3339), strconv.FormatInt(o.Total, 10)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// buildArchive reads the user's data and writes the ZIP to w.
// ds:def id=app-buildarchive-y4guuwge
func (a *App) buildArchive(ctx context.Context, w io.Writer, userID string) error {
	profile, err := a.Data.Profile(ctx, userID)
	if err != nil {
		return err
	}
	orders, err := a.Data.Orders(ctx, userID)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	pw, err := zw.Create("profile.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(pw).Encode(profile); err != nil {
		return err
	}
	if err := writeOrders(zw, orders); err != nil {
		return err
	}
	return zw.Close()
}

// downloadName is the name the browser saves the export as.
func downloadName(key string) string {
	return "export-" + filepath.Base(key)
}

// store streams the archive into the disk through a pipe: the ZIP is
// written by one goroutine and read by Disk.Write as it is produced, so no
// temporary file and no in-memory copy of the whole export exists.
// IfNotExists makes a concurrent second request for the same day fail with
// CodeExists instead of overwriting; MaxSize aborts a runaway export.
// ds:def id=app-store-3qrnrf2f
func (a *App) store(ctx context.Context, key, userID string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(a.buildArchive(ctx, pw, userID)) }()
	opts := filesystem.WriteOptions{ContentType: "application/zip", IfNotExists: true, MaxSize: maxExportBytes}
	if a.Disk.Capabilities().Headers {
		opts.Headers.ContentDisposition = records.AttachmentDisposition(downloadName(key))
	}
	_, err := a.Disk.Write(ctx, key, pr, opts)
	// Unblocks the producer if Write stopped reading early (refused,
	// too large, or the object already existed).
	pr.CloseWithError(err)
	return err
}

// Export makes sure today's export of userID exists and returns its key
// and whether this call created it. An export that already exists today is
// reused, also when another request created it a moment ago.
// ds:def id=app-export-7xz2vvb7
func (a *App) Export(ctx context.Context, userID string) (string, bool, error) {
	if !userIDPattern.MatchString(userID) {
		return "", false, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid user id")
	}
	key := exportKey(userID, a.Now())
	// The cheap check skips rebuilding an export that exists; IfNotExists
	// in store settles the race between two requests that both missed it.
	if ok, err := a.Disk.Exists(ctx, key); err != nil || ok {
		return key, false, err
	}
	err := a.store(ctx, key, userID)
	switch filesystem.CodeOf(err) {
	case "":
		return key, true, nil
	case filesystem.CodeExists:
		return key, false, nil
	}
	return "", false, err
}

// Link returns a signed download link for key. The signed
// Content-Disposition makes the browser save the file under a readable
// name, whatever the backend stored.
// ds:def id=app-link-ucr5t82z
func (a *App) Link(ctx context.Context, key string) (filesystem.SignedRequest, error) {
	return a.Disk.SignedURL(ctx, key, filesystem.PresignOptions{
		Method:             filesystem.MethodGet,
		Expires:            linkLifetime,
		ContentType:        "application/zip",
		ContentDisposition: records.AttachmentDisposition(downloadName(key)),
	})
}

// exportResponse is the JSON answer of the export route.
type exportResponse struct {
	Key       string    `json:"key"`
	Created   bool      `json:"created"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// createExport is the route: export (or reuse today's), then link.
// ds:def id=app-createexport-cryb6828
func (a *App) createExport(w http.ResponseWriter, r *http.Request) {
	key, created, err := a.Export(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := a.Link(r.Context(), key)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, exportResponse{Key: key, Created: created, URL: link.URL, ExpiresAt: link.ExpiresAt})
}

// Sweep deletes exports older than retention and returns how many. The
// library starts no background work; the app schedules this.
// ds:def id=app-sweep-spgm4eu7
func (a *App) Sweep(ctx context.Context) (int, error) {
	return upload.Sweep(ctx, a.Disk, exportDir, a.Now().Add(-retention))
}

// sweepLoop runs Sweep every sweepEvery until ctx ends.
// ds:def id=app-sweeploop-3cpwhxhd
func (a *App) sweepLoop(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.Sweep(ctx); err != nil || n > 0 {
				fmt.Fprintf(os.Stderr, "export sweep: deleted %d, err %v\n", n, err)
			}
		}
	}
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

// demoData is a fixed Source for the demo: every user has the same orders.
type demoData struct{}

// Profile implements Source.
func (demoData) Profile(_ context.Context, userID string) (Profile, error) {
	return Profile{ID: userID, Email: userID + "@example.com"}, nil
}

// Orders implements Source.
func (demoData) Orders(context.Context, string) ([]Order, error) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	return []Order{{ID: "ord_1", Placed: at, Total: 1999}, {ID: "ord_2", Placed: at.Add(48 * time.Hour), Total: 4500}}, nil
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-exports-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(dir, baseURL, bytes.Repeat([]byte("change-me"), 4), demoData{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go app.sweepLoop(ctx)
	fmt.Println("listening on", baseURL, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
