// Package placeholder draws the image shown for a user, team or item that
// has no picture of its own: the initials of a display name on a coloured
// square, or an identicon (a symmetric 5×5 pattern) for a seed with no
// usable name. Both are deterministic: the same seed, name, size and style
// always give the same PNG bytes, so the result can be cached forever and
// served with an ETag, and a user's fallback keeps its colour across
// devices and deployments.
//
// The seed (a user ID, an email hash) picks the colour and the identicon
// pattern; the name gives the initials. The text is drawn with the Go Bold
// font embedded in golang.org/x/image, which covers Latin, Greek and
// Cyrillic; a name whose initials the font cannot draw (CJK, emoji) falls
// back to the identicon, so a fallback is never a box of missing glyphs.
package placeholder

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"github.com/ubgo/filesystem"
)

// Style is what a placeholder shows.
type Style string

// The styles.
const (
	// StyleInitials draws up to two initials of Name, or an identicon
	// when Name has none the font can draw.
	StyleInitials Style = "initials"
	// StyleIdenticon draws the seed's 5×5 pattern and ignores Name.
	StyleIdenticon Style = "identicon"
)

// Size bounds, in pixels per side. A placeholder is a small square; the
// bounds keep a request from asking for a costly render (MaxSize is about
// 1 MiB of pixels) and keep tiny ones legible.
const (
	MinSize     = 16
	MaxSize     = 512
	DefaultSize = 128
)

// Input bounds, in bytes. A seed is an ID, a name a display name; anything
// longer is a mistake or an attack, and both go into the ETag.
const (
	MaxSeedBytes = 256
	MaxNameBytes = 256
)

// Version is part of every ETag. It changes whenever the drawing changes,
// so caches that revalidate fetch the new look; put it in your URLs (a
// ?v= parameter) to reach caches that never revalidate.
const Version = "1"

// Request describes one placeholder.
type Request struct {
	// Seed picks the colour and the identicon pattern; required, at most
	// MaxSeedBytes. Use a stable, non-secret ID.
	Seed string
	// Name gives the initials (StyleInitials); at most MaxNameBytes.
	Name string
	// Size is the side in pixels, MinSize to MaxSize; 0 means DefaultSize.
	Size int
	// Style is StyleInitials or StyleIdenticon; empty means StyleInitials.
	Style Style
}

// normalize applies the defaults and checks the bounds.
func (r Request) normalize() (Request, error) {
	if r.Size == 0 {
		r.Size = DefaultSize
	}
	if r.Style == "" {
		r.Style = StyleInitials
	}
	switch {
	case r.Seed == "" || len(r.Seed) > MaxSeedBytes:
		return r, filesystem.Errorf(filesystem.CodeInvalidArgument, "placeholder: seed must be 1 to %d bytes", MaxSeedBytes)
	case len(r.Name) > MaxNameBytes:
		return r, filesystem.Errorf(filesystem.CodeInvalidArgument, "placeholder: name is over %d bytes", MaxNameBytes)
	case r.Size < MinSize || r.Size > MaxSize:
		return r, filesystem.Errorf(filesystem.CodeInvalidArgument, "placeholder: size %d is not in [%d, %d]", r.Size, MinSize, MaxSize)
	case r.Style != StyleInitials && r.Style != StyleIdenticon:
		return r, filesystem.Errorf(filesystem.CodeInvalidArgument, "placeholder: unknown style %q", r.Style)
	}
	return r, nil
}

// resolved is a request reduced to what the drawing depends on: the
// initials actually drawn (empty for an identicon), so two names with the
// same initials share one image and one ETag.
type resolved struct {
	seed     string
	initials string
	size     int
}

func resolve(r Request) (resolved, error) {
	r, err := r.normalize()
	if err != nil {
		return resolved{}, err
	}
	out := resolved{seed: r.Seed, size: r.Size}
	if r.Style == StyleInitials {
		if in := Initials(r.Name); drawable(in) {
			out.initials = in
		}
	}
	return out, nil
}

// maxInitials is how many letters Initials returns.
const maxInitials = 2

// Initials returns up to two upper-case initials of name: the first letter
// or digit of its first word and of its last word ("Ada Lovelace" → "AL",
// "Ада" → "А", "  jean-luc picard " → "JP"). Words are split on spaces and
// punctuation is skipped, so "@ada" gives "A"; a name with no letter or
// digit gives "". Invalid UTF-8 is treated as no name.
func Initials(name string) string {
	if !utf8.ValidString(name) {
		return ""
	}
	var firsts []rune
	for _, w := range strings.Fields(name) {
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				firsts = append(firsts, unicode.ToUpper(r))
				break
			}
		}
	}
	switch len(firsts) {
	case 0:
		return ""
	case 1:
		return string(firsts[0])
	}
	return string([]rune{firsts[0], firsts[len(firsts)-1]})
}

// ETag is the entity tag of the placeholder r describes, computed without
// drawing it, so a handler answers a revalidation with 304 for free. It
// is quoted, as the header carries it.
func ETag(r Request) (string, error) {
	res, err := resolve(r)
	if err != nil {
		return "", err
	}
	return res.etag(), nil
}

func (res resolved) etag() string {
	h := sha256.New()
	for _, part := range []string{Version, res.seed, res.initials} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(part)))
		h.Write([]byte(part))
	}
	_ = binary.Write(h, binary.BigEndian, uint32(res.size))
	return `"` + hex.EncodeToString(h.Sum(nil)[:etagBytes]) + `"`
}

