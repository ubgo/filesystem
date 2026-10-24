package memory

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
)

// upload is one multipart upload in progress.
type upload struct {
	key   string
	opts  filesystem.WriteOptions
	parts map[int][]byte
	// active is when the upload was created or last received a part, by
	// the driver's clock; SweepAbandoned compares it with its cutoff.
	active time.Time
}

func noUpload(id string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "no multipart upload %q", id)
}

// CreateMultipart implements filesystem.MultipartUploader.
func (d *Driver) CreateMultipart(ctx context.Context, key string, opts filesystem.WriteOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	id := strconv.FormatUint(d.nextID, 10)
	opts.Metadata = maps.Clone(opts.Metadata)
	d.uploads[id] = &upload{key: key, opts: opts, parts: make(map[int][]byte), active: d.now()}
	return id, nil
}

// UploadPart implements filesystem.MultipartUploader. size is advisory; the
// part is whatever body yields.
func (d *Driver) UploadPart(ctx context.Context, key, uploadID string, number int, body io.Reader, _ int64) (filesystem.Part, error) {
	data, err := io.ReadAll(&ctxReader{ctx: ctx, r: body})
	if err != nil {
		return filesystem.Part{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.uploads[uploadID]
	if !ok || u.key != key {
		return filesystem.Part{}, noUpload(uploadID)
	}
	u.parts[number] = data
	u.active = d.now()
	return partOf(number, data), nil
}

func partOf(number int, data []byte) filesystem.Part {
	sum := md5.Sum(data)
	return filesystem.Part{Number: number, ETag: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

// ListParts implements filesystem.MultipartUploader.
func (d *Driver) ListParts(ctx context.Context, key, uploadID string) ([]filesystem.Part, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	u, ok := d.uploads[uploadID]
	if !ok || u.key != key {
		return nil, noUpload(uploadID)
	}
	numbers := slices.Sorted(maps.Keys(u.parts))
	parts := make([]filesystem.Part, 0, len(numbers))
	for _, n := range numbers {
		parts = append(parts, partOf(n, u.parts[n]))
	}
	return parts, nil
}

// CompleteMultipart implements filesystem.MultipartUploader. Parts must be
// listed in ascending order, each with the ETag UploadPart returned, as on
// S3; anything else is CodeInvalidArgument and the upload stays open.
func (d *Driver) CompleteMultipart(ctx context.Context, key, uploadID string, parts []filesystem.Part) (filesystem.Entry, error) {
	if err := ctx.Err(); err != nil {
		return filesystem.Entry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.uploads[uploadID]
	if !ok || u.key != key {
		return filesystem.Entry{}, noUpload(uploadID)
	}
	var buf bytes.Buffer
	prev := 0
	for _, p := range parts {
		data, ok := u.parts[p.Number]
		if !ok || p.Number <= prev || partOf(p.Number, data).ETag != p.ETag {
			return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "part %d is missing, out of order or has the wrong ETag", p.Number)
		}
		prev = p.Number
		buf.Write(data)
	}
	if _, exists := d.objects[key]; exists && u.opts.IfNotExists {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeExists, "object %q exists", key)
	}
	o := d.newObject(buf.Bytes(), u.opts.ContentType, u.opts.Visibility, u.opts.Metadata, u.opts.Headers)
	d.put(key, o)
	delete(d.uploads, uploadID)
	return o.entry(key), nil
}

// AbortMultipart implements filesystem.MultipartUploader.
func (d *Driver) AbortMultipart(ctx context.Context, _ string, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.uploads, uploadID)
	return nil
}

// SweepAbandoned implements filesystem.AbandonedSweeper: it drops every
// multipart upload whose last activity (creation or a part received) is
// before before. The memory driver has no staging files.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	found := d.abandoned(before)
	for _, a := range found {
		delete(d.uploads, a.ID)
	}
	return len(found), nil
}

// FindAbandoned implements filesystem.AbandonedFinder: the uploads
// SweepAbandoned would drop, left in place.
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.abandoned(before), nil
}

// abandoned is the one definition of an abandoned upload both
// SweepAbandoned and FindAbandoned use: last active before before.
// Callers hold the lock.
func (d *Driver) abandoned(before time.Time) []filesystem.Abandoned {
	var found []filesystem.Abandoned
	for id, u := range d.uploads {
		if u.active.Before(before) {
			found = append(found, filesystem.Abandoned{Kind: filesystem.AbandonedMultipart, ID: id, Key: u.key, LastActivity: u.active})
		}
	}
	return found
}
