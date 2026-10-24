package variants_test

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
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// mapFocal is a FocalSource kept in memory, as an app's face detector
// might be: a second implementation beside the focal plugin.
type mapFocal struct {
	mu  sync.Mutex
	pts map[string]imaging.Focal
}

func (m *mapFocal) Focal(f records.File) (imaging.Focal, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pt, ok := m.pts[f.ID]
	return pt, ok
}
func (m *mapFocal) FocalChanged() records.EventKind { return "map_focal.changed" }
func (m *mapFocal) set(id string, pt imaging.Focal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pts[id] = pt
}

// thirdsPNG is 300 × 100: red, green and blue thirds.
func thirdsPNG(t *testing.T) []byte {
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

var card = map[string]imaging.Transform{"card": {Mode: imaging.ModeFill, Width: 50, Height: 50, Format: imaging.FormatPNG, Gravity: imaging.GravityFocal}}

// centre is the colour at the middle of a variant's object.
func (e *env) centre(t *testing.T, v variants.Variant) [3]int {
	t.Helper()
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

// TestFocalSource pins IM-17 in variants: a GravityFocal variant crops
// around the source's point (centre without one), and the variant's
// digest stays the declared transform's.
func TestFocalSource(t *testing.T) {
	src := &mapFocal{pts: map[string]imaging.Focal{}}
	if _, err := variants.New(card, imaging.Options{}, variants.WithFocal(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("nil source = %v", err)
	}
	p, _ := variants.New(card, imaging.Options{}, variants.WithFocal(src))
	e := newEnvWithPlugin(t, memory.New(), p)
	f := e.image(t, "a.png", thirdsPNG(t), filesystem.VisibilityPrivate)
	g := e.image(t, "b.png", thirdsPNG(t), filesystem.VisibilityPrivate)
	src.set(f.ID, imaging.Focal{X: 0.9, Y: 0.5})
	v, err := p.Get(t.Context(), e.store, "t", f.ID, "card")
	if err != nil || e.centre(t, v) != [3]int{0, 0, 255} || v.Digest != card["card"].Digest() {
		t.Fatalf("focal variant %+v = %v, centre %v", v, err, e.centre(t, v))
	}
	if w, _ := p.Get(t.Context(), e.store, "t", g.ID, "card"); e.centre(t, w) != [3]int{0, 255, 0} {
		t.Errorf("no point: centre %v, want green", e.centre(t, w))
	}
}

// TestFocalChangeDuringGeneration pins the staleness check: a point
// changed after a variant was written but before it was recorded makes it
// generate again around the new point.
func TestFocalChangeDuringGeneration(t *testing.T) {
	src := &mapFocal{pts: map[string]imaging.Focal{}}
	p, _ := variants.New(card, imaging.Options{}, variants.WithFocal(src))
	var once sync.Once
	var fileID string
	moved := false
	hook := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		if op.Kind == filesystem.OpWrite && strings.HasPrefix(op.Key, variants.KeyDir+"/") {
			once.Do(func() { src.set(fileID, imaging.Focal{X: 0.05, Y: 0.5}); moved = true })
		}
		return err
	}
	e := newEnvWithPlugin(t, memory.New(), p, filesystem.WithInterceptors(hook))
	f := e.image(t, "a.png", thirdsPNG(t), filesystem.VisibilityPrivate)
	fileID = f.ID
	src.set(f.ID, imaging.Focal{X: 0.9, Y: 0.5})
	v, err := p.Get(t.Context(), e.store, "t", f.ID, "card")
	if err != nil || !moved {
		t.Fatalf("Get = %v (point moved: %v)", err, moved)
	}
	if got := e.centre(t, v); got != [3]int{255, 0, 0} {
		t.Errorf("centre %v: the crop around the old point was kept", got)
	}
}
