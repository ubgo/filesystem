package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"capabilities: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]",
		`stat: thumbs/1.json application/json public source=1.png cache-control="public, max-age=60" at 2026-10-03T12:00:00Z`,
		"second conditional write: exists",
		"signed URL without a signer: unsupported",
		"same DSN opened twice: in a=true, in b=false",
		"would sweep: multipart upload for videos/big.mp4",
		"swept 1; the upload is now not_found",
		`versions: 2, oldest holds "draft"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
