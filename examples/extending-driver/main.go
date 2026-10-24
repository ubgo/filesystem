// Command extending-driver is a complete storage driver for a made-up
// key-value service, the shape every real driver has: it adapts a vendor
// API (Backend) to the six methods of filesystem.Driver, maps the vendor's
// errors onto filesystem codes, declares exactly the capabilities it
// delivers, opens from a DSN, and offers its native client through
// filesystem.As. main_test.go runs the conformance suite against it.
//
//	go run ./extending-driver
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// Backend is the API of the service the driver adapts. In a real driver it
// is the vendor's SDK client; here it is an interface so the example can
// supply an in-memory implementation.
// ds:def id=backend-4xk4syeq
type Backend interface {
	// Get returns the object, or ErrNoSuchKey.
	Get(ctx context.Context, key string) (Object, error)
	// Put stores the object. With ifAbsent it fails with ErrConflict when
	// the key exists, atomically.
	Put(ctx context.Context, key string, o Object, ifAbsent bool) error
	// Delete removes the key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Keys lists the keys starting with prefix, in any order.
	Keys(ctx context.Context, prefix string) ([]string, error)
}

// Object is what the service stores per key.
type Object struct {
	Data        []byte
	ContentType string
	Metadata    map[string]string
	Modified    time.Time
}

// The service's errors, as its SDK would export them.
var (
	ErrNoSuchKey = errors.New("kv: no such key")
	ErrConflict  = errors.New("kv: key exists")
	ErrThrottled = errors.New("kv: slow down")
	ErrForbidden = errors.New("kv: access denied")
)

// Driver adapts a Backend to filesystem.Driver. It holds no state of its
// own, so it is safe for concurrent use whenever the Backend is.
// ds:def id=driver-kmz69jg5
type Driver struct {
	backend Backend
}

// New returns a driver over b.
func New(b Backend) *Driver { return &Driver{backend: b} }

// Capabilities declares what this driver really does: it stores content
// types and metadata and honours IfNotExists atomically. It does not
// declare Copy, Move or Presign, so the Disk falls back to streaming for
// Copy and Move, and refuses SignedURL unless the disk has a URLSigner.
// Each declared bit is checked by the conformance suite.
// ds:def id=driver-capabilities-t392pkc6
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		ContentType:      true,
		Metadata:         true,
		ConditionalWrite: true,
	}
}

// mapErr turns the service's errors into filesystem codes, keeping the
// original as the cause so errors.As can still reach it. An error it does
// not recognise becomes CodeUnknown, never a new code.
// ds:def id=maperr-zra7zy73
func mapErr(err error) error {
	code := filesystem.CodeUnknown
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoSuchKey):
		code = filesystem.CodeNotFound
	case errors.Is(err, ErrConflict):
		code = filesystem.CodeExists
	case errors.Is(err, ErrThrottled):
		code = filesystem.CodeUnavailable // the one code layer.Retry retries
	case errors.Is(err, ErrForbidden):
		code = filesystem.CodePermission
	}
	return filesystem.NewError(code, err)
}

// entry describes a stored object. Size and ModTime are required; the
// content type and metadata come back because the driver declared them.
// ds:def id=entry-pq75fsxj
func entry(key string, o Object) filesystem.Entry {
	return filesystem.Entry{
		Key:         key,
		Size:        int64(len(o.Data)),
		ModTime:     o.Modified,
		ContentType: o.ContentType,
		Metadata:    maps.Clone(o.Metadata),
	}
}

// Stat implements filesystem.Driver. A missing key must be CodeNotFound.
// ds:def id=driver-stat-aqyx7ujn
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	o, err := d.backend.Get(ctx, key)
	if err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return entry(key, o), nil
}

// Read implements filesystem.Driver, applying the byte range: Length 0
// means to the end, and a range past the end is an empty read.
// ds:def id=driver-read-9fmufg7s
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	o, err := d.backend.Get(ctx, key)
	if err != nil {
		return nil, mapErr(err)
	}
	size := int64(len(o.Data))
	start, end := min(rng.Offset, size), size
	if rng.Length > 0 {
		end = min(start+rng.Length, size)
	}
	return io.NopCloser(bytes.NewReader(o.Data[start:end])), nil
}

