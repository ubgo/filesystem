package imaging

import (
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
)

// XMPProperty names one XMP property by its namespace URI and local name
// (the prefix a file uses is its own choice, so it is not part of the
// name): XMPWebStatement is the namespace
// "http://ns.adobe.com/xap/1.0/rights/" and the name "WebStatement".
//
// Naming properties in OptimizeOptions.KeepXMP or Transform.KeepXMP
// keeps them in an XMP packet rebuilt from scratch: only their values
// are copied, as text, never the source's XML, so nothing the caller did
// not name survives. Properties that can locate where a photo was taken
// are never kept, whoever names them (see CheckXMP).
type XMPProperty struct{ Namespace, Name string }

// String is the property as "<namespace>Name".
func (p XMPProperty) String() string { return "<" + p.Namespace + ">" + p.Name }

// XMP namespaces of the properties named below.
const (
	XMPNamespaceRights    = "http://ns.adobe.com/xap/1.0/rights/"
	XMPNamespaceDC        = "http://purl.org/dc/elements/1.1/"
	XMPNamespacePLUS      = "http://ns.useplus.org/ldf/xmp/1.0/"
	XMPNamespaceCC        = "http://creativecommons.org/ns#"
	XMPNamespacePhotoshop = "http://ns.adobe.com/photoshop/1.0/"
	XMPNamespaceIPTCCore  = "http://iptc.org/std/Iptc4xmpCore/1.0/xmlns/"
	XMPNamespaceIPTCExt   = "http://iptc.org/std/Iptc4xmpExt/2008-02-29/"
)

// Licensing and credit properties an application commonly keeps: the
// IPTC fields Google Images reads for its "Licensable" badge
// (WebStatement, Licensor, Credit, Creator, Rights), Creative Commons'
// license, and the usage terms.
var (
	XMPWebStatement = XMPProperty{XMPNamespaceRights, "WebStatement"}
	XMPUsageTerms   = XMPProperty{XMPNamespaceRights, "UsageTerms"}
	XMPMarked       = XMPProperty{XMPNamespaceRights, "Marked"}
	XMPLicensor     = XMPProperty{XMPNamespacePLUS, "Licensor"}
	XMPCCLicense    = XMPProperty{XMPNamespaceCC, "license"}
	XMPRights       = XMPProperty{XMPNamespaceDC, "rights"}
	XMPCreator      = XMPProperty{XMPNamespaceDC, "creator"}
	XMPDescription  = XMPProperty{XMPNamespaceDC, "description"}
	XMPCredit       = XMPProperty{XMPNamespacePhotoshop, "Credit"}
)

// xmpLocating are the namespaces whose every property may locate the
// photo (EXIF holds the GPS position) and the properties of other
// namespaces that name a place. CheckXMP refuses them, and a struct's
// fields among them are dropped, so no allowlist can keep a location.
var (
	xmpLocatingNamespaces = []string{
		"http://ns.adobe.com/exif/1.0/", "http://ns.adobe.com/exif/1.0/aux/",
		"http://cipa.jp/exif/1.0/", "http://ns.adobe.com/tiff/1.0/",
	}
	xmpLocatingProperties = []XMPProperty{
		{XMPNamespacePhotoshop, "City"}, {XMPNamespacePhotoshop, "State"}, {XMPNamespacePhotoshop, "Country"},
		{XMPNamespaceIPTCCore, "Location"}, {XMPNamespaceIPTCCore, "CountryCode"},
		{XMPNamespaceIPTCExt, "LocationCreated"}, {XMPNamespaceIPTCExt, "LocationShown"},
	}
)

// XMP packet framing.
const (
	nsRDF = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	nsXML = "http://www.w3.org/XML/1998/namespace"
	// jpegXMPHeader starts the APP1 segment that holds a JPEG's XMP.
	jpegXMPHeader = "http://ns.adobe.com/xap/1.0/\x00"
	// pngXMPKeyword is the iTXt keyword of a PNG's XMP.
	pngXMPKeyword = "XML:com.adobe.xmp"
	// maxXMPBytes bounds the XMP packet read from a source, compressed
	// or not; a larger one is not read and keeps nothing.
	maxXMPBytes = 1 << 20
	// maxXMPDepth bounds the element nesting read.
	maxXMPDepth = 32
	// maxXMPName bounds a property's name, maxXMPNamespace its namespace.
	maxXMPName      = 64
	maxXMPNamespace = 256
)

// xmpNamePattern is an XML local name as a property may have.
var xmpNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)

// xmpLocating reports whether p may locate the photo.
func xmpLocating(p XMPProperty) bool {
	return slices.Contains(xmpLocatingNamespaces, p.Namespace) || slices.Contains(xmpLocatingProperties, p) ||
		strings.Contains(strings.ToLower(p.Name), "gps")
}

