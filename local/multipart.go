package local

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// Layout of one upload under multipartDir/<id>/:
//
//	upload.json   the key and write options (uploadMeta)
//	<n>           the bytes of part n
//	<n>.etag      the hex MD5 of part n, its ETag
const (
	metaFile   = "upload.json"
	etagSuffix = ".etag"
)

// uploadIDPattern is the shape of the names this driver generates, upload
// IDs and staging file names alike (randomName: at
// least 26 base32 characters, open-ended because crypto/rand may lengthen
// Text in a future Go). An ID is a directory name, so anything else is
// rejected before it reaches the filesystem.
var uploadIDPattern = regexp.MustCompile(`^[A-Z2-7]{26,}$`)

// uploadMeta is what CreateMultipart records for CompleteMultipart.
type uploadMeta struct {
	Key         string                `json:"key"`
	Visibility  filesystem.Visibility `json:"visibility"`
	IfNotExists bool                  `json:"if_not_exists"`
}

func uploadDir(id string) string { return multipartDir + "/" + id }

func noUpload(id string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "no multipart upload %q", id)
}

// loadUpload validates id and reads its metadata, checking it belongs to key.
func (d *Driver) loadUpload(key, id string) (uploadMeta, error) {
	var m uploadMeta
	if !uploadIDPattern.MatchString(id) {
		return m, noUpload(id)
	}
	b, err := d.root.ReadFile(uploadDir(id) + "/" + metaFile)
	if errors.Is(err, fs.ErrNotExist) {
		return m, noUpload(id)
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Key != key {
		return m, noUpload(id)
	}
	return m, nil
}

// CreateMultipart implements filesystem.MultipartUploader.
func (d *Driver) CreateMultipart(ctx context.Context, key string, opts filesystem.WriteOptions) (string, error) {
	if err := firstErr(ctx.Err(), d.checkNewKey(key)); err != nil {
		return "", err
	}
	id := randomName()
	// Marshalling a struct of strings and a bool cannot fail.
	meta, _ := json.Marshal(uploadMeta{Key: key, Visibility: opts.Visibility, IfNotExists: opts.IfNotExists})
	if err := d.root.MkdirAll(uploadDir(id), d.dirMode); err != nil {
		return "", err
	}
	if err := d.root.WriteFile(uploadDir(id)+"/"+metaFile, meta, d.filePrivate); err != nil {
		return "", err
	}
	return id, nil
}

// UploadPart implements filesystem.MultipartUploader. The part is staged and
// renamed into the upload directory, so a failed upload of a part leaves
// any earlier copy of that part intact.
func (d *Driver) UploadPart(ctx context.Context, key, uploadID string, number int, body io.Reader, _ int64) (filesystem.Part, error) {
	if _, err := d.loadUpload(key, uploadID); err != nil {
		return filesystem.Part{}, err
	}
	h := md5.New()
	tmp, err := d.stage(ctx, io.TeeReader(body, h), d.filePrivate)
	if err != nil {
		return filesystem.Part{}, err
	}
	defer func() { _ = d.root.Remove(tmp) }()
	etag := hex.EncodeToString(h.Sum(nil))
	partPath := uploadDir(uploadID) + "/" + strconv.Itoa(number)
	if err := d.root.WriteFile(partPath+etagSuffix, []byte(etag), d.filePrivate); err != nil {
		return filesystem.Part{}, err
	}
	if err := d.root.Rename(tmp, partPath); err != nil {
		return filesystem.Part{}, err
	}
	info, err := d.root.Stat(partPath)
	if err != nil {
		return filesystem.Part{}, err
	}
	return filesystem.Part{Number: number, ETag: etag, Size: info.Size()}, nil
}

// ListParts implements filesystem.MultipartUploader.
func (d *Driver) ListParts(ctx context.Context, key, uploadID string) ([]filesystem.Part, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := d.loadUpload(key, uploadID); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(d.root.FS(), uploadDir(uploadID))
	if err != nil {
		return nil, err
	}
	var parts []filesystem.Part
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := d.part(uploadID, n)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	slices.SortFunc(parts, func(a, b filesystem.Part) int { return a.Number - b.Number })
	return parts, nil
}

// part reads part n's ETag and size.
func (d *Driver) part(uploadID string, n int) (filesystem.Part, error) {
	partPath := uploadDir(uploadID) + "/" + strconv.Itoa(n)
	info, err := d.root.Stat(partPath)
	if err != nil {
		return filesystem.Part{}, err
	}
	etag, err := d.root.ReadFile(partPath + etagSuffix)
	if err != nil {
		return filesystem.Part{}, err
	}
	return filesystem.Part{Number: n, ETag: strings.TrimSpace(string(etag)), Size: info.Size()}, nil
}

// CompleteMultipart implements filesystem.MultipartUploader: the listed
// parts are concatenated into a staging file that is committed like a Write.
func (d *Driver) CompleteMultipart(ctx context.Context, key, uploadID string, parts []filesystem.Part) (filesystem.Entry, error) {
	meta, err := d.loadUpload(key, uploadID)
	if err != nil {
		return filesystem.Entry{}, err
	}
	readers := make([]io.Reader, 0, len(parts))
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	prev := 0
	for _, p := range parts {
		stored, err := d.part(uploadID, p.Number)
		if err != nil || p.Number <= prev || stored.ETag != p.ETag {
			return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "part %d is missing, out of order or has the wrong ETag", p.Number)
		}
		prev = p.Number
		f, err := d.root.Open(uploadDir(uploadID) + "/" + strconv.Itoa(p.Number))
		if err != nil {
			return filesystem.Entry{}, err
		}
		closers = append(closers, f)
		readers = append(readers, f)
	}
	tmp, err := d.stage(ctx, io.MultiReader(readers...), d.mode(meta.Visibility))
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer func() { _ = d.root.Remove(tmp) }()
	e, err := d.commit(ctx, tmp, key, meta.IfNotExists, "")
	if err != nil {
		return filesystem.Entry{}, err
	}
	_ = d.root.RemoveAll(uploadDir(uploadID))
	return e, nil
}