// Write implements filesystem.Driver. It reads the body to the end before
// storing anything, so a failing body (including the Disk's MaxSize cap)
// stores nothing and its error is returned. It never closes the body.
// Keys arrive cleaned by the Disk; options arrive validated against the
// declared capabilities.
// ds:def id=driver-write-gnbg8y2n
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return filesystem.Entry{}, err
	}
	o := Object{Data: data, ContentType: opts.ContentType, Metadata: maps.Clone(opts.Metadata), Modified: time.Now().UTC()}
	if err := d.backend.Put(ctx, key, o, opts.IfNotExists); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return entry(key, o), nil
}

// Delete implements filesystem.Driver; deleting a missing key succeeds.
// ds:def id=driver-delete-scc574x4
func (d *Driver) Delete(ctx context.Context, key string) error {
	return mapErr(d.backend.Delete(ctx, key))
}

// List implements filesystem.Driver. The service lists keys by prefix, so
// the driver gathers the objects under dir and lets filesystem.PageOf do
// the sorting, directory folding, cursor and limit. A driver for a backend
// with its own paging (S3's continuation tokens) pages natively instead.
// ds:def id=driver-list-rckcrhms
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	keys, err := d.backend.Keys(ctx, prefix)
	if err != nil {
		return filesystem.Page{}, mapErr(err)
	}
	var objects []filesystem.Entry
	for _, k := range keys {
		o, err := d.backend.Get(ctx, k)
		if errors.Is(err, ErrNoSuchKey) {
			continue // deleted since Keys listed it
		}
		if err != nil {
			return filesystem.Page{}, mapErr(err)
		}
		objects = append(objects, entry(k, o))
	}
	return filesystem.PageOf(objects, dir, opts), nil
}

// Native returns the backend client, for the typed escape hatch:
// filesystem.As[Backend](disk).
// ds:def id=driver-native-p84djt65
func (d *Driver) Native() Backend { return d.backend }

// Scheme is the DSN scheme the driver registers under.
const Scheme = "kv"

// Opener opens "kv://name" DSNs on a fresh in-memory backend. It refuses
// every query parameter it does not know, so a typo fails at start-up.
// ds:def id=opener-yetbbr5h
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u); err != nil {
		return nil, err
	}
	return New(NewMapBackend()), nil
}

// mapBackend is an in-memory Backend.
type mapBackend struct {
	mu      sync.RWMutex
	objects map[string]Object
}

// NewMapBackend returns an empty in-memory Backend.
func NewMapBackend() Backend { return &mapBackend{objects: map[string]Object{}} }

// Get implements Backend.
func (m *mapBackend) Get(_ context.Context, key string) (Object, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return Object{}, ErrNoSuchKey
	}
	return o, nil
}

// Put implements Backend.
func (m *mapBackend) Put(_ context.Context, key string, o Object, ifAbsent bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok && ifAbsent {
		return ErrConflict
	}
	m.objects[key] = o
	return nil
}

// Delete implements Backend.
func (m *mapBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Keys implements Backend.
func (m *mapBackend) Keys(_ context.Context, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run opens the driver from a DSN and uses it through the Disk, which adds
// key cleaning, content-type detection, hashing and the portable copy.
func run(ctx context.Context, out io.Writer) error {
	mux := filesystem.NewMux()
	if err := mux.Register(Scheme, Opener); err != nil {
		return err
	}
	d, err := mux.Open(ctx, "kv", "kv://demo")
	if err != nil {
		return err
	}
	e, err := d.WriteBytes(ctx, "/docs//readme.txt", []byte("hello from a custom driver\n"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (%d bytes, %s, sha256 %s…)\n", e.Key, e.Size, e.ContentType, e.SHA256[:12])
	if _, err := d.Copy(ctx, "docs/readme.txt", "docs/copy.txt"); err != nil {
		return err
	}
	var keys []string
	for e, err := range d.List(ctx, "docs", filesystem.ListOptions{}) {
		if err != nil {
			return err
		}
		keys = append(keys, e.Key)
	}
	fmt.Fprintf(out, "listed %v\ncapabilities %s\n", keys, d.Capabilities())
	if _, ok := filesystem.As[Backend](d); ok {
		fmt.Fprintln(out, "native client reachable through filesystem.As")
	}
	return nil
}
