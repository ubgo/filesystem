package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/sniff"
)

// TestRun walks the whole program in a temporary directory and checks the
// lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"capabilities: [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap", // then ",case-insensitive" on a case-folding filesystem such as macOS
		"site/logo.svg: mode -rw-r--r--, public, image/svg+xml",
		"users/u1/id.pdf: mode -rw-r-----, private, application/pdf",
		"second conditional write: exists",
		`moved: done/job-7 holds "worker-a"`,
		"multipart: exports/data.csv is 14 bytes",
		"staging key: invalid_key",
		"metadata: unsupported",
		"headers: unsupported",
		"write exports/con.txt: invalid_key",
		"write exports/console.txt: ok",
		`signed GET: 200 "signed copy"`,
		"public URL: https://cdn.example.com/media/img/a%20b.png",
		"durability levels: [full file none]",
		"cache write (none): thumbs/p1.txt is 11 bytes",
		"unknown durability: invalid_argument",
		`fixed types: Stat studies/chest.dcm -> "application/dicom"`,
		`fixed types: Stat notes/readme.md -> "text/markdown"`,
		`fixed types: Stat notes/data.unknownext -> ""`,
		`fixed types: List studies/chest.dcm -> "application/dicom"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestAppContentTypeIsUsed pins that the .dcm type can only come from the
// app's table: the fixed table does not know the extension, so a Stat
// that reports application/dicom went through WithContentType.
func TestAppContentTypeIsUsed(t *testing.T) {
	if got := sniff.TypeByExtension("chest.dcm"); got != "" {
		t.Fatalf("sniff now knows .dcm (%q); pick another extension for the demo", got)
	}
	var out bytes.Buffer
	if err := fixedTypes(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Stat studies/chest.dcm -> "application/dicom"`) {
		t.Errorf("app table not used:\n%s", out.String())
	}
}
