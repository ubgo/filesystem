// Command recipe-private-downloads serves private documents (invoices,
// contracts, payslips) to the one customer allowed to see them: the app
// checks ownership, then redirects to a short-lived signed URL, which
// fshttp.Signed serves with ranges, ETags and conditional requests. The
// signing keys rotate without breaking links already handed out.
//
//	go run ./recipe-private-downloads       (listens on localhost:8112)
//
// Routes (the signed-in customer is the X-Customer header, demo only):
//
//	GET /invoices                   the customer's documents
//	GET /invoices/{id}              302 to a signed URL that displays it
//	GET /invoices/{id}/download     302 to a signed URL that saves it
//	GET /files/…                    signed URLs, served by fshttp
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8112"
	// baseURL is the server's public origin; signed URLs start with it.
	baseURL = "http://" + listenAddr
	// filesPrefix is where fshttp.Signed is mounted, and so the signer's base.
	filesPrefix = "/files"
	// diskName names the private disk.
	diskName = "documents"
	// ownerCustomer is the owner type documents attach to.
	ownerCustomer = "customer"
	// roleInvoices is the role a customer's documents are attached under.
	roleInvoices = "invoices"
	// headerCustomer carries the signed-in customer. Demo only: a real app
	// takes it from its session.
	headerCustomer = "X-Customer"
	// linkLifetime is how long a signed link works. Short, because the link
	// is the authorization: whoever holds it can download until it expires.
	linkLifetime = 2 * time.Minute
	// maxDocumentBytes caps an uploaded document.
	maxDocumentBytes = 20 << 20
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// Signing key IDs and the environment variables their secrets come from.
// The newest key signs; the previous one only verifies, so links issued
// before a rotation keep working until they expire.
const (
	keyIDCurrent   = "k2"
	keyIDPrevious  = "k1"
	envKeyCurrent  = "SIGNING_KEY_CURRENT"
	envKeyPrevious = "SIGNING_KEY_PREVIOUS"
)

// demoSecret stands in for a missing environment variable, so the demo runs
// without setup. Never ship a secret in code.
var demoSecret = bytes.Repeat([]byte("demo-only-secret"), 2)

// documentPolicy accepts the documents staff upload: PDFs only, private.
// ds:def id=documentpolicy-4t8shmq3
var documentPolicy = upload.Policy{
	MaxSize:    maxDocumentBytes,
	Allowed:    []string{"application/pdf"},
	Key:        upload.RandomKey("invoices"),
	Visibility: filesystem.VisibilityPrivate,
}

// signingKeys builds the key ring from the environment: the current key
// first (it signs), then the previous one when set (it still verifies).
// ds:def id=signingkeys-age44m9x
func signingKeys(lookup func(string) (string, bool)) []signedurl.Key {
	current, ok := lookup(envKeyCurrent)
	if !ok {
		current = string(demoSecret)
	}
	keys := []signedurl.Key{{ID: keyIDCurrent, Secret: []byte(current)}}
	if previous, ok := lookup(envKeyPrevious); ok {
		keys = append(keys, signedurl.Key{ID: keyIDPrevious, Secret: []byte(previous)})
	}
	return keys
}

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	Disk    *filesystem.Disk
	Handler http.Handler
}

// openDisk opens a private local disk whose SignedURL returns links to
// fshttp.Signed. On S3, GCS or Azure the driver signs its own URLs and
// neither the signer nor the handler is needed.
// ds:def id=opendisk-v6dguss8
func openDisk(dataDir, base string, keys []signedurl.Key) (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(base+filesPrefix, keys)
	if err != nil {
		return nil, nil, err
	}
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer), filesystem.WithDefaultVisibility(filesystem.VisibilityPrivate))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, signer, err
}

// New wires the app over a data directory, the server's public origin and
// the signing key ring.
func New(ctx context.Context, dataDir, base string, keys []signedurl.Key) (*App, error) {
	disks, signer, err := openDisk(dataDir, base, keys)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerCustomer))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	disk, err := disks.Disk(diskName)
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, Disk: disk}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /invoices", app.list)
	mux.HandleFunc("GET /invoices/{id}", app.view)
	mux.HandleFunc("GET /invoices/{id}/download", app.download)
	mux.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(disks, signer)))
	app.Handler = mux
	return app, nil
}

