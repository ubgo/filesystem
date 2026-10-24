package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun runs the tour and checks every step reported what the concept
// page says it does.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"wrote docs/hello.txt: 14 bytes, text/plain; charset=utf-8, sha256 60cf5d8443f5",
		`read "hello, storage"`,
		`range 7+7: "storage"`,
		"visibility private",
		"stat missing: not_found",
		"exists missing: false",
		"recursive: docs/a.txt docs/b.txt docs/hello.txt docs/img/logo.png",
		`page 1: docs/a.txt docs/b.txt next="docs/b.txt"`,
		`page 2: docs/hello.txt docs/img/ next=""`,
		"copied and moved to archive/hello.txt (old key exists: false)",
		"copy onto itself: invalid_argument",
		"deleted a missing key: ok",
		"delete the root: invalid_argument",
		"left: docs/hello.txt docs/img/logo.png",
		"multipart: 3 parts listed, video/clip.bin is 10 bytes",
		"append past 32 bytes: too_large",
		"log: 25 bytes, modified 2026-01-02T03:04:05Z",
		"read if changed: not_modified",
		"write with a stale etag: conflict",
		`read if changed: {"v":2} (etag is the new one: true)`,
		`temp file: extension .txt, "hello, storage"`,
		"temp file over its cap: too_large",
		"cancelled write: unavailable, is context.Canceled: true, stored: false",
		`fs.ReadFile: "hello, storage"`,
		"bad option at start-up: invalid_argument",
		"check: ok",
		"as *os.Root: true, rooted at the disk's directory: true",
		"as *strings.Builder: false",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
