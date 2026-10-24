package svg_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/svg"
)

// sanitize runs Sanitize with the default options.
func sanitize(t *testing.T, in string) (string, svg.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := svg.Sanitize(strings.NewReader(in), &out, svg.Options{})
	if err != nil {
		t.Fatalf("Sanitize(%q): %v", in, err)
	}
	return out.String(), rep
}

const open = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 10 10">`

// TestSanitize_RemovesActiveContent is the attack list: each input must
// come out without the marker, which only the dangerous part carries.
func TestSanitize_RemovesActiveContent(t *testing.T) {
	const marker = "PWNED"
	for name, body := range map[string]string{
		"script":                     `<script>alert("PWNED")</script>`,
		"script in another case":     `<SCRIPT>alert("PWNED")</SCRIPT>`,
		"script via namespace":       `<h:script xmlns:h="http://www.w3.org/1999/xhtml">PWNED</h:script>`,
		"onload":                     `<rect onload="PWNED()" width="1" height="1"/>`,
		"mixed-case handler":         `<rect OnClick="PWNED()" width="1"/>`,
		"foreignObject":              `<foreignObject><div xmlns="http://www.w3.org/1999/xhtml">PWNED</div></foreignObject>`,
		"iframe":                     `<iframe src="https://evil.example/PWNED"/>`,
		"javascript href":            `<a href="javascript:PWNED()"><rect width="1"/></a>`,
		"spaced javascript href":     `<use href="java&#x09;script:PWNED()"/>`,
		"xlink javascript":           `<a xlink:href="javascript:PWNED()"><text>x</text></a>`,
		"external use":               `<use href="https://evil.example/sprite.svg#PWNED"/>`,
		"external image":             `<image href="https://evil.example/PWNED.png" width="1" height="1"/>`,
		"svg data uri":               `<image href="data:image/svg+xml;base64,PWNED" width="1"/>`,
		"external fill":              `<rect fill="url(https://evil.example/PWNED#g)" width="1"/>`,
		"external feImage":           `<filter id="f"><feImage href="https://evil.example/PWNED"/></filter>`,
		"style import":               `<style>@import url(https://evil.example/PWNED.css);</style>`,
		"style external url":         `<style>rect { fill: url(https://evil.example/PWNED) }</style>`,
		"style escape":               `<style>rect { background: \75 rl(PWNED) }</style>`,
		"unterminated style comment": `<style>rect { fill: red } /* PWNED</style>`,
		"style comment hiding":       `<style>rect { b: u/**/rl(x) } @im/**/port "PWNED";</style>`,
		"style attribute url":        `<rect style="fill: url('https://evil.example/PWNED')" width="1"/>`,
		"style expression":           `<rect style="width: expression(PWNED())"/>`,
		"animate href":               `<a><animate attributeName="href" values="javascript:PWNED()"/><text>x</text></a>`,
		"set xlink:href":             `<set attributeName="xlink:href" to="https://evil.example/PWNED"/>`,
		"set onclick":                `<set attributeName="onclick" to="PWNED()"/>`,
		"animate values javascript":  `<animate attributeName="fill" values="javascript:PWNED"/>`,
		"metadata":                   `<metadata><creator>PWNED</creator></metadata>`,
		"editor namespace":           `<sodipodi:namedview xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd" pagecolor="PWNED"/>`,
		"editor attribute":           `<rect xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape" inkscape:label="PWNED" width="1"/>`,
		"comment":                    `<!-- PWNED -->`,
		"foreign element, SVG name":  `<x:title xmlns:x="urn:other">PWNED</x:title>`,
		"split javascript in value":  `<animate attributeName="fill" values="java&#x0A;script:PWNED"/>`,
		"processing instruction":     `<?php PWNED ?>`,
	} {
		out, rep := sanitize(t, open+body+`</svg>`)
		if strings.Contains(out, marker) {
			t.Errorf("%s: %s", name, out)
		}
		if rep.Clean() {
			t.Errorf("%s: the report lists no removal", name)
		}
	}
}

