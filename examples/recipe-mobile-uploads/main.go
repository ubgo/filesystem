// Command recipe-mobile-uploads is the backend of "back up my camera roll":
// a phone sends up to 20 photos in one request, each is checked and
// stripped of its GPS position on its own, a bad photo does not sink the
// others, and a request retried after a lost response does not store the
// same photo twice.
//
//	go run ./recipe-mobile-uploads       (listens on localhost:8107)
//
// Routes (the demo trusts the X-User header; a real app uses its session):
//
//	GET  /photos/limits  what a photo may be: types, bytes, largest side
//	POST /photos         multipart form, up to 20 files in field "file"
//	GET  /photos         the user's photos, oldest first
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
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
	listenAddr = "localhost:8107"
	// diskName names the one disk in the manager.
	diskName = "photos"
	// formField is the multipart field holding the photos.
	formField = "file"
	// maxFilesPerRequest caps one batch. A phone sends a camera roll in
	// batches of this size, so one request never runs for minutes.
	maxFilesPerRequest = 20
	// maxPhotoBytes caps one photo as sent: above any phone camera's JPEG.
	maxPhotoBytes = 25 << 20
	// maxPhotoSide is the longest side the app displays, so the longest
	// side a photo may have. The limits endpoint publishes it, the phone
	// downscales to it, and the server refuses anything larger.
	maxPhotoSide = 4096
	// formOverhead is the multipart framing allowed on top of the files.
	formOverhead = 64 << 10
	// ownerUser and roleCameraRoll are where photos are attached.
	ownerUser      = "user"
	roleCameraRoll = "camera-roll"
	// headerUser carries the demo's user; a real app reads its session.
	headerUser = "X-User"
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// photoPolicy accepts the formats phones export. Sanitize re-encodes each
// photo, dropping the GPS position and camera details in its EXIF; its
// MaxBytes caps the photo as sent, because Policy.MaxSize applies to what
// Process returns. HEIC is not in the list: it is detected (image/heic) but
// cannot be decoded in pure Go, so it could not be sanitized; the app asks
// the phone for JPEG instead.
// ds:def id=photopolicy-sccupwen
var photoPolicy = upload.Policy{
	MaxSize: maxPhotoBytes,
	Allowed: []string{"image/jpeg", "image/png", "image/webp"},
	Key:     upload.RandomKey("photos"),
	Process: imaging.Sanitize(imaging.Options{MaxBytes: maxPhotoBytes}),
}

// errDuplicate rolls back the upload of a photo the user already has.
var errDuplicate = errors.New("duplicate photo")

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	Handler http.Handler
	policy  upload.Policy
}

// photoLimits completes photoPolicy with the dimension check and builds
// the endpoint that publishes the same limits to clients. Both come from
// one place, so what the phone is told and what the server enforces
// cannot drift apart: CheckDimensions refuses a photo over maxPhotoSide
// on the server, and LimitsHandler only describes that check.
// ds:def id=photolimits-se3qfup2
func photoLimits() (upload.Policy, http.Handler, error) {
	dims, err := imaging.CheckDimensions(imaging.DimensionLimits{MaxWidth: maxPhotoSide, MaxHeight: maxPhotoSide})
	if err != nil {
		return upload.Policy{}, nil, err
	}
	p := photoPolicy
	p.Inspect = dims
	return p, upload.LimitsHandler(p, upload.LimitsOptions{MaxWidth: maxPhotoSide, MaxHeight: maxPhotoSide}), nil
}

// New wires the app over a data directory.
func New(ctx context.Context, dataDir string) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	policy, limits, err := photoLimits()
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, policy: policy}
	mux := http.NewServeMux()
	mux.Handle("GET /photos/limits", limits)
	mux.HandleFunc("POST /photos", app.upload)
	mux.HandleFunc("GET /photos", app.list)
	app.Handler = mux
	return app, nil
}

