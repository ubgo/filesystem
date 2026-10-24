// Command recipe-avatars is the profile-picture feature of a social or SaaS
// app: a user uploads a photo, and the server re-encodes it, dropping EXIF
// and GPS (at its own size, or as a 512 px square of the region chosen in a
// cropper when the upload passes ?crop=), keeps exactly one avatar per user,
// generates the 64 and 256 px sizes right after the upload commits, and
// serves them as public URLs.
//
//	go run ./recipe-avatars       (listens on localhost:8101)
//
// Routes:
//
//	PUT    /users/{id}/avatar[?crop=x,y,w,h]   multipart form, field "file"
//	GET    /users/{id}/avatar?size=64|256      302 to the public URL
//	DELETE /users/{id}/avatar                  removes it for good
//	GET    /public/…                           the public files
//
// Authentication is out of scope: a real app checks that the signed-in
// user is {id} (or an admin) before any of these handlers run.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/placeholder"
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
	listenAddr = "localhost:8101"
	// baseURL is the server's public origin; public file URLs start with it.
	baseURL = "http://" + listenAddr
	// publicPrefix is where fshttp.Public serves the disk's public objects.
	publicPrefix = "/public"
	// fallbackPrefix serves generated avatars for users without a photo.
	fallbackPrefix = "/avatars/fallback/"
	// diskName names the one disk in the manager.
	diskName = "media"
	// ownerUser is the only owner type files attach to here.
	ownerUser = "user"
	// roleAvatar is a single role: at most one file per user.
	roleAvatar = "avatar"
	// maxAvatarBytes caps the uploaded photo, before it is re-encoded.
	maxAvatarBytes = 8 << 20
	// formOverhead is the multipart framing allowed on top of the file.
	formOverhead = 64 << 10
	// avatarSide is the stored square's side in pixels: the largest size
	// any screen shows, so every smaller size is a downscale.
	avatarSide = 512
	// avatarQuality is the JPEG quality of the stored square.
	avatarQuality = 88
	// sizeSmall and sizeLarge are the variant names clients ask for.
	sizeSmall = "64"
	sizeLarge = "256"
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// inputLimits bound the photo a processor reads. Policy.MaxSize applies to
// what the processor returns, so the cap on the uploaded bytes lives here.
// ds:def id=inputlimits-sybzgm7e
var inputLimits = imaging.Options{MaxBytes: maxAvatarBytes}

// avatarPolicy accepts the photo formats phones and browsers produce.
// Sanitize re-encodes the photo so the GPS position and camera details in its
// EXIF never reach storage; policyFor swaps in a crop when the client sent
// one. Avatars are public, so they can be cached and linked directly.
// ds:def id=avatarpolicy-nqds67xq
var avatarPolicy = upload.Policy{
	MaxSize:    maxAvatarBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp"},
	Key:        upload.RandomKey("avatars"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(inputLimits),
}

// sizes are the variants of every avatar, declared once. Only these names
// can be generated, so a client cannot ask for an arbitrary size.
// ds:def id=sizes-nnfzky83
var sizes = map[string]imaging.Transform{
	"avatar-" + sizeSmall: {Mode: imaging.ModeFill, Width: 64, Height: 64, Format: imaging.FormatJPEG},
	"avatar-" + sizeLarge: {Mode: imaging.ModeFill, Width: 256, Height: 256, Format: imaging.FormatJPEG},
}

// App holds the wired dependencies.
type App struct {
	Store    *records.Store
	Variants *variants.Plugin
	Handler  http.Handler
}

// openDisk opens the local directory as a public-capable disk. WithPublicURL
// makes PublicURL return publicBase + "/" + key, which fshttp.Public serves.
// ds:def id=opendisk-r8e7pzhe
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

// newVariants declares the sizes and generates both right after an avatar
// is recorded (WithEager), so the first profile view does not wait for a
// resize. A failed eager resize is logged and retried on first request.
// ds:def id=newvariants-8s2acvgm
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(sizes, imaging.Options{}, variants.WithEager(logFailure, "avatar-"+sizeSmall, "avatar-"+sizeLarge))
}

// openStore opens SQLite and the records Store. WithSingleRoles makes the
// database hold at most one avatar per user, even under concurrent uploads.
// ds:def id=openstore-h5yb7sge
func openStore(ctx context.Context, dataDir string, disks *filesystem.Manager, v *variants.Plugin) (*records.Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser),
		records.WithSingleRoles(roleAvatar),
		records.WithPlugins(v))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}

// New wires the app over a data directory; publicBase is the origin public
// URLs start with.
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
	app := &App{Store: store, Variants: v}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /users/{id}/avatar", app.putAvatar)
	mux.HandleFunc("GET /users/{id}/avatar", app.getAvatar)
	mux.HandleFunc("DELETE /users/{id}/avatar", app.deleteAvatar)
	mux.Handle(fallbackPrefix, http.StripPrefix(fallbackPrefix, placeholder.Handler(placeholder.HandlerOptions{})))
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	app.Handler = mux
	return app, nil
}

// parseCrop reads "?crop=x,y,w,h", the square a cropper widget reports in
// pixels of the upright photo. No parameter means no crop.
func parseCrop(raw string) (image.Rectangle, error) {
	if raw == "" {
		return image.Rectangle{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return image.Rectangle{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "crop must be x,y,w,h")
	}
	var n [4]int
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 {
			return image.Rectangle{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "crop must be four non-negative integers")
		}
		n[i] = v
	}
	if n[2] == 0 || n[3] == 0 {
		return image.Rectangle{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "crop width and height must be positive")
	}
	return image.Rect(n[0], n[1], n[0]+n[2], n[1]+n[3]), nil
}

