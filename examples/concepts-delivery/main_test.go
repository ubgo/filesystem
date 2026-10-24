package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks every delivery behaviour the guide describes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cloudfront URL: https://cdn.example.com/media/videos/42/poster.jpg?Expires&Key-Pair-Id&Signature",
		"cloudfront HTML link: unsupported",
		"cloudfront cookies: CloudFront-Policy,CloudFront-Signature,CloudFront-Key-Pair-Id (Secure true, HttpOnly true)",
		"PrivateKey: [REDACTED]",
		"cloudflare URL: https://downloads.example.com/files/reports/q3.pdf?verify, valid 10m0s",
		"cloudflare link longer than the rule's TTL: invalid_argument",
		`sendfile /logos/acme%20corp.png: 200 X-Accel-Redirect="/protected/logos/acme%20corp.png" body 0 bytes | image/png | nosniff`,
		`sendfile /logos/../../etc/passwd: 400 X-Accel-Redirect=""`,
		"data URI pixel.png: data:image/png;base64,… (114 chars)",
		"data URI logo.svg: rejected",
		"data URI hero.png: too_large",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
