package imaging

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"

	"golang.org/x/image/bmp"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func sanitize(t *testing.T, opts Options, in upload.Info, data []byte, options ...SanitizeOption) ([]byte, upload.Info, error) {
	t.Helper()
	r, out, err := Sanitize(opts, options...)(t.Context(), in, bytes.NewReader(data))
	if err != nil {
		return nil, out, err
	}
	b, _ := io.ReadAll(r)
	return b, out, nil
}

func TestSanitizeStripsMetadata(t *testing.T) {
	// A portrait phone photo: stored 40×20 with EXIF orientation 6.
	photo := realJPEG(t, 40, 20, 6)
	if !bytes.Contains(photo, []byte("Exif")) {
		t.Fatal("the sample has no EXIF")
	}
	out, info, err := sanitize(t, Options{}, upload.Info{Filename: "me.jpg", ContentType: "image/jpeg"}, photo)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("Exif")) || info.ContentType != "image/jpeg" || info.Filename != "me.jpg" {
		t.Fatalf("output still carries EXIF, or %+v", info)
	}
	if p, _ := Probe(bytes.NewReader(out)); p.Width != 20 || p.Height != 40 {
		t.Fatalf("output is %dx%d, want the upright 20x40", p.Width, p.Height)
	}

	// A PNG with a text chunk and a payload appended after its end.
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 8, 8)))
	smuggled := append(buf.Bytes(), []byte("PK\x03\x04secret-archive")...)
	out, info, err = sanitize(t, Options{}, upload.Info{Filename: "a.png", ContentType: "image/png"}, smuggled)
	if err != nil || bytes.Contains(out, []byte("secret")) || info.ContentType != "image/png" {
		t.Fatalf("png = %v, %+v", err, info)
	}

	// BMP becomes PNG, with the name following.
	var b bytes.Buffer
	_ = bmp.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 4, 4)))
	_, info, err = sanitize(t, Options{}, upload.Info{Filename: "scan.bmp", ContentType: "image/bmp"}, b.Bytes())
	if err != nil || info.ContentType != "image/png" || info.Filename != "scan.png" {
		t.Fatalf("bmp = %+v, %v", info, err)
	}
}

func TestSanitizePassesAndRefuses(t *testing.T) {
	for _, ct := range []string{"image/gif", "application/pdf"} {
		out, info, err := sanitize(t, Options{}, upload.Info{Filename: "x", ContentType: ct}, []byte("untouched"))
		if err != nil || string(out) != "untouched" || info.ContentType != ct {
			t.Errorf("%s = %q %+v %v", ct, out, info, err)
		}
	}
	if _, _, err := sanitize(t, Options{}, upload.Info{ContentType: "image/jpeg"}, []byte("\xff\xd8not a jpeg")); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("corrupt = %v", err)
	}
	if _, _, err := sanitize(t, Options{MaxBytes: 10}, upload.Info{ContentType: "image/jpeg"}, realJPEG(t, 8, 8, 1)); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("too large = %v", err)
	}
	if _, _, err := Sanitize(Options{})(t.Context(), upload.Info{ContentType: "image/png"}, errReader{}); err == nil {
		t.Error("a read error was ignored")
	}
}

// TestSanitizeInAPolicy stores a photo through upload.Receive.
func TestSanitizeInAPolicy(t *testing.T) {
	ctx := context.Background()
	disk, _ := filesystem.NewDisk("media", memory.New())
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Process: Sanitize(Options{})}
	res, err := upload.Receive(ctx, disk, bytes.NewReader(realJPEG(t, 40, 20, 6)), "IMG_0001.JPG", p)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := disk.ReadAll(ctx, res.Key)
	if bytes.Contains(stored, []byte("Exif")) || !strings.HasPrefix(res.ContentType, "image/jpeg") {
		t.Fatalf("stored %d bytes of %s with EXIF %v", len(stored), res.ContentType, bytes.Contains(stored, []byte("Exif")))
	}
}

// TestSanitizeQuality proves WithQuality reaches the encoder: a photo
// saved at a low quality grows when re-encoded at the default, and does
// not when re-encoded at its own quality.
func TestSanitizeQuality(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 96, 96))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7919 % 251) // detail that costs bytes at high quality
	}
	var buf bytes.Buffer
	const lowQuality = 40
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: lowQuality}); err != nil {
		t.Fatal(err)
	}
	in := upload.Info{Filename: "low.jpg", ContentType: "image/jpeg"}
	def, _, err := sanitize(t, Options{}, in, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	low, _, err := sanitize(t, Options{}, in, buf.Bytes(), WithQuality(lowQuality))
	if err != nil {
		t.Fatal(err)
	}
	if len(def) <= buf.Len() || len(low) >= len(def) || len(low) > buf.Len()*11/10 {
		t.Fatalf("input %d bytes, default %d, WithQuality(%d) %d", buf.Len(), len(def), lowQuality, len(low))
	}

	for _, q := range []int{0, -1, 101} {
		if _, _, err := sanitize(t, Options{}, in, buf.Bytes(), WithQuality(q)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("WithQuality(%d) = %v", q, err)
		}
	}
	// The quality only concerns JPEG output.
	var p bytes.Buffer
	_ = png.Encode(&p, image.NewNRGBA(image.Rect(0, 0, 4, 4)))
	if _, _, err := sanitize(t, Options{}, upload.Info{Filename: "a.png", ContentType: "image/png"}, p.Bytes(), WithQuality(0)); err != nil {
		t.Errorf("png with an unused quality = %v", err)
	}
}
