package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the visibility rules the public-and-private page states.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"default: private",
		"after SetVisibility: public",
		"local public: -rw-r--r--",
		"local private: -rw-------",
		"private write on a public bucket: unsupported",
		"SetVisibility on a public bucket: unsupported",
		"public on cdn: https://cdn.example.com/",
		"private on vault: https://app.example.com/files/vault/",
		"still on the CDN disk: false",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
