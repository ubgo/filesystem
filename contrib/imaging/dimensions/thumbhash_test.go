package dimensions_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// colourPNG is a w × h image with a horizontal gradient, so its hash is
// not that of a flat colour.
func colourPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / w), 80, uint8(y * 255 / h), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestThumbHashIsStored(t *testing.T) {
	e := newEnv(t, dimensions.WithThumbHash(imaging.Options{}))
	data := colourPNG(t, 300, 200)
	f, err := e.record(t, "a.png", data, records.NewFile{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := dimensions.ThumbHashOf(f)
	want, _ := imaging.ThumbHashOf(imaging.Default(), bytes.NewReader(data), imaging.Options{})
	if !ok || !bytes.Equal(got, want) || dims(f) != [3]int{300, 200, 1} {
		t.Fatalf("thumbhash %x (ok %v), want %x; dims %v", got, ok, want, dims(f))
	}
	// A replacement gets the new picture's hash; a non-image loses it.
	anything := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}
	g, err := e.store.Replace(t.Context(), "t", f.ID, bytes.NewReader(colourPNG(t, 50, 100)), "", anything)
	if h, _ := dimensions.ThumbHashOf(g); err != nil || bytes.Equal(h, got) {
		t.Errorf("after replace: %x, %v", h, err)
	}
	g, err = e.store.Replace(t.Context(), "t", f.ID, bytes.NewReader([]byte("now text")), "", anything)
	if _, ok := dimensions.ThumbHashOf(g); err != nil || ok {
		t.Errorf("a text file kept a thumbhash: %v", g.Metadata)
	}
	// Without the option, no hash.
	plain := newEnv(t)
	h, _ := plain.record(t, "a.png", data, records.NewFile{})
	if _, ok := dimensions.ThumbHashOf(h); ok {
		t.Error("a hash without WithThumbHash")
	}
}

func TestThumbHashOverLimitsOrCorrupt(t *testing.T) {
	small := imaging.Options{MaxPixels: 100}
	e := newEnv(t, dimensions.WithThumbHash(small))
	f, err := e.record(t, "big.png", colourPNG(t, 20, 20), records.NewFile{})
	if _, ok := dimensions.ThumbHashOf(f); err != nil || ok || dims(f) != [3]int{20, 20, 1} {
		t.Errorf("over the limit = %v, %v", f.Metadata, err)
	}
	// A readable header over a body cut short: dimensions, no hash.
	cut := colourPNG(t, 40, 40)
	cut = cut[:len(cut)/2]
	lenient := newEnv(t, dimensions.WithThumbHash(imaging.Options{}))
	g, err := lenient.record(t, "cut.png", cut, records.NewFile{})
	if _, ok := dimensions.ThumbHashOf(g); err != nil || ok || dims(g)[2] != 1 {
		t.Errorf("cut short = %v, %v", g.Metadata, err)
	}
	strict := newEnv(t, dimensions.WithThumbHash(small), dimensions.WithStrict())
	if _, err := strict.record(t, "big.png", colourPNG(t, 20, 20), records.NewFile{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("strict over the limit = %v", err)
	}
	if _, ok := dimensions.ThumbHashOf(records.File{Metadata: map[string]string{dimensions.MetaThumbHash: "%%%"}}); ok {
		t.Error("a malformed stored hash was read")
	}
}

// TestThumbHashReadFailure pins that a failure to read the full object is
// an error, not a silent missing hash (only an image problem is).
func TestThumbHashReadFailure(t *testing.T) {
	var reads atomic.Int32
	failSecond := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpRead && reads.Add(1) == 2 {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	}
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(failSecond))
	e := newEnvOn(t, disk, dimensions.WithThumbHash(imaging.Options{}))
	if _, err := e.record(t, "a.png", colourPNG(t, 10, 10), records.NewFile{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("read failure = %v", err)
	}
}

// TestBackfillAddsMissingHashes pins that a file measured before
// WithThumbHash was turned on is not Present until it has a hash.
func TestBackfillAddsMissingHashes(t *testing.T) {
	e := newEnv(t)
	f, _ := e.record(t, "a.png", colourPNG(t, 30, 30), records.NewFile{})
	res, err := dimensions.Backfill(t.Context(), e.store, dimensions.BackfillOptions{Scope: "t", Plugin: dimensions.New(dimensions.WithThumbHash(imaging.Options{}))})
	if err != nil || res.Measured != 1 || res.Present != 0 {
		t.Fatalf("backfill = %+v, %v", res, err)
	}
	g, _ := e.store.File(t.Context(), "t", f.ID)
	if _, ok := dimensions.ThumbHashOf(g); !ok {
		t.Error("no hash after the backfill")
	}
	res, _ = dimensions.Backfill(t.Context(), e.store, dimensions.BackfillOptions{Scope: "t", Plugin: dimensions.New(dimensions.WithThumbHash(imaging.Options{}))})
	if res.Present != 1 {
		t.Errorf("second backfill = %+v", res)
	}
}
