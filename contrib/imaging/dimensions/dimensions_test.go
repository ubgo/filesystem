package dimensions_test

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

type env struct {
	disk  *filesystem.Disk
	store *records.Store
	p     *dimensions.Plugin
}

func newEnv(t *testing.T, opts ...dimensions.Option) *env {
	t.Helper()
	disk, _ := filesystem.NewDisk("media", memory.New())
	return newEnvOn(t, disk, opts...)
}

// newEnvOn is newEnv on a disk the test built.
func newEnvOn(t *testing.T, disk *filesystem.Disk, opts ...dimensions.Option) *env {
	t.Helper()
	db, _ := recordstest.Open(t)
	e := &env{p: dimensions.New(opts...), disk: disk}
	mgr, _ := filesystem.NewManager(e.disk)
	var err error
	if e.store, err = records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(e.p)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *env) record(t *testing.T, key string, data []byte, in records.NewFile) (records.File, error) {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), key, data, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	in.Scope, in.Disk, in.Key = "t", "media", key
	return e.store.CreateFile(t.Context(), in)
}

func dims(f records.File) [3]int {
	w, h, ok := dimensions.Of(f)
	okInt := 0
	if ok {
		okInt = 1
	}
	return [3]int{w, h, okInt}
}

func TestCreate(t *testing.T) {
	e := newEnv(t)
	caller := map[string]string{"alt": "a cat"}
	f, err := e.record(t, "a.png", pngOf(t, 80, 30), records.NewFile{Metadata: caller})
	if err != nil {
		t.Fatal(err)
	}
	if dims(f) != [3]int{80, 30, 1} || f.Metadata["alt"] != "a cat" {
		t.Fatalf("recorded %+v", f)
	}
	if len(caller) != 1 {
		t.Fatalf("the caller's metadata map was changed: %v", caller)
	}
	stored, _ := e.store.File(t.Context(), "t", f.ID)
	if dims(stored) != [3]int{80, 30, 1} {
		t.Fatalf("stored %+v", stored.Metadata)
	}

	for name, c := range map[string]struct {
		data []byte
		ct   string
	}{
		"text":          {[]byte("hello"), ""},
		"svg":           {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "image/svg+xml"},
		"corrupt image": {[]byte("\x89PNG\r\n\x1a\nbroken"), "image/png"},
	} {
		f, err := e.record(t, name, c.data, records.NewFile{ContentType: c.ct})
		if err != nil || dims(f) != [3]int{0, 0, 0} || f.Metadata != nil {
			t.Errorf("%s: %+v, %v", name, f.Metadata, err)
		}
	}
}

func TestStrict(t *testing.T) {
	e := newEnv(t, dimensions.WithStrict())
	_, err := e.record(t, "bad.png", []byte("\x89PNG\r\n\x1a\nbroken"), records.NewFile{ContentType: "image/png"})
	if filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a corrupt image in strict mode = %v", err)
	}
	if _, err := e.record(t, "ok.txt", []byte("hello"), records.NewFile{}); err != nil {
		t.Fatalf("a non-image in strict mode: %v", err)
	}
}

func TestReplace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f, err := e.record(t, "a.png", pngOf(t, 80, 80), records.NewFile{})
	if err != nil {
		t.Fatal(err)
	}
	anything := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}
	g, err := e.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngOf(t, 100, 20)), "", anything)
	if err != nil || dims(g) != [3]int{100, 20, 1} {
		t.Fatalf("replaced with an image = %+v, %v", g.Metadata, err)
	}
	g, err = e.store.Replace(ctx, "t", f.ID, strings.NewReader("now text"), "", anything)
	if err != nil || dims(g) != [3]int{0, 0, 0} {
		t.Fatalf("replaced with text = %+v, %v", g.Metadata, err)
	}
}

// TestHookContract calls the hooks as any Store might, with a file whose
// disk or object is gone.
func TestHookContract(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	if e.p.Name() != dimensions.PluginName {
		t.Fatal("name")
	}
	unknownDisk := &records.File{Disk: "nowhere", Key: "a.png", ContentType: "image/png"}
	if err := e.p.BeforeCreate(ctx, e.store, unknownDisk); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("unknown disk = %v", err)
	}
	missing := &records.File{Disk: "media", Key: "gone.png", ContentType: "image/png"}
	if err := e.p.BeforeReplace(ctx, e.store, records.File{}, missing); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("missing object = %v", err)
	}
	if e.p.AfterCreate(ctx, e.store, records.File{}) != nil || e.p.BeforePurge(ctx, e.store, records.File{}) != nil {
		t.Error("no-op hooks failed")
	}
	if _, _, ok := dimensions.Of(records.File{Metadata: map[string]string{dimensions.MetaWidth: "x", dimensions.MetaHeight: "1"}}); ok {
		t.Error("a malformed width was accepted")
	}
}
