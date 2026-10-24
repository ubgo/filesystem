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

// mostlyTeal is 200 × 100: 70% teal, 30% orange.
func mostlyTeal(t *testing.T, alpha uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	for y := range 100 {
		for x := range 200 {
			c := color.NRGBA{0, 128, 128, alpha}
			if x >= 140 {
				c = color.NRGBA{255, 140, 0, alpha}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// TestDominantColourIsStored pins IM-15 in the plugin: the colour is
// stored as hex, follows a replacement, shares the decode with ThumbHash,
// and is left out for an image with no opaque pixel.
func TestDominantColourIsStored(t *testing.T) {
	e := newEnv(t, dimensions.WithDominantColour(imaging.Options{}), dimensions.WithThumbHash(imaging.Options{}))
	f, err := e.record(t, "a.png", mostlyTeal(t, 255), records.NewFile{})
	if hex, ok := dimensions.ColourOf(f); err != nil || !ok || hex != "#008080" {
		t.Fatalf("colour = %q %v, %v", hex, ok, err)
	}
	if _, ok := dimensions.ThumbHashOf(f); !ok {
		t.Error("the shared decode lost the ThumbHash")
	}
	anything := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}
	g, err := e.store.Replace(t.Context(), "t", f.ID, bytes.NewReader(mostlyTeal(t, 0)), "", anything)
	if _, ok := dimensions.ColourOf(g); err != nil || ok {
		t.Errorf("a transparent replacement kept a colour: %v, %v", g.Metadata, err)
	}
	// Without the option, none.
	plain := newEnv(t)
	h, _ := plain.record(t, "a.png", mostlyTeal(t, 255), records.NewFile{})
	if _, ok := dimensions.ColourOf(h); ok {
		t.Error("a colour without WithDominantColour")
	}
	// Each option keeps its own limits: an image over the ThumbHash's gets
	// a colour but no hash, and the other way round.
	small := imaging.Options{MaxPixels: 100}
	for name, opts := range map[string][2]dimensions.Option{
		"hash limited":   {dimensions.WithThumbHash(small), dimensions.WithDominantColour(imaging.Options{})},
		"colour limited": {dimensions.WithThumbHash(imaging.Options{}), dimensions.WithDominantColour(small)},
	} {
		e := newEnv(t, opts[0], opts[1])
		f, err := e.record(t, "a.png", mostlyTeal(t, 255), records.NewFile{})
		_, hashed := dimensions.ThumbHashOf(f)
		_, coloured := dimensions.ColourOf(f)
		if err != nil || hashed == (name == "hash limited") || coloured == (name == "colour limited") {
			t.Errorf("%s: hash %v, colour %v, %v", name, hashed, coloured, err)
		}
	}
	// The backfill adds it to files measured before the option.
	res, err := dimensions.Backfill(t.Context(), plain.store, dimensions.BackfillOptions{Scope: "t", Plugin: dimensions.New(dimensions.WithDominantColour(imaging.Options{}))})
	if err != nil || res.Measured != 1 {
		t.Errorf("backfill = %+v, %v", res, err)
	}
}

// TestDominantColourReadFailure pins that failing to read the object for
// the colour fails the change, as for the ThumbHash.
func TestDominantColourReadFailure(t *testing.T) {
	var reads atomic.Int32
	failSecond := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpRead && reads.Add(1) == 2 {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	}
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(failSecond))
	e := newEnvOn(t, disk, dimensions.WithDominantColour(imaging.Options{}))
	if _, err := e.record(t, "a.png", mostlyTeal(t, 255), records.NewFile{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("read failure = %v", err)
	}
}
