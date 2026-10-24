// Package memory is an in-process filesystem.Driver that keeps objects in a
// map. It supports every capability that needs no network: copy, move,
// visibility, bulk prefix delete, bulk key delete, stored metadata, content
// type and HTTP headers, atomic conditional writes, server-side multipart
// uploads, sweeping (and previewing the sweep of) multipart uploads nobody
// finished, and directories that exist while empty (MakeDir).
//
// Every method honours its context: a context that has ended fails the
// call with the context's error (which the Disk classifies as
// CodeUnavailable), and a write or part upload stops reading its body when
// the context ends mid-stream.
//
// Use it in tests (filesystemtest.Fake builds on it), for caches that need
// no persistence, and as the reference implementation the conformance suite
// is written against. Nothing survives the process.
package memory

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// Driver is the memory backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	mu      sync.RWMutex
	objects map[string]*object
	uploads map[string]*upload
	// dirs are the directories MakeDir created, which exist even when
	// empty (see dirs.go).
	dirs   map[string]struct{}
	now    func() time.Time
	nextID uint64
	// versioned keeps every version of every key (WithVersions); history
	// holds them, oldest first, and is nil otherwise.
	versioned bool
	history   map[string][]*version
}

type object struct {
	data        []byte
	modTime     time.Time
	contentType string
	etag        string
	visibility  filesystem.Visibility
	metadata    map[string]string
	headers     filesystem.HTTPHeaders
	tags        map[string]string
	class       string // storage class; "" until SetStorageClass
}

// Option configures a Driver.
type Option func(*Driver)

// WithClock replaces time.Now for modification times, so tests can assert
// exact timestamps.
func WithClock(now func() time.Time) Option {
	return func(d *Driver) { d.now = now }
}

// WithVersions keeps every version of every key, like a versioned bucket:
// each write adds a version, each delete a delete marker, and the driver
// declares the Versions and VersionRead capabilities. Memory grows with
// every write, so use it for tests and short-lived processes.
func WithVersions() Option {
	return func(d *Driver) {
		d.versioned = true
		d.history = make(map[string][]*version)
	}
}

// New returns an empty memory driver.
func New(opts ...Option) *Driver {
	d := &Driver{
		objects: make(map[string]*object),
		uploads: make(map[string]*upload),
		dirs:    make(map[string]struct{}),
		now:     time.Now,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Capabilities reports everything the memory driver does natively.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Copy:             true,
		Move:             true,
		SetVisibility:    true,
		DeletePrefix:     true,
		DeleteKeys:       true,
		Multipart:        true,
		Append:           true,
		SetModTime:       true,
		Tags:             true,
		CompareAndSwap:   true,
		SweepAbandoned:   true,
		FindAbandoned:    true,
		Dirs:             true,
		Metadata:         true,
		Headers:          true,
		ContentType:      true,
		ConditionalWrite: true,
		StorageClass:     true,
		Versions:         d.versioned,
		VersionRead:      d.versioned,
	}
}

func (o *object) entry(key string) filesystem.Entry {
	return filesystem.Entry{
		Key:          key,
		Size:         int64(len(o.data)),
		ModTime:      o.modTime,
		ContentType:  o.contentType,
		ETag:         o.etag,
		Visibility:   o.visibility,
		Metadata:     maps.Clone(o.metadata),
		Headers:      o.headers,
		StorageClass: o.class,
	}
}

func notFound(key string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "no object %q", key)
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := ctx.Err(); err != nil {
		return filesystem.Entry{}, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	o, ok := d.objects[key]
	if !ok {
		return filesystem.Entry{}, notFound(key)
	}
	return o.entry(key), nil
}

// Read implements filesystem.Driver. The returned reader reads a snapshot:
// a concurrent overwrite does not change bytes already being read.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	o, ok := d.objects[key]
	d.mu.RUnlock()
	if !ok {
		return nil, notFound(key)
	}
	return io.NopCloser(bytes.NewReader(slice(o.data, rng))), nil
}

// slice applies a ByteRange to data, clamping to its bounds.
func slice(data []byte, rng filesystem.ByteRange) []byte {
	size := int64(len(data))
	start := min(rng.Offset, size)
	end := size
	if rng.Length > 0 {
		end = min(start+rng.Length, size)
	}
	return data[start:end]
}

// Write implements filesystem.Driver. The body is read completely before the
// object is stored, so a failing body, or a context that ends while it is
// read, stores nothing.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	data, err := io.ReadAll(&ctxReader{ctx: ctx, r: body})
	if err != nil {
		return filesystem.Entry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, exists := d.objects[key]
	if exists && opts.IfNotExists {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeExists, "object %q exists", key)
	}
	if opts.IfMatch != "" && (!exists || cur.etag != opts.IfMatch) {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeConflict, "object %q changed since ETag %s", key, opts.IfMatch)
	}
	o := d.newObject(data, opts.ContentType, opts.Visibility, opts.Metadata, opts.Headers)
	d.put(key, o)
	return o.entry(key), nil
}

// ctxReader fails with the context's error once the context ends, so a
// long body stops being read when the caller gives up. Its last Read (the
// one that reports io.EOF) checks too, so a body read in full under an
// ended context is still refused.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// newObject builds an object; callers hold the lock (for the clock only).
func (d *Driver) newObject(data []byte, contentType string, v filesystem.Visibility, md map[string]string, h filesystem.HTTPHeaders) *object {
	sum := md5.Sum(data)
	return &object{
		data:        data,
		modTime:     d.now(),
		contentType: contentType,
		etag:        hex.EncodeToString(sum[:]),
		visibility:  v,
		metadata:    maps.Clone(md),
		headers:     h,
	}
}

// Delete implements filesystem.Driver.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.remove(key)
	return nil
}

// DeleteKeys implements filesystem.KeysDeleter in one locked pass.
func (d *Driver) DeleteKeys(ctx context.Context, keys []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range keys {
		d.remove(k)
	}
	return nil
}

// List implements filesystem.Driver.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	if err := ctx.Err(); err != nil {
		return filesystem.Page{}, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var all []filesystem.Entry
	for k, o := range d.objects {
		if filesystem.KeyInDir(k, dir) {
			all = append(all, o.entry(k))
		}
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// Copy implements filesystem.Copier.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if err := ctx.Err(); err != nil {
		return filesystem.Entry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.objects[src]
	if !ok {
		return filesystem.Entry{}, notFound(src)
	}
	cp := d.newObject(o.data, o.contentType, o.visibility, o.metadata, o.headers)
	d.put(dst, cp)
	return cp.entry(dst), nil
}

// Move implements filesystem.Mover atomically.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if err := ctx.Err(); err != nil {
		return filesystem.Entry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.objects[src]
	if !ok {
		return filesystem.Entry{}, notFound(src)
	}
	d.put(dst, o)
	d.remove(src)
	return o.entry(dst), nil
}

// SetVisibility implements filesystem.VisibilitySetter.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	return d.update(ctx, key, func(o *object) { o.visibility = v })
}

// DeletePrefix implements filesystem.PrefixDeleter.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.objects {
		if filesystem.KeyInDir(k, dir) {
			d.remove(k)
		}
	}
	d.removeDirs(dir)
	return nil
}
