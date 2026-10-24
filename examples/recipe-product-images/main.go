// Command recipe-product-images is the image gallery of an e-commerce
// catalog: a merchant uploads several photos of a product, puts them in
// order (the first one is the cover), removes one, and the storefront shows
// a thumbnail grid, a product card and a zoomable large image, all from
// public URLs a CDN can cache.
//
//	go run ./recipe-product-images       (listens on localhost:8102)
//
// Routes:
//
//	POST   /api/owners/product/{id}/gallery         multipart, field "file", several files
//	GET    /api/owners/product/{id}/gallery         the images in order
//	PUT    /api/owners/product/{id}/gallery/order   {"ids": ["att_…", …]}
//	DELETE /products/{id}/images/{fileID}           remove an image for good
//	GET    /products/{id}                           the storefront view
//	GET    /variants/{fileID}/{name}                302 to a size, made on first request
//	GET    /public/…                                the public files
//
// Authentication is out of scope: the demo treats a request carrying
// X-Merchant: demo as the merchant and every other request as a shopper.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8102"
	// baseURL is the server's public origin; public file URLs start with it.
	baseURL = "http://" + listenAddr
	// publicPrefix is where fshttp.Public serves the disk's public objects.
	publicPrefix = "/public"
	// variantsPrefix is where the variants handler is mounted.
	variantsPrefix = "/variants/"
	// diskName names the one disk in the manager.
	diskName = "media"
	// ownerProduct is the only owner type files attach to here.
	ownerProduct = "product"
	// roleGallery holds a product's images, in order; position 1 is the cover.
	roleGallery = "gallery"
	// maxImageBytes caps each uploaded photo, before it is re-encoded.
	maxImageBytes = 15 << 20
	// maxImagesPerRequest caps the files of one upload request.
	maxImagesPerRequest = 10
	// headerMerchant and merchantToken stand in for real authentication.
	headerMerchant = "X-Merchant"
	merchantToken  = "demo"
	// linkLifetime is used only for files that are not public.
	linkLifetime = time.Hour
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// Variant names the storefront uses.
const (
	sizeThumb = "thumb"
	sizeCard  = "card"
	sizeZoom  = "zoom"
)

// imageLimits bound the photo Sanitize reads. Policy.MaxSize applies to what
// Sanitize returns, so the cap on the uploaded bytes lives here.
// ds:def id=imagelimits-6yvh999c
var imageLimits = imaging.Options{MaxBytes: maxImageBytes}

// galleryPolicy accepts photos, re-encodes them so camera metadata and GPS
// never reach the public bucket, and stores them public under products/.
// ds:def id=gallerypolicy-59s4f88q
var galleryPolicy = upload.Policy{
	MaxSize:    maxImageBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp"},
	Key:        upload.RandomKey("products"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(imageLimits),
}

// sizes are the only variants that can exist. thumb crops to a square for
// the grid, card pads to a square so a tall bottle is never cut off, zoom
// keeps the whole photo up to 1600 px.
// ds:def id=sizes-yj3z9sby
var sizes = map[string]imaging.Transform{
	sizeThumb: {Mode: imaging.ModeFill, Width: 200, Height: 200, Format: imaging.FormatJPEG},
	sizeCard:  {Mode: imaging.ModePad, Width: 600, Height: 600, Format: imaging.FormatJPEG},
	sizeZoom:  {Mode: imaging.ModeFit, Width: 1600, Height: 1600, Format: imaging.FormatJPEG, Quality: 90},
}

// App holds the wired dependencies.
type App struct {
	Store    *records.Store
	Variants *variants.Plugin
	Handler  http.Handler
}

// openDisk opens the local directory as the catalog disk. On S3 or R2 the
// public base is the CDN domain and fshttp.Public is not needed.
// ds:def id=opendisk-jgkb2fbf
func openDisk(dataDir, publicBase string) (*filesystem.Manager, *filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithPublicURL(publicBase+publicPrefix))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, disk, err
}

// newVariants declares the sizes. thumb and card are on every listing
// page, so they are made right after an upload commits; zoom is opened by
// few shoppers, so it is made on its first request.
// ds:def id=newvariants-22rj7var
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(sizes, imaging.Options{}, variants.WithEager(logFailure, sizeThumb, sizeCard))
}

// openStore opens SQLite and the records Store with the variants plugin.
// ds:def id=openstore-jhrmunv8
func openStore(ctx context.Context, dataDir string, disks *filesystem.Manager, v *variants.Plugin) (*records.Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "catalog.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerProduct),
		records.WithPlugins(v))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}

// catalogScope is the scope every request acts in: one shop. A
// marketplace returns the merchant's shop ID here.
func catalogScope(*http.Request) (string, error) { return "", nil }

// isMerchant stands in for the app's session check.
// ds:def id=ismerchant-uvt2f22h
func isMerchant(r *http.Request) bool { return r.Header.Get(headerMerchant) == merchantToken }

