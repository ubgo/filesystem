package imaging

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// sourceXMP is an XMP packet with licensing properties beside location
// ones: a GPS position, a city, a struct field holding a GPS position,
// and properties nobody names (usage terms, a description).
const sourceXMP = `<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
<rdf:Description rdf:about="" xmlns:xmpRights="http://ns.adobe.com/xap/1.0/rights/" xmlns:exif="http://ns.adobe.com/exif/1.0/"
  xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" xmlns:plus="http://ns.useplus.org/ldf/xmp/1.0/"
  xmlns:cc="http://creativecommons.org/ns#" xmlns:dc="http://purl.org/dc/elements/1.1/"
  xmpRights:WebStatement="https://photos.example/license?a=1&amp;b=2" xmpRights:UsageTerms="SECRET-TERMS"
  exif:GPSLatitude="48,51.5N" photoshop:City="Paris">
 <plus:Licensor><rdf:Seq><rdf:li rdf:parseType="Resource">
   <plus:LicensorName>Aman Bansal</plus:LicensorName>
   <plus:LicensorURL>https://photos.example/licensing</plus:LicensorURL>
   <exif:GPSLongitude>2,17.7E</exif:GPSLongitude>
 </rdf:li></rdf:Seq></plus:Licensor>
 <cc:license rdf:resource="https://creativecommons.org/licenses/by/4.0/"/>
 <dc:rights><rdf:Alt><rdf:li xml:lang="x-default">(c) Aman &lt;Bansal&gt;</rdf:li></rdf:Alt></dc:rights>
 <dc:description><rdf:Alt><rdf:li xml:lang="x-default">SECRET-CAPTION</rdf:li></rdf:Alt></dc:description>
 <exif:GPSAltitude>35</exif:GPSAltitude>
</rdf:Description></rdf:RDF></x:xmpmeta><?xpacket end="r"?>`

// keptProps is the allowlist the tests keep.
var keptProps = []XMPProperty{XMPWebStatement, XMPLicensor, XMPCCLicense, XMPRights}

// leaked are strings no rebuilt packet may hold: the location, the
// unnamed properties, and the source's own namespace declarations for
// EXIF.
var leaked = []string{"GPS", "48,51", "2,17", "Paris", "City", "SECRET", "ns.adobe.com/exif"}

// checkRebuilt fails when out lacks the kept values or holds a leaked
// string, and when its XMP does not read back the same values.
func checkRebuilt(t *testing.T, name string, out []byte) {
	t.Helper()
	for _, want := range []string{"https://photos.example/license?a=1&amp;b=2", "Aman Bansal", "https://photos.example/licensing", "creativecommons.org/licenses/by/4.0/", "(c) Aman &lt;Bansal&gt;", "xmpmeta"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("%s: %q was dropped", name, want)
		}
	}
	for _, s := range leaked {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("%s: %q survived", name, s)
		}
	}
	at := bytes.Index(out, []byte("<?xpacket begin"))
	end := bytes.Index(out, []byte(`<?xpacket end="r"?>`))
	if at < 0 || end < at {
		t.Fatalf("%s: no XMP packet", name)
	}
	found := keptXMP(out[at:end+len(`<?xpacket end="r"?>`)], keptProps)
	if len(found) != len(keptProps) || found[XMPWebStatement].text != "https://photos.example/license?a=1&b=2" ||
		found[XMPCCLicense].resource != "https://creativecommons.org/licenses/by/4.0/" ||
		len(found[XMPRights].items) != 1 || found[XMPRights].items[0].lang != "x-default" || found[XMPRights].items[0].text != "(c) Aman <Bansal>" ||
		len(found[XMPLicensor].items) != 1 || len(found[XMPLicensor].items[0].fields) != 2 {
		t.Errorf("%s: the rebuilt packet reads back as %+v", name, found)
	}
}

// pngITXt is an iTXt chunk with the PNG XMP keyword, zlib-compressed
// when compressed is set.
func pngITXt(t *testing.T, packet string, compressed bool) []byte {
	t.Helper()
	text, flag := []byte(packet), byte(0)
	if compressed {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		_, _ = zw.Write(text)
		_ = zw.Close()
		text, flag = z.Bytes(), 1
	}
	return pngChunk("iTXt", pngXMPKeyword+"\x00"+string([]byte{flag, 0})+"\x00\x00"+string(text))
}

