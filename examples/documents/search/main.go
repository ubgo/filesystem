// Command search uploads documents the way an application that indexes
// them would: a PDF is described by contrib/pdf and its text stored
// beside it, a scanned PDF is read page by page with contrib/ocr, an
// office document is converted with contrib/office first, and an image's
// text is read by contrib/ocr's own plugin. It prints what was recorded
// and the start of each stored text, which is what a search indexer
// would read.
//
//	go run ./search report.pdf scan.pdf receipt.png deck.pptx
//	(from examples/documents; needs Poppler, Tesseract for scans and
//	images, LibreOffice for office files)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/contrib/office"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	diskMedia = "media"
	scope     = "demo"
	// excerpt is how much of each stored text is printed.
	excerpt = 60
)

// policy accepts PDFs, office documents and images up to 50 MiB.
var policy = upload.Policy{
	MaxSize: 50 << 20,
	Allowed: append([]string{pdf.ContentType, "image/png", "image/jpeg"}, office.DefaultContentTypes()...),
	Key:     upload.RandomKey("documents"),
}

// plugins are the document plugins: contrib/pdf describes PDFs and what
// LibreOffice converts to PDF, storing a preview and the text, and reads
// scans through Tesseract; contrib/ocr stores the text of images.
// ds:def id=plugins-vbctunwf
func plugins() []records.Plugin {
	reader := ocr.Tool{Languages: []string{"eng"}}
	docs := pdf.NewPlugin(pdf.Tool{MaxOCRPages: 10},
		pdf.WithPreview(),
		pdf.WithOCR(reader),
		pdf.WithConverter(office.Tool{}),
	)
	return []records.Plugin{docs, ocr.NewPlugin(reader)}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: search <file>...")
	}
	ctx := context.Background()
	store, disk, err := newStore(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, name := range os.Args[1:] {
		if err := upload1(ctx, os.Stdout, store, disk, name); err != nil {
			log.Fatal(err)
		}
	}
}

// upload1 uploads the file at path and reports it.
// ds:def id=upload1-k8w3c6nr
func upload1(ctx context.Context, out io.Writer, store *records.Store, disk *filesystem.Disk, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	file, err := store.Upload(ctx, scope, diskMedia, f, filepath.Base(path), policy)
	if err != nil {
		return err
	}
	// The preview, text and OCR are made once the upload commits; the row
	// as stored now has them, or a failed status saying why not.
	if file, err = store.File(ctx, scope, file.ID); err != nil {
		return err
	}
	return report(ctx, out, disk, file)
}

// newStore opens the records store with the document plugins.
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
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugins()...))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, disk, nil
}

// report prints what was recorded for f and the start of its text: the
// PDF plugin's metadata and preview, and the text object either plugin
// stored beside the file, which is what a search indexer reads.
// ds:def id=report-p6t2v9wm
func report(ctx context.Context, out io.Writer, disk *filesystem.Disk, f records.File) error {
	fmt.Fprintf(out, "%s (%s)\n", f.Filename, f.ContentType)
	if f.Status == records.StatusFailed {
		fmt.Fprintf(out, "  failed    %s\n", f.StatusDetail)
	}
	if m, ok := pdf.Of(f); ok {
		fmt.Fprintf(out, "  pages     %d\n", m.Pages)
		if m.Title != "" {
			fmt.Fprintf(out, "  title     %s\n", m.Title)
		}
	}
	if _, ok := pdf.PreviewKey(f); ok {
		fmt.Fprintln(out, "  preview   stored")
	}
	key, ok := pdf.TextKey(f)
	if !ok {
		key, ok = ocr.TextKey(f)
	}
	if !ok {
		fmt.Fprintln(out, "  text      none")
		return nil
	}
	text, err := disk.ReadAll(ctx, key)
	if err != nil {
		return err
	}
	how := "text layer"
	if ocred, _ := strconv.ParseBool(f.Metadata[pdf.MetaTextOCR]); ocred || strings.HasPrefix(key, f.Key+ocr.TextInfix) {
		how = "OCR"
	}
	line := strings.Join(strings.Fields(string(text)), " ")
	if len(line) > excerpt {
		line = line[:excerpt] + "..."
	}
	fmt.Fprintf(out, "  text      %d bytes (%s): %s\n", len(text), how, line)
	return nil
}
