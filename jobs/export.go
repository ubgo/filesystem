package jobs

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Limits of a subject export.
const (
	// DefaultExportMaxBytes caps an export's archive: 4 GiB.
	DefaultExportMaxBytes int64 = 4 << 30
	// DefaultExportMaxFiles caps the files of one export.
	DefaultExportMaxFiles = 10000
	// ExportManifest is the name of the archive's first entry: a JSON
	// array describing every file (ID, name, type, size, SHA-256, dates)
	// in the order the files follow it.
	ExportManifest = "manifest.json"
	// defaultExportFilename is the name the browser saves the archive as
	// when ExportOptions.Filename is empty.
	defaultExportFilename = "export.zip"
	// exportIDBytes is the entropy of an export's key: the key is never
	// guessable, though only the signed link grants access.
	exportIDBytes = 16
)

// Subject is whose files an export gathers, inside one scope: the files
// UploadedBy recorded, the files attached to Owner, or both (each file
// once). At least one must be set.
type Subject struct {
	Scope      string
	UploadedBy string
	// Owner selects the files attached to it under any role; its type
	// must be registered (records.WithOwnerTypes). Zero means none.
	Owner records.Owner
}

// ExportOptions configure ExportSubject.
type ExportOptions struct {
	// Dir is the directory the archive is written under, as
	// Dir/<random>.zip. Required and never the root, so that
	// UploadSweep(disk, Dir, ≥ TTL) can delete expired exports and
	// nothing else.
	Dir string
	// TTL is the lifetime of the download link. Required: an export link
	// always expires.
	TTL time.Duration
	// Filename is the name the browser saves the archive as; empty means
	// "export.zip".
	Filename string
	// MaxBytes caps the archive; 0 means DefaultExportMaxBytes. A larger
	// export is aborted and nothing is stored.
	MaxBytes int64
	// MaxFiles caps the files; 0 means DefaultExportMaxFiles.
	MaxFiles int
}

// Export is a finished export.
type Export struct {
	// Key is the archive's key on the export disk.
	Key string
	// URL is the signed download link, valid until ExpiresAt.
	URL       string
	ExpiresAt time.Time
	// Files is how many files the archive holds, Bytes their total size.
	Files int
	Bytes int64
}

// manifestEntry is one file in the export's manifest.
type manifestEntry struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UploadedBy  string    `json:"uploaded_by,omitempty"`
}

// ExportSubject bundles every live file of a subject (a data-portability
// request, "download everything I uploaded") into one zip archive on
// disk and returns a signed link to it that expires after opts.TTL. The
// archive holds manifest.json, then each file as <file ID>/<filename>,
// the filename reduced to a safe base name (upload.SafeFilename), so the
// archive extracts the same everywhere and never outside its folder.
//
// The archive is streamed into storage as it is built (an io.Pipe into
// Disk.Write with MaxSize), so neither the archive nor any file is held in
// memory, and an export over MaxBytes or MaxFiles is refused, with
// nothing stored. The archive is always written private, whatever the
// disk's default, so only the signed link reads it; a disk that cannot
// store a private object refuses the export (CodeUnsupported). Each file is read from its own disk, so files spread
// across disks by tiering are all included. The link makes the browser
// save the archive as opts.Filename.
//
// The library starts no background work: schedule UploadSweep(disk,
// opts.Dir, minAge) with minAge at least TTL to delete expired archives.
// The caller authorises the request; ExportSubject exports whatever
// subject it is given.
func ExportSubject(ctx context.Context, store *records.Store, disk *filesystem.Disk, sub Subject, opts ExportOptions) (Export, error) {
	if opts.TTL <= 0 {
		return Export{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "export: the link lifetime %s is not positive", opts.TTL)
	}
	dir, err := filesystem.CleanDir(opts.Dir)
	if err != nil {
		return Export{}, err
	}
	if dir == "" {
		return Export{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "export: refusing to write exports at the root of disk %s", disk.Name())
	}
	maxFiles, maxBytes := opts.MaxFiles, opts.MaxBytes
	if maxFiles <= 0 {
		maxFiles = DefaultExportMaxFiles
	}
	if maxBytes <= 0 {
		maxBytes = DefaultExportMaxBytes
	}
	files, err := subjectFiles(ctx, store, sub, maxFiles)
	if err != nil {
		return Export{}, err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if total > maxBytes {
		return Export{}, filesystem.Errorf(filesystem.CodeTooLarge, "export: the files total %d bytes, over the limit of %d", total, maxBytes)
	}
	id := make([]byte, exportIDBytes)
	_, _ = rand.Read(id) // crypto/rand.Read never fails
	key := dir + "/" + hex.EncodeToString(id) + ".zip"

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeExport(ctx, store, pw, files)) }()
	_, err = disk.Write(ctx, key, pr, filesystem.WriteOptions{ContentType: "application/zip", MaxSize: maxBytes, Visibility: filesystem.VisibilityPrivate})
	_ = pr.CloseWithError(err) // unblocks the writer when the upload failed
	if err != nil {
		return Export{}, err
	}
	name := opts.Filename
	if name == "" {
		name = defaultExportFilename
	}
	req, err := disk.SignedURL(ctx, key, filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: opts.TTL,
		ContentType: "application/zip", ContentDisposition: records.AttachmentDisposition(name),
	})
	if err != nil {
		return Export{}, errors.Join(err, disk.Delete(context.WithoutCancel(ctx), key))
	}
	return Export{Key: key, URL: req.URL, ExpiresAt: req.ExpiresAt, Files: len(files), Bytes: total}, nil
}

