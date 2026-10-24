// Command recipe-tenant-branding is the "your logo, your colours" settings
// page of a white-label SaaS: each tenant uploads a logo, a favicon and an
// email header, each slot with its own rules (a favicon must be square, an
// email header wide), each slot holding exactly one file, and every image
// resized to the sizes the app renders, served from public URLs that stay
// valid in emails opened months later.
//
//	go run ./recipe-tenant-branding       (listens on localhost:8123)
//
// Routes:
//
//	PUT    /tenants/{id}/branding/{slot}   slot is logo, favicon or email-header; field "file"
//	DELETE /tenants/{id}/branding/{slot}   back to the app's default
//	GET    /tenants/{id}/branding          the URL of every size of every slot set
//	GET    /public/…                       the public files
//
// Authentication is out of scope: a real app checks that the signed-in
// user is an admin of tenant {id} before PUT and DELETE run.
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
	listenAddr = "localhost:8123"
	// baseURL is the server's public origin; public file URLs start with it.
	baseURL = "http://" + listenAddr
	// publicPrefix is where fshttp.Public serves the disk's public objects.
	publicPrefix = "/public"
	// diskName names the one disk in the manager.
	diskName = "branding"
	// ownerTenant is the only owner type branding attaches to.
	ownerTenant = "tenant"
	// maxBrandingBytes caps any branding upload before it is re-encoded.
	maxBrandingBytes = 5 << 20
	// formOverhead is the multipart framing allowed on top of the file.
	formOverhead = 64 << 10
	// linkLifetime only matters for private files; branding is public.
	linkLifetime = time.Hour
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// The slots a tenant can brand. Each is a single role: one file per tenant.
const (
	slotLogo        = "logo"
	slotFavicon     = "favicon"
	slotEmailHeader = "email-header"
)

// slot is everything the app decides about one branding slot: what an
// upload must be, and which sizes are made from it.
type slot struct {
	limits imaging.DimensionLimits
	sizes  []string
}

// slots is the branding model in one table. A logo is shown at most 64
// pixels high in the app's header (2x for retina); a favicon must be
// square and gives the 32-pixel browser icon and the 180-pixel Apple touch
// icon; an email header is 600 pixels wide in a mail client (1200 at 2x)
// and must be wider than tall.
// ds:def id=slots-rcnmhvks
var slots = map[string]slot{
	slotLogo:        {limits: imaging.DimensionLimits{MinHeight: 64, MaxWidth: 4000, MaxHeight: 4000}, sizes: []string{"logo-header"}},
	slotFavicon:     {limits: imaging.DimensionLimits{MinWidth: 180, MinHeight: 180, Aspect: 1}, sizes: []string{"favicon-32", "favicon-180"}},
	slotEmailHeader: {limits: imaging.DimensionLimits{MinWidth: 600, MaxPixels: 4_000_000}, sizes: []string{"email-header-600"}},
}

// sizes are the variants every slot's sizes come from. PNG keeps a logo's
// transparency; ModeFit never enlarges, so a small logo stays sharp.
// ds:def id=sizes-qs9ewxs7
var sizes = map[string]imaging.Transform{
	"logo-header":      {Mode: imaging.ModeFit, Width: 512, Height: 128, Format: imaging.FormatPNG},
	"favicon-32":       {Mode: imaging.ModeFill, Width: 32, Height: 32, Format: imaging.FormatPNG},
	"favicon-180":      {Mode: imaging.ModeFill, Width: 180, Height: 180, Format: imaging.FormatPNG},
	"email-header-600": {Mode: imaging.ModeFit, Width: 1200, Height: 600, Format: imaging.FormatJPEG},
}