// TestOptimizeKeepXMP pins R-I19: a named property's value is kept in an
// XMP packet rebuilt from scratch, in JPEG and in PNG (plain and
// compressed iTXt); nothing unnamed survives, and no location ever does,
// not even a GPS field inside a kept struct; without KeepXMP the XMP goes
// as before; a source without the named properties keeps no XMP.
func TestOptimizeKeepXMP(t *testing.T) {
	jpg := withSegments(encodeJPEG(t, noisy(32, 32), 60), "", segment(jpegAPP1, jpegXMPHeader+sourceXMP))
	out, err := optimize(t, OptimizeOptions{KeepXMP: keptProps}, "image/jpeg", jpg)
	if err != nil {
		t.Fatal(err)
	}
	checkRebuilt(t, "jpeg", out)
	if _, _, _, err := stripJPEG(out, keepSet{}); err != nil {
		t.Fatalf("the output is not a well-formed JPEG: %v", err)
	}
	if out, _ = optimize(t, OptimizeOptions{}, "image/jpeg", jpg); bytes.Contains(out, []byte("xmpmeta")) {
		t.Error("the default kept XMP")
	}
	// Recompressed above the floor: the rebuilt packet is spliced back in.
	high := withSegments(encodeJPEG(t, noisy(64, 64), 95), "", segment(jpegAPP1, jpegXMPHeader+sourceXMP))
	if out, err = optimize(t, OptimizeOptions{Quality: 50, KeepXMP: keptProps}, "image/jpeg", high); err != nil || len(out) >= len(high) {
		t.Fatalf("recompress: %v", err)
	}
	checkRebuilt(t, "recompressed", out)

	for _, compressed := range []bool{false, true} {
		png := withChunks(flatPNG(t, 64, 64), "", pngITXt(t, sourceXMP, compressed))
		out, err := optimize(t, OptimizeOptions{KeepXMP: keptProps}, "image/png", png)
		if err != nil {
			t.Fatal(err)
		}
		checkRebuilt(t, "png", out)
		if out, _ = optimize(t, OptimizeOptions{}, "image/png", png); bytes.Contains(out, []byte("xmpmeta")) {
			t.Error("the PNG default kept XMP")
		}
	}

	none := withSegments(encodeJPEG(t, noisy(32, 32), 60), "", segment(jpegAPP1, jpegXMPHeader+strings.ReplaceAll(strings.ReplaceAll(sourceXMP, "WebStatement", "Other"), "plus:Licensor", "plus:Other")))
	if out, _ = optimize(t, OptimizeOptions{KeepXMP: []XMPProperty{XMPWebStatement, XMPLicensor}}, "image/jpeg", none); bytes.Contains(out, []byte("xmpmeta")) {
		t.Error("a packet without the named properties was kept")
	}
	broken := withSegments(encodeJPEG(t, noisy(32, 32), 60), "", segment(jpegAPP1, jpegXMPHeader+"<x:xmpmeta><rdf:RDF"))
	if out, err = optimize(t, OptimizeOptions{KeepXMP: keptProps}, "image/jpeg", broken); err != nil || bytes.Contains(out, []byte("xmpmeta")) {
		t.Errorf("a malformed packet: %v", err)
	}
}

