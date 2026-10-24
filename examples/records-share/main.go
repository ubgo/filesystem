// Command records-share shows the share plugin of the records Store:
// creating a link to one file with a lifetime and a download limit,
// resolving it, listing and revoking links, the one not-found every
// unusable link answers with, and the HTTP handler that turns a link into
// a short-lived signed download.
//
//	go run ./records-share          (prints the Go walk-through)
//	go run ./records-share serve    (serves on localhost:8090 and prints a link)
//
// main_test.go runs the walk-through and the handler offline.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// tenant is the scope of every file of the demo.
	tenant = "acme"
	// addr is where "serve" listens.
	addr = "localhost:8090"
	// baseURL is the server's public origin; signed URLs point under it.
	baseURL = "http://" + addr
	// sharePrefix is where share links are mounted: baseURL + "/s/" + token.
	sharePrefix = "/s/"
	// filesPrefix is where fshttp.Signed serves the signed downloads the
	// share handler redirects to.
	filesPrefix = "/files"
	// linkTTL is how long a share link works.
	linkTTL = 7 * 24 * time.Hour
	// linkDownloads is how often a share link works.
	linkDownloads = 2
	// maxUpload caps one upload.
	maxUpload = 1 << 20
	// readTimeout bounds how long the demo server reads one request.
	readTimeout = 10 * time.Second
)

// signingSecret signs download URLs; a real app loads it from its secret
// store.
var signingSecret = []byte("0123456789abcdef0123456789abcdef")

// docPolicy accepts plain text and PDFs.
var docPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain", "application/pdf"}, Key: upload.RandomKey("docs")}

// newDisks builds one memory disk whose signed URLs point at the
// fshttp.Signed handler mounted under filesPrefix, and returns the signer
// that handler verifies with.
func newDisks() (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(baseURL+filesPrefix, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
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

// openSQLite opens a SQLite database file with one connection (SQLite
// has one writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newStore installs the share plugin and creates every table, fs_shares
// included.
// ds:def id=newstore-ndhmkyzh
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, opts ...share.Option) (*records.Store, *share.Plugin, error) {
	p := share.New(opts...)
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, p, nil
}

// uploadDoc stores and records one document of the tenant.
func uploadDoc(ctx context.Context, s *records.Store, name, body string) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader(body), name, docPolicy)
}

// shareFile makes a link that works linkDownloads times within linkTTL.
// The returned Share carries the token: show it to the user now, because
// the database keeps only its SHA-256 and cannot give it back.
// ds:def id=sharefile-s3tjks9n
func shareFile(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) (share.Share, error) {
	return p.Create(ctx, s, tenant, fileID, share.Options{TTL: linkTTL, MaxDownloads: linkDownloads})
}

// linkURL is the address a user sends to someone else.
func linkURL(token string) string { return baseURL + sharePrefix + token }

// open spends one download of the link and returns its file. Every
// unusable link is the same CodeNotFound.
// ds:def id=open-zpzbgxf2
func open(ctx context.Context, s *records.Store, p *share.Plugin, token string) (records.File, error) {
	return p.Resolve(ctx, s, token)
}

// linksOf lists a file's links for its owner's "shared links" panel:
// newest first, with counts and expiry, never with tokens.
// ds:def id=linksof-czvcpu4u
func linksOf(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) ([]share.Share, error) {
	return p.Shares(ctx, s, tenant, fileID)
}

// stopSharing revokes a link at once; revoking twice does nothing.
// ds:def id=stopsharing-y4m2w3yj
func stopSharing(ctx context.Context, s *records.Store, p *share.Plugin, shareID string) error {
	return p.Revoke(ctx, s, tenant, shareID)
}

// newMux mounts the share handler, and the signed-download handler its
// redirects point at, each with its prefix stripped.
// ds:def id=newmux-6ne3q9m6
func newMux(s *records.Store, p *share.Plugin, disks *filesystem.Manager, signer *signedurl.Signer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(sharePrefix, http.StripPrefix(sharePrefix, share.Handler(s, p)))
	mux.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(disks, signer)))
	return mux
}