// CheckXMP refuses an XMP allowlist that names a property that may
// locate the photo (any EXIF or TIFF property, where the GPS position
// lives; a city, state, country or location property; any name holding
// "GPS"), or a malformed name or namespace: CodeInvalidArgument. GPS is
// never kept, whatever an allowlist says.
func CheckXMP(props []XMPProperty) error {
	for _, p := range props {
		switch {
		case p.Namespace == "" || len(p.Namespace) > maxXMPNamespace || strings.ContainsAny(p.Namespace, "\"<>&' \t\r\n"):
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: XMP namespace %q is not a namespace URI", p.Namespace)
		case len(p.Name) > maxXMPName || !xmpNamePattern.MatchString(p.Name):
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: XMP property name %q is not a name", p.Name)
		case xmpLocating(p):
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: XMP property %s may locate the photo and is never kept", p)
		}
	}
	return nil
}

// xmpValue is one kept property's value: text, a resource URI, a struct
// of simple fields, or an array (Bag, Seq or Alt) of text or structs.
type xmpValue struct {
	text     string
	resource string
	fields   []xmpField
	isStruct bool
	array    string // "Bag", "Seq" or "Alt"; "" for a single value
	items    []xmpItem
}

// xmpField is one field of a struct value.
type xmpField struct {
	prop           XMPProperty
	text, resource string
}

// xmpItem is one array item: text with its language, or a struct.
type xmpItem struct {
	text, lang string
	fields     []xmpField
	isStruct   bool
}

// xmpElem is a parsed XML element: just what reading RDF needs.
type xmpElem struct {
	name     xml.Name
	attrs    []xml.Attr
	children []*xmpElem
	text     strings.Builder
}

