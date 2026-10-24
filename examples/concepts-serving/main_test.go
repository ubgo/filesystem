package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks every serving behaviour the page describes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"link: /files/private/invoices/42.pdf?exp&kid&m&sig",
		`GET 200 application/pdf "%PDF-1.7 invoice"`,
		`Range 206 "%PDF"`,
		"If-None-Match 304",
		`tampered 403 "Forbidden"`,
		"eight-day link: invalid_argument",
		"expired 403",
		"signed with the old key 200",
		"html: text/html; charset=utf-8 | attachment | nosniff | default-src 'none'; sandbox",
		"PUT with another type 403",
		"PUT with the signed headers 200, stored 19 bytes as image/png",
		"public logo.png 200 public, max-age=31536000, immutable",
		"public draft.png 404",
		"public missing.png 404",
		"public URL https://cdn.example.com/products/blue%20shirt.jpg",
		"zip 200 attachment; filename=invoices.zip 2026/41.pdf,2026/42.pdf",
		"zip with ../ in a name 400",
		`fs walk invoices/41.pdf,invoices/42.pdf | read "%PDF-1.7 41" | missing is fs.ErrNotExist: true`,
		"link for 127.0.0.1/32: 200",
		"link for 203.0.113.0/24: 403",
		"non-canonical range: invalid_argument",
		"HEAD signed 200 length=11 type=application/pdf body=0 bytes",
		"HEAD public missing 404",
		`bearer GET with its token: 200 "quarterly"`,
		`bearer GET without a token: 403 "Forbidden"`,
		`bearer GET with an upload token: 403 "Forbidden"`,
		`bearer GET with an expired token: 403 "Forbidden"`,
		`WithURLSigner(cdn) on S3: GET CloudFront link on cdn.example.com, PUT S3 presigned on s3.example.com`,
		`WithURLSigner(cdn, GET, PUT) on S3: GET CloudFront link on cdn.example.com, PUT unsupported`,
		`own handler as bob: 403`,
		`own handler as alice: 200 "Q3 report: revenue up" | nosniff | private, max-age=300`,
		`own handler Range: 206 "Q3" bytes 0-1/21`,
		`own handler If-None-Match: 304`,
		`own handler html asked inline: attachment | default-src 'none'; sandbox`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
