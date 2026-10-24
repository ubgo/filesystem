package variants_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

var immutable = filesystem.HTTPHeaders{CacheControl: "public, max-age=31536000, immutable"}

// newEnvWith is newEnv with plugin options.
func newEnvWith(t *testing.T, drv filesystem.Driver, options ...variants.Option) *env {
	t.Helper()
	p, err := variants.New(transforms, imaging.Options{}, options...)
	if err != nil {
		t.Fatal(err)
	}
	return newEnvWithPlugin(t, drv, p)
}

// newEnvWithPlugin is an env around a plugin the test built, on a disk
// with the given options.
func newEnvWithPlugin(t *testing.T, drv filesystem.Driver, p *variants.Plugin, opts ...filesystem.Option) *env {
	t.Helper()
	db, _ := recordstest.Open(t)
	disk, err := filesystem.NewDisk("media", drv, opts...)
	if err != nil {
		t.Fatal(err)
	}
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, disk: disk, store: s, plugin: p}
}

// TestHeadersAreStoredAndNeverInherited pins that WithHeaders reaches the
// stored variant, that a variant without them gets none, and that the
// original's own headers (an attachment disposition above all) are never
// copied to its variants.
func TestHeadersAreStoredAndNeverInherited(t *testing.T) {
	ctx := t.Context()
	e := newEnvWith(t, memory.New(), variants.WithHeaders(immutable, "thumb"))
	original := filesystem.HTTPHeaders{CacheControl: "no-store", ContentDisposition: `attachment; filename="a.png"`}
	if _, err := e.disk.WriteBytes(ctx, "a.png", pngBytes(t, 40, 40), filesystem.WriteOptions{Headers: original}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := e.disk.Stat(ctx, thumb.Key); st.Headers != immutable {
		t.Errorf("thumb headers = %+v, want %+v", st.Headers, immutable)
	}
	square, err := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := e.disk.Stat(ctx, square.Key); !st.Headers.IsZero() {
		t.Errorf("square inherited headers %+v", st.Headers)
	}
}

// TestHeadersForEveryVariant pins that WithHeaders without names covers
// every declared variant.
func TestHeadersForEveryVariant(t *testing.T) {
	ctx := t.Context()
	e := newEnvWith(t, memory.New(), variants.WithHeaders(immutable))
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	for name := range transforms {
		v, err := e.plugin.Get(ctx, e.store, "t", f.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := e.disk.Stat(ctx, v.Key); st.Headers != immutable {
			t.Errorf("%s headers = %+v", name, st.Headers)
		}
	}
}

// noHeaders is a memory driver without the Headers capability.
type noHeaders struct{ *memory.Driver }

func (d noHeaders) Capabilities() filesystem.Capabilities {
	c := d.Driver.Capabilities()
	c.Headers = false
	return c
}

func TestHeadersOnADiskWithoutThem(t *testing.T) {
	ctx := t.Context()
	e := newEnvWith(t, noHeaders{memory.New()}, variants.WithHeaders(immutable, "thumb"))
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	_, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeUnsupported)
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "square"); err != nil {
		t.Errorf("a variant without headers failed: %v", err)
	}
}

func TestWithHeadersRefusals(t *testing.T) {
	for _, opt := range []variants.Option{
		variants.WithHeaders(filesystem.HTTPHeaders{CacheControl: "public\r\nX-Evil: 1"}),
		variants.WithHeaders(immutable, "undeclared"),
	} {
		_, err := variants.New(transforms, imaging.Options{}, opt)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

// TestVariantKeysAreNeverReused pins that every generation writes a key
// never written before: after a replacement, after a visibility round trip
// (same content, same object key), and after ReplaceObject returns the
// file to an object key it had before (A, B, then A again with new bytes),
// so a cached copy of one key never shows another image.
func TestVariantKeysAreNeverReused(t *testing.T) {
	ctx := t.Context()
	e := newEnvWith(t, memory.New(), variants.WithHeaders(immutable))
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	seen := map[string]bool{}
	next := func(when string) variants.Variant {
		t.Helper()
		v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
		if err != nil {
			t.Fatal(err)
		}
		if seen[v.Key] {
			t.Errorf("%s: key %s reused", when, v.Key)
		}
		seen[v.Key] = true
		return v
	}
	next("first")
	if _, err := e.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 60, 60)), "", anything); err != nil {
		t.Fatal(err)
	}
	next("after replace")
	_ = e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic)
	_ = e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate)
	next("after a visibility round trip")

	// A, B, then A again holding different bytes.
	write := func(key string, w int) {
		t.Helper()
		if _, err := e.disk.WriteBytes(ctx, key, pngBytes(t, w, 50), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	write("objA.png", 100)
	if _, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "objA.png"}); err != nil {
		t.Fatal(err)
	}
	first := next("on A")
	write("objB.png", 100)
	if _, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "objB.png"}); err != nil {
		t.Fatal(err)
	}
	next("on B")
	write("objA.png", 50)
	if _, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "objA.png"}); err != nil {
		t.Fatal(err)
	}
	again := next("on A again")
	if again.Height == first.Height {
		t.Errorf("A again rendered %dx%d, the old content's size", again.Width, again.Height)
	}
}
