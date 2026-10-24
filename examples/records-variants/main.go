// Command records-variants shows the variants plugin: named image
// variants ("thumb", "card") declared by the application, generated on
// first request or eagerly after an upload, served through a redirect
// handler, dropped when the original is replaced or changes visibility,
// pruned after a transform changes, and deleted with their file.
//
//	go run ./records-variants
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk originals and variants are stored on.
	diskName = "media"
	// tenant is the scope of every file of the demo.
	tenant = "acme"
	// signBase is where a real app mounts fshttp.Signed.
	signBase = "https://app.example.com/files"
	// cdnBase is the public URL prefix of public files.
	cdnBase = "https://cdn.example.com"
	// linkTTL is how long a signed variant link works.
	linkTTL = 10 * time.Minute
	// maxUpload caps one upload.
	maxUpload = 10 << 20
	// nameThumb and nameCard are the variant names the app declares.
	nameThumb = "thumb"
	nameCard  = "card"
	// photoW and photoH are the size of the generated test photo.
	photoW, photoH = 1200, 800
)

// signingSecret signs URLs; a real app loads it from its secret store.
var signingSecret = []byte("0123456789abcdef0123456789abcdef")

// transforms are the only variants that exist: a square thumbnail cropped
// to fill, and a card image that fits inside 400×400 keeping the ratio.
// A client can never ask for any other size.
// ds:def id=transforms-n65mkxr9
var transforms = map[string]imaging.Transform{
	nameThumb: {Mode: imaging.ModeFill, Width: 128, Height: 128, Format: imaging.FormatJPEG},
	nameCard:  {Mode: imaging.ModeFit, Width: 400, Height: 400, Format: imaging.FormatJPEG, Quality: 80},
}

// photoPolicy accepts images only.
var photoPolicy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"image/*"},
	Key:     upload.RandomKey("photos"),
}

// newDisks builds one memory disk that signs URLs and builds public URLs.
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

// newPlugin declares the variants and makes "thumb" eager: it is generated
// right after an image is recorded, replaced or changes visibility, after
// the transaction commits. A failure goes to onEager and the variant is
// made on its first request instead.
// ds:def id=newplugin-k2uvchpd
func newPlugin(t map[string]imaging.Transform, onEager func(context.Context, records.File, string, error)) (*variants.Plugin, error) {
	return variants.New(t, imaging.Options{}, variants.WithEager(onEager, nameThumb))
}

// logEager reports a failed eager variant.
// ds:def id=logeager-dtsbqr6p
func logEager(_ context.Context, f records.File, name string, err error) {
	log.Printf("variant %s of %s: %v", name, f.ID, err)
}

// newStore installs the plugin and creates the tables, fs_variants
// included.
// ds:def id=newstore-5u24ynfu
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, p *variants.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// makePNG draws a w×h PNG, standing in for a user's photo.
func makePNG(w, h int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	return buf.Bytes(), err
}

// uploadPhoto stores and records a photo; the eager thumbnail is made
// once Upload's transaction has committed.
// ds:def id=uploadphoto-vphdmbu3
func uploadPhoto(ctx context.Context, s *records.Store, w, h int) (records.File, error) {
	data, err := makePNG(w, h)
	if err != nil {
		return records.File{}, err
	}
	return s.Upload(ctx, tenant, diskName, bytes.NewReader(data), "beach.png", photoPolicy)
}

// card returns the "card" variant, generating it on this first request.
// ds:def id=card-ky3dmcrb
func card(ctx context.Context, s *records.Store, p *variants.Plugin, fileID string) (variants.Variant, error) {
	return p.Get(ctx, s, tenant, fileID, nameCard)
}

// scopeOf returns the tenant a variant request may read. Demo only: a
// real app reads it from the session, never from the URL.
// ds:def id=scopeof-wrkqp89b
func scopeOf(*http.Request) (string, error) { return tenant, nil }

// newHandler serves GET /variants/{fileID}/{name} as a redirect to the
// variant, generating it first if needed.
// ds:def id=newhandler-7bqv9349
func newHandler(s *records.Store, p *variants.Plugin) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/variants/", http.StripPrefix("/variants/",
		variants.Handler(s, p, scopeOf, variants.HandlerOptions{LinkLifetime: linkTTL})))
	return mux
}

// fetch calls h for path and returns the status and the Location header.
func fetch(h http.Handler, path string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Header().Get("Location")
}

