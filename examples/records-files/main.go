// Command records-files shows the file API of the records Store on SQLite
// and an in-memory disk: wiring a Store, recording an object that is
// already stored (CreateFile), uploading and recording in one call
// (Upload), reading one file back, listing a scope with keyset
// pagination and filters, and handing out links (URL, DownloadURL).
//
//	go run ./records-files
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// tenant is the scope every file of the demo belongs to: in a real app
	// the signed-in user's workspace or account.
	tenant = "acme"
	// signBase is where a real app mounts fshttp.Signed; signed URLs point
	// under it.
	signBase = "https://app.example.com/files"
	// cdnBase is the public URL prefix of public files.
	cdnBase = "https://cdn.example.com"
	// maxUpload caps one upload.
	maxUpload = 5 << 20
	// linkTTL is how long a signed link works.
	linkTTL = 10 * time.Minute
	// pageSize is the listing page size; tiny so pagination shows.
	pageSize = 1
	// bigFile is the size threshold of the filtered listing.
	bigFile = 10
)

// signingSecret signs URLs; a real app loads it from its secret store.
var signingSecret = []byte("0123456789abcdef0123456789abcdef")

// newDisks builds the manager the Store reaches bytes through: one memory
// disk that signs its own URLs and builds public URLs under a CDN prefix.
// ds:def id=newdisks-ke5p8pyb
func newDisks() (*filesystem.Manager, error) {
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(),
		filesystem.WithURLSigner(signer), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(disk)
}

// openSQLite opens a SQLite database file the way the records tests do:
// one connection (SQLite has one writer) and a busy timeout.
// ds:def id=opensqlite-cjy2vqp7
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newStore wires a Store over db and disks and creates its tables.
// Migrate is safe to call on every start: it applies only what is new.
// ds:def id=newstore-c3s67q5f
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// recordExisting writes an object straight to the disk, then records it:
// the path for bytes that arrived some other way (a worker's output, an
// import, a presigned upload). CreateFile checks the object exists and
// fills size, type and digest from the disk.
// ds:def id=recordexisting-x7uxukhd
func recordExisting(ctx context.Context, s *records.Store, key string, body []byte) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, bytes.NewReader(body), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{
		Scope: tenant, Disk: diskName, Key: key, Filename: "Quarterly report.txt",
		Metadata: map[string]string{"source": "export-job"},
	})
}

// notesPolicy accepts plain text only, with a random key under notes/.
// ds:def id=notespolicy-4kaczpxx
var notesPolicy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"text/plain"},
	Key:     upload.RandomKey("notes"),
}

// uploadNote stores body under notesPolicy and records it, in one call.
// The actor in the context becomes the file's UploadedBy.
// ds:def id=uploadnote-z5zuwjmw
func uploadNote(ctx context.Context, s *records.Store, filename string, body io.Reader) (records.File, error) {
	ctx = records.WithActor(ctx, "user:alice")
	return s.Upload(ctx, tenant, diskName, body, filename, notesPolicy)
}

// listAll walks every page of the tenant's live files, newest first,
// following NextCursor until it is empty.
// ds:def id=listall-hce32ebc
func listAll(ctx context.Context, s *records.Store) ([]records.File, int, error) {
	var all []records.File
	pages := 0
	q := records.FileQuery{Scope: tenant, Limit: pageSize}
	for {
		page, err := s.Files(ctx, q)
		if err != nil {
			return nil, pages, err
		}
		pages++
		all = append(all, page.Files...)
		if page.NextCursor == "" {
			return all, pages, nil
		}
		q.Cursor = page.NextCursor
	}
}

// listBig lists only files larger than bigFile bytes. Filters are extra
// conditions on the fs_files table; order and pagination stay the Store's.
// ds:def id=listbig-x8dnvzmv
func listBig(ctx context.Context, s *records.Store) (records.FilePage, error) {
	return s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{sqlb.Greater(records.FileSize, int64(bigFile))},
	})
}

// links returns the two links a file can have: URL (public URL for a
// public file, otherwise a signed GET) and DownloadURL (always signed,
// with a Content-Disposition that saves it under its name).
// ds:def id=links-gn8gsk2p
func links(ctx context.Context, s *records.Store, f records.File) (view, download string, err error) {
	if view, err = s.URL(ctx, f, linkTTL); err != nil {
		return "", "", err
	}
	download, err = s.DownloadURL(ctx, f, linkTTL)
	return view, download, err
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	s, err := newStore(ctx, db, disks)
	if err != nil {
		return err
	}
	if err := demo(ctx, w, s); err != nil {
		return err
	}
	full, err := newFullStore(ctx, db, disks)
	if err != nil {
		return err
	}
	if err := extras(ctx, w, full); err != nil {
		return err
	}
	return sessionAndSort(ctx, w, db, disks)
}

// demo is run's body once the Store exists.
func demo(ctx context.Context, w io.Writer, s *records.Store) error {
	report, err := recordExisting(ctx, s, "exports/q3.txt", []byte("revenue,costs\n10,4\n"))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "created %s %q %s %d bytes %s\n", report.ID[:4], report.Filename, report.ContentType, report.Size, report.Visibility)
	note, err := uploadNote(ctx, s, "todo.txt", strings.NewReader("buy milk"))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "uploaded %q key=%s… by %s\n", note.Filename, note.Key[:len("notes/")], note.UploadedBy)
	if _, err := uploadNote(ctx, s, "evil.txt", strings.NewReader("\x89PNG\r\n\x1a\n")); err != nil {
		fmt.Fprintf(w, "refused: %s\n", filesystem.CodeOf(err))
	}
	all, pages, err := listAll(ctx, s)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "listed %d files in %d pages\n", len(all), pages)
	big, err := listBig(ctx, s)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "files over %d bytes: %d (%q)\n", bigFile, len(big.Files), big.Files[0].Filename)
	view, download, err := links(ctx, s, report)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "view: %s\ndownload: %s\n", view, download)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-files")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