// AddDocument stores a document through the policy and attaches it to a
// customer, in one transaction. Staff tooling or a billing job calls it.
// ds:def id=app-adddocument-52earrv5
func (a *App) AddDocument(ctx context.Context, customer string, body []byte, filename string) (records.File, error) {
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.Upload(ctx, "", diskName, bytes.NewReader(body), filename, documentPolicy); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
		return err
	})
	return f, err
}

// ImportLegacy records a document migrated from an older system, which
// never went through an upload policy and may be anything, HTML included.
// It writes the object first and records it second (the object-first rule).
// ds:def id=app-importlegacy-nayyqcgg
func (a *App) ImportLegacy(ctx context.Context, customer, key string, body []byte, filename string) (records.File, error) {
	if _, err := a.Disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.CreateFile(ctx, records.NewFile{Disk: diskName, Key: key, Filename: filename}); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
		return err
	})
	return f, err
}

// customerOf returns the signed-in customer, or CodePermission.
// ds:def id=customerof-ksugmynf
func customerOf(r *http.Request) (string, error) {
	if c := r.Header.Get(headerCustomer); c != "" {
		return c, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "not signed in")
}

// owned returns the file when it is attached to customer, and CodeNotFound
// otherwise: another customer's document and a missing one look the same,
// so IDs cannot be probed.
// ds:def id=app-owned-89sypjub
func (a *App) owned(ctx context.Context, customer, fileID string) (records.File, error) {
	list, err := a.Store.Attachments(ctx, "", records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
	if err != nil {
		return records.File{}, err
	}
	for _, af := range list {
		if af.File.ID == fileID {
			return af.File, nil
		}
	}
	return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "no document %s", fileID)
}

// document is one entry of the list answer.
type document struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// list answers with the signed-in customer's documents. It hands out no
// links: each link is issued when it is used, so none sits in a page for
// longer than its lifetime.
// ds:def id=app-list-3auchpx3
func (a *App) list(w http.ResponseWriter, r *http.Request) {
	customer, err := customerOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	attached, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]document, 0, len(attached))
	for _, af := range attached {
		out = append(out, document{ID: af.File.ID, Filename: af.File.Filename, ContentType: af.File.ContentType, Size: af.File.Size})
	}
	writeJSON(w, http.StatusOK, out)
}

// redirect checks ownership and sends the browser to a fresh signed link,
// made by sign. The redirect itself must not be cached: it carries a link
// that stops working in minutes.
// ds:def id=app-redirect-sjw5ktcm
func (a *App) redirect(w http.ResponseWriter, r *http.Request, sign func(context.Context, records.File) (string, error)) {
	customer, err := customerOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := a.owned(r.Context(), customer, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := sign(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, link, http.StatusFound)
}

// view links to the document for display in the browser (a PDF viewer).
// ds:def id=app-view-38xbwttz
func (a *App) view(w http.ResponseWriter, r *http.Request) {
	a.redirect(w, r, func(ctx context.Context, f records.File) (string, error) {
		return a.Store.URL(ctx, f, linkLifetime)
	})
}

// download links to the document with a Content-Disposition that saves it
// under its original name, non-ASCII characters included.
// ds:def id=app-download-nrwju9uf
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	a.redirect(w, r, func(ctx context.Context, f records.File) (string, error) {
		return a.Store.DownloadURL(ctx, f, linkLifetime)
	})
}

// errorResponse is the JSON body of a failed request.
type errorResponse struct {
	Error string          `json:"error"`
	Code  filesystem.Code `json:"code"`
}

// writeError answers with the status for err's code. Server-side failures
// say only their status text, so internals do not leak.
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, errorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

// samplePDF is a tiny document for the demo; its first bytes make it a PDF.
const samplePDF = "%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n"

// legacyHTML is an imported document that would run script if a browser
// rendered it on this origin.
const legacyHTML = "<html><body><script>alert(document.cookie)</script>Terms</body></html>"

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-private-downloads-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, baseURL, signingKeys(os.LookupEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f, err := app.AddDocument(ctx, "c1", []byte(samplePDF+string(bytes.Repeat([]byte("%\n"), 200))), "Rechnung März 2026.pdf")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	legacy, err := app.ImportLegacy(ctx, "c1", "legacy/terms.html", []byte(legacyHTML), "terms.html")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", baseURL, "data in", dir)
	fmt.Println("customer c1 has documents", f.ID, "and", legacy.ID)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
