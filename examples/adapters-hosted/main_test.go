package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against fresh fakes and checks the
// lines the README and the guides quote.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"public URL: https://storeabc123.public.blob.vercel-storage.com/avatars/u1.png",
		"public URL: https://storeabc123.public.blob.vercel-storage.com/docs/annual%20report.pdf",
		`config prints as: vercelblob.Config{Access: "public", Token: [redacted]}`,
		"stat: avatars/u1.png 29 bytes image/png etag=e1",
		`range read: "PNG"`,
		"list avatars: 2 entries after a server-side copy",
		"after delete: exists=false (ok)",
		"public store capabilities: [copy,delete-keys,public-url,content-type,compare-and-swap]",
		"private store capabilities: [copy,delete-keys,content-type,compare-and-swap]",
		"private store PublicURL: unsupported",
		"private write on the public store: unsupported",
		"IfMatch with the current etag: ok; with the same etag again: conflict",
		"wrong token: permission",
		"rate limited: unavailable (retry after 30s)",
		"over the size limit: too_large",
		`config prints as: cloudinary.Config{CloudName: "acme", APIKey: "123456789012345", APISecret: [redacted], Type: ""}`,
		`write entry type "image/png", stat type "", 29 bytes, etag=version 1`,
		"read back identical: true",
		"after move and copy: avatars/user-1-copy.png, avatars/user-1.png",
		"after DeletePrefix: 0 resources; Admin API calls so far: 6",
		"stored as: raw/upload/docs/annual~20report~20~28v2~29.pdf",
		"listed as: docs/annual report (v2).pdf",
		"public URL path: /acme/raw/upload/docs/annual~20report~20~28v2~29.pdf",
		"brand/ holds 3 resources, the driver lists: brand/terms.txt",
		"upload type capabilities: [move,delete-prefix,delete-keys,public-url]",
		"authenticated type capabilities: [move,delete-prefix,delete-keys]",
		"authenticated PublicURL: unsupported",
		"SignedURL: unsupported",
		"private write on the upload type: unsupported",
		"wrong secret: permission",
		"Admin API limit: unavailable (retry after 30s)",
		"over the raw-upload limit: too_large",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if n := strings.Count(out.String(), "IfNotExists: unsupported"); n != 2 {
		t.Errorf("IfNotExists refused %d times, want 2 (one per driver):\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "secretsecret") || strings.Contains(out.String(), cloudAPISecret) {
		t.Errorf("a credential reached the output:\n%s", out.String())
	}
}
