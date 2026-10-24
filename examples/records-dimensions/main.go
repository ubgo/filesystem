// Command records-dimensions shows the dimensions plugin: the width and
// height of every raster image are stored in its file's metadata when it
// is recorded and again when its content is replaced, read from the
// header only. Other files are left alone; a corrupt image is recorded
// without dimensions, or refused with WithStrict.
//
//	go run ./records-dimensions
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// tenant is the scope of every file of the demo.
	tenant = "acme"
	// maxUpload caps one upload.
	maxUpload = 10 << 20
)

// pngSignature is the 8-byte PNG magic number; followed by garbage it
// makes a file that is detected as PNG but cannot be read.
const pngSignature = "\x89PNG\r\n\x1a\n"

// policy accepts images and plain text.
var policy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"image/*", "text/plain"},
	Key:     upload.RandomKey("files"),
}

// newDisks builds the manager: one memory disk.
func newDisks() (*filesystem.Manager, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(disk)
}

// openSQLite opens a SQLite database file: one connection (SQLite has one
// writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newStore installs the dimensions plugin. It has no table: dimensions
// live in the file's metadata column.
// ds:def id=newstore-mk9e9dyp
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, opts ...dimensions.Option) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(dimensions.New(opts...)))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// makePNG encodes a blank w×h PNG.
func makePNG(w, h int) ([]byte, error) {
	var buf bytes.Buffer
	err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)))
	return buf.Bytes(), err
}

// uploadAndMeasure uploads body and returns the file and the dimensions
// the plugin recorded, read back with dimensions.Of.
// ds:def id=uploadandmeasure-2szbd73c
func uploadAndMeasure(ctx context.Context, s *records.Store, name string, body []byte) (records.File, string, error) {
	f, err := s.Upload(ctx, tenant, diskName, bytes.NewReader(body), name, policy)
	if err != nil {
		return records.File{}, "", err
	}
	w, h, ok := dimensions.Of(f)
	if !ok {
		return f, "no dimensions", nil
	}
	return f, fmt.Sprintf("%dx%d", w, h), nil
}

// replaceAndMeasure replaces a file's content and returns its new
// dimensions: the old ones never outlive the content they described.
// ds:def id=replaceandmeasure-zhdjsmdx
func replaceAndMeasure(ctx context.Context, s *records.Store, id string, body []byte) (string, error) {
	f, err := s.Replace(ctx, tenant, id, bytes.NewReader(body), "", policy)
	if err != nil {
		return "", err
	}
	w, h, ok := dimensions.Of(f)
	if !ok {
		return "no dimensions", nil
	}
	return fmt.Sprintf("%dx%d", w, h), nil
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
	lenient, err := newStore(ctx, db, disks)
	if err != nil {
		return err
	}
	strict, err := newStore(ctx, db, disks, dimensions.WithStrict())
	if err != nil {
		return err
	}
	return demo(ctx, w, lenient, strict)
}

// demo uploads an image, a text file and a corrupt image, replaces the
// image, and tries the corrupt one under WithStrict.
// ds:def id=demo-sx8w9dyt
func demo(ctx context.Context, w io.Writer, lenient, strict *records.Store) error {
	photo, err := makePNG(640, 480)
	if err != nil {
		return err
	}
	f, dims, err := uploadAndMeasure(ctx, lenient, "photo.png", photo)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "photo.png: %s, metadata %v\n", dims, f.Metadata)
	_, dims, err = uploadAndMeasure(ctx, lenient, "notes.txt", []byte("plain text"))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "notes.txt: %s\n", dims)
	corrupt := []byte(pngSignature + strings.Repeat("x", 64))
	_, dims, err = uploadAndMeasure(ctx, lenient, "broken.png", corrupt)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "broken.png: %s\n", dims)
	if _, _, err = uploadAndMeasure(ctx, strict, "broken.png", corrupt); err != nil {
		fmt.Fprintf(w, "broken.png with WithStrict: %s\n", filesystem.CodeOf(err))
	}
	wide, err := makePNG(1920, 1080)
	if err != nil {
		return err
	}
	dims, err = replaceAndMeasure(ctx, lenient, f.ID, wide)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "photo.png after replace: %s\n", dims)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-dimensions")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
