package memory

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strconv"

	"github.com/ubgo/filesystem"
)

// version is one stored version of a key: content, or a delete marker
// (obj nil).
type version struct {
	id  string
	obj *object
}

// put stores o as key's current object, adding a version when the driver
// keeps them. Callers hold the write lock.
func (d *Driver) put(key string, o *object) {
	d.objects[key] = o
	if d.versioned {
		d.nextID++
		d.history[key] = append(d.history[key], &version{id: strconv.FormatUint(d.nextID, 10), obj: o})
	}
}

// remove deletes key's current object, adding a delete marker when the
// driver keeps versions and the key existed. Callers hold the write lock.
func (d *Driver) remove(key string) {
	if _, ok := d.objects[key]; !ok {
		return
	}
	delete(d.objects, key)
	if d.versioned {
		d.nextID++
		d.history[key] = append(d.history[key], &version{id: strconv.FormatUint(d.nextID, 10)})
	}
}

// Versions implements filesystem.VersionLister, newest first. A key with no
// versions is CodeNotFound.
func (d *Driver) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	h := d.history[key]
	if len(h) == 0 {
		return nil, notFound(key)
	}
	out := make([]filesystem.Version, 0, len(h))
	for i, v := range slices.Backward(h) {
		fv := filesystem.Version{ID: v.id, Latest: i == len(h)-1, DeleteMarker: v.obj == nil}
		if v.obj != nil {
			fv.Size, fv.ModTime, fv.ETag = int64(len(v.obj.data)), v.obj.modTime, v.obj.etag
		}
		out = append(out, fv)
	}
	return out, nil
}

// find returns key's version id with content, or CodeNotFound. Callers
// hold the lock.
func (d *Driver) find(key, id string) (*object, error) {
	for _, v := range d.history[key] {
		if v.id == id && v.obj != nil {
			return v.obj, nil
		}
	}
	return nil, filesystem.Errorf(filesystem.CodeNotFound, "no version %q of %q", id, key)
}

// ReadVersion implements filesystem.VersionReader.
func (d *Driver) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	o, err := d.find(key, id)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

// RestoreVersion implements filesystem.VersionReader: the version's
// content becomes a new latest version.
func (d *Driver) RestoreVersion(ctx context.Context, key, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	o, err := d.find(key, id)
	if err != nil {
		return err
	}
	cp := d.newObject(o.data, o.contentType, o.visibility, o.metadata, o.headers)
	cp.tags, cp.class = o.tags, o.class
	d.put(key, cp)
	return nil
}

// SetStorageClass implements filesystem.StorageClasser. The memory driver
// accepts any class name and stores it; it changes nothing else.
func (d *Driver) SetStorageClass(ctx context.Context, key, class string) error {
	return d.update(ctx, key, func(o *object) { o.class = class })
}
