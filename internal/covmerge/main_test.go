package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.out")
	b := filepath.Join(dir, "b.out")
	_ = os.WriteFile(a, []byte("mode: set\nx.go:1.1,2.2 1 0\nx.go:3.1,4.2 1 1\n"), 0o600)
	_ = os.WriteFile(b, []byte("mode: count\nx.go:1.1,2.2 1 5\nx.go:3.1,4.2 1 0\n\n"), 0o600)
	got, err := merge([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := "mode: set\nx.go:1.1,2.2 1 1\nx.go:3.1,4.2 1 1\n"
	if got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}
	bad := filepath.Join(dir, "bad.out")
	_ = os.WriteFile(bad, []byte("nospace\n"), 0o600)
	if _, err := merge([]string{bad}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("malformed profile = %v", err)
	}
	if _, err := merge([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing profile accepted")
	}
}