func (e *xmpElem) attr(space, local string) (string, bool) {
	for _, a := range e.attrs {
		if a.Name.Space == space && a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// parseXMP reads an XMP packet into a tree; nil when it is not
// well-formed XML within the bounds.
func parseXMP(data []byte) *xmpElem {
	if len(data) > maxXMPBytes {
		return nil
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	root := &xmpElem{}
	stack := []*xmpElem{root}
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > maxXMPDepth {
				return nil
			}
			e := &xmpElem{name: t.Name, attrs: slices.Clone(t.Attr)}
			top.children = append(top.children, e)
			stack = append(stack, e)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			top.text.Write(t)
		}
	}
	return root
}

// keptXMP returns the values of the properties in keep found in an XMP
// packet; nil when there are none or the packet cannot be read.
func keptXMP(packet []byte, keep []XMPProperty) map[XMPProperty]xmpValue {
	root := parseXMP(packet)
	if root == nil {
		return nil
	}
	found := map[XMPProperty]xmpValue{}
	// Only the top-level descriptions (children of rdf:RDF) hold the
	// image's properties; one nested in a value is that value's.
	var walk func(e *xmpElem)
	walk = func(e *xmpElem) {
		if e.name.Space == nsRDF && e.name.Local == "RDF" {
			for _, d := range e.children {
				if d.name.Space == nsRDF && d.name.Local == "Description" {
					readXMPDescription(d, keep, found)
				}
			}
			return
		}
		for _, c := range e.children {
			walk(c)
		}
	}
	walk(root)
	if len(found) == 0 {
		return nil
	}
	return found
}

// readXMPDescription adds to found the values of keep's properties that
// one rdf:Description holds, as attributes or child elements; the first
// value of a property found wins.
func readXMPDescription(e *xmpElem, keep []XMPProperty, found map[XMPProperty]xmpValue) {
	for _, a := range e.attrs {
		p := XMPProperty{a.Name.Space, a.Name.Local}
		if _, done := found[p]; !done && slices.Contains(keep, p) {
			found[p] = xmpValue{text: a.Value}
		}
	}
	for _, c := range e.children {
		p := XMPProperty{c.name.Space, c.name.Local}
		if _, done := found[p]; done || !slices.Contains(keep, p) {
			continue
		}
		if v, ok := readXMPValue(c); ok {
			found[p] = v
		}
	}
}

// readXMPValue reads a property element's value; ok is false for a
// shape it does not read (nested structs, qualifiers), which is dropped.
func readXMPValue(e *xmpElem) (xmpValue, bool) {
	if r, ok := e.attr(nsRDF, "resource"); ok {
		return xmpValue{resource: r}, true
	}
	if pt, _ := e.attr(nsRDF, "parseType"); pt == "Resource" {
		return xmpValue{isStruct: true, fields: readXMPFields(e)}, true
	}
	if len(e.children) == 0 {
		return xmpValue{text: e.text.String()}, true
	}
	if len(e.children) != 1 {
		return xmpValue{}, false
	}
	c := e.children[0]
	switch {
	case c.name.Space == nsRDF && (c.name.Local == "Bag" || c.name.Local == "Seq" || c.name.Local == "Alt"):
		v := xmpValue{array: c.name.Local}
		for _, li := range c.children {
			if li.name.Space != nsRDF || li.name.Local != "li" {
				continue
			}
			lang, _ := li.attr(nsXML, "lang")
			if item, ok := readXMPItem(li); ok {
				item.lang = lang
				v.items = append(v.items, item)
			}
		}
		return v, true
	case c.name.Space == nsRDF && c.name.Local == "Description":
		return xmpValue{isStruct: true, fields: readXMPFields(c)}, true
	}
	return xmpValue{}, false
}

// readXMPItem reads one rdf:li: text, or a struct of simple fields.
func readXMPItem(li *xmpElem) (xmpItem, bool) {
	if pt, _ := li.attr(nsRDF, "parseType"); pt == "Resource" {
		return xmpItem{isStruct: true, fields: readXMPFields(li)}, true
	}
	if len(li.children) == 0 {
		return xmpItem{text: li.text.String()}, true
	}
	if len(li.children) == 1 && li.children[0].name.Space == nsRDF && li.children[0].name.Local == "Description" {
		return xmpItem{isStruct: true, fields: readXMPFields(li.children[0])}, true
	}
	return xmpItem{}, false
}

// readXMPFields reads a struct's simple fields, from attributes and
// child elements; a field that may locate the photo, or that is not
// simple text or a resource, is dropped.
func readXMPFields(e *xmpElem) []xmpField {
	var out []xmpField
	for _, a := range e.attrs {
		p := XMPProperty{a.Name.Space, a.Name.Local}
		if p.fieldOK() {
			out = append(out, xmpField{prop: p, text: a.Value})
		}
	}
	for _, c := range e.children {
		p := XMPProperty{c.name.Space, c.name.Local}
		if !p.fieldOK() || len(c.children) != 0 {
			continue
		}
		f := xmpField{prop: p, text: c.text.String()}
		if r, ok := c.attr(nsRDF, "resource"); ok {
			f = xmpField{prop: p, resource: r}
		}
		out = append(out, f)
	}
	return out
}

// fieldOK reports whether p is a field a kept struct may carry: not
// RDF's or XML's own attributes, and a name CheckXMP accepts, so never
// one that may locate the photo.
func (p XMPProperty) fieldOK() bool {
	return p.Namespace != "" && p.Namespace != nsRDF && p.Namespace != nsXML && p.Namespace != "xmlns" &&
		CheckXMP([]XMPProperty{p}) == nil
}

// xmpPrefixes are the prefixes a rebuilt packet uses for well-known
// namespaces; any other gets "ns<n>".
var xmpPrefixes = map[string]string{
	XMPNamespaceRights: "xmpRights", XMPNamespaceDC: "dc", XMPNamespacePLUS: "plus", XMPNamespaceCC: "cc",
	XMPNamespacePhotoshop: "photoshop", XMPNamespaceIPTCCore: "Iptc4xmpCore", XMPNamespaceIPTCExt: "Iptc4xmpExt",
}

// buildXMP writes a new XMP packet holding the found values of keep, in
// keep's order, and nothing else; nil when none was found. Every value is
// escaped text: nothing of the source's XML is copied.
func buildXMP(found map[XMPProperty]xmpValue, keep []XMPProperty) []byte {
	if len(found) == 0 {
		return nil
	}
	prefixes := map[string]string{}
	prefix := func(ns string) string {
		if p, ok := prefixes[ns]; ok {
			return p
		}
		p, ok := xmpPrefixes[ns]
		if !ok {
			p = "ns" + strconv.Itoa(len(prefixes)+1)
		}
		prefixes[ns] = p
		return p
	}
	qname := func(p XMPProperty) string { return prefix(p.Namespace) + ":" + p.Name }
	var body strings.Builder
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails a write
		return b.String()
	}
	writeFields := func(fields []xmpField) {
		for _, f := range fields {
			if f.resource != "" {
				body.WriteString("<" + qname(f.prop) + " rdf:resource=\"" + esc(f.resource) + "\"/>")
				continue
			}
			body.WriteString("<" + qname(f.prop) + ">" + esc(f.text) + "</" + qname(f.prop) + ">")
		}
	}
	for _, p := range keep {
		v, ok := found[p]
		if !ok {
			continue
		}
		name := qname(p)
		switch {
		case v.resource != "":
			body.WriteString("<" + name + " rdf:resource=\"" + esc(v.resource) + "\"/>")
		case v.isStruct:
			body.WriteString("<" + name + " rdf:parseType=\"Resource\">")
			writeFields(v.fields)
			body.WriteString("</" + name + ">")
		case v.array != "":
			body.WriteString("<" + name + "><rdf:" + v.array + ">")
			for _, it := range v.items {
				switch {
				case it.isStruct:
					body.WriteString("<rdf:li rdf:parseType=\"Resource\">")
					writeFields(it.fields)
					body.WriteString("</rdf:li>")
				case it.lang != "":
					body.WriteString("<rdf:li xml:lang=\"" + esc(it.lang) + "\">" + esc(it.text) + "</rdf:li>")
				default:
					body.WriteString("<rdf:li>" + esc(it.text) + "</rdf:li>")
				}
			}
			body.WriteString("</rdf:" + v.array + "></" + name + ">")
		default:
			body.WriteString("<" + name + ">" + esc(v.text) + "</" + name + ">")
		}
	}
	var out strings.Builder
	out.WriteString("<?xpacket begin=\"\ufeff\" id=\"W5M0MpCehiHzreSzNTczkc9d\"?>")
	out.WriteString("<x:xmpmeta xmlns:x=\"adobe:ns:meta/\"><rdf:RDF xmlns:rdf=\"" + nsRDF + "\"><rdf:Description rdf:about=\"\"")
	nss := make([]string, 0, len(prefixes))
	for ns := range prefixes {
		nss = append(nss, ns)
	}
	slices.Sort(nss)
	for _, ns := range nss {
		out.WriteString(" xmlns:" + prefixes[ns] + "=\"" + esc(ns) + "\"")
	}
	out.WriteString(">" + body.String() + "</rdf:Description></rdf:RDF></x:xmpmeta><?xpacket end=\"r\"?>")
	return []byte(out.String())
}

