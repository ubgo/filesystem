// Command recipe-cover-images is the cover or banner image of a profile,
// channel, group or event page: one wide image per owner, refused when it
// is too small to fill a desktop header, re-encoded without EXIF, cropped
// to one aspect ratio at up to three widths around its most detailed region, and
// served as a srcset so a phone downloads the 600-pixel version and a
// retina laptop the 1800-pixel one.
//
//	go run ./recipe-cover-images       (listens on localhost:8121)
//
// Routes:
//
//	PUT    /channels/{id}/cover   multipart form, field "file"
//	GET    /channels/{id}/cover   JSON: srcset and the URL of each width
//	DELETE /channels/{id}/cover   removes it for good
//	GET    /public/…              the public files
//
// Authentication is out of scope: a real app checks that the signed-in
// user may edit channel {id} before PUT and DELETE run.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
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
	listenAddr = "localhost:8121"
	// baseURL is the server's public origin; public file URLs start with it.
	baseURL = "http://" + listenAddr
	// publicPrefix is where fshttp.Public serves the disk's public objects.
	publicPrefix = "/public"
	// diskName names the one disk in the manager.
	diskName = "media"
	// ownerChannel is the only owner type covers attach to here.
	ownerChannel = "channel"
	// roleCover is a single role: at most one cover per channel.
	roleCover = "cover"
	// maxCoverBytes caps the uploaded image, before it is re-encoded.
	maxCoverBytes = 15 << 20
	// formOverhead is the multipart framing allowed on top of the file.
	formOverhead = 64 << 10
	// minCoverWidth and minCoverHeight are the smallest image accepted: it
	// fills a desktop header at 1x (1200 × 400) without being enlarged.
	// Larger uploads also give the 1800-pixel version for retina screens.
	minCoverWidth  = 1200
	minCoverHeight = 400
	// linkLifetime only matters for private files; covers are public, so
	// their URLs do not expire.
	linkLifetime = time.Hour
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// coverPolicy accepts the photo formats browsers export, re-encodes the
// image (dropping EXIF and GPS), and caps the bytes as sent through
// Sanitize's MaxBytes, because Policy.MaxSize applies to what Process
// returns. The dimension check is added by newPolicy.
// ds:def id=coverpolicy-6xds275s
var coverPolicy = upload.Policy{
	MaxSize:    maxCoverBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp"},
	Key:        upload.RandomKey("covers"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(imaging.Options{MaxBytes: maxCoverBytes}),
}

// newPolicy completes coverPolicy with the minimum size. CheckDimensions
// reads only the stored image's header and refuses one too small to fill
// the desktop header, which would otherwise be upscaled into a blur.
// ds:def id=newpolicy-n9mrwk4d
func newPolicy() (upload.Policy, error) {
	dims, err := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: minCoverWidth, MinHeight: minCoverHeight})
	if err != nil {
		return upload.Policy{}, err
	}
	p := coverPolicy
	p.Inspect = dims
	return p, nil
}

// widths are the cover's variants: one 3:1 aspect ratio, cropped with
// GravitySmart around the most detailed region so a banner whose subject
// is off-centre keeps it. "cover" is the 600-pixel base; WithDensities in
// newVariants derives the 1200 and 1800-pixel versions from it, and unlike
// a separately declared 1800-pixel ModeFill (which would enlarge a
// 1300-pixel upload into a blur) a density version never enlarges its
// source. One aspect ratio and growing widths is what SrcSet expects: the
// browser then chooses by width alone.
// ds:def id=widths-xpg9ktvc
var widths = map[string]imaging.Transform{
	"cover": {Mode: imaging.ModeFill, Width: 600, Height: 200, Gravity: imaging.GravitySmart, Format: imaging.FormatJPEG},
}

// widthNames lists the variants smallest first, the order a srcset is
// built in: the base and its 2x and 3x versions ("cover-2x", "cover-3x").
var widthNames = []string{"cover", variants.DensityName("cover", 2), variants.DensityName("cover", 3)}

