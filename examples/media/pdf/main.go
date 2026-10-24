// Command pdf uploads a PDF the way an application would with
// contrib/pdf: the policy refuses it when pdfinfo cannot read it or it is
// longer than 500 pages (Inspect), and the records plugin stores its page
// count and first page size with a preview of the first page beside it.
// It prints what was recorded.
//
//	go run ./pdf report.pdf      (from examples/media; needs pdfinfo and pdftoppm)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	diskMedia = "media"
	scope     = "demo"
	// maxPages is the longest document the policy accepts.
	maxPages = 500
	// previewSide is the preview's longest side in pixels.
	previewSide = 600
)

// tool runs Poppler with a 600-pixel preview and otherwise its defaults:
// programs on PATH, 30 seconds per run, at most 200 MiB copied.
var tool = pdf.Tool{MaxSide: previewSide}

// policy accepts PDFs up to 50 MiB and at most maxPages pages.
// ds:def id=policy-c9vcfm4h
var policy = upload.Policy{
	MaxSize: 50 << 20,
	Allowed: []string{pdf.ContentType},
	Key:     upload.RandomKey("documents"),
	Inspect: tool.Inspect(pdf.Limits{MaxPages: maxPages}),
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: pdf <file>")
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := run(context.Background(), os.Stdout, f, os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

// run uploads body as filename and prints what was recorded.
func run(ctx context.Context, out io.Writer, body io.Reader, filename string) error {
	store, disk, err := newStore(ctx)
	if err != nil {
		return err
	}
	file, err := store.Upload(ctx, scope, diskMedia, body, filename, policy)
	if err != nil {
		return err
	}
	// The preview is made after the upload commits, so read the row again
	// to see what the plugin recorded.
	if file, err = store.File(ctx, scope, file.ID); err != nil {
		return err
	}
	return report(ctx, out, disk, file)
}

// newStore opens the records store with the pdf plugin installed.
// ds:def id=newstore-fd7vah94
func newStore(ctx context.Context) (*records.Store, *filesystem.Disk, error) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := pdf.NewPlugin(tool, pdf.WithPreview())
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, disk, nil
}

// report prints the file's recorded metadata and its preview.
func report(ctx context.Context, out io.Writer, disk *filesystem.Disk, f records.File) error {
	m, ok := pdf.Of(f)
	if !ok {
		return fmt.Errorf("no metadata recorded for %s", f.ID)
	}
	fmt.Fprintf(out, "stored    %s (%s, %d bytes)\n", f.Key, f.ContentType, f.Size)
	fmt.Fprintf(out, "pages     %d\n", m.Pages)
	fmt.Fprintf(out, "page 1    %gx%g pt\n", m.PageWidth, m.PageHeight)
	key, ok := pdf.PreviewKey(f)
	if !ok {
		return fmt.Errorf("no preview recorded for %s", f.ID)
	}
	preview, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "preview   %s (%s, %d bytes)\n", preview.Key, preview.ContentType, preview.Size)
	return nil
}