// rebuiltXMP is the XMP packet keep keeps of a source packet, or nil.
func rebuiltXMP(packet []byte, keep []XMPProperty) []byte {
	if len(keep) == 0 {
		return nil
	}
	return buildXMP(keptXMP(packet, keep), keep)
}

// jpegXMPSegment returns a new APP1 segment holding the rebuilt XMP of an
// XMP APP1 body, or nil (none kept, or too large for one segment).
func jpegXMPSegment(body []byte, keep []XMPProperty) []byte {
	packet, ok := bytes.CutPrefix(body, []byte(jpegXMPHeader))
	if !ok {
		return nil
	}
	x := rebuiltXMP(packet, keep)
	if x == nil || len(jpegXMPHeader)+len(x)+jpegLengthBytes > maxJPEGSegment {
		return nil
	}
	return jpegSegmentOf(jpegAPP1, append([]byte(jpegXMPHeader), x...))
}

// An iTXt chunk's data is its keyword and a NUL, then iTXtFlagBytes (the
// compression flag and method), then iTXtTextFields NUL-terminated texts
// (the language tag and the translated keyword), then the text itself.
const (
	iTXtFlagBytes  = 2
	iTXtTextFields = 2
)

// isPNGXMP reports whether an iTXt chunk's data is a PNG's XMP.
func isPNGXMP(data []byte) bool {
	return bytes.HasPrefix(data, append([]byte(pngXMPKeyword), 0))
}

// pngXMPChunk returns a new iTXt chunk holding the rebuilt XMP of an
// iTXt chunk's data when it is the XMP one, or nil.
func pngXMPChunk(data []byte, keep []XMPProperty) []byte {
	keyword, rest, ok := bytes.Cut(data, []byte{0})
	if !ok || string(keyword) != pngXMPKeyword || len(rest) < iTXtFlagBytes {
		return nil
	}
	compressed, method := rest[0], rest[1]
	rest = rest[iTXtFlagBytes:]
	// The language tag and the translated keyword, each NUL-terminated.
	for range iTXtTextFields {
		if _, rest, ok = bytes.Cut(rest, []byte{0}); !ok {
			return nil
		}
	}
	packet := rest
	if compressed == 1 {
		if method != 0 {
			return nil
		}
		zr, err := zlib.NewReader(bytes.NewReader(rest))
		if err != nil {
			return nil
		}
		if packet, err = io.ReadAll(io.LimitReader(zr, maxXMPBytes+1)); err != nil {
			return nil
		}
	}
	x := rebuiltXMP(packet, keep)
	if x == nil {
		return nil
	}
	chunk := append([]byte(pngXMPKeyword), 0, 0, 0, 0, 0)
	return pngChunkOf("iTXt", append(chunk, x...))
}