// newVariants declares the base width and its densities, and generates
// all three right after an upload commits, so the first page view does
// not wait for a resize. WithDensities must come before WithEager, which
// only sees variants declared before it.
// ds:def id=newvariants-tv3ar5nt
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(widths, imaging.Options{},
		variants.WithDensities("cover", 2, 3),
		variants.WithEager(logFailure, widthNames...))
}

// App holds the wired dependencies.
type App struct {
	Store    *records.Store
	Variants *variants.Plugin
	Handler  http.Handler
	policy   upload.Policy
}

// New wires the app over a data directory; publicBase is the origin public
// URLs start with.
func New(ctx context.Context, dataDir, publicBase string) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithPublicURL(publicBase+publicPrefix))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	v, err := newVariants()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerChannel),
		records.WithSingleRoles(roleCover),
		records.WithPlugins(v))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	policy, err := newPolicy()
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, Variants: v, policy: policy}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /channels/{id}/cover", app.putCover)
	mux.HandleFunc("GET /channels/{id}/cover", app.getCover)
	mux.HandleFunc("DELETE /channels/{id}/cover", app.deleteCover)
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	app.Handler = mux
	return app, nil
}

// setCover stores the image and makes it the channel's cover in one
// transaction. PreviousPurge deletes the old cover and its variants once
// the transaction commits; a failure before the commit deletes the new
// object instead and leaves the old cover in place.
// ds:def id=app-setcover-gcgfdwya
func (a *App) setCover(ctx context.Context, channel string, body io.Reader, filename string) (records.File, error) {
	var stored records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if stored, err = a.Store.Upload(ctx, "", diskName, body, filename, a.policy); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, "", stored.ID, records.Owner{Type: ownerChannel, ID: channel}, roleCover, records.PreviousPurge)
		return err
	})
	return stored, err
}

// coverResponse is what a page needs to render the cover.
type coverResponse struct {
	FileID string            `json:"file_id"`
	SrcSet string            `json:"srcset"`
	URLs   map[string]string `json:"urls"`
}

// describe builds the srcset and the URL of each width. SrcSet uses the
// variants' real widths as w descriptors, smallest first.
// ds:def id=app-describe-8dymvrvv
func (a *App) describe(ctx context.Context, f records.File) (coverResponse, error) {
	srcset, candidates, err := a.Variants.SrcSet(ctx, a.Store, f.Scope, f.ID, linkLifetime, widthNames...)
	if err != nil {
		return coverResponse{}, err
	}
	urls := make(map[string]string, len(candidates))
	for _, c := range candidates {
		urls[c.Name] = c.URL
	}
	return coverResponse{FileID: f.ID, SrcSet: srcset, URLs: urls}, nil
}

// putCover handles the upload: the body is capped before the form is read.
// ds:def id=app-putcover-tdryamu3
func (a *App) putCover(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCoverBytes+formOverhead)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the image in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.setCover(r.Context(), r.PathValue("id"), file, header.Filename)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := a.describe(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// current returns the channel's cover file, or CodeNotFound.
func (a *App) current(ctx context.Context, channel string) (records.File, error) {
	list, err := a.Store.Attachments(ctx, "", records.Owner{Type: ownerChannel, ID: channel}, roleCover)
	if err != nil {
		return records.File{}, err
	}
	if len(list) == 0 {
		return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "channel %s has no cover", channel)
	}
	return list[0].File, nil
}

// getCover answers the srcset of the channel's cover.
func (a *App) getCover(w http.ResponseWriter, r *http.Request) {
	f, err := a.current(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := a.describe(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// deleteCover purges the cover, its row, its attachment and its variants.
func (a *App) deleteCover(w http.ResponseWriter, r *http.Request) {
	f, err := a.current(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.Store.Purge(r.Context(), "", f.ID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeError answers with the status for err's code and a JSON body that
// never carries a disk name or key.
func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, fshttp.StatusOf(err), upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
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
	dir, err := os.MkdirTemp("", "recipe-cover-images-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", baseURL, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