// etagBytes is how much of the SHA-256 an ETag carries.
const etagBytes = 16

// PNG draws the placeholder r describes. An out-of-bounds or unknown
// request is CodeInvalidArgument.
func PNG(r Request) ([]byte, error) {
	res, err := resolve(r)
	if err != nil {
		return nil, err
	}
	return res.png()
}

func (res resolved) png() ([]byte, error) {
	img, err := drawInitials(res)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// palette is the closed set of background colours: mid-tone hues that keep
// white text readable (contrast 4.5:1 or better) and stay distinct from
// each other. The seed picks one; changing this list changes users'
// colours, so it is part of Version.
var palette = []color.RGBA{
	{0xC6, 0x28, 0x28, 0xFF}, // red
	{0xAD, 0x14, 0x57, 0xFF}, // pink
	{0x6A, 0x1B, 0x9A, 0xFF}, // purple
	{0x45, 0x27, 0xA0, 0xFF}, // deep purple
	{0x28, 0x35, 0x93, 0xFF}, // indigo
	{0x15, 0x65, 0xC0, 0xFF}, // blue
	{0x02, 0x77, 0xBD, 0xFF}, // light blue
	{0x00, 0x83, 0x8F, 0xFF}, // cyan
	{0x00, 0x69, 0x5C, 0xFF}, // teal
	{0x2E, 0x7D, 0x32, 0xFF}, // green
	{0xBF, 0x36, 0x0C, 0xFF}, // deep orange
	{0x4E, 0x34, 0x2E, 0xFF}, // brown
	{0x37, 0x47, 0x4F, 0xFF}, // blue grey
}

// identiconBackground is the light ground an identicon's cells sit on.
var identiconBackground = color.RGBA{0xF0, 0xF0, 0xF0, 0xFF}

// seedHash is the seed's SHA-256: the colour comes from its first bytes and
// the identicon pattern from the rest, so the two vary independently.
func seedHash(seed string) [sha256.Size]byte { return sha256.Sum256([]byte(seed)) }

func colourOf(sum [sha256.Size]byte) color.RGBA {
	return palette[binary.BigEndian.Uint32(sum[:4])%uint32(len(palette))]
}

// Font size and placement as fractions of the side.
const (
	// textScale is the font size: two capitals fill about half the width.
	textScale = 0.42
	// identiconCells is the grid's side; it is mirrored, so only
	// (identiconCells+1)/2 columns are free.
	identiconCells = 5
	// identiconMargin is the empty border, in cells.
	identiconMargin = 0.5
)

// goBold is the parsed font, parsed once on first use and never changed
// after. The bytes are embedded in golang.org/x/image and every test
// parses them, so a failure is a broken build, not a runtime condition:
// it panics rather than threading an impossible error through every call.
var goBold = sync.OnceValue(func() *opentype.Font {
	f, err := opentype.Parse(gobold.TTF)
	if err != nil {
		panic("placeholder: the embedded Go Bold font does not parse: " + err.Error())
	}
	return f
})

// drawable reports whether the font has a glyph for every rune of s.
func drawable(s string) bool {
	if s == "" {
		return false
	}
	f := goBold()
	var buf sfnt.Buffer
	for _, r := range s {
		if i, err := f.GlyphIndex(&buf, r); err != nil || i == 0 {
			return false
		}
	}
	return true
}

// drawInitials draws res's initials, or its identicon when it has none.
func drawInitials(res resolved) (*image.RGBA, error) {
	if res.initials == "" {
		return drawIdenticon(res), nil
	}
	img := image.NewRGBA(image.Rect(0, 0, res.size, res.size))
	draw.Draw(img, img.Bounds(), image.NewUniform(colourOf(seedHash(res.seed))), image.Point{}, draw.Src)
	face, err := opentype.NewFace(goBold(), &opentype.FaceOptions{Size: float64(res.size) * textScale, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	defer func() { _ = face.Close() }()
	d := &font.Drawer{Dst: img, Src: image.White, Face: face}
	// Centre on the ink, not the advance or line metrics, so every
	// letter pair sits in the optical middle.
	ink, _ := d.BoundString(res.initials)
	side := fixed.I(res.size)
	d.Dot = fixed.Point26_6{
		X: (side-(ink.Max.X-ink.Min.X))/2 - ink.Min.X,
		Y: (side-(ink.Max.Y-ink.Min.Y))/2 - ink.Min.Y,
	}
	d.DrawString(res.initials)
	return img, nil
}

func drawIdenticon(res resolved) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, res.size, res.size))
	draw.Draw(img, img.Bounds(), image.NewUniform(identiconBackground), image.Point{}, draw.Src)
	sum := seedHash(res.seed)
	fg := image.NewUniform(colourOf(sum))
	cell := float64(res.size) / (identiconCells + 2*identiconMargin)
	half := (identiconCells + 1) / 2
	bit := 0
	for col := range half {
		for row := range identiconCells {
			// Bits from byte 4 on; the first four chose the colour.
			on := sum[4+bit/8]>>(bit%8)&1 == 1
			bit++
			if !on {
				continue
			}
			for _, c := range []int{col, identiconCells - 1 - col} {
				x0 := int((identiconMargin + float64(c)) * cell)
				y0 := int((identiconMargin + float64(row)) * cell)
				x1 := int((identiconMargin + float64(c+1)) * cell)
				y1 := int((identiconMargin + float64(row+1)) * cell)
				draw.Draw(img, image.Rect(x0, y0, x1, y1), fg, image.Point{}, draw.Src)
			}
		}
	}
	return img
}
