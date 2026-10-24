// Package svg makes user-uploaded SVG safe to store and serve: a pure-Go
// sanitiser that rebuilds a document from an allowlist of SVG elements and
// attributes, so scripts, event handlers, foreignObject, external
// references and everything else a browser could run or fetch are gone.
//
// SVG is an image format that is also a document format: a browser
// opening an uploaded SVG from the application's origin runs its
// <script> elements and onload handlers with the user's session, which is
// why the upload package refuses SVG by default. Sanitize removes that:
//
//   - Elements outside the allowlist are dropped with everything inside
//     them (script, foreignObject, iframe, metadata, editor namespaces);
//     <a> is unwrapped (its children kept, the link dropped).
//   - Attributes outside the allowlist are dropped, every on* handler
//     among them, and so are values that reach outside the document:
//     href and xlink:href other than "#fragment" (or a raster data: URI
//     with AllowDataImages), url(...) other than url(#fragment), and
//     anything naming javascript:.
//   - CSS (style attributes and <style> elements) is kept only when it has
//     no @import, no url() outside the document, no escapes and no
//     legacy script hooks; otherwise the whole rule set is dropped.
//   - Animation elements that would animate href or an event handler are
//     dropped.
//   - DOCTYPEs, entities beyond XML's five, processing instructions and
//     comments are dropped; nothing is fetched or expanded, so XXE and
//     billion-laughs documents cannot do anything.
//
// The input is bounded (MaxBytes, MaxDepth, MaxElements), and the output
// is a fresh document with one svg root in the SVG namespace. It is a
// module of its own so the core's sniffing never grows an XML rewriter.
package svg

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
)

// ContentType is the type the package acts on.
const ContentType = "image/svg+xml"

// Namespaces the sanitiser recognises.
const (
	NamespaceSVG   = "http://www.w3.org/2000/svg"
	NamespaceXLink = "http://www.w3.org/1999/xlink"
	NamespaceXML   = "http://www.w3.org/XML/1998/namespace"
)

// Default limits, used when Options leaves a field zero.
const (
	DefaultMaxBytes    int64 = 10 << 20
	DefaultMaxDepth          = 256
	DefaultMaxElements       = 100_000
)

// Options bound and tune Sanitize. The zero value is safe.
type Options struct {
	// MaxBytes bounds the input; 0 means DefaultMaxBytes. More is
	// CodeTooLarge.
	MaxBytes int64
	// MaxDepth bounds element nesting; 0 means DefaultMaxDepth.
	MaxDepth int
	// MaxElements bounds the elements read; 0 means DefaultMaxElements.
	MaxElements int
	// AllowDataImages keeps href values that are base64 data: URIs of
	// PNG, JPEG, GIF or WebP images (an embedded raster in an <image>),
	// which design tools often write. Never SVG or HTML data: URIs.
	AllowDataImages bool
	// DenyElements names extra elements to drop with their content, on
	// top of the built-in allowlist: an app that must not render
	// animation can deny "animate", "animateMotion", "animateTransform"
	// and "set"; one that bans raster embeds can deny "image" and
	// "feImage". Names are matched as SVG spells them, case included,
	// like the allowlist. The list can only narrow what is kept: there is
	// deliberately no option to allow an element outside the allowlist,
	// because every element left out of it (script, foreignObject,
	// iframe and the like) can run code or embed HTML. Denying "svg"
	// drops nested svg elements; the root element is always kept, since
	// the document is refused without it.
	DenyElements []string
	// DenyAttributes names extra attributes to drop, on top of the
	// built-in allowlist, such as "style" (to refuse inline CSS) or
	// "href" and "xlink:href" (to refuse even in-document links). Names
	// are matched without case, like the allowlist; a namespaced
	// attribute is spelled with its prefix ("xlink:href", "xml:lang").
	// Like DenyElements it can only narrow: no option widens the
	// attribute allowlist, which holds no on* event handler.
	DenyAttributes []string
}

// Report lists what Sanitize removed, one entry per removal, such as
// "element script" or "attribute onload".
type Report struct {
	Removed []string
}

// Clean reports whether nothing was removed.
func (r Report) Clean() bool { return len(r.Removed) == 0 }

// Sanitize reads an SVG document from r and writes the sanitised document
// to w. A document that is not well-formed XML, whose root is not an svg
// element, or that exceeds the limits is refused (CodeRejected,
// CodeTooLarge) before anything is written to w.
func Sanitize(r io.Reader, w io.Writer, opts Options) (Report, error) {
	s := &sanitizer{opts: opts.withDefaults(), denyElements: setOf(opts.DenyElements...), denyAttributes: lowerSetOf(opts.DenyAttributes)}
	if err := s.run(r); err != nil {
		return Report{}, err
	}
	_, err := w.Write(s.out.Bytes())
	return Report{Removed: s.removed}, err
}

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxElements <= 0 {
		o.MaxElements = DefaultMaxElements
	}
	return o
}

