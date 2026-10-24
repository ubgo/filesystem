package imaging_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func receive(t *testing.T, check upload.Inspector, name string, data []byte) (upload.Result, *filesystem.Disk, error) {
	t.Helper()
	d, _ := filesystem.NewDisk("media", memory.New())
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*", "text/plain"}, Inspect: check}
	r, err := upload.Receive(t.Context(), d, bytes.NewReader(data), name, p)
	return r, d, err
}

func TestCheckDimensions(t *testing.T) {
	check, err := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: 20, MinHeight: 10, MaxWidth: 100, MaxHeight: 80, MaxPixels: 5000})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		w, h int
		want string
	}{
		{50, 50, ""},
		{20, 10, ""},
		{19, 50, "at least 20 pixels wide"},
		{50, 9, "at least 10 pixels high"},
		{101, 20, "at most 100 pixels wide"},
		{50, 81, "at most 80 pixels high"},
		{100, 60, "at most 5000 pixels"},
	}
	for _, c := range cases {
		_, d, err := receive(t, check, "a.png", photo(t, c.w, c.h, pngEnc))
		if c.want == "" {
			if err != nil {
				t.Errorf("%dx%d refused: %v", c.w, c.h, err)
			}
			continue
		}
		if filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%dx%d = %v, want %q", c.w, c.h, err, c.want)
		}
		// A refused upload leaves nothing stored.
		n := 0
		for range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
			n++
		}
		if n != 0 {
			t.Errorf("%dx%d left %d objects", c.w, c.h, n)
		}
	}
}

func TestCheckDimensionsPassesWhatHasNoSize(t *testing.T) {
	check, _ := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: 1000})
	if _, _, err := receive(t, check, "notes.txt", []byte("plain text")); err != nil {
		t.Errorf("text = %v", err)
	}
	// SVG is refused by the policy unless allowed by name; the check
	// itself passes it, having no pixel size to bound.
	d, _ := filesystem.NewDisk("media", memory.New())
	_, _ = d.WriteBytes(t.Context(), "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), filesystem.WriteOptions{})
	if err := check(t.Context(), d, upload.Result{Key: "logo.svg", ContentType: "image/svg+xml"}); err != nil {
		t.Errorf("svg = %v", err)
	}
	// An image whose size cannot be read is refused: it cannot be checked.
	if _, _, err := receive(t, check, "broken.png", []byte("\x89PNG\r\n\x1a\nnot a header at all")); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("unreadable header = %v", err)
	}
}

func TestCheckDimensionsWithProber(t *testing.T) {
	var seen []string
	check, _ := imaging.CheckDimensions(imaging.DimensionLimits{MaxWidth: 10}, imaging.WithProber(imaging.NewPipeline(imaging.WithDecoder(spyPNG{seen: &seen}))))
	if _, _, err := receive(t, check, "a.png", photo(t, 11, 4, pngEnc)); filesystem.CodeOf(err) != filesystem.CodeRejected || len(seen) == 0 {
		t.Errorf("with a prober = %v (prober saw %v)", err, seen)
	}
}

func TestCheckDimensionsReadFailure(t *testing.T) {
	check, _ := imaging.CheckDimensions(imaging.DimensionLimits{})
	d, _ := filesystem.NewDisk("media", memory.New())
	err := check(t.Context(), d, upload.Result{Key: "missing.png", ContentType: "image/png"})
	if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("missing object = %v", err)
	}
}

func TestCheckDimensionsRefusesBadLimits(t *testing.T) {
	for _, l := range []imaging.DimensionLimits{
		{MinWidth: -1}, {MinHeight: -1}, {MaxWidth: -1}, {MaxHeight: -1}, {MaxPixels: -1},
		{MinWidth: 10, MaxWidth: 5}, {MinHeight: 10, MaxHeight: 5},
	} {
		if _, err := imaging.CheckDimensions(l); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v = %v", l, err)
		}
	}
	// A minimum with no maximum is fine.
	if _, err := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: 10}); err != nil {
		t.Error(err)
	}
}

// TestCheckDimensionsAspect pins VA-07: the ratio is checked from the
// header within the tolerance, with a message naming both shapes.
func TestCheckDimensionsAspect(t *testing.T) {
	poster, err := imaging.CheckDimensions(imaging.DimensionLimits{Aspect: 16.0 / 9})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		w, h int
		ok   bool
	}{{160, 90, true}, {161, 90, true}, {150, 90, false}, {90, 160, false}} {
		_, _, err := receive(t, poster, "a.png", photo(t, c.w, c.h, pngEnc))
		if c.ok != (err == nil) {
			t.Errorf("%dx%d = %v", c.w, c.h, err)
		}
		if !c.ok && (filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), "its shape must be 1.78:1 (within 1%)")) {
			t.Errorf("%dx%d message = %v", c.w, c.h, err)
		}
	}
	loose, _ := imaging.CheckDimensions(imaging.DimensionLimits{Aspect: 1, AspectTolerance: 0.1})
	if _, _, err := receive(t, loose, "a.png", photo(t, 105, 100, pngEnc)); err != nil {
		t.Errorf("within 10%% = %v", err)
	}
	for _, l := range []imaging.DimensionLimits{{Aspect: -1}, {AspectTolerance: -0.1}} {
		if _, err := imaging.CheckDimensions(l); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v = %v", l, err)
		}
	}
}
