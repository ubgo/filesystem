package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for k, v := range files {
		p := filepath.Join(dir, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncCommand(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{"photos/a.jpg": "aaa", "photos/sub/b.jpg": "bbb"})
	writeFiles(t, dst, map[string]string{"backup/a.jpg": "aaa", "backup/gone.jpg": "x", "keep/other": "y"})

	// -dry-run -json lists every planned action and changes nothing.
	r := fsRun(t, "", "-json", "sync", "-delete", "-dry-run", "-checksum", loc(src, "photos"), loc(dst, "backup"))
	if r.code != exitOK {
		t.Fatalf("dry run = %+v", r)
	}
	var out syncOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if !out.DryRun || out.Copied != 1 || out.Deleted != 1 || out.Unchanged != 1 || len(out.Items) != 2 {
		t.Fatalf("dry run output %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dst, "backup", "gone.jpg")); err != nil {
		t.Fatalf("dry run deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "backup", "sub", "b.jpg")); !os.IsNotExist(err) {
		t.Fatalf("dry run copied: %v", err)
	}

	// The real run does it; -delete stays inside backup/.
	r = fsRun(t, "", "sync", "-delete", "-checksum", loc(src, "photos"), loc(dst, "backup"))
	if r.code != exitOK || !strings.Contains(r.stdout, "1 copied, 1 deleted, 1 unchanged, 0 failed") {
		t.Fatalf("sync = %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "backup", "sub", "b.jpg")); err != nil || string(b) != "bbb" {
		t.Fatalf("copied object = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep", "other")); err != nil {
		t.Fatalf("sync deleted outside the target: %v", err)
	}

	// Overlapping directories on one DSN are refused.
	if r := fsRun(t, "", "sync", loc(src, "photos"), loc(src, "photos/copy")); r.code != exitFailed {
		t.Fatalf("overlapping sync = %+v", r)
	}
	if r := fsRun(t, "", "sync", loc(src, "photos")); r.code != exitUsage {
		t.Fatalf("sync with one argument = %+v", r)
	}
}

func TestSweepCommand(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"incoming/old.bin": "o", "incoming/new.bin": "n"})
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "incoming", "old.bin"), old, old); err != nil {
		t.Fatal(err)
	}
	r := fsRun(t, "", "-json", "sweep", "-older", "24h", "-dry-run", loc(dir, "incoming"))
	var out sweepOutput
	if r.code != exitOK || json.Unmarshal([]byte(r.stdout), &out) != nil || !out.DryRun || out.Count != 1 || len(out.Keys) != 1 || out.Keys[0] != "incoming/old.bin" {
		t.Fatalf("dry run = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "incoming", "old.bin")); err != nil {
		t.Fatalf("dry run deleted: %v", err)
	}
	if r := fsRun(t, "", "sweep", "-older", "24h", loc(dir, "incoming")); r.code != exitOK || !strings.Contains(r.stdout, "1 deleted") {
		t.Fatalf("sweep = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "incoming", "new.bin")); err != nil {
		t.Fatalf("sweep deleted a new object: %v", err)
	}
	if r := fsRun(t, "", "sweep", "-older", "24h", loc(dir, "")); r.code != exitFailed {
		t.Fatalf("sweep of the root = %+v", r)
	}
	if r := fsRun(t, "", "sweep", loc(dir, "incoming")); r.code != exitUsage {
		t.Fatalf("sweep without -older = %+v", r)
	}
}
