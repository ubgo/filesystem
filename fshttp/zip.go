package fshttp

import (
	"archive/zip"
	"context"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
)

// Limits of a zip download. Every object is listed and sized before the
// first byte is sent, so these bound the work a request can ask for.
const (
	// DefaultZipMaxFiles caps the entries of one archive.
	DefaultZipMaxFiles = 1000
	// DefaultZipMaxBytes caps the objects' total size: 4 GiB.
	DefaultZipMaxBytes int64 = 4 << 30
	// MaxZipNameBytes caps one entry's name, a common limit of the file
	// systems an archive is extracted on.
	MaxZipNameBytes = 1024
	// defaultZipFilename names the download when the caller gives none.
	defaultZipFilename = "download.zip"
)

// ZipFile is one object of a zip download.
type ZipFile struct {
	// Key is the object on the disk.
	Key string
	// Name is its path inside the archive; empty means Key. It must be a
	// clean relative path (see ServeZip).
	Name string
}

// ZipOptions configure ServeZip. The zero value is safe.
type ZipOptions struct {
	// MaxFiles caps the entries; 0 means DefaultZipMaxFiles.
	MaxFiles int
	// MaxBytes caps the objects' total size; 0 means DefaultZipMaxBytes.
	MaxBytes int64
	// Deflate compresses the entries. Off by default: most files people
	// download together (photos, videos, PDFs) are already compressed,
	// and storing them costs no CPU.
	Deflate bool
	// OnError, when set, receives a failure that happens after the
	// response has started (a read failing, the client going away), so it
	// can be logged; the response is aborted either way.
	OnError func(error)
	// CacheControl is sent with the archive. Empty means
	// DefaultSignedCacheControl ("private, no-store"): an archive is
	// usually assembled for one user and must not be kept by a shared
	// cache. Set it for archives of public files a CDN may cache.
	CacheControl string
	// Errors renders a failure found before the first byte (a bad list,
	// too much, a missing object); see ErrorWriter. nil answers the
	// status text as text/plain. A failure while streaming cannot change
	// the response and goes to OnError.
	Errors ErrorWriter
}

// ServeZip streams the objects of d named by files to w as one zip
// archive called filename (default "download.zip"), without holding any
// object or the archive in memory. Authorise the request and choose the
// keys before calling it: it serves whatever it is given.
//
// Everything that can be checked is checked before the first byte: the
// entry count and every name (a clean relative path, valid UTF-8, no
// control characters, backslashes, drive letters or "." and ".."
// elements, at most MaxZipNameBytes, unique ignoring case, and never both
// a file and the directory of another entry, so the archive extracts the
// same on every system and never outside its folder), then every object's
// existence and the total size. A failure there is an ordinary error
// response: 400 for a bad list, 413 for too much, the Disk's status for a
// missing object.
//
// After the headers are sent an error cannot change the status, and a zip
// cut short could still look like a complete download. So a failure while
// streaming (a read error, an object whose size changed since it was
// sized, the client gone) is passed to OnError and the response is aborted
// with http.ErrAbortHandler: the connection is reset, the archive never
// gets its central directory, and the client sees a failed download rather
// than a silently incomplete one.
//
// HEAD answers with the headers only, after the same checks.
func ServeZip(w http.ResponseWriter, r *http.Request, d *filesystem.Disk, filename string, files []ZipFile, opts ZipOptions) {
	entries, err := planZip(r.Context(), d, files, opts)
	if err != nil {
		failErr(opts.Errors, w, r, err)
		return
	}
	if filename == "" {
		filename = defaultZipFilename
	}
	secure(w)
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", mime.FormatMediaType(attachment, map[string]string{"filename": filename}))
	h.Set("Cache-Control", cacheControl("", opts.CacheControl, false, DefaultSignedCacheControl))
	if r.Method == http.MethodHead {
		return
	}
	if err := writeZip(r.Context(), w, d, entries, opts.Deflate); err != nil {
		if opts.OnError != nil {
			opts.OnError(err)
		}
		panic(http.ErrAbortHandler)
	}
}

// zipEntry is a checked file with its size and modification time.
type zipEntry struct {
	name string
	e    filesystem.Entry
}

