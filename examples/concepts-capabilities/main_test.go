package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the fallbacks, refusals and declarations the
// capabilities page describes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"local:  [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap", // then ",case-insensitive" on a case-folding filesystem such as macOS
		"memory: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]",
		"streamed copy: b.txt 11 bytes, public",
		"deleted two keys one by one: ok",
		"metadata: unsupported",
		"headers: unsupported",
		"signed URL without a signer: unsupported",
		"public URL without a base: unsupported",
		"m.txt stored anyway: false",
		"write after checking Metadata: <nil>",
		"declares capability Copy but does not implement its interface",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