// subjectFiles lists the subject's live files, each once, in the order
// the listings return them, refusing more than maxFiles.
func subjectFiles(ctx context.Context, store *records.Store, sub Subject, maxFiles int) ([]records.File, error) {
	var filters []sqlb.Predicate
	if sub.UploadedBy != "" {
		filters = append(filters, sqlb.Eq(records.FileUploadedBy, sub.UploadedBy))
	}
	if sub.Owner != (records.Owner{}) {
		attached, err := store.AttachedTo(sub.Scope, sub.Owner, "")
		if err != nil {
			return nil, err
		}
		filters = append(filters, attached)
	}
	if len(filters) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "export: the subject needs an uploader or an owner")
	}
	seen := map[string]bool{}
	var out []records.File
	for _, filter := range filters {
		cursor := ""
		for {
			page, err := store.Files(ctx, records.FileQuery{Scope: sub.Scope, Limit: records.MaxPageSize, Cursor: cursor, Filters: []sqlb.Predicate{filter}})
			if err != nil {
				return nil, err
			}
			for _, f := range page.Files {
				if seen[f.ID] {
					continue
				}
				if seen[f.ID] = true; len(seen) > maxFiles {
					return nil, filesystem.Errorf(filesystem.CodeTooLarge, "export: the subject has more than %d files", maxFiles)
				}
				out = append(out, f)
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
	}
	return out, nil
}

// writeExport writes the manifest and then every file to w as a zip.
func writeExport(ctx context.Context, store *records.Store, w io.Writer, files []records.File) error {
	zw := zip.NewWriter(w)
	manifest := make([]manifestEntry, len(files))
	for i, f := range files {
		manifest[i] = manifestEntry{ID: f.ID, Path: exportPath(f), Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, SHA256: f.SHA256, CreatedAt: f.CreatedAt, UploadedBy: f.UploadedBy}
	}
	mw, err := zw.Create(ExportManifest)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return err
	}
	for i, f := range files {
		if err := copyExportFile(ctx, store, zw, f, manifest[i].Path); err != nil {
			return err
		}
	}
	return zw.Close()
}

// copyExportFile adds one file's object to the archive, refusing an
// object whose size differs from the row's.
func copyExportFile(ctx context.Context, store *records.Store, zw *zip.Writer, f records.File, name string) error {
	disk, err := store.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	hdr := &zip.FileHeader{Name: name, Method: zip.Store, Modified: f.CreatedAt.UTC()}
	hdr.SetMode(0o644)
	dst, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, io.LimitReader(rc, f.Size+1))
	if err != nil {
		return err
	}
	if n != f.Size {
		return filesystem.Errorf(filesystem.CodeUnavailable, "export: file %q has %d bytes, its record says %d", f.ID, n, f.Size)
	}
	return nil
}

// exportPath is a file's path in the archive: its ID (unique, made of
// safe characters) then its filename reduced again with
// upload.SafeFilename, since a row edited outside the Store could hold
// anything.
func exportPath(f records.File) string {
	return f.ID + "/" + upload.SafeFilename(f.Filename)
}