// basePolicy is shared by every slot: raster images only, re-encoded so no
// metadata (or anything appended to the image) is stored, public because
// branding appears on login pages and in emails.
// ds:def id=basepolicy-hkeghjms
var basePolicy = upload.Policy{
	MaxSize:    maxBrandingBytes,
	Allowed:    []string{"image/png", "image/jpeg", "image/webp"},
	Key:        upload.RandomKey("branding"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(imaging.Options{MaxBytes: maxBrandingBytes}),
}

// policies builds one upload policy per slot: the shared base plus that
// slot's dimension check, which refuses (and deletes) an image outside it.
// ds:def id=policies-n3qm6xw3
func policies() (map[string]upload.Policy, error) {
	out := make(map[string]upload.Policy, len(slots))
	for name, s := range slots {
		check, err := imaging.CheckDimensions(s.limits)
		if err != nil {
			return nil, fmt.Errorf("slot %s: %w", name, err)
		}
		p := basePolicy
		p.Inspect = check
		out[name] = p
	}
	return out, nil
}

// App holds the wired dependencies.
type App struct {
	Store    *records.Store
	Variants *variants.Plugin
	Handler  http.Handler
	policies map[string]upload.Policy
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
	v, err := variants.New(sizes, imaging.Options{})
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerTenant),
		records.WithSingleRoles(slotLogo, slotFavicon, slotEmailHeader),
		records.WithPlugins(v))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	ps, err := policies()
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, Variants: v, policies: ps}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /tenants/{id}/branding/{slot}", app.putSlot)
	mux.HandleFunc("DELETE /tenants/{id}/branding/{slot}", app.deleteSlot)
	mux.HandleFunc("GET /tenants/{id}/branding", app.getBranding)
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	app.Handler = mux
	return app, nil
}

// setSlot stores the image and attaches it to the tenant's slot, purging
// the previous file of that slot (and its sizes) after the commit, then
// generates the slot's sizes. Generating after the commit keeps image work
// out of the transaction; a failure there leaves the file in place, and
// the size is made on its first request instead.
// ds:def id=app-setslot-v4yntvfq
func (a *App) setSlot(ctx context.Context, tenant, name string, body io.Reader, filename string) (records.File, error) {
	p, ok := a.policies[name]
	if !ok {
		return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "no branding slot %q", name)
	}
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.Upload(ctx, tenant, diskName, body, filename, p); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, tenant, f.ID, records.Owner{Type: ownerTenant, ID: tenant}, name, records.PreviousPurge)
		return err
	})
	if err != nil {
		return records.File{}, err
	}
	if err := a.Variants.Generate(ctx, a.Store, tenant, f.ID, slots[name].sizes...); err != nil {
		fmt.Fprintf(os.Stderr, "branding %s/%s: %v\n", tenant, name, err)
	}
	return f, nil
}

// branding is every slot a tenant has set, with the URL of each size.
type branding map[string]map[string]string

// describe lists the tenant's slots. Branded files are public, so each
// URL is the disk's public URL: it never expires, which is what an email
// opened next year needs. A slot not set is absent, and the page falls
// back to the app's own logo.
// ds:def id=app-describe-39kec2pp
func (a *App) describe(ctx context.Context, tenant string) (branding, error) {
	out := branding{}
	for name, s := range slots {
		list, err := a.Store.Attachments(ctx, tenant, records.Owner{Type: ownerTenant, ID: tenant}, name)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			continue
		}
		urls := map[string]string{}
		for _, size := range s.sizes {
			u, err := a.Variants.URL(ctx, a.Store, tenant, list[0].File.ID, size, linkLifetime)
			if err != nil {
				return nil, err
			}
			urls[size] = u
		}
		out[name] = urls
	}
	return out, nil
}

// putSlot handles an upload to one slot and answers the whole branding.
func (a *App) putSlot(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, maxBrandingBytes+formOverhead)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the image in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	if _, err := a.setSlot(r.Context(), tenant, r.PathValue("slot"), file, header.Filename); err != nil {
		writeError(w, err)
		return
	}
	a.answer(w, r, tenant)
}

// deleteSlot purges a slot's file, so the tenant falls back to the default.
func (a *App) deleteSlot(w http.ResponseWriter, r *http.Request) {
	tenant, name := r.PathValue("id"), r.PathValue("slot")
	list, err := a.Store.Attachments(r.Context(), tenant, records.Owner{Type: ownerTenant, ID: tenant}, name)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(list) == 0 {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "tenant %s has no %s", tenant, name))
		return
	}
	if err := a.Store.Purge(r.Context(), tenant, list[0].File.ID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getBranding answers the tenant's branding.
func (a *App) getBranding(w http.ResponseWriter, r *http.Request) {
	a.answer(w, r, r.PathValue("id"))
}

// answer writes the tenant's branding as JSON.
func (a *App) answer(w http.ResponseWriter, r *http.Request, tenant string) {
	b, err := a.describe(r.Context(), tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
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
	dir, err := os.MkdirTemp("", "recipe-tenant-branding-")
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