// planZip checks the list and sizes every object, before any byte is sent.
func planZip(ctx context.Context, d *filesystem.Disk, files []ZipFile, opts ZipOptions) ([]zipEntry, error) {
	maxFiles, maxBytes := opts.MaxFiles, opts.MaxBytes
	if maxFiles <= 0 {
		maxFiles = DefaultZipMaxFiles
	}
	if maxBytes <= 0 {
		maxBytes = DefaultZipMaxBytes
	}
	switch {
	case len(files) == 0:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: a zip needs at least one file")
	case len(files) > maxFiles:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: %d files is over the limit of %d", len(files), maxFiles)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
		if names[i] == "" {
			names[i] = f.Key
		}
		if err := checkZipName(names[i]); err != nil {
			return nil, err
		}
	}
	if err := checkZipTree(names); err != nil {
		return nil, err
	}
	entries := make([]zipEntry, len(files))
	var total int64
	for i, f := range files {
		e, err := d.Stat(ctx, f.Key)
		if err != nil {
			return nil, err
		}
		if total += e.Size; total > maxBytes {
			return nil, filesystem.Errorf(filesystem.CodeTooLarge, "fshttp: the files total more than %d bytes", maxBytes)
		}
		entries[i] = zipEntry{name: names[i], e: e}
	}
	return entries, nil
}

// checkZipName refuses a name that is not a clean, portable relative path.
func checkZipName(name string) error {
	bad := func(why string) error {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: zip entry name %q %s", name, why)
	}
	switch {
	case !utf8.ValidString(name):
		return bad("is not valid UTF-8")
	case len(name) > MaxZipNameBytes:
		return bad("is too long")
	case strings.ContainsFunc(name, unicode.IsControl):
		return bad("contains a control character")
	case strings.Contains(name, `\`):
		return bad("contains a backslash")
	case strings.HasPrefix(name, "/"), len(name) >= 2 && name[1] == ':':
		return bad("is absolute")
	case path.Clean(name) != name, name == ".", name == "..", strings.HasPrefix(name, "../"):
		return bad("is not a clean relative path")
	}
	return nil
}

// checkZipTree refuses names that would collide when extracted: equal
// ignoring case (one overwrites the other on Windows and macOS), or one
// name being a directory of another ("a" and "a/b" cannot both exist).
func checkZipTree(names []string) error {
	files := map[string]string{}
	dirs := map[string]string{}
	for _, n := range names {
		folded := strings.ToLower(n)
		if prev, ok := files[folded]; ok {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: zip entries %q and %q collide", prev, n)
		}
		if prev, ok := dirs[folded]; ok {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: zip entry %q is also a directory of %q", n, prev)
		}
		files[folded] = n
		for dir := path.Dir(folded); dir != "."; dir = path.Dir(dir) {
			if prev, ok := files[dir]; ok {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "fshttp: zip entry %q is also a directory of %q", prev, n)
			}
			dirs[dir] = n
		}
	}
	return nil
}

// writeZip streams the entries, then the central directory. It reads each
// object once and refuses one whose size differs from the planned size, so
// MaxBytes holds even if an object grows during the download.
func writeZip(ctx context.Context, w io.Writer, d *filesystem.Disk, entries []zipEntry, deflate bool) error {
	zw := zip.NewWriter(w)
	method := zip.Store
	if deflate {
		method = zip.Deflate
	}
	for _, en := range entries {
		hdr := &zip.FileHeader{Name: en.name, Method: method, Modified: en.e.ModTime.UTC()}
		hdr.SetMode(0o644)
		dst, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if err := copyEntry(ctx, dst, d, en); err != nil {
			return err
		}
	}
	return zw.Close()
}

func copyEntry(ctx context.Context, dst io.Writer, d *filesystem.Disk, en zipEntry) error {
	rc, err := d.Read(ctx, en.e.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	n, err := io.Copy(dst, io.LimitReader(rc, en.e.Size+1))
	if err != nil {
		return err
	}
	if n != en.e.Size {
		return filesystem.Errorf(filesystem.CodeUnavailable, "fshttp: %q changed during the download (%d bytes, sized at %d)", en.e.Key, n, en.e.Size)
	}
	return nil
}