// app is the wired demo.
type app struct {
	db     *sql.DB
	store  *records.Store
	share  *share.Plugin
	disks  *filesystem.Manager
	signer *signedurl.Signer
}

// newApp wires the demo over a data directory.
func newApp(ctx context.Context, dir string, opts ...share.Option) (*app, error) {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return nil, err
	}
	disks, signer, err := newDisks()
	if err != nil {
		return nil, err
	}
	s, p, err := newStore(ctx, db, disks, opts...)
	if err != nil {
		return nil, err
	}
	return &app{db: db, store: s, share: p, disks: disks, signer: signer}, nil
}

// run walks through the Go API and prints what each call returned.
func run(ctx context.Context, w io.Writer, dir string) error {
	a, err := newApp(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = a.db.Close() }()
	f, err := uploadDoc(ctx, a.store, "contract.txt", "signed by both parties")
	if err != nil {
		return err
	}
	sh, err := shareFile(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "link %s… token %d chars, expires in %s, %d downloads\n", sh.ID[:4], len(sh.Token), sh.ExpiresAt.Sub(sh.CreatedAt), sh.MaxDownloads)
	for i := 1; i <= linkDownloads+1; i++ {
		got, err := open(ctx, a.store, a.share, sh.Token)
		if err != nil {
			fmt.Fprintf(w, "download %d: %s (%v)\n", i, filesystem.CodeOf(err), err)
			continue
		}
		fmt.Fprintf(w, "download %d: %q\n", i, got.Filename)
	}
	if err := unusable(ctx, w, a, f); err != nil {
		return err
	}
	if err := collectionsDemo(ctx, w, a.db, a.disks); err != nil {
		return err
	}
	return policyDemo(ctx, w, a.db, a.disks)
}

// unusable shows that revoked, trashed and unknown links fail like an
// exhausted one, and that a purge deletes a file's links.
// ds:def id=unusable-2jbw3fmt
func unusable(ctx context.Context, w io.Writer, a *app, f records.File) error {
	sh, err := shareFile(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	if err := stopSharing(ctx, a.store, a.share, sh.ID); err != nil {
		return err
	}
	_, err = open(ctx, a.store, a.share, sh.Token)
	fmt.Fprintf(w, "revoked: %v\n", err)
	_, err = open(ctx, a.store, a.share, "no-such-token")
	fmt.Fprintf(w, "unknown: %v\n", err)
	live, err := shareFile(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	if err := a.store.Trash(ctx, tenant, f.ID); err != nil {
		return err
	}
	_, err = open(ctx, a.store, a.share, live.Token)
	fmt.Fprintf(w, "file trashed: %v\n", err)
	list, err := linksOf(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "links of the file: %d (revoked: %t, token listed: %q)\n", len(list), list[1].RevokedAt != nil, list[0].Token)
	if err := a.store.Purge(ctx, tenant, f.ID); err != nil {
		return err
	}
	list, err = linksOf(ctx, a.store, a.share, f.ID)
	fmt.Fprintf(w, "links after purge: %d\n", len(list))
	return err
}

// serveLink records a file, shares it, prints the link and serves until
// ctx is cancelled (Ctrl+C or SIGTERM), then shuts down cleanly.
func serveLink(ctx context.Context, dir string) error {
	a, err := newApp(ctx, dir)
	if err != nil {
		return err
	}
	f, err := uploadDoc(ctx, a.store, "contract.txt", "signed by both parties")
	if err != nil {
		return err
	}
	sh, err := shareFile(ctx, a.store, a.share, f.ID)
	if err != nil {
		return err
	}
	fmt.Println(linkURL(sh.Token))
	srv := &http.Server{Addr: addr, Handler: newMux(a.store, a.share, a.disks, a.signer), ReadHeaderTimeout: readTimeout}
	return serve.UntilDone(ctx, srv)
}

func main() {
	dir, err := os.MkdirTemp("", "records-share")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred clean-up
	// above runs.
	ctx, stop := serve.Context()
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		err = serveLink(ctx, dir)
	} else {
		err = run(ctx, os.Stdout, dir)
	}
	if err != nil {
		log.Fatal(err)
	}
}
