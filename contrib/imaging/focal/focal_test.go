package focal_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/focal"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// eventLog records the plugin events it hears.
type eventLog struct {
	mu     sync.Mutex
	events []records.Event
}

func (l *eventLog) Name() string { return "event_log" }
func (l *eventLog) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Kind == focal.EventChanged {
		l.events = append(l.events, e)
	}
	return nil
}

type env struct {
	disk  *filesystem.Disk
	store *records.Store
	fp    *focal.Plugin
	vp    *variants.Plugin
	log   *eventLog
}

var card = map[string]imaging.Transform{"card": {Mode: imaging.ModeFill, Width: 50, Height: 50, Format: imaging.FormatPNG, Gravity: imaging.GravityFocal}}

func newEnv(t *testing.T) (*env, func(string) error) {
	t.Helper()
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	fp := focal.New()
	vp, err := variants.New(card, imaging.Options{}, variants.WithFocal(fp), variants.WithEager(nil, "card"))
	if err != nil {
		t.Fatal(err)
	}
	l := &eventLog{}
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(fp, vp, l))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) error { _, err := db.Exec(q); return err }
	return &env{disk: disk, store: s, fp: fp, vp: vp, log: l}, exec
}

// thirds is 300 × 100: red, green and blue thirds.
func thirds(t *testing.T) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	for y := range 100 {
		for x := range 300 {
			img.SetNRGBA(x, y, []color.NRGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}}[x/100])
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func (e *env) record(t *testing.T, meta map[string]string) records.File {
	t.Helper()
	key := "img/" + strings.ReplaceAll(t.Name(), "/", "_") + ".png"
	if _, err := e.disk.WriteBytes(t.Context(), key, thirds(t), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: key, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// centre is the colour at the middle of the file's card variant.
func (e *env) centre(t *testing.T, id string) [3]int {
	t.Helper()
	v, err := e.vp.Get(t.Context(), e.store, "t", id, "card")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := e.disk.Read(t.Context(), v.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	img, err := png.Decode(rc)
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(25, 25).RGBA()
	return [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
}

var (
	red, green, blue = [3]int{255, 0, 0}, [3]int{0, 255, 0}, [3]int{0, 0, 255}
)

// TestSetMovesTheCrop pins RC-16 with the variants plugin: setting a point
// stores it, emits focal.changed, and the variants are made again around
// it; clearing it goes back to the centre.
func TestSetMovesTheCrop(t *testing.T) {
	e, _ := newEnv(t)
	f := e.record(t, map[string]string{"alt": "kept"})
	if got := e.centre(t, f.ID); got != green {
		t.Fatalf("no point: %v, want the centre", got)
	}
	g, err := e.fp.Set(t.Context(), e.store, "t", f.ID, &imaging.Focal{X: 0.9, Y: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if pt, ok := focal.Of(g); !ok || pt.X != 0.9 || g.Metadata["alt"] != "kept" {
		t.Fatalf("after Set: %v %v %v", pt, ok, g.Metadata)
	}
	if got := e.centre(t, f.ID); got != blue {
		t.Errorf("after Set: %v, want the crop around the point", got)
	}
	if len(e.log.events) != 1 || e.log.events[0].File == nil || e.log.events[0].Detail[focal.DetailX] != "0.9" {
		t.Errorf("events = %+v", e.log.events)
	}
	// The same point again changes nothing and emits nothing.
	if _, err := e.fp.Set(t.Context(), e.store, "t", f.ID, &imaging.Focal{X: 0.9, Y: 0.5}); err != nil || len(e.log.events) != 1 {
		t.Errorf("same point: %v, events %d", err, len(e.log.events))
	}
	g, err = e.fp.Set(t.Context(), e.store, "t", f.ID, nil)
	if _, ok := focal.Of(g); err != nil || ok || g.Metadata["alt"] != "kept" {
		t.Errorf("clear = %v, %v", g.Metadata, err)
	}
	if got := e.centre(t, f.ID); got != green {
		t.Errorf("after clearing: %v, want the centre", got)
	}
	// Clearing a file that has no point, and no other metadata, is a no-op.
	bare := e.record(t, nil)
	if _, err := e.fp.Set(t.Context(), e.store, "t", bare.ID, nil); err != nil || len(e.log.events) != 2 {
		t.Errorf("clear without a point: %v, events %d", err, len(e.log.events))
	}
}

// TestPointAtUpload pins a point set through NewFile.Metadata.
func TestPointAtUpload(t *testing.T) {
	e, _ := newEnv(t)
	f := e.record(t, map[string]string{focal.MetaX: "0.05", focal.MetaY: "0.5"})
	if got := e.centre(t, f.ID); got != red {
		t.Errorf("point at upload: %v", got)
	}
	for _, m := range []map[string]string{nil, {focal.MetaX: "x", focal.MetaY: "0.5"}, {focal.MetaX: "1.5", focal.MetaY: "0.5"}} {
		if _, ok := focal.Of(records.File{Metadata: m}); ok {
			t.Errorf("Of(%v) found a point", m)
		}
	}
	// Clearing the only metadata leaves none.
	if g, err := e.fp.Set(t.Context(), e.store, "t", f.ID, nil); err != nil || g.Metadata != nil {
		t.Errorf("cleared = %v, %v", g.Metadata, err)
	}
	if e.fp.Name() != focal.PluginName || e.fp.FocalChanged() != focal.EventChanged {
		t.Error("names")
	}
}

func TestSetRefusals(t *testing.T) {
	e, exec := newEnv(t)
	f := e.record(t, nil)
	if _, err := e.fp.Set(t.Context(), e.store, "t", f.ID, &imaging.Focal{X: 2}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("out of range = %v", err)
	}
	if _, err := e.fp.Set(t.Context(), e.store, "t", "fil_missing", &imaging.Focal{}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("missing = %v", err)
	}
	if err := e.store.Trash(t.Context(), "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fp.Set(t.Context(), e.store, "t", f.ID, &imaging.Focal{}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("trashed = %v", err)
	}
	recordstest.OnlySQLite(t) // injects failures with SQLite triggers
	g := e.record(t, nil)
	if err := exec("CREATE TRIGGER no_meta BEFORE UPDATE OF metadata ON " + records.TableFiles.Name + " BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fp.Set(t.Context(), e.store, "t", g.ID, &imaging.Focal{X: 0.5, Y: 0.5}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("metadata write failure = %v", err)
	}
	if err := exec("CREATE TRIGGER no_lock BEFORE UPDATE OF storage_key ON " + records.TableFiles.Name + " BEGIN SELECT RAISE(FAIL, 'locked'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fp.Set(t.Context(), e.store, "t", g.ID, &imaging.Focal{X: 0.5, Y: 0.5}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("lock failure = %v", err)
	}
}