// countVariantObjects counts the objects under the variants directory.
func countVariantObjects(ctx context.Context, s *records.Store) (int, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, err := range disk.List(ctx, variants.KeyDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// replacePhoto gives the file new content; its variants show the old
// picture, so the plugin drops them and the eager thumb is made again.
// ds:def id=replacephoto-nnpjgrgs
func replacePhoto(ctx context.Context, s *records.Store, fileID string, w, h int) error {
	data, err := makePNG(w, h)
	if err != nil {
		return err
	}
	_, err = s.Replace(ctx, tenant, fileID, bytes.NewReader(data), "", photoPolicy)
	return err
}

// pruneChanged opens the same tables with a changed "card" transform and
// prunes: every variant made by the old transform is outdated.
// ds:def id=prunechanged-224hztec
func pruneChanged(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (int, error) {
	changed := map[string]imaging.Transform{
		nameThumb: transforms[nameThumb],
		nameCard:  {Mode: imaging.ModeFit, Width: 600, Height: 600, Format: imaging.FormatJPEG},
	}
	p, err := newPlugin(changed, logEager)
	if err != nil {
		return 0, err
	}
	s, err := newStore(ctx, db, disks, p)
	if err != nil {
		return 0, err
	}
	return p.Prune(ctx, s)
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
	p, err := newPlugin(transforms, logEager)
	if err != nil {
		return err
	}
	s, err := newStore(ctx, db, disks, p)
	if err != nil {
		return err
	}
	f, err := firstSteps(ctx, w, s, p)
	if err != nil {
		return err
	}
	if err := lastSteps(ctx, w, s, p, db, disks, f); err != nil {
		return err
	}
	if err := moreSteps(ctx, w, dir); err != nil {
		return err
	}
	if err := otherDisk(ctx, w, dir); err != nil {
		return err
	}
	return proxied(ctx, w, dir)
}

// firstSteps uploads, generates and serves variants.
func firstSteps(ctx context.Context, w io.Writer, s *records.Store, p *variants.Plugin) (records.File, error) {
	f, err := uploadPhoto(ctx, s, photoW, photoH)
	if err != nil {
		return records.File{}, err
	}
	n, err := countVariantObjects(ctx, s)
	if err != nil {
		return records.File{}, err
	}
	fmt.Fprintf(w, "uploaded %dx%d photo; variant objects after upload: %d (eager thumb)\n", photoW, photoH, n)
	v, err := card(ctx, s, p, f.ID)
	if err != nil {
		return records.File{}, err
	}
	fmt.Fprintf(w, "card: %dx%d %s %d bytes at %s\n", v.Width, v.Height, v.ContentType, v.Size, v.Key[:len(variants.KeyDir)+1]+"…")
	h := newHandler(s, p)
	code, loc := fetch(h, "/variants/"+f.ID+"/"+nameThumb)
	fmt.Fprintf(w, "GET thumb: %d to %s…\n", code, loc[:len(signBase)])
	code, _ = fetch(h, "/variants/"+f.ID+"/huge")
	fmt.Fprintf(w, "GET huge: %d\n", code)
	return f, nil
}

// lastSteps replaces, makes public, prunes and purges.
func lastSteps(ctx context.Context, w io.Writer, s *records.Store, p *variants.Plugin, db *sql.DB, disks *filesystem.Manager, f records.File) error {
	if err := replacePhoto(ctx, s, f.ID, photoH, photoW); err != nil {
		return err
	}
	if n, err := countVariantObjects(ctx, s); err == nil {
		fmt.Fprintf(w, "variant objects after replace: %d (old ones dropped, eager thumb remade)\n", n)
	}
	v, err := card(ctx, s, p, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "card after replace: %dx%d\n", v.Width, v.Height)
	if err := s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic); err != nil {
		return err
	}
	u, err := p.URL(ctx, s, tenant, f.ID, nameThumb, linkTTL)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "public thumb: %s…\n", u[:len(cdnBase)+len(variants.KeyDir)+2])
	if _, err := card(ctx, s, p, f.ID); err != nil {
		return err
	}
	pruned, err := pruneChanged(ctx, db, disks)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "pruned after the card transform changed: %d\n", pruned)
	if err := s.Purge(ctx, tenant, f.ID); err != nil {
		return err
	}
	n, err := countVariantObjects(ctx, s)
	fmt.Fprintf(w, "variant objects after purge: %d\n", n)
	return err
}

func main() {
	dir, err := os.MkdirTemp("", "records-variants")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
