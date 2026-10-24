package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks every rule the keys page states.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CleanKey("/avatars//u1.png") -> "avatars/u1.png"`,
		`CleanKey("docs\\2026\\report.pdf") -> "docs/2026/report.pdf"`,
		`CleanKey("./a/./b.txt") -> "a/b.txt"`,
		`CleanKey("photos/") -> "photos"`,
		`CleanKey("a/../b.txt") -> invalid_key`,
		`CleanKey("../../etc/passwd") -> invalid_key`,
		`CleanKey("") -> invalid_key`,
		`CleanKey("bad\x00name.txt") -> invalid_key`,
		"CleanKey(<1025 bytes>) -> invalid_key",
		`CleanDir("/") -> "" <nil>`,
		`CleanDir("/avatars/") -> "avatars" <nil>`,
		"found as avatars/u1.png: true",
		`read with ..: filesystem: read media:avatars/../../secrets.txt: invalid_key`,
		"local .filesystem/tmp/x: invalid_key",
		"local report then report/2026.pdf: invalid_argument",
		"memory report then report/2026.pdf: ok",
		"EscapeKey -> na%C3%AFve%20caf%C3%A9/r%C3%A9sum%C3%A9%20%231.pdf",
		"dirs capability: true, inbox exists: true, keys listed: 0",
		"MakeDir over a file: exists",
		"reserved allowed=false docs/con.txt: invalid_key",
		"reserved allowed=false notes./a.txt: invalid_key",
		"reserved allowed=true docs/con.txt: ok",
		"reserved allowed=true notes./a.txt: ok",
		"NFC normaliser false: composed spelling found: false",
		"NFC normaliser true: composed spelling found: true",
		`escaped "reports/Q3: draft?.txt" -> "reports/Q3%3A draft%3F.txt" -> "reports/Q3: draft?.txt"`,
		"decode a name never written: invalid_key",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