// stored is one photo of a batch that is now the user's, new or not.
type stored struct {
	Filename  string `json:"filename"`
	FileID    string `json:"file_id"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// batchResponse answers a batch: what is stored, and what failed and why.
// ds:def id=batchresponse-xwtcum37
type batchResponse struct {
	Files  []stored             `json:"files"`
	Failed []upload.FileFailure `json:"failed,omitempty"`
}

// storeOne stores one photo and attaches it to the user, in its own
// transaction, so one photo's failure never undoes another's. A photo the
// user already has (same SHA-256 after sanitizing, which is deterministic)
// is rolled back, which makes Store.Upload delete the new object, and the
// existing file is returned: retrying a batch is safe.
// ds:def id=app-storeone-cd7dtawe
func (a *App) storeOne(ctx context.Context, user string, body io.Reader, name string) (stored, error) {
	out := stored{Filename: upload.SafeFilename(name)}
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.Upload(ctx, user, diskName, body, name, a.policy)
		if err != nil {
			return err
		}
		if existing, ok, err := a.findBySHA(ctx, user, f); err != nil || ok {
			out.FileID, out.Duplicate = existing, ok
			return errors.Join(err, errDuplicate)
		}
		out.FileID = f.ID
		_, err = a.Store.Attach(ctx, user, f.ID, records.Owner{Type: ownerUser, ID: user}, roleCameraRoll)
		return err
	})
	if out.Duplicate && errors.Is(err, errDuplicate) {
		return out, nil
	}
	return out, err
}

// findBySHA returns another live file of the user's with the content of f.
// fs_files has no index on sha256; add one in your own migration if users
// keep tens of thousands of photos.
// ds:def id=app-findbysha-e7ny7uwv
func (a *App) findBySHA(ctx context.Context, user string, f records.File) (string, bool, error) {
	page, err := a.Store.Files(ctx, records.FileQuery{
		Scope: user, Limit: 1,
		Filters: []sqlb.Predicate{sqlb.Eq(records.FileSHA256, f.SHA256), sqlb.NotEq(records.FileID, f.ID)},
	})
	if err != nil || len(page.Files) == 0 {
		return "", false, err
	}
	return page.Files[0].ID, true, nil
}

// receive walks the multipart body part by part, streaming each photo to
// storeOne. A photo's own failure is listed and the batch goes on; a
// failure of the request itself (malformed, over the size cap, too many
// files) ends it, keeping the photos already stored.
// ds:def id=app-receive-mqtjrgav
func (a *App) receive(ctx context.Context, user string, mr *multipart.Reader) (batchResponse, error) {
	resp := batchResponse{Files: []stored{}}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return resp, nil
		}
		if err != nil {
			return resp, requestError(err)
		}
		if part.FormName() != formField || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if len(resp.Files)+len(resp.Failed) == maxFilesPerRequest {
			_ = part.Close()
			return resp, filesystem.Errorf(filesystem.CodeInvalidArgument, "more than %d files", maxFilesPerRequest)
		}
		s, err := a.storeOne(ctx, user, part, part.FileName())
		_ = part.Close()
		var mbe *http.MaxBytesError
		switch {
		case err == nil:
			resp.Files = append(resp.Files, s)
		case errors.As(err, &mbe):
			return resp, requestError(err)
		default:
			resp.Failed = append(resp.Failed, upload.FileFailure{Filename: s.Filename, Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
		}
	}
}

// upload handles a batch: 201 when every photo is stored, 200 when some
// failed, the first failure's status when none could be stored.
// ds:def id=app-upload-2ekk2k73
func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFilesPerRequest*maxPhotoBytes+formOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, filesystem.NewError(filesystem.CodeInvalidArgument, err))
		return
	}
	resp, err := a.receive(r.Context(), user, mr)
	switch {
	case err != nil:
		writeError(w, err)
	case len(resp.Files) == 0 && len(resp.Failed) > 0:
		writeJSON(w, fshttp.StatusOf(filesystem.NewError(resp.Failed[0].Code, nil)), resp)
	case len(resp.Failed) > 0:
		writeJSON(w, http.StatusOK, resp)
	default:
		writeJSON(w, http.StatusCreated, resp)
	}
}

// photoResponse describes one stored photo.
type photoResponse struct {
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// list returns the user's camera roll in upload order.
func (a *App) list(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	attached, err := a.Store.Attachments(r.Context(), user, records.Owner{Type: ownerUser, ID: user}, roleCameraRoll)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]photoResponse, 0, len(attached))
	for _, af := range attached {
		f := af.File
		out = append(out, photoResponse{FileID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, SHA256: f.SHA256})
	}
	writeJSON(w, http.StatusOK, out)
}

// requestError turns the request-size cap into CodeTooLarge.
func requestError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return filesystem.NewError(filesystem.CodeTooLarge, err)
	}
	return filesystem.NewError(filesystem.CodeInvalidArgument, err)
}

// writeError answers with the status for err's code and a JSON body.
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
	dir, err := os.MkdirTemp("", "recipe-mobile-uploads-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on http://"+listenAddr, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