// sanitizer holds one run's state.
type sanitizer struct {
	opts Options
	// denyElements and denyAttributes are Options.DenyElements and
	// Options.DenyAttributes as sets; they are consulted after the
	// allowlists, so they can only remove.
	denyElements   map[string]bool
	denyAttributes map[string]bool
	out            bytes.Buffer
	removed        []string
	// stack holds, for every open input element, how it was handled.
	stack []frame
	// skipping counts open elements inside a dropped subtree.
	skipping int
	elements int
	rooted   bool
	// style collects the text of an open <style> element.
	style *strings.Builder
}

// frame is one open element of the input: name is what was written for
// it, or "" for an unwrapped element (its tag dropped, children kept).
type frame struct {
	name string
}

// errSecondRoot refuses a document with more than one root element.
var errSecondRoot = filesystem.Errorf(filesystem.CodeRejected, "svg: the document has more than one root element")

// errNotSVG refuses a document whose root is not svg.
var errNotSVG = filesystem.Errorf(filesystem.CodeRejected, "svg: the document is not an SVG image")

func (s *sanitizer) run(r io.Reader) error {
	lr := &io.LimitedReader{R: r, N: s.opts.MaxBytes + 1}
	dec := xml.NewDecoder(lr)
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if lr.N <= 0 {
			return filesystem.Errorf(filesystem.CodeTooLarge, "svg: larger than %d bytes", s.opts.MaxBytes)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("svg: not well-formed XML: %w", err))
		}
		if err := s.token(tok); err != nil {
			return err
		}
	}
	if !s.rooted || len(s.stack) != 0 {
		return errNotSVG
	}
	return nil
}

// token handles one XML token.
func (s *sanitizer) token(tok xml.Token) error {
	switch t := tok.(type) {
	case xml.StartElement:
		return s.start(t)
	case xml.EndElement:
		s.end()
	case xml.CharData:
		s.text(t)
	case xml.Comment:
		s.remove("comment")
	case xml.ProcInst:
		if t.Target != "xml" {
			s.remove("processing instruction " + t.Target)
		}
	case xml.Directive:
		s.remove("directive")
	}
	return nil
}

func (s *sanitizer) start(t xml.StartElement) error {
	s.elements++
	if s.elements > s.opts.MaxElements {
		return filesystem.Errorf(filesystem.CodeTooLarge, "svg: more than %d elements", s.opts.MaxElements)
	}
	if len(s.stack)+s.skipping >= s.opts.MaxDepth {
		return filesystem.Errorf(filesystem.CodeTooLarge, "svg: nested deeper than %d elements", s.opts.MaxDepth)
	}
	if s.skipping > 0 {
		s.skipping++
		return nil
	}
	name := t.Name.Local
	if !s.rooted {
		if !svgSpace(t.Name.Space) || name != "svg" {
			return errNotSVG
		}
		s.rooted = true
	} else if len(s.stack) == 0 {
		// encoding/xml accepts a second root element; an XML document has
		// one, and a second svg would be written as a second root.
		return errSecondRoot
	}
	if !svgSpace(t.Name.Space) || !allowedElements[name] || (len(s.stack) > 0 && s.denyElements[name]) || unsafeAnimation(name, t.Attr) {
		s.remove("element " + name)
		s.skipping = 1
		return nil
	}
	if unwrapped[name] {
		s.remove("element " + name)
		s.stack = append(s.stack, frame{})
		return nil
	}
	s.out.WriteByte('<')
	s.out.WriteString(name)
	if len(s.stack) == 0 {
		s.out.WriteString(` xmlns="` + NamespaceSVG + `" xmlns:xlink="` + NamespaceXLink + `"`)
	}
	for _, a := range t.Attr {
		s.attr(a)
	}
	s.out.WriteByte('>')
	s.stack = append(s.stack, frame{name: name})
	if name == "style" {
		s.style = &strings.Builder{}
	}
	return nil
}

func (s *sanitizer) end() {
	if s.skipping > 0 {
		s.skipping--
		return
	}
	// The stack is not empty: in strict mode every end matches a start,
	// and every start inside the root is skipped or pushes a frame (a
	// start after the root is refused).
	f := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	if f.name == "" {
		return
	}
	if f.name == "style" && s.style != nil {
		if css := s.style.String(); safeCSS(css) {
			xmlEscape(&s.out, css)
		} else {
			s.remove("style rules")
		}
		s.style = nil
	}
	s.out.WriteString("</")
	s.out.WriteString(f.name)
	s.out.WriteString(">")
}

func (s *sanitizer) text(t xml.CharData) {
	if s.skipping > 0 || len(s.stack) == 0 {
		return
	}
	if s.style != nil {
		s.style.Write(t)
		return
	}
	xmlEscape(&s.out, string(t))
}