// TestCheckXMP pins the GPS floor: no allowlist can name a property that
// may locate the photo, and a malformed name or namespace is refused;
// Optimize fails closed on such a list.
func TestCheckXMP(t *testing.T) {
	if err := CheckXMP(append(keptProps, XMPCredit, XMPCreator, XMPProperty{"https://example.com/ns/", "LicenseID"})); err != nil {
		t.Fatalf("a licensing allowlist: %v", err)
	}
	for _, p := range []XMPProperty{
		{"http://ns.adobe.com/exif/1.0/", "GPSLatitude"},
		{"http://ns.adobe.com/exif/1.0/", "DateTimeOriginal"},
		{"http://ns.adobe.com/tiff/1.0/", "Make"},
		{"https://example.com/ns/", "gpsPosition"},
		{XMPNamespacePhotoshop, "City"},
		{XMPNamespaceIPTCExt, "LocationShown"},
		{XMPNamespaceIPTCCore, "Location"},
		{"", "rights"},
		{`http://x/" onload="`, "a"},
		{XMPNamespaceDC, "1bad"},
		{XMPNamespaceDC, "a:b"},
	} {
		if err := CheckXMP([]XMPProperty{p}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%v: %v", p, err)
		}
	}
	jpg := encodeJPEG(t, noisy(8, 8), 60)
	if _, err := optimize(t, OptimizeOptions{KeepXMP: []XMPProperty{{"http://ns.adobe.com/exif/1.0/", "GPSLatitude"}}}, "image/jpeg", jpg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("Optimize with a GPS property: %v", err)
	}
}

// TestKeepCaption pins MetadataCaption: the IPTC caption (2:120) is kept
// on its own, and MetadataCopyright alone still drops it, as before.
func TestKeepCaption(t *testing.T) {
	record := append(append(iim(iptcCopyright, "(c) Aman Bansal"), iim(iptcCaption, "Spring issue cover")...), iim(90, "Paris")...)
	body := append([]byte(photoshopTag), photoshopBlock(photoshopIPTC, record)...)
	jpg := withSegments(encodeJPEG(t, noisy(32, 32), 60), "", segment(jpegAPP13, string(body)))
	out, err := optimize(t, OptimizeOptions{KeepMetadata: []Metadata{MetadataCaption}}, "image/jpeg", jpg)
	if err != nil || !bytes.Contains(out, []byte("Spring issue cover")) || bytes.Contains(out, []byte("Aman")) || bytes.Contains(out, []byte("Paris")) {
		t.Errorf("caption: %v, %q", err, out[:min(len(out), 200)])
	}
	out, err = optimize(t, OptimizeOptions{KeepMetadata: []Metadata{MetadataCopyright}}, "image/jpeg", jpg)
	if err != nil || bytes.Contains(out, []byte("Spring")) || !bytes.Contains(out, []byte("Aman")) {
		t.Errorf("copyright alone: %v", err)
	}
}

