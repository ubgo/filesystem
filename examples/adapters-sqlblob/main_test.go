package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against a fresh SQLite file and checks
// the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"journal mode: wal",
		"capabilities: [copy,move,set-visibility,delete-prefix,delete-keys,metadata,content-type,headers,conditional-write,compare-and-swap]",
		`schema: CREATE TABLE IF NOT EXISTS "avatars" ("key" TEXT NOT NULL, "data" BLOB NOT NULL,`,
		`"modified_ns" INTEGER NOT NULL, PRIMARY KEY ("key"))`,
		`stored: users/42.png 24 bytes image/png metadata=map[uploaded-by:42] cache="public, max-age=86400"`,
		"etag is the sha256: true",
		`range 8+4: "fake"`,
		"swap with a stale etag: conflict",
		"swap with the current etag: ok",
		"swap on a missing key: conflict",
		"create-only over an existing key: exists",
		"over MaxObjectSize: too_large",
		"listed: users/42-backup.png\nlisted: users/42.png\n",
		"rows in the avatars table: 2",
		"under users/: 2 objects, 6 bytes",
		"moved to archive/42.png, old key: not_found",
		"signed URL without a signer: unsupported",
		`signed download: status 200, "new"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
