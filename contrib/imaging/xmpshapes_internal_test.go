package imaging

import (
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

// exampleNS is a namespace no xmpPrefixes entry names.
const exampleNS = "http://example.com/ns/"

// shapesXMP holds every value shape the reader takes or drops: a struct
// property with attribute, text, resource and nested fields; an array
// with plain, struct, unreadable and non-li children; a struct written as
// one rdf:Description child; properties with one foreign child or two
// children; and a property of an unknown namespace.
const shapesXMP = `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
<rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:plus="http://ns.useplus.org/ldf/xmp/1.0/"
  xmlns:cc="http://creativecommons.org/ns#" xmlns:ex="http://example.com/ns/">
 <plus:Licensor rdf:parseType="Resource" plus:LicensorID="L1">
   <plus:LicensorName>Ann</plus:LicensorName>
   <plus:LicensorURL rdf:resource="https://l.example/"/>
   <plus:Nested><plus:Inner>DEEP</plus:Inner></plus:Nested>
 </plus:Licensor>
 <dc:creator><rdf:Seq>
   <rdf:li>Ann</rdf:li>
   <ex:junk>JUNK</ex:junk>
   <rdf:li><rdf:Description><plus:LicensorName>Bob</plus:LicensorName></rdf:Description></rdf:li>
   <rdf:li><ex:a>TWO</ex:a><ex:b>CHILDREN</ex:b></rdf:li>
 </rdf:Seq></dc:creator>
 <cc:license><rdf:Description><cc:permits>share</cc:permits></rdf:Description></cc:license>
 <dc:description><ex:foo>FOREIGN</ex:foo></dc:description>
 <dc:rights><ex:a>A</ex:a><ex:b>B</ex:b></dc:rights>
 <ex:Custom>val</ex:Custom>
</rdf:Description></rdf:RDF></x:xmpmeta>`

// TestRebuiltXMPShapes pins how each value shape is rebuilt: struct
// fields from attributes, text and resources (a nested field dropped);
// array items as text or structs (non-li children and items of several
// elements dropped); a lone rdf:Description as a struct; a property with
// one foreign child or several children dropped; an unknown namespace
// given an "ns<n>" prefix; and a named property the source lacks skipped.
func TestRebuiltXMPShapes(t *testing.T) {
	custom := XMPProperty{exampleNS, "Custom"}
	keep := []XMPProperty{custom, XMPLicensor, XMPCreator, XMPCCLicense, XMPDescription, XMPRights, XMPCredit}
	out := string(rebuiltXMP([]byte(shapesXMP), keep))
	if parseXMP([]byte(out)) == nil {
		t.Fatalf("the rebuilt packet is not well-formed: %s", out)
	}
	for _, want := range []string{
		`xmlns:ns1="http://example.com/ns/"`, `<ns1:Custom>val</ns1:Custom>`,
		`<plus:Licensor rdf:parseType="Resource"><plus:LicensorID>L1</plus:LicensorID><plus:LicensorName>Ann</plus:LicensorName><plus:LicensorURL rdf:resource="https://l.example/"/></plus:Licensor>`,
		`<dc:creator><rdf:Seq><rdf:li>Ann</rdf:li><rdf:li rdf:parseType="Resource"><plus:LicensorName>Bob</plus:LicensorName></rdf:li></rdf:Seq></dc:creator>`,
		`<cc:license rdf:parseType="Resource"><cc:permits>share</cc:permits></cc:license>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rebuilt packet lacks %s:\n%s", want, out)
		}
	}
	for _, gone := range []string{"DEEP", "JUNK", "TWO", "CHILDREN", "FOREIGN", "dc:description", "dc:rights", "Credit"} {
		if strings.Contains(out, gone) {
			t.Errorf("rebuilt packet holds %s:\n%s", gone, out)
		}
	}
}

// TestXMPBounds: a packet past maxXMPBytes or nested past maxXMPDepth is
// not read (nothing kept), one at the depth bound is; an empty keep list
// rebuilds nothing.
func TestXMPBounds(t *testing.T) {
	if keptXMP(make([]byte, maxXMPBytes+1), keptProps) != nil {
		t.Error("an oversized packet was read")
	}
	nested := func(depth int) []byte {
		return []byte(strings.Repeat("<a>", depth) + strings.Repeat("</a>", depth))
	}
	if parseXMP(nested(maxXMPDepth)) == nil {
		t.Error("a packet at the depth bound was refused")
	}
	if parseXMP(nested(maxXMPDepth+1)) != nil {
		t.Error("a packet past the depth bound was read")
	}
	if rebuiltXMP([]byte(sourceXMP), nil) != nil {
		t.Error("an empty keep list rebuilt a packet")
	}
}

// TestXMPContainersRefused: a JPEG APP1 body without the XMP header, and
// PNG iTXt data that is not a readable XMP chunk (another keyword, too
// short, a missing NUL, an unknown compression method, broken or
// truncated zlib, or no named property), make no segment or chunk.
func TestXMPContainersRefused(t *testing.T) {
	if jpegXMPSegment([]byte("Exif\x00\x00"+sourceXMP), keptProps) != nil {
		t.Error("an APP1 body without the XMP header made a segment")
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write([]byte(sourceXMP))
	_ = zw.Close()
	zipped := z.Bytes()
	head := pngXMPKeyword + "\x00"
	for name, data := range map[string]string{
		"another keyword":    "Comment\x00\x00\x00\x00\x00" + sourceXMP,
		"too short":          head + "\x00",
		"no language NUL":    head + "\x00\x00en",
		"no translated NUL":  head + "\x00\x00en\x00XMP",
		"unknown method":     head + "\x01\x01\x00\x00" + string(zipped),
		"broken zlib":        head + "\x01\x00\x00\x00" + "not zlib",
		"truncated zlib":     head + "\x01\x00\x00\x00" + string(zipped[:len(zipped)/2]),
		"nothing kept":       head + "\x00\x00\x00\x00" + `<x:xmpmeta xmlns:x="adobe:ns:meta/"/>`,
		"compressed nothing": head + "\x01\x00\x00\x00" + string(zipped),
	} {
		keep := keptProps
		if name == "compressed nothing" {
			keep = []XMPProperty{XMPCredit}
		}
		if got := pngXMPChunk([]byte(data), keep); got != nil {
			t.Errorf("%s made a chunk: %q", name, got)
		}
	}
	if pngXMPChunk([]byte(head+"\x01\x00\x00\x00"+string(zipped)), keptProps) == nil {
		t.Error("a well-formed compressed XMP chunk made nothing")
	}
	// A kept value too long for one JPEG segment drops the rebuilt XMP
	// rather than cutting it; a PNG chunk has no such bound and keeps it.
	long := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description xmlns:xmpRights="http://ns.adobe.com/xap/1.0/rights/" xmpRights:WebStatement="https://l.example/` +
		strings.Repeat("a", maxJPEGSegment) + `"/></rdf:RDF></x:xmpmeta>`
	if jpegXMPSegment([]byte(jpegXMPHeader+long), keptProps) != nil {
		t.Error("a rebuilt XMP past one JPEG segment was written")
	}
	if pngXMPChunk([]byte(head+"\x00\x00\x00\x00"+long), keptProps) == nil {
		t.Error("a long kept XMP value was dropped from PNG")
	}
	short := strings.Replace(long, strings.Repeat("a", maxJPEGSegment), "a", 1)
	if jpegXMPSegment([]byte(jpegXMPHeader+short), keptProps) == nil {
		t.Error("a short rebuilt XMP made no JPEG segment")
	}
}
