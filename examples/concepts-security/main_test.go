package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks every security default the page lists.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"traversal key: invalid_key",
		"detect invoice.pdf -> image/png (active: false)",
		"detect photo.jpg -> text/html; charset=utf-8 (active: true)",
		"detect logo.png -> image/svg+xml (active: true)",
		"detect page.html -> text/plain; charset=utf-8 (active: false)",
		"policy allowing image/* and an SVG: rejected",
		`name shell.php.jpg: "rejected"`,
		`name .htaccess: "rejected"`,
		`name setup.exe: "rejected"`,
		`name holiday.jpg: ""`,
		"the name says application/pdf but the content is image/png",
		"header with CRLF: invalid_argument",
		"PublicAddr(127.0.0.1): false",
		"PublicAddr(169.254.169.254): false",
		"PublicAddr(::ffff:127.0.0.1): false",
		"PublicAddr(64:ff9b::a00:1): false",
		"PublicAddr(93.184.215.14): true",
		"expiry 0s: invalid_argument",
		"expiry 168h0m1s: invalid_argument",
		"5-byte signing key: invalid_argument",
		"signed URL with no signer configured: unsupported",
		`scan (down=false): "rejected" stored=0 filesystem: rejected: scan: the file is infected (Win.Test.EICAR_HDB-1)`,
		`scan (down=true): "unavailable" stored=0`,
		`scan (down=false): "" stored=1 <nil>`,
		"sanitized image/jpeg: payload in upload true, payload stored false",
		`named "photo": stored with extension .png`,
		`named "photo.gif": stored with extension .png`,
		`named "photo.png.exe.jpg": rejected`,
		"TypeByExtension(report.PDF) = application/pdf",
		"Extension(image/jpeg) = .jpg",
		"Matches(image/png, image/*) = true",
		"MatchesAny(application/pdf, [image/* text/*]) = false",
		"IsActive(application/xhtml+xml) = true",
		"detector, scan without the detector: rejected",
		"detector, scan with the detector: stored as application/dicom",
		"detector, unknown binary: rejected",
		"detector, HTML named .dcm: rejected",
		"detector on the disk: Write sniffs application/dicom",
		`key printed: signedurl.Key{ID: "2026-10", Secret: [REDACTED]}`,
		"secret in any print or log: false",
		`DSN error: filesystem: invalid_key: disk media: key contains a ".." segment (password shown: false)`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
