package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// secret is what the EXIF block carries besides the GPS position; it must
// not survive.
const secret = "Camera serial 4CE0-1337"

// payload is what an attacker appends after a JPEG's end.
const payload = "<?php system($_GET['c']); ?>"

func picture(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 60, 255})
		}
	}
	return img
}

// exifTIFF builds a little-endian TIFF block with IFD0 (an
// ImageDescription, the orientation, a pointer to the GPS IFD) and a GPS
// IFD holding 48°51'29" N.
func exifTIFF(orientation uint16) []byte {
	le := binary.LittleEndian
	desc := secret + "\x00"
	descOff := 8 + 2 + 3*12 + 4
	gpsOff := descOff + len(desc)
	if gpsOff%2 == 1 {
		gpsOff++
	}
	ratOff := gpsOff + 2 + 2*12 + 4
	t := []byte("II\x2a\x00")
	t = le.AppendUint32(t, 8)
	entry := func(tag, typ uint16, count, value uint32) {
		t = le.AppendUint16(t, tag)
		t = le.AppendUint16(t, typ)
		t = le.AppendUint32(t, count)
		t = le.AppendUint32(t, value)
	}
	t = le.AppendUint16(t, 3)
	entry(0x010E, 2, uint32(len(desc)), uint32(descOff)) // ImageDescription
	entry(0x0112, 3, 1, uint32(orientation))             // Orientation
	entry(0x8825, 4, 1, uint32(gpsOff))                  // GPS IFD
	t = le.AppendUint32(t, 0)
	t = append(t, desc...)
	for len(t) < gpsOff {
		t = append(t, 0)
	}
	t = le.AppendUint16(t, 2)
	entry(0x0001, 2, 2, uint32('N'))    // GPSLatitudeRef
	entry(0x0002, 5, 3, uint32(ratOff)) // GPSLatitude
	t = le.AppendUint32(t, 0)
	for _, v := range []uint32{48, 1, 51, 1, 29, 1} {
		t = le.AppendUint32(t, v)
	}
	return t
}

// phonePhoto encodes a w×h JPEG with the EXIF block inserted after SOI and
// the payload appended after its end, as a phone (and an attacker) would.
func phonePhoto(t *testing.T, w, h int, orientation uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, picture(w, h), nil); err != nil {
		t.Fatal(err)
	}
	tiff := exifTIFF(orientation)
	seg := []byte{0xFF, 0xE1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(2+6+len(tiff)))
	seg = append(append(seg, "Exif\x00\x00"...), tiff...)
	raw := buf.Bytes()
	out := append(append(append([]byte{}, raw[:2]...), seg...), raw[2:]...)
	return append(out, payload...)
}

func setup(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	disk, err := openDisk(t.TempDir(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = newHandler(disk)
	return srv.URL
}

// send uploads one file and returns the status and the stored result.
func send(t *testing.T, base, name string, data []byte) (int, upload.Result) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	resp, err := http.Post(base+"/photos", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out upload.Response
	if resp.StatusCode != http.StatusCreated {
		return resp.StatusCode, upload.Result{}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Files) != 1 {
		t.Fatalf("response: %v %+v", err, out)
	}
	return resp.StatusCode, out.Files[0]
}

func stored(t *testing.T, base, key string) []byte {
	t.Helper()
	resp, err := http.Get(base + publicPrefix + "/" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return b
}

// assertClean checks that nothing of the phone's metadata or the payload
// is left, and the photo is upright.
func assertClean(t *testing.T, data []byte, w, h int) {
	t.Helper()
	for _, leak := range []string{"Exif", secret, payload} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("stored photo still contains %q", leak)
		}
	}
	info, err := imaging.Probe(bytes.NewReader(data))
	if err != nil || info.Width != w || info.Height != h {
		t.Errorf("stored photo = %+v %v, want %dx%d", info, err, w, h)
	}
}

func TestFormUploadStripsMetadata(t *testing.T) {
	base := setup(t)
	in := phonePhoto(t, 300, 200, 6) // stored landscape, displayed portrait
	if !bytes.Contains(in, []byte(secret)) {
		t.Fatal("fixture has no EXIF")
	}
	code, res := send(t, base, "IMG_0001.jpg", in)
	if code != http.StatusCreated || res.ContentType != "image/jpeg" || !strings.HasSuffix(res.Key, ".jpg") {
		t.Fatalf("upload = %d %+v", code, res)
	}
	assertClean(t, stored(t, base, res.Key), 200, 300)
}

func TestFormatsAndLimits(t *testing.T) {
	base := setup(t)
	var p bytes.Buffer
	_ = png.Encode(&p, picture(40, 30))
	if code, res := send(t, base, "shot.png", append(p.Bytes(), payload...)); code != http.StatusCreated || res.Filename != "shot.png" || !strings.HasSuffix(res.Key, ".png") {
		t.Errorf("png = %d %+v", code, res)
	} else if got := stored(t, base, res.Key); bytes.Contains(got, []byte(payload)) {
		t.Error("png payload survived")
	}
	var g bytes.Buffer
	_ = gif.Encode(&g, picture(20, 20), nil)
	if code, res := send(t, base, "loop.gif", g.Bytes()); code != http.StatusCreated || !bytes.Equal(stored(t, base, res.Key), g.Bytes()) {
		t.Errorf("gif = %d, want it stored untouched", code)
	}
	if code, _ := send(t, base, "broken.jpg", []byte("\xff\xd8\xff\xe0broken")); code != http.StatusUnsupportedMediaType {
		t.Errorf("undecodable jpeg = %d", code)
	}
	big := append(phonePhoto(t, 10, 10, 1), make([]byte, maxPhotoBytes)...)
	if code, _ := send(t, base, "big.jpg", big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the input cap = %d", code)
	}
}

func TestImportsAreSanitizedToo(t *testing.T) {
	photo := phonePhoto(t, 300, 200, 6)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(photo) }))
	t.Cleanup(remote.Close)
	disk, _ := filesystem.NewDisk("m", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	loopback := func(a netip.Addr) bool { return a.IsLoopback() } // the test server only
	res, err := upload.FromURL(t.Context(), disk, remote.URL+"/IMG_0002.jpg", photoPolicy, upload.FetchOptions{AllowAddr: loopback})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := disk.ReadAll(t.Context(), res.Key)
	assertClean(t, data, 200, 300)
}

func TestSanitizeStored(t *testing.T) {
	ctx := t.Context()
	disk, _ := filesystem.NewDisk("m", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	const direct = "incoming/abc.jpg"
	if _, err := disk.WriteBytes(ctx, direct, phonePhoto(t, 300, 200, 6), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := sanitizeStored(ctx, disk, direct)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := disk.Exists(ctx, direct); ok {
		t.Error("original still stored")
	}
	data, _ := disk.ReadAll(ctx, res.Key)
	assertClean(t, data, 200, 300)
	if !strings.HasPrefix(res.Key, photoDir+"/") {
		t.Errorf("key = %s", res.Key)
	}
}