// AbortMultipart implements filesystem.MultipartUploader.
func (d *Driver) AbortMultipart(ctx context.Context, _ string, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !uploadIDPattern.MatchString(uploadID) {
		return nil
	}
	if err := d.root.RemoveAll(uploadDir(uploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SweepAbandoned implements filesystem.AbandonedSweeper. It removes
// multipart uploads (multipartDir/<id>) whose last activity, the newest
// modification time of the upload's directory and files, is before before,
// and staging files (tmpDir) last written before before: the leftovers of
// uploads nobody completed or aborted and of writes interrupted by a crash.
// Entries in the staging directory that this driver did not create (names
// not of the randomName shape, or not of the expected kind) are left alone.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := d.scanAbandoned(ctx, before, func(l leftover) error {
		var err error
		if l.Kind == filesystem.AbandonedMultipart {
			err = d.root.RemoveAll(l.path)
		} else if err = d.root.Remove(l.path); errors.Is(err, fs.ErrNotExist) {
			err = nil // committed or removed by its write meanwhile
		}
		if err == nil {
			n++
		}
		return err
	})
	return n, err
}

// FindAbandoned implements filesystem.AbandonedFinder: the leftovers
// SweepAbandoned would remove, found by the same scan, with each upload's
// key read from its metadata (empty when that is gone or damaged).
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	var found []filesystem.Abandoned
	err := d.scanAbandoned(ctx, before, func(l leftover) error {
		if l.Kind == filesystem.AbandonedMultipart {
			key, err := d.uploadKey(l.ID)
			if err != nil {
				return err
			}
			l.Key = key
		}
		found = append(found, l.Abandoned)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// leftover is one item scanAbandoned found: what FindAbandoned reports,
// and the staging path SweepAbandoned removes.
type leftover struct {
	filesystem.Abandoned
	path string
}

// scanAbandoned calls visit, in turn, for every multipart upload and then
// every staging file whose last activity is before before; an error from
// visit ends the scan. It is the one definition of "abandoned" both
// SweepAbandoned and FindAbandoned use, so a dry run reports exactly what
// a sweep would remove.
func (d *Driver) scanAbandoned(ctx context.Context, before time.Time, visit func(leftover) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uploads, err := d.readStaging(multipartDir)
	if err != nil {
		return err
	}
	for _, e := range uploads {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() || !uploadIDPattern.MatchString(e.Name()) {
			continue
		}
		last, err := d.lastActivity(uploadDir(e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // completed or aborted while the sweep ran
		}
		if err != nil {
			return err
		}
		if !last.Before(before) {
			continue
		}
		l := leftover{Abandoned: filesystem.Abandoned{Kind: filesystem.AbandonedMultipart, ID: e.Name(), LastActivity: last}, path: uploadDir(e.Name())}
		if err := visit(l); err != nil {
			return err
		}
	}
	staged, err := d.readStaging(tmpDir)
	if err != nil {
		return err
	}
	for _, e := range staged {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only names this driver generates (randomName, the shape of an
		// upload ID) are its staging files; anything else in tmp is not
		// ours to delete.
		if !e.Type().IsRegular() || !uploadIDPattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // committed or removed by its write meanwhile
		}
		if err != nil {
			return err
		}
		if !info.ModTime().Before(before) {
			continue
		}
		l := leftover{Abandoned: filesystem.Abandoned{Kind: filesystem.AbandonedStaging, ID: e.Name(), LastActivity: info.ModTime()}, path: tmpDir + "/" + e.Name()}
		if err := visit(l); err != nil {
			return err
		}
	}
	return nil
}

// uploadKey is the key upload id was started for, or "" when its metadata
// is gone (completed or aborted meanwhile) or damaged: the sweep removes
// such an upload all the same, so the preview reports it. Failing to read
// it is an error.
func (d *Driver) uploadKey(id string) (string, error) {
	b, err := d.root.ReadFile(uploadDir(id) + "/" + metaFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m uploadMeta
	if json.Unmarshal(b, &m) != nil {
		return "", nil
	}
	return m.Key, nil
}

// readStaging lists a staging directory, empty when it does not exist yet.
func (d *Driver) readStaging(dir string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(d.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// lastActivity is the newest modification time of dir and the files in it:
// creating an upload writes its metadata, and every part received writes
// the part and its ETag.
func (d *Driver) lastActivity(dir string) (time.Time, error) {
	info, err := d.root.Stat(dir)
	if err != nil {
		return time.Time{}, err
	}
	last := info.ModTime()
	entries, err := fs.ReadDir(d.root.FS(), dir)
	if err != nil {
		return time.Time{}, err
	}
	for _, e := range entries {
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last, nil
}