// TestProcessKeepMetadata pins R-I18: Transform.KeepMetadata and KeepXMP
// carry the kept metadata into a resized JPEG and PNG, rebuilt as
// Optimize rebuilds it (no GPS, no unnamed property); without them the
// output keeps nothing, as before; an output format this package writes
// no metadata for is CodeUnsupported, an unknown kind and a locating XMP
// property CodeInvalidArgument; a source of another format keeps
// nothing; and the digest changes only when they are set.
func TestProcessKeepMetadata(t *testing.T) {
	exif := exifJPEG(1, binary.BigEndian, "MM")
	exif = exif[2 : len(exif)-4]
	src := withSegments(encodeJPEG(t, noisy(64, 64), 90), "", exif, segment(jpegAPP1, jpegXMPHeader+sourceXMP), photoshopIPTCSegment())
	tr := Transform{Mode: ModeFit, Width: 16, Height: 16, Format: FormatJPEG, KeepMetadata: []Metadata{MetadataCopyright}, KeepXMP: keptProps}
	res, err := Process(bytes.NewReader(src), tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 16 || !bytes.Contains(res.Data, []byte("(c) Aman Bansal")) {
		t.Fatalf("variant %dx%d lacks the copyright", res.Width, res.Height)
	}
	checkRebuilt(t, "variant", res.Data)
	if _, _, _, err := stripJPEG(res.Data, keepSet{}); err != nil {
		t.Fatalf("the variant is not a well-formed JPEG: %v", err)
	}
	plain := tr
	plain.KeepMetadata, plain.KeepXMP = nil, nil
	res, err = Process(bytes.NewReader(src), plain, Options{})
	if err != nil || bytes.Contains(res.Data, []byte("Aman")) || bytes.Contains(res.Data, []byte("xmpmeta")) {
		t.Fatalf("the default kept metadata: %v", err)
	}

	png := withChunks(flatPNG(t, 64, 64), "", pngChunk("tEXt", "Copyright\x00(c) Aman Bansal"), pngChunk("tEXt", "Location\x00Paris"), pngITXt(t, sourceXMP, true))
	pt := Transform{Mode: ModeFit, Width: 16, Height: 16, Format: FormatPNG, KeepMetadata: []Metadata{MetadataCopyright}, KeepXMP: keptProps}
	res, err = Process(bytes.NewReader(png), pt, Options{})
	if err != nil || !bytes.Contains(res.Data, []byte("(c) Aman Bansal")) || bytes.Contains(res.Data, []byte("Paris")) {
		t.Fatalf("png variant: %v", err)
	}
	checkRebuilt(t, "png variant", res.Data)
	cross := pt
	if res, err = Process(bytes.NewReader(src), cross, Options{}); err != nil || bytes.Contains(res.Data, []byte("Aman")) {
		t.Fatalf("a PNG made from a JPEG kept metadata: %v", err)
	}

	gif := tr
	gif.Format = FormatGIF
	if _, err := Process(bytes.NewReader(src), gif, Options{}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("GIF with kept metadata: %v", err)
	}
	for name, bad := range map[string]Transform{
		"kind": {Mode: ModeFit, Width: 16, Height: 16, Format: FormatJPEG, KeepMetadata: []Metadata{"gps"}},
		"gps":  {Mode: ModeFit, Width: 16, Height: 16, Format: FormatJPEG, KeepXMP: []XMPProperty{{"http://ns.adobe.com/exif/1.0/", "GPSLatitude"}}},
	} {
		if _, err := Process(bytes.NewReader(src), bad, Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if plain.Digest() == tr.Digest() {
		t.Error("kept metadata does not change the digest")
	}
	reordered := tr
	reordered.KeepMetadata = []Metadata{MetadataCopyright, MetadataCopyright}
	if reordered.Digest() != tr.Digest() {
		t.Error("a repeated kind changed the digest")
	}
	if got := (Transform{Mode: ModeFit, Width: 16, Height: 16, Format: FormatJPEG}).Digest(); got != plain.Digest() {
		t.Errorf("nil and empty lists digest differently: %s, %s", got, plain.Digest())
	}
}

// TestProcessKeepMetadataNothingToKeep: a source with no metadata of the
// kinds kept makes the same output as a Process keeping nothing, on JPEG
// and on PNG, and a JPEG that decodes but whose segments the keeper
// cannot walk (a fill byte before a marker, which decoders skip) is
// refused with CodeRejected rather than written without its metadata.
func TestProcessKeepMetadataNothingToKeep(t *testing.T) {
	for name, c := range map[string]struct {
		src    []byte
		format Format
	}{
		"jpeg": {encodeJPEG(t, noisy(64, 64), 90), FormatJPEG},
		"png":  {flatPNG(t, 64, 64), FormatPNG},
	} {
		keep := Transform{Mode: ModeFit, Width: 16, Height: 16, Format: c.format, KeepMetadata: []Metadata{MetadataCopyright}, KeepXMP: keptProps}
		plain := keep
		plain.KeepMetadata, plain.KeepXMP = nil, nil
		got, err := Process(bytes.NewReader(c.src), keep, Options{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, err := Process(bytes.NewReader(c.src), plain, Options{})
		if err != nil {
			t.Fatalf("%s plain: %v", name, err)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("%s: keeping absent metadata changed the output (%d bytes, plain %d)", name, len(got.Data), len(want.Data))
		}
	}
	src := encodeJPEG(t, noisy(64, 64), 90)
	filled := append([]byte{jpegMarker, jpegSOI, jpegMarker}, src[2:]...)
	tr := Transform{Mode: ModeFit, Width: 16, Height: 16, Format: FormatJPEG}
	if _, err := Process(bytes.NewReader(filled), tr, Options{}); err != nil {
		t.Fatalf("a JPEG with a fill byte does not decode: %v", err)
	}
	tr.KeepMetadata = []Metadata{MetadataCopyright}
	if _, err := Process(bytes.NewReader(filled), tr, Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("keeping metadata of an unwalkable JPEG = %v", err)
	}
}