// attr writes a if it is allowed and its value safe.
func (s *sanitizer) attr(a xml.Attr) {
	name := strings.ToLower(a.Name.Local)
	var out string
	switch a.Name.Space {
	case "":
		out = a.Name.Local
	case NamespaceXLink, "xlink":
		out = "xlink:" + a.Name.Local
		name = "xlink:" + name
	case NamespaceXML, "xml":
		out = "xml:" + a.Name.Local
		name = "xml:" + name
	case "xmlns":
		return // namespace declarations: the root declares its own
	default:
		s.remove("attribute " + a.Name.Space + ":" + a.Name.Local)
		return
	}
	if a.Name.Space == "" && name == "xmlns" {
		return
	}
	if !allowedAttributes[name] || s.denyAttributes[name] || !s.safeValue(name, a.Value) {
		s.remove("attribute " + out)
		return
	}
	s.out.WriteString(" ")
	s.out.WriteString(out)
	s.out.WriteString(`="`)
	attrEscape(&s.out, a.Value)
	s.out.WriteByte('"')
}

// safeValue reports whether an attribute's value stays inside the
// document and names no script.
func (s *sanitizer) safeValue(name, value string) bool {
	if mentionsScript(value) {
		return false
	}
	switch name {
	case "href", "xlink:href":
		return strings.HasPrefix(strings.TrimSpace(value), "#") || (s.opts.AllowDataImages && dataImage.MatchString(strings.TrimSpace(value)))
	case "style":
		return safeCSS(value)
	}
	return localURLs(value)
}

// remove records a removal for the report.
func (s *sanitizer) remove(what string) { s.removed = append(s.removed, what) }

// svgSpace reports whether an element namespace is SVG; a document
// without a namespace is read as SVG and written with one.
func svgSpace(space string) bool { return space == NamespaceSVG || space == "" }

// unsafeAnimation reports whether an animation element targets href or an
// event handler, which would put a link or script back after sanitising.
func unsafeAnimation(name string, attrs []xml.Attr) bool {
	if !animations[name] {
		return false
	}
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, "attributeName") {
			target := strings.ToLower(strings.TrimSpace(a.Value))
			if _, local, ok := strings.Cut(target, ":"); ok {
				target = local
			}
			if target == "href" || strings.HasPrefix(target, "on") {
				return true
			}
		}
	}
	return false
}

// dataImage matches the raster data: URIs AllowDataImages keeps.
var dataImage = regexp.MustCompile(`(?i)^data:image/(png|jpeg|gif|webp);base64,[a-z0-9+/=\s]*$`)

// controlOrSpace matches what browsers ignore inside a URL scheme, which
// attackers use to split "javascript:".
var controlOrSpace = regexp.MustCompile(`[\x00-\x20\x7f]+`)

// mentionsScript reports whether v names a script URL, however spaced or
// cased, or an HTML entity that could spell one out.
func mentionsScript(v string) bool {
	squeezed := strings.ToLower(controlOrSpace.ReplaceAllString(v, ""))
	return strings.Contains(squeezed, "javascript:") || strings.Contains(squeezed, "vbscript:") || strings.Contains(squeezed, "data:text/html")
}

// urlRef matches url( ... ) references in an attribute value or CSS.
var urlRef = regexp.MustCompile(`(?i)url\s*\(\s*(['"]?)([^'")]*)`)

// localURLs reports whether every url(...) in v points inside the
// document (#fragment).
func localURLs(v string) bool {
	for _, m := range urlRef.FindAllStringSubmatch(v, -1) {
		if !strings.HasPrefix(strings.TrimSpace(m[2]), "#") {
			return false
		}
	}
	return true
}

// cssComment matches CSS comments, removed before checking so they cannot
// hide a construct.
var cssComment = regexp.MustCompile(`/\*[\s\S]*?\*/`)

// unsafeCSSTokens are CSS constructs that load or run something.
var unsafeCSSTokens = []string{"@import", "expression(", "behavior", "-moz-binding", "javascript:", "@font-face", "image-set("}

// safeCSS reports whether css loads nothing from outside the document and
// runs nothing. Escapes are refused outright: they can spell any of the
// checked constructs.
func safeCSS(css string) bool {
	if strings.Contains(css, `\`) || strings.Contains(css, "<") {
		return false
	}
	plain := strings.ToLower(cssComment.ReplaceAllString(css, ""))
	if strings.Contains(plain, "/*") {
		return false // an unterminated comment
	}
	if slices.ContainsFunc(unsafeCSSTokens, func(tok string) bool { return strings.Contains(plain, tok) }) {
		return false
	}
	return localURLs(plain) && !mentionsScript(plain)
}

// textEscaper escapes character data: the characters XML reserves, and
// nothing else, so line breaks stay line breaks.
var textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#xD;")

// attrEscaper escapes a double-quoted attribute value, including the
// whitespace XML would otherwise normalise to spaces.
var attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "\n", "&#xA;", "\r", "&#xD;", "\t", "&#x9;")

// xmlEscape writes character data.
func xmlEscape(b *bytes.Buffer, s string) { _, _ = textEscaper.WriteString(b, s) }

// attrEscape writes an attribute value for a double-quoted attribute.
func attrEscape(b *bytes.Buffer, s string) { _, _ = attrEscaper.WriteString(b, s) }
