package filesystem_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

func TestCleanKey(t *testing.T) {
	ok := map[string]string{
		"a.txt":            "a.txt",
		"/a/b.txt":         "a/b.txt",
		"a//b///c":         "a/b/c",
		"./a/./b":          "a/b",
		`a\b\c.txt`:        "a/b/c.txt",
		"a/b/":             "a/b",
		"naïve/résumé.pdf": "naïve/résumé.pdf",
		"a..b/c..":         "a..b/c..",
	}
	for in, want := range ok {
		got, err := filesystem.CleanKey(in)
		if err != nil || got != want {
			t.Errorf("CleanKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "/", ".", "a/../b", "..", "../etc/passwd", `a\..\b`, "a\x00b", "a\nb", "\xff\xfe", strings.Repeat("a", filesystem.MaxKeyLength+1)}
	for _, in := range bad {
		if _, err := filesystem.CleanKey(in); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("CleanKey(%q) err = %v, want invalid_key", in, err)
		}
	}
	if _, err := filesystem.CleanKey(strings.Repeat("a", filesystem.MaxKeyLength)); err != nil {
		t.Errorf("key of exactly MaxKeyLength rejected: %v", err)
	}
}

func TestCleanDir(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", ".": "", "a/b/": "a/b", "/a": "a"} {
		got, err := filesystem.CleanDir(in)
		if err != nil || got != want {
			t.Errorf("CleanDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := filesystem.CleanDir("a/../b"); err == nil {
		t.Error("CleanDir accepted a .. segment")
	}
}

func TestKeyHelpers(t *testing.T) {
	if got := filesystem.JoinKey("", "a", "", "b/c"); got != "a/b/c" {
		t.Errorf("JoinKey = %q", got)
	}
	cases := []struct {
		key, dir string
		in       bool
		rel      string
	}{
		{"a/b", "a", true, "b"},
		{"a", "a", false, "a"},
		{"ab/c", "a", false, "ab/c"},
		{"x", "", true, "x"},
		{"", "", false, ""},
	}
	for _, c := range cases {
		if got := filesystem.KeyInDir(c.key, c.dir); got != c.in {
			t.Errorf("KeyInDir(%q, %q) = %v", c.key, c.dir, got)
		}
		if got := filesystem.RelativeKey(c.key, c.dir); got != c.rel {
			t.Errorf("RelativeKey(%q, %q) = %q, want %q", c.key, c.dir, got, c.rel)
		}
	}
	if got := filesystem.EscapeKey("a b/ü?#.txt"); got != "a%20b/%C3%BC%3F%23.txt" {
		t.Errorf("EscapeKey = %q", got)
	}
}

// An invalid_key message reaches clients, so it names the reason and never
// echoes the key.
func TestInvalidKeyMessageOmitsTheKey(t *testing.T) {
	_, err := filesystem.CleanKey("tenants/acme/../globex/secret.txt")
	var fe *filesystem.Error
	if !errors.As(err, &fe) || fe.Code != filesystem.CodeInvalidKey || strings.Contains(fe.Err.Error(), "globex") || !strings.Contains(fe.Err.Error(), `".."`) {
		t.Errorf("cause %v", err)
	}
}
