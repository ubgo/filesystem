package placeholder_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/placeholder"
)

func TestInitials(t *testing.T) {
	cases := map[string]string{
		"Ada Lovelace":           "AL",
		"ada":                    "A",
		"  jean-luc   picard ":   "JP",
		"@ada lovelace":          "AL",
		"Ада Лавлейс":            "АЛ",
		"élodie de la durand":    "ÉD",
		"李小龙":                    "李",
		"Ada 😀":                  "A",
		"😀 🎉":                    "",
		"":                       "",
		"\xff\xfe":               "",
		"Mary Ann Evans-Cross 3": "M3",
	}
	for in, want := range cases {
		if got := placeholder.Initials(in); got != want {
			t.Errorf("Initials(%q) = %q, want %q", in, got, want)
		}
	}
}

func render(t *testing.T, r placeholder.Request) []byte {
	t.Helper()
	b, err := placeholder.PNG(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// pixels hashes the decoded pixels rather than the PNG bytes, so a change
// in the standard library's compressor does not look like a new drawing.
func pixels(t *testing.T, b []byte) string {
	t.Helper()
	img := decode(t, b)
	h := sha256.New()
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			c := rgba(img.At(x, y))
			h.Write([]byte{c.R, c.G, c.B, c.A})
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// TestDeterminism pins that a request always gives the same bytes, that
// the seed changes them, and that names with the same initials share one
// image and one ETag.
func TestDeterminism(t *testing.T) {
	a := placeholder.Request{Seed: "usr_1", Name: "Ada Lovelace"}
	if !bytes.Equal(render(t, a), render(t, a)) {
		t.Fatal("two renders of one request differ")
	}
	other := a
	other.Seed = "usr_2"
	if bytes.Equal(render(t, a), render(t, other)) {
		t.Error("two seeds drew the same image")
	}
	same := a
	same.Name = "Alan Lee"
	if !bytes.Equal(render(t, a), render(t, same)) {
		t.Error("names with the same initials drew different images")
	}
	ta, _ := placeholder.ETag(a)
	ts, _ := placeholder.ETag(same)
	to, _ := placeholder.ETag(other)
	if ta != ts || ta == to || !strings.HasPrefix(ta, `"`) || !strings.HasSuffix(ta, `"`) {
		t.Errorf("etags %s %s %s", ta, ts, to)
	}
	bigger := a
	bigger.Size = 64
	if tb, _ := placeholder.ETag(bigger); tb == ta {
		t.Error("the size is not in the ETag")
	}
}

// TestGolden pins the exact pixels, so any change to the drawing is seen
// and comes with a Version bump (caches keep old images otherwise).
func TestGolden(t *testing.T) {
	cases := []struct {
		r    placeholder.Request
		want string
	}{
		{placeholder.Request{Seed: "usr_1", Name: "Ada Lovelace", Size: 64}, "58f8cf0c0c224834"},
		{placeholder.Request{Seed: "usr_1", Style: placeholder.StyleIdenticon, Size: 64}, "6df0290fad85ae40"},
	}
	for _, c := range cases {
		if got := pixels(t, render(t, c.r)); got != c.want {
			t.Errorf("%+v: digest %s, want %s (bump placeholder.Version if the change is intended)", c.r, got, c.want)
		}
	}
	if placeholder.Version != "1" {
		t.Errorf("Version = %q; update the golden digests with it", placeholder.Version)
	}
}

func TestBounds(t *testing.T) {
	bad := []placeholder.Request{
		{Seed: ""},
		{Seed: strings.Repeat("s", placeholder.MaxSeedBytes+1)},
		{Seed: "s", Name: strings.Repeat("n", placeholder.MaxNameBytes+1)},
		{Seed: "s", Size: placeholder.MinSize - 1},
		{Seed: "s", Size: placeholder.MaxSize + 1},
		{Seed: "s", Size: -1},
		{Seed: "s", Style: "robot"},
	}
	for _, r := range bad {
		if _, err := placeholder.PNG(r); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("PNG(%+v) = %v", r, err)
		}
		if _, err := placeholder.ETag(r); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("ETag(%+v) = %v", r, err)
		}
	}
	for _, size := range []int{0, placeholder.MinSize, placeholder.MaxSize} {
		for _, style := range []placeholder.Style{placeholder.StyleInitials, placeholder.StyleIdenticon} {
			img := decode(t, render(t, placeholder.Request{Seed: "s", Name: "Ada", Size: size, Style: style}))
			want := size
			if size == 0 {
				want = placeholder.DefaultSize
			}
			if b := img.Bounds(); b.Dx() != want || b.Dy() != want {
				t.Errorf("size %d %s: %v", size, style, b)
			}
		}
	}
}

func rgba(c color.Color) color.RGBA {
	r, g, b, a := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// TestInitialsAreDrawn pins that text lands on the square: white ink near
// the centre, the background colour in the corners.
func TestInitialsAreDrawn(t *testing.T) {
	img := decode(t, render(t, placeholder.Request{Seed: "usr_1", Name: "Ada Lovelace", Size: 128}))
	corner := rgba(img.At(0, 0))
	if corner == (color.RGBA{255, 255, 255, 255}) || corner != rgba(img.At(127, 127)) {
		t.Fatalf("corners %v %v", corner, rgba(img.At(127, 127)))
	}
	white := 0
	for y := 32; y < 96; y++ {
		for x := 16; x < 112; x++ {
			if rgba(img.At(x, y)) == (color.RGBA{255, 255, 255, 255}) {
				white++
			}
		}
	}
	if white < 200 {
		t.Errorf("only %d white pixels in the middle: no initials drawn", white)
	}
}

// TestUndrawableInitialsFallBackToIdenticon pins that a name the font
// cannot draw gives the identicon, never missing-glyph boxes.
func TestUndrawableInitialsFallBackToIdenticon(t *testing.T) {
	icon := render(t, placeholder.Request{Seed: "usr_9", Style: placeholder.StyleIdenticon})
	for _, name := range []string{"李小龙", "😀", "", "!!!"} {
		if got := render(t, placeholder.Request{Seed: "usr_9", Name: name}); !bytes.Equal(got, icon) {
			t.Errorf("name %q did not fall back to the identicon", name)
		}
	}
	if bytes.Equal(render(t, placeholder.Request{Seed: "usr_9", Name: "Ada"}), icon) {
		t.Error("a drawable name gave the identicon")
	}
}

// TestIdenticonIsMirrored pins the left-right symmetry identicons are
// recognised by, sampled at cell centres.
func TestIdenticonIsMirrored(t *testing.T) {
	const size = 120 // 6 cells of 20 px with a half-cell margin
	img := decode(t, render(t, placeholder.Request{Seed: "usr_3", Style: placeholder.StyleIdenticon, Size: size}))
	on := 0
	for row := range 5 {
		for col := range 5 {
			x, y := 10+20*col+10, 10+20*row+10
			mx := 10 + 20*(4-col) + 10
			if rgba(img.At(x, y)) != rgba(img.At(mx, y)) {
				t.Errorf("cell (%d,%d) is not mirrored", col, row)
			}
			if rgba(img.At(x, y)) != rgba(img.At(0, 0)) {
				on++
			}
		}
	}
	if on == 0 || on == 25 {
		t.Errorf("%d of 25 cells on: no pattern", on)
	}
}
