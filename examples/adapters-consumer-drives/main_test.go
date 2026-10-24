package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks all three drives against fresh fakes and checks every line
// the README and the guides quote.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		"gdrive capabilities: [copy,move,delete-prefix,content-type]",
		"gdrive: read \"buy milk\\n\", stat size=9 etag set=true type=\"text/plain\"",
		"gdrive: stat NOTES/TODO.TXT: not_found",
		"gdrive: list notes: [notes/todo.txt]",
		"gdrive: IfNotExists on an existing key: unsupported",
		"gdrive: IfMatch current etag: unsupported, same etag again: unsupported",
		"gdrive: after copy and move: [archive/2026/todo.txt notes/todo.txt]",
		"gdrive: signed URL: unsupported, public write: unsupported, metadata: unsupported",
		"gdrive: large body 819200 bytes in 4 session requests, read back identical=true",
		"gdrive: IfMatch on a large body: unsupported",
		"gdrive: failing token source: permission, refused token: permission",
		"gdrive: rate limited: unavailable, retry after 7s",
		"dropbox capabilities: [copy,move,delete-prefix,conditional-write,compare-and-swap,case-insensitive]",
		"dropbox: read \"buy milk\\n\", stat size=9 etag set=true type=\"\"",
		"dropbox: stat NOTES/TODO.TXT: ok",
		"dropbox: list notes: [notes/todo.txt]",
		"dropbox: IfNotExists on an existing key: exists",
		"dropbox: IfMatch current etag: ok, same etag again: conflict",
		"dropbox: after copy and move: [archive/2026/todo.txt notes/todo.txt]",
		"dropbox: signed URL: unsupported, public write: unsupported, metadata: unsupported",
		"dropbox: large body 819200 bytes in 4 session requests, read back identical=true",
		"dropbox: IfMatch on a large body: ok",
		"dropbox: failing token source: permission, refused token: permission",
		"dropbox: rate limited: unavailable, retry after 7s",
		"onedrive capabilities: [move,delete-prefix,conditional-write,compare-and-swap,case-insensitive]",
		"onedrive: read \"buy milk\\n\", stat size=9 etag set=true type=\"\"",
		"onedrive: stat NOTES/TODO.TXT: ok",
		"onedrive: list notes: [notes/todo.txt]",
		"onedrive: IfNotExists on an existing key: exists",
		"onedrive: IfMatch current etag: ok, same etag again: conflict",
		"onedrive: after copy and move: [archive/2026/todo.txt notes/todo.txt]",
		"onedrive: signed URL: unsupported, public write: unsupported, metadata: unsupported",
		"onedrive: large body 819200 bytes in 3 session requests, read back identical=true",
		"onedrive: IfMatch on a large body: unsupported",
		"onedrive: failing token source: permission, refused token: permission",
		"onedrive: rate limited: unavailable, retry after 7s",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), out.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %s\nwant %s", i+1, got[i], want[i])
		}
	}
}
