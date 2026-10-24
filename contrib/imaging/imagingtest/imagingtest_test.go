package imagingtest_test

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging/imagingtest"
)

// The package's own -update flag, as a golden-file test package defines it.
var _ = flag.Bool("update", false, "rewrite golden files")

func flat(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

// recorder is a testing.TB that records failures instead of failing.
type recorder struct {
	*testing.T
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

// run calls Golden with a recorder and returns its failures.
func run(t *testing.T, got image.Image, path string, opts ...imagingtest.Options) []string {
	r := &recorder{T: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		imagingtest.Golden(r, got, path, opts...)
	}()
	<-done
	return r.msgs
}

func TestCompare(t *testing.T) {
	grey := flat(10, 10, color.NRGBA{128, 128, 128, 255})
	if d, err := imagingtest.Compare(grey, grey, 16); err != nil || d != (imagingtest.Diff{}) {
		t.Errorf("same = %+v, %v", d, err)
	}
	// Green counts more than blue for the same change.
	g := flat(10, 10, color.NRGBA{128, 160, 128, 255})
	b := flat(10, 10, color.NRGBA{128, 128, 160, 255})
	dg, _ := imagingtest.Compare(grey, g, 16)
	db, _ := imagingtest.Compare(grey, b, 16)
	if dg.Mean <= db.Mean || dg.Different != 1 || db.Different != 0 {
		t.Errorf("green %+v, blue %+v", dg, db)
	}
	// Transparent compares by what shows through (mid grey).
	if d, _ := imagingtest.Compare(flat(4, 4, color.NRGBA{}), flat(4, 4, color.NRGBA{128, 128, 128, 255}), 16); d.Max > 1 {
		t.Errorf("transparent vs grey = %+v", d)
	}
	if _, err := imagingtest.Compare(grey, flat(9, 10, color.NRGBA{}), 16); err == nil {
		t.Error("different sizes compared")
	}
	// Bounds that do not start at the origin.
	sub := flat(20, 20, color.NRGBA{128, 128, 128, 255}).SubImage(image.Rect(5, 5, 15, 15))
	if d, _ := imagingtest.Compare(sub, grey, 16); d.Max != 0 {
		t.Errorf("sub-image = %+v", d)
	}
	if d, _ := imagingtest.Compare(image.NewNRGBA(image.Rect(0, 0, 0, 0)), image.NewNRGBA(image.Rect(0, 0, 0, 0)), 16); d != (imagingtest.Diff{}) {
		t.Errorf("empty = %+v", d)
	}
}

func TestGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "card.golden.png")
	base := flat(40, 40, color.NRGBA{200, 100, 50, 255})
	if msgs := run(t, base, path); len(msgs) != 1 || !strings.Contains(msgs[0], "-update") {
		t.Fatalf("missing golden = %v", msgs)
	}
	if msgs := run(t, base, path, imagingtest.Options{Update: true}); len(msgs) != 0 {
		t.Fatalf("update = %v", msgs)
	}
	if msgs := run(t, base, path); len(msgs) != 0 {
		t.Errorf("same image = %v", msgs)
	}
	// One level everywhere: invisible, passes.
	if msgs := run(t, flat(40, 40, color.NRGBA{201, 101, 51, 255}), path); len(msgs) != 0 {
		t.Errorf("one level = %v", msgs)
	}
	// A visibly different block fails and saves what it got.
	moved := flat(40, 40, color.NRGBA{200, 100, 50, 255})
	for y := range 10 {
		for x := range 10 {
			moved.SetNRGBA(x, y, color.NRGBA{0, 0, 255, 255})
		}
	}
	msgs := run(t, moved, path)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "differs") {
		t.Fatalf("moved = %v", msgs)
	}
	saved := msgs[0][strings.LastIndex(msgs[0], "saved as ")+len("saved as "):]
	if _, err := os.Stat(saved); err != nil {
		t.Errorf("got was not saved: %v", err)
	}
	_ = os.RemoveAll(filepath.Dir(saved))
	// Looser limits let it pass.
	if msgs := run(t, moved, path, imagingtest.Options{MaxMean: 100, MaxDifferent: 1, PixelThreshold: 255}); len(msgs) != 0 {
		t.Errorf("loose = %v", msgs)
	}
	if msgs := run(t, flat(10, 10, color.NRGBA{}), path); len(msgs) != 1 || !strings.Contains(msgs[0], "sizes differ") {
		t.Errorf("size = %v", msgs)
	}
	bad := filepath.Join(t.TempDir(), "bad.png")
	_ = os.WriteFile(bad, []byte("not a png"), 0o644)
	if msgs := run(t, base, bad); len(msgs) != 1 {
		t.Errorf("corrupt golden = %v", msgs)
	}
}

// TestUpdateFlag pins -update: the test package's flag rewrites the file.
func TestUpdateFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flag.png")
	if err := flag.Set("update", "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flag.Set("update", "false") }()
	if msgs := run(t, flat(4, 4, color.NRGBA{1, 2, 3, 255}), path); len(msgs) != 0 {
		t.Fatal(msgs)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("-update did not write: %v", err)
	}
	// Unwritable places fail the test with the reason.
	if msgs := run(t, flat(4, 4, color.NRGBA{}), filepath.Join(path, "x", "y.png")); len(msgs) != 1 {
		t.Errorf("unwritable dir = %v", msgs)
	}
	dir := filepath.Join(t.TempDir(), "d")
	_ = os.MkdirAll(filepath.Join(dir, "y.png"), 0o755)
	if msgs := run(t, flat(4, 4, color.NRGBA{}), filepath.Join(dir, "y.png")); len(msgs) != 1 {
		t.Errorf("unwritable file = %v", msgs)
	}
	if msgs := run(t, image.NewNRGBA(image.Rect(0, 0, 0, 0)), filepath.Join(dir, "z.png")); len(msgs) != 1 {
		t.Errorf("unencodable image = %v", msgs)
	}
}
