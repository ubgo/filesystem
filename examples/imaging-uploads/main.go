// Command imaging-uploads shows the image tools an upload path combines:
// imaging.Optimize shrinks photos losslessly first (metadata out,
// recompression only above a quality floor), imaging.CheckDimensions
// refuses images that are too small, the dimensions plugin stores a
// ThumbHash placeholder, and the variants plugin builds a srcset.
//
//	go run ./imaging-uploads
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName  = "media"
	tenant    = "acme"
	cdnBase   = "https://cdn.example.com"
	maxUpload = 20 << 20
	// minSide is the smallest photo the app accepts on either side.
	minSide = 200
	// linkLifetime bounds signed links; the demo's files are public, so
	// its links never expire.
	linkLifetime = time.Hour
)

// widths is a srcset family: one aspect ratio, growing widths.
var widths = map[string]imaging.Transform{
	"w320":  {Mode: imaging.ModeFit, Width: 320, Height: 4000, Format: imaging.FormatJPEG},
	"w640":  {Mode: imaging.ModeFit, Width: 640, Height: 4000, Format: imaging.FormatJPEG},
	"w1280": {Mode: imaging.ModeFit, Width: 1280, Height: 4000, Format: imaging.FormatJPEG},
}

// photoPolicy optimises every photo and refuses ones under minSide.
// ds:def id=photopolicy-psgqc2r8
func photoPolicy() (upload.Policy, error) {
	check, err := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: minSide, MinHeight: minSide, MaxPixels: 40_000_000})
	if err != nil {
		return upload.Policy{}, err
	}
	return upload.Policy{
		MaxSize:    maxUpload,
		Allowed:    []string{"image/jpeg", "image/png"},
		Key:        upload.RandomKey("photos"),
		Visibility: filesystem.VisibilityPublic,
		Process:    imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{Quality: 80}),
		Inspect:    check,
	}, nil
}

// newStore installs the dimensions plugin with ThumbHash and the srcset
// variants on a SQLite database.
// ds:def id=newstore-em5m7df6
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *variants.Plugin, error) {
	v, err := variants.New(widths, imaging.Options{})
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(
		dimensions.New(dimensions.WithThumbHash(imaging.Options{})), v))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, v, nil
}

// uploadPhoto stores a photo and prints what the page needs: its size,
// its ThumbHash and its srcset.
// ds:def id=uploadphoto-6tjxscru
func uploadPhoto(ctx context.Context, w io.Writer, s *records.Store, v *variants.Plugin, p upload.Policy, name string, body []byte) error {
	f, err := s.Upload(ctx, tenant, diskName, bytes.NewReader(body), name, p)
	if err != nil {
		fmt.Fprintf(w, "%s refused: %s: %v\n", name, filesystem.CodeOf(err), err)
		return nil
	}
	width, height, _ := dimensions.Of(f)
	hash, _ := dimensions.ThumbHashOf(f)
	set, _, err := v.SrcSet(ctx, s, tenant, f.ID, linkLifetime, "w320", "w640", "w1280")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: %d bytes uploaded, %d stored, %dx%d\n", name, len(body), f.Size, width, height)
	fmt.Fprintf(w, "thumbhash: %s\n", base64.StdEncoding.EncodeToString(hash))
	fmt.Fprintf(w, "srcset: %s\n", set)
	return nil
}

// cameraJPEG is a noisy photo saved at quality 95 with an EXIF block, as
// a phone would send it.
func cameraJPEG(w, h int) ([]byte, error) {
	rng := rand.New(rand.NewPCG(7, 9))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(64)) + uint8(i%4*40)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		return nil, err
	}
	exif := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x00GPS 51.5N 0.12W")
	out := append([]byte{}, buf.Bytes()[:2]...)
	out = append(out, 0xFF, 0xE1, byte((len(exif)+2)>>8), byte(len(exif)+2))
	out = append(out, exif...)
	return append(out, buf.Bytes()[2:]...), nil
}

func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	s, v, err := newStore(ctx, db, disks)
	if err != nil {
		return err
	}
	p, err := photoPolicy()
	if err != nil {
		return err
	}
	big, err := cameraJPEG(900, 600)
	if err != nil {
		return err
	}
	if err := uploadPhoto(ctx, w, s, v, p, "beach.jpg", big); err != nil {
		return err
	}
	small, err := cameraJPEG(120, 120)
	if err != nil {
		return err
	}
	if err := uploadPhoto(ctx, w, s, v, p, "icon.jpg", small); err != nil {
		return err
	}
	return exifAuthorship(ctx, w)
}

func main() {
	dir, err := os.MkdirTemp("", "imaging-uploads")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
