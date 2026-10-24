package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"wrote docs/hello.txt: 14 bytes, text/plain; charset=utf-8",
		`read back "hello, storage"`,
		"refused a traversal key",
		"listed docs/hello.txt",
		"listed tenants/42/logo.png",
		"copied to the memory disk",
		`downloaded "hello, storage" through a signed URL (status 200)`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