// TestSanitize_KeepsTheImage pins that an ordinary drawing comes through
// whole: shapes, gradients and their url(#...), use of a symbol, text,
// local styles, filters, a safe animation and accessibility.
func TestSanitize_KeepsTheImage(t *testing.T) {
	in := open + `<title>Logo</title><desc>A blue square</desc>` +
		`<defs><linearGradient id="g"><stop offset="0" stop-color="#00f"/></linearGradient>` +
		`<symbol id="s"><circle cx="5" cy="5" r="2"/></symbol>` +
		`<filter id="f"><feGaussianBlur stdDeviation="1"/></filter></defs>` +
		`<style>/* brand */ .a { fill: url(#g); stroke: #000 } rect > circle { opacity: .5 }</style>` +
		`<rect class="a" width="10" height="10" fill="url(#g)" filter="url(#f)" style="stroke-width: 2"/>` +
		`<use href="#s" xlink:href="#s"/><text x="1" y="9" font-family="sans-serif">Hi &amp; bye</text>` +
		`<animateTransform attributeName="transform" type="rotate" from="0 5 5" to="360 5 5" dur="2s"/>` +
		`<g aria-label="mark" role="img"><path d="M0 0L10 10"/></g><text xml:space="preserve">  a  </text></svg>`
	out, rep := sanitize(t, in)
	if !rep.Clean() {
		t.Fatalf("removed %v from a clean drawing:\n%s", rep.Removed, out)
	}
	for _, want := range []string{`<title>Logo</title>`, `fill="url(#g)"`, `filter="url(#f)"`, `<use href="#s" xlink:href="#s">`, `Hi &amp; bye`, `.a { fill: url(#g); stroke: #000 } rect &gt; circle`, `<animateTransform attributeName="transform"`, `role="img"`, `d="M0 0L10 10"`, `<text xml:space="preserve">  a  </text>`, `viewBox="0 0 10 10"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The output is itself clean: sanitising it again changes nothing.
	again, rep := sanitize(t, out)
	if again != out || !rep.Clean() {
		t.Fatalf("not idempotent: %v\n%s\n%s", rep.Removed, out, again)
	}
}

func TestSanitize_UnwrapsLinks(t *testing.T) {
	out, rep := sanitize(t, open+`<a href="https://example.com"><text>visible</text></a></svg>`)
	if !strings.Contains(out, "<text>visible</text>") || strings.Contains(out, "<a") || strings.Contains(out, "example.com") || rep.Clean() {
		t.Fatalf("out = %s, removed %v", out, rep.Removed)
	}
}

func TestSanitize_DataImages(t *testing.T) {
	const png = `<image href="data:image/png;base64,iVBORw0KGgo=" width="1" height="1"/>`
	if out, _ := sanitize(t, open+png+`</svg>`); strings.Contains(out, "data:") {
		t.Fatalf("a data: image was kept without AllowDataImages: %s", out)
	}
	var out bytes.Buffer
	if _, err := svg.Sanitize(strings.NewReader(open+png+`</svg>`), &out, svg.Options{AllowDataImages: true}); err != nil || !strings.Contains(out.String(), "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("AllowDataImages dropped a PNG: %s %v", out.String(), err)
	}
	out.Reset()
	if _, err := svg.Sanitize(strings.NewReader(open+`<image href="data:image/svg+xml;base64,PHN2Zz4=" width="1"/></svg>`), &out, svg.Options{AllowDataImages: true}); err != nil || strings.Contains(out.String(), "data:") {
		t.Fatalf("AllowDataImages kept an SVG data URI: %s %v", out.String(), err)
	}
}

func TestSanitize_AddsTheNamespace(t *testing.T) {
	out, _ := sanitize(t, `<svg width="1"><rect width="1"/></svg>`)
	if !strings.HasPrefix(out, `<svg xmlns="http://www.w3.org/2000/svg"`) {
		t.Fatalf("out = %s", out)
	}
}

func TestSanitize_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		opts svg.Options
		code filesystem.Code
	}{
		"html":             {`<html><body>x</body></html>`, svg.Options{}, filesystem.CodeRejected},
		"svg not at root":  {`<div><svg/></div>`, svg.Options{}, filesystem.CodeRejected},
		"svg in xhtml ns":  {`<svg xmlns="http://www.w3.org/1999/xhtml"/>`, svg.Options{}, filesystem.CodeRejected},
		"not xml":          {`not xml at all`, svg.Options{}, filesystem.CodeRejected},
		"unclosed":         {`<svg><rect>`, svg.Options{}, filesystem.CodeRejected},
		"external entity":  {`<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg>&x;</svg>`, svg.Options{}, filesystem.CodeRejected},
		"billion laughs":   {`<!DOCTYPE svg [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;">]><svg>&b;</svg>`, svg.Options{}, filesystem.CodeRejected},
		"too large":        {open + strings.Repeat(" ", 200) + `</svg>`, svg.Options{MaxBytes: 100}, filesystem.CodeTooLarge},
		"too deep":         {open + strings.Repeat("<g>", 10) + strings.Repeat("</g>", 10) + `</svg>`, svg.Options{MaxDepth: 5}, filesystem.CodeTooLarge},
		"too many":         {open + strings.Repeat("<rect/>", 20) + `</svg>`, svg.Options{MaxElements: 10}, filesystem.CodeTooLarge},
		"deep in dropped":  {open + `<script>` + strings.Repeat("<g>", 10) + strings.Repeat("</g>", 10) + `</script></svg>`, svg.Options{MaxDepth: 5}, filesystem.CodeTooLarge},
		"two roots":        {open + `</svg><svg/>`, svg.Options{}, filesystem.CodeRejected},
		"non-UTF-8 header": {`<?xml version="1.0" encoding="ISO-8859-1"?><svg/>`, svg.Options{}, filesystem.CodeRejected},
	} {
		var out bytes.Buffer
		_, err := svg.Sanitize(strings.NewReader(tc.in), &out, tc.opts)
		if filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
		if out.Len() != 0 {
			t.Errorf("%s: wrote %q before refusing", name, out.String())
		}
	}
}

// TestSanitize_EscapesValues pins that a value carrying quotes and markup
// cannot break out of its attribute or text in the output.
func TestSanitize_EscapesValues(t *testing.T) {
	out, _ := sanitize(t, open+`<rect id='a"b&lt;c' width="1"/><text>1 &lt; 2 &amp; "q"`+"\n"+`</text></svg>`)
	if !strings.Contains(out, `id="a&quot;b&lt;c"`) || !strings.Contains(out, "1 &lt; 2 &amp; \"q\"\n</text>") {
		t.Fatalf("out = %s", out)
	}
	if again, rep := sanitize(t, out); again != out || !rep.Clean() {
		t.Fatalf("the output does not read back as itself: %s", again)
	}
}

// TestSanitize_DenyNarrows pins the deny lists: a denied element goes
// with its content, a denied attribute goes (matched without case), the
// root survives a deny of "svg", and nothing outside the allowlist comes
// back whatever the lists hold.
func TestSanitize_DenyNarrows(t *testing.T) {
	in := open + `<g STYLE="fill:red"><animate attributeName="opacity" to="0"/><rect width="1" style="stroke:0"/></g>` +
		`<svg><circle r="1"/></svg><script>x</script></svg>`
	opts := svg.Options{DenyElements: []string{"animate", "svg", "script"}, DenyAttributes: []string{"Style"}}
	var out bytes.Buffer
	rep, err := svg.Sanitize(strings.NewReader(in), &out, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, gone := range []string{"<animate", "style=", "STYLE=", "<circle", "<script"} {
		if strings.Contains(got, gone) {
			t.Errorf("output keeps %q:\n%s", gone, got)
		}
	}
	if !strings.HasPrefix(got, "<svg ") || !strings.Contains(got, `<rect width="1">`) || rep.Clean() {
		t.Fatalf("the root or an allowed element went, or nothing was reported: %v\n%s", rep.Removed, got)
	}
	// The default keeps what the deny lists dropped.
	def, _ := sanitize(t, in)
	if !strings.Contains(def, "<animate") || !strings.Contains(def, `style="stroke:0"`) || !strings.Contains(def, "<circle") {
		t.Fatalf("the default changed:\n%s", def)
	}
}