// authorize lets shoppers list and read a gallery, and only the merchant
// upload and reorder. Trash and detach are refused: a catalogue has no
// trash to restore from, and a trashed or detached image would only keep
// its bytes, so removal goes through removeImage, which purges. Every
// owner but a product gallery is refused.
// ds:def id=authorize-f9wmr8n3
func authorize(r *http.Request, action recordshttp.Action, t recordshttp.Target) error {
	if t.Owner.Type != "" && (t.Owner.Type != ownerProduct || t.Role != roleGallery) {
		return filesystem.Errorf(filesystem.CodePermission, "only product galleries")
	}
	switch action {
	case recordshttp.ActionList, recordshttp.ActionRead:
		return nil
	case recordshttp.ActionUpload, recordshttp.ActionReorder:
		if isMerchant(r) {
			return nil
		}
	}
	return filesystem.Errorf(filesystem.CodePermission, "not allowed")
}

// galleryAPI is the upload, list, reorder and trash endpoint set, served by
// recordshttp rather than written by hand.
// ds:def id=galleryapi-dhdu2xrj
func galleryAPI(store *records.Store) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:        store,
		Disk:         diskName,
		Policy:       galleryPolicy,
		MaxFiles:     maxImagesPerRequest,
		Scope:        catalogScope,
		Authorize:    authorize,
		LinkLifetime: linkLifetime,
	})
}

// New wires the app over a data directory; publicBase is the origin public
// URLs start with.
// ds:def id=new-y9s4sspz
func New(ctx context.Context, dataDir, publicBase string) (*App, error) {
	disks, disk, err := openDisk(dataDir, publicBase)
	if err != nil {
		return nil, err
	}
	v, err := newVariants()
	if err != nil {
		return nil, err
	}
	store, err := openStore(ctx, dataDir, disks, v)
	if err != nil {
		return nil, err
	}
	api, err := galleryAPI(store)
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, Variants: v}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", api))
	mux.HandleFunc("GET /products/{id}", app.product)
	mux.HandleFunc("DELETE /products/{id}/images/{fileID}", app.removeImage)
	mux.Handle(variantsPrefix, http.StripPrefix(variantsPrefix, variants.Handler(store, v, catalogScope, variants.HandlerOptions{})))
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	app.Handler = mux
	return app, nil
}

// productImage is one image of the storefront view.
type productImage struct {
	FileID string `json:"file_id"`
	Thumb  string `json:"thumb"`
	Card   string `json:"card"`
	Zoom   string `json:"zoom"`
}

// productView is the storefront's answer: the cover and every image in the
// merchant's order.
type productView struct {
	Product string         `json:"product"`
	Cover   *productImage  `json:"cover"`
	Images  []productImage `json:"images"`
}

// image builds one entry: the eager sizes as their public URLs (already
// made, so this is a lookup), zoom as a link to the variants handler, which
// makes it on the first click instead of on every page view.
// ds:def id=app-image-sgkz2taq
func (a *App) image(ctx context.Context, f records.File) (productImage, error) {
	thumb, err := a.Variants.URL(ctx, a.Store, f.Scope, f.ID, sizeThumb, linkLifetime)
	if err != nil {
		return productImage{}, err
	}
	card, err := a.Variants.URL(ctx, a.Store, f.Scope, f.ID, sizeCard, linkLifetime)
	if err != nil {
		return productImage{}, err
	}
	return productImage{FileID: f.ID, Thumb: thumb, Card: card, Zoom: variantsPrefix + f.ID + "/" + sizeZoom}, nil
}

// product answers the storefront view. The gallery's order is the
// attachments' positions, so the first image is the cover; trashed images
// are already left out by Attachments.
// ds:def id=app-product-pjf7w76c
func (a *App) product(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	list, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerProduct, ID: id}, roleGallery)
	if err != nil {
		writeError(w, err)
		return
	}
	view := productView{Product: id, Images: []productImage{}}
	for _, af := range list {
		img, err := a.image(r.Context(), af.File)
		if err != nil {
			writeError(w, err)
			return
		}
		view.Images = append(view.Images, img)
	}
	if len(view.Images) > 0 {
		view.Cover = &view.Images[0]
	}
	writeJSON(w, http.StatusOK, view)
}

// removeImage deletes one image of a product for good: Purge removes its
// attachment, its row and its variant rows in one transaction, then the
// objects, so the photo also leaves the public URL space. The file must be
// in this product's gallery, so a merchant cannot delete by guessing IDs.
// ds:def id=app-removeimage-m7ak2yp4
func (a *App) removeImage(w http.ResponseWriter, r *http.Request) {
	if !isMerchant(r) {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "merchants only"))
		return
	}
	product := records.Owner{Type: ownerProduct, ID: r.PathValue("id")}
	list, err := a.Store.Attachments(r.Context(), "", product, roleGallery)
	if err != nil {
		writeError(w, err)
		return
	}
	fileID := r.PathValue("fileID")
	if !slices.ContainsFunc(list, func(af records.AttachedFile) bool { return af.File.ID == fileID }) {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "product %s has no image %s", product.ID, fileID))
		return
	}
	if err := a.Store.Purge(r.Context(), "", fileID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-product-images-")
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
