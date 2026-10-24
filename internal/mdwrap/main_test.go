package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	good := []string{
		"---", "title: x", "wrapped: front", "matter", "---",
		"# Heading", "", "One paragraph on one line.", "",
		"- a bullet", "- another", "  continued indent is a code-ish block", "",
		"| a | b |", "|---|---|", "| 1 | 2 |", "",
		"```", "code can", "wrap freely", "```", "",
		"<Callout>", "", "Inside.", "", "</Callout>", "> quote", "> more", "last line",
	}
	if got := check("good.md", good); len(got) != 0 {
		t.Errorf("false positives: %q", got)
	}
	bad := []string{"This sentence was", "hard wrapped here.", "", "Fine."}
	got := check("bad.md", bad)
	if len(got) != 1 || !strings.HasPrefix(got[0], "bad.md:1:") {
		t.Errorf("findings = %q", got)
	}
}

func TestScanTreeFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "doc.md"), []byte("wrapped\nprose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.md"), []byte("fine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hidden", "x.md"), []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "docsi")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "again")); err != nil {
		t.Fatal(err)
	}
	got, err := scanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "doc.md:1:") {
		t.Errorf("findings = %q", got)
	}
	if _, err := scanTree(filepath.Join(root, "missing")); err == nil {
		t.Error("missing root accepted")
	}
	if _, err := scanFile(filepath.Join(root, "missing.md")); err == nil {
		t.Error("missing file accepted")
	}
}
