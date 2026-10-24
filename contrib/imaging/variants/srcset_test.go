package variants_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
)

var widths = map[string]imaging.Transform{
	"w20":  {Mode: imaging.ModeFit, Width: 20, Height: 1000, Format: imaging.FormatJPEG},
	"w40":  {Mode: imaging.ModeFit, Width: 40, Height: 1000, Format: imaging.FormatJPEG},
	"w100": {Mode: imaging.ModeFit, Width: 100, Height: 1000, Format: imaging.FormatJPEG},
	"w120": {Mode: imaging.ModeFit, Width: 120, Height: 1000, Format: imaging.FormatJPEG},
}

func TestSrcSet(t *testing.T) {
	ctx := t.Context()
	p, err := variants.New(widths, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWithPlugin(t, memory.New(), p, linked...)
	f := e.image(t, "a.png", pngBytes(t, 80, 40), filesystem.VisibilityPublic)
	set, list, err := p.SrcSet(ctx, e.store, "t", f.ID, time.Hour, "w100", "w20", "w120", "w40")
	if err != nil {
		t.Fatal(err)
	}
	// w100 and w120 both come out 80 wide (fit never enlarges): one
	// candidate, the first named. Smallest first.
	if len(list) != 3 || list[0].Name != "w20" || list[1].Name != "w40" || list[2].Name != "w100" ||
		list[0].Width != 20 || list[1].Width != 40 || list[2].Width != 80 {
		t.Fatalf("candidates = %+v", list)
	}
	parts := strings.Split(set, ", ")
	if len(parts) != 3 || !strings.HasSuffix(parts[0], " 20w") || !strings.HasSuffix(parts[2], " 80w") || !strings.HasPrefix(parts[0], "https://cdn.test/variants/") {
		t.Errorf("srcset = %q", set)
	}
	for i, c := range list {
		if parts[i] != c.URL+" "+map[int]string{0: "20w", 1: "40w", 2: "80w"}[i] {
			t.Errorf("part %d = %q for %+v", i, parts[i], c)
		}
	}
}

func TestSrcSetRefusals(t *testing.T) {
	ctx := t.Context()
	p, _ := variants.New(widths, imaging.Options{})
	e := newEnvWithPlugin(t, memory.New(), p, linked...)
	f := e.image(t, "a.png", pngBytes(t, 80, 40), filesystem.VisibilityPrivate)
	if _, _, err := p.SrcSet(ctx, e.store, "t", f.ID, time.Hour); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("no names = %v", err)
	}
	if _, _, err := p.SrcSet(ctx, e.store, "t", f.ID, time.Hour, "w20", "huge"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("an undeclared name = %v", err)
	}
	if _, _, err := p.SrcSet(ctx, e.store, "t", "fil_missing", time.Hour, "w20"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a missing file = %v", err)
	}
	// A private file gets signed links.
	set, _, err := p.SrcSet(ctx, e.store, "t", f.ID, time.Hour, "w20")
	if err != nil || !strings.HasPrefix(set, "https://app.test/media/") {
		t.Errorf("private srcset = %q, %v", set, err)
	}
	// A disk that can neither sign nor serve publicly fails the call
	// rather than returning a srcset without links.
	bare := newEnvWithPlugin(t, memory.New(), p)
	g := bare.image(t, "a.png", pngBytes(t, 80, 40), filesystem.VisibilityPrivate)
	if _, _, err := p.SrcSet(ctx, bare.store, "t", g.ID, time.Hour, "w20"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("no way to link = %v", err)
	}
}

// linked are the disk options of newEnv: public URLs and a signer.
var linked = []filesystem.Option{filesystem.WithPublicURL("https://cdn.test"), filesystem.WithURLSigner(signer{})}