// cropTo returns a Processor that cuts the chosen region out of the upright
// photo and scales it to the stored square. Like Sanitize it re-encodes, so
// EXIF and GPS are dropped here too; a region outside the photo is refused.
// ds:def id=cropto-t875xnwf
func cropTo(region image.Rectangle) upload.Processor {
	t := imaging.Transform{Mode: imaging.ModeFill, Width: avatarSide, Height: avatarSide, Format: imaging.FormatJPEG, Quality: avatarQuality, Crop: region}
	return func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		res, err := imaging.Process(body, t, inputLimits)
		if err != nil {
			return nil, upload.Info{}, err
		}
		name := strings.TrimSuffix(in.Filename, filepath.Ext(in.Filename)) + t.Extension()
		return bytes.NewReader(res.Data), upload.Info{Filename: name, ContentType: res.ContentType}, nil
	}
}

// policyFor is the avatar policy for one request: with a crop, the crop
// processor replaces Sanitize; everything else stays the same.
// ds:def id=policyfor-dy8uujc8
func policyFor(r *http.Request) (upload.Policy, error) {
	region, err := parseCrop(r.URL.Query().Get("crop"))
	if err != nil {
		return upload.Policy{}, err
	}
	p := avatarPolicy
	if region != (image.Rectangle{}) {
		p.Process = cropTo(region)
	}
	return p, nil
}

// setAvatar stores the photo and makes it the user's only avatar in one
// transaction. PreviousPurge deletes the old avatar for good (its object and
// variants after the commit); a failure anywhere rolls back and deletes the
// new upload, so the user keeps the old avatar.
// ds:def id=app-setavatar-2bfqagmp
func (a *App) setAvatar(ctx context.Context, userID string, body io.Reader, filename string, p upload.Policy) (records.File, error) {
	var stored records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if stored, err = a.Store.Upload(ctx, "", diskName, body, filename, p); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, "", stored.ID, records.Owner{Type: ownerUser, ID: userID}, roleAvatar, records.PreviousPurge)
		return err
	})
	return stored, err
}

// avatarResponse is the JSON answer of a successful upload.
type avatarResponse struct {
	FileID string            `json:"file_id"`
	Type   string            `json:"content_type"`
	Size   int64             `json:"size"`
	URLs   map[string]string `json:"urls"`
}

// putAvatar handles the upload: cap the body, read the file field, choose
// the policy, store and attach, answer with the URLs of every size.
// ds:def id=app-putavatar-ac86bchk
func (a *App) putAvatar(w http.ResponseWriter, r *http.Request) {
	p, err := policyFor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+formOverhead)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the photo in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.setAvatar(r.Context(), r.PathValue("id"), file, header.Filename, p)
	if err != nil {
		writeError(w, err)
		return
	}
	urls, err := a.urls(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, avatarResponse{FileID: f.ID, Type: f.ContentType, Size: f.Size, URLs: urls})
}

// urls returns the public URL of every declared size of an avatar.
func (a *App) urls(ctx context.Context, f records.File) (map[string]string, error) {
	out := map[string]string{}
	for _, size := range []string{sizeSmall, sizeLarge} {
		u, err := a.Variants.URL(ctx, a.Store, f.Scope, f.ID, "avatar-"+size, time.Hour)
		if err != nil {
			return nil, err
		}
		out[size] = u
	}
	return out, nil
}

// current returns the user's avatar, or CodeNotFound.
func (a *App) current(ctx context.Context, userID string) (records.File, error) {
	list, err := a.Store.Attachments(ctx, "", records.Owner{Type: ownerUser, ID: userID}, roleAvatar)
	if err != nil {
		return records.File{}, err
	}
	if len(list) == 0 {
		return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "user %s has no avatar", userID)
	}
	return list[0].File, nil
}

// getAvatar redirects to the requested size. An unknown size is a 400
// rather than a resize: only declared sizes exist. A user without an
// avatar is redirected to a generated fallback seeded by their ID, so the
// client never shows a broken image; the demo has no display names, so it
// is an identicon, and an app adds name=<display name> for initials.
// ds:def id=app-getavatar-6n4vcnet
func (a *App) getAvatar(w http.ResponseWriter, r *http.Request) {
	size := r.URL.Query().Get("size")
	if size == "" {
		size = sizeLarge
	}
	if _, ok := sizes["avatar-"+size]; !ok {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "size must be %s or %s", sizeSmall, sizeLarge))
		return
	}
	f, err := a.current(r.Context(), r.PathValue("id"))
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		q := url.Values{"size": {size}, "v": {placeholder.Version}}
		http.Redirect(w, r, fallbackPrefix+url.PathEscape(r.PathValue("id"))+".png?"+q.Encode(), http.StatusFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	u, err := a.Variants.URL(r.Context(), a.Store, f.Scope, f.ID, "avatar-"+size, time.Hour)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// deleteAvatar removes the avatar for good: Purge deletes the row and the
// variant rows in one transaction, then the objects after it commits.
// ds:def id=app-deleteavatar-vgd2gujt
func (a *App) deleteAvatar(w http.ResponseWriter, r *http.Request) {
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
// names the code, so a client can tell "too large" from "not an image".
// ds:def id=writeerror-6eybqhn9
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
	dir, err := os.MkdirTemp("", "recipe-avatars-")
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
