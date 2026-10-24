// Command recipe-strip-photo-metadata removes what phones hide in photos
// before anything is stored: the GPS position of where the photo was taken
// (often the user's home), the camera's serial number, the capture time,
// XMP, comments, and any payload appended after the image data. It applies
// the EXIF orientation first, so the stored photo is upright everywhere.
//
//	go run ./recipe-strip-photo-metadata       (listens on localhost:8104)
//
// Routes:
//
//	POST /photos     multipart form, field "file", up to 10 photos
//	GET  /public/…   the stored photos
//
// Authentication is out of scope.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8104"
	// baseURL is the server's public origin.
	baseURL = "http://" + listenAddr
	// publicPrefix is where fshttp.Public serves the stored photos.
	publicPrefix = "/public"
	// diskName names the photo disk.
	diskName = "photos"
	// photoDir is where accepted photos are stored.
	photoDir = "photos"
	// maxPhotoBytes caps one uploaded photo, before it is re-encoded.
	maxPhotoBytes = 25 << 20
	// maxPhotosPerRequest caps the files of one request.
	maxPhotosPerRequest = 10
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// photoLimits bound what Sanitize reads and decodes. They are the real cap
// on the upload: Policy.MaxSize is checked against what Sanitize returns,
// which is the re-encoded photo, usually smaller than what was sent.
// ds:def id=photolimits-6wkabw72
var photoLimits = imaging.Options{MaxBytes: maxPhotoBytes, MaxPixels: imaging.DefaultMaxPixels}

// photoPolicy re-encodes every JPEG, PNG and WebP before it is stored.
// GIF is accepted and passes through untouched: re-encoding would keep only
// its first frame, and GIF carries no EXIF.
// ds:def id=photopolicy-xck424xv
var photoPolicy = upload.Policy{
	MaxSize:    maxPhotoBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp", "image/gif"},
	Key:        upload.RandomKey(photoDir),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(photoLimits),
}

// openDisk opens the local photo directory, public under publicBase.
// ds:def id=opendisk-5m3qp7xy
func openDisk(dataDir, publicBase string) (*filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(diskName, drv, filesystem.WithPublicURL(publicBase+publicPrefix))
}

// newHandler mounts the upload handler and the public files. upload.Handler
// runs the policy's Process on every file of the form, so no photo reaches
// the disk with its metadata.
// ds:def id=newhandler-9ufwqrgz
func newHandler(disk *filesystem.Disk) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /photos", upload.Handler(disk, photoPolicy, upload.HandlerOptions{MaxFiles: maxPhotosPerRequest}))
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	return mux
}

// sanitizeStored applies the photo policy to an object that arrived
// without passing through Process: a presigned or Uppy upload, which goes
// straight to the bucket. It reads the object, stores it again through
// upload.Receive (detection, Sanitize, size cap, new random key) and
// deletes the original. Run it after upload.Confirm, before the file is
// recorded or shown to anyone.
// ds:def id=sanitizestored-zb7zw82r
func sanitizeStored(ctx context.Context, disk *filesystem.Disk, key string) (upload.Result, error) {
	rc, err := disk.Read(ctx, key)
	if err != nil {
		return upload.Result{}, err
	}
	res, err := upload.Receive(ctx, disk, rc, path.Base(key), photoPolicy)
	_ = rc.Close()
	if err != nil {
		return upload.Result{}, err
	}
	if err := disk.Delete(ctx, key); err != nil {
		return upload.Result{}, err
	}
	return res, nil
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-strip-photo-metadata-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	disk, err := openDisk(dir, baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", baseURL, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: newHandler(disk), ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
