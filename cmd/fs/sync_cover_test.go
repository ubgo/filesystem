package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unknownScheme is a DSN no registered opener serves.
const unknownScheme = "nosuchscheme://x"

// TestSyncCommandOpenFailures: a location that cannot be opened, on
// either side, fails the command before anything is copied.
func TestSyncCommandOpenFailures(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{"a": "1"})
	for _, args := range [][2]string{{unknownScheme + "#a", loc(dst, "")}, {loc(src, ""), unknownScheme + "#b"}} {
		r := fsRun(t, "", "sync", args[0], args[1])
		if r.code != exitFailed || !strings.Contains(r.stderr, "nosuchscheme") {
			t.Errorf("sync %s %s = %+v; want a failure naming the scheme", args[0], args[1], r)
		}
	}
	if entries, err := os.ReadDir(dst); err != nil || len(entries) != 0 {
		t.Fatalf("target changed: %v, %v", entries, err)
	}
}

// TestSyncCommandDryRunText: a dry run without -json prints each planned
// action as "would ..." and the totals, and changes nothing.
func TestSyncCommandDryRunText(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{"a": "1"})
	r := fsRun(t, "", "sync", "-dry-run", loc(src, ""), loc(dst, "copy"))
	if r.code != exitOK || !strings.Contains(r.stdout, "would copy a -> copy/a") || !strings.Contains(r.stdout, "1 copied, 0 deleted, 0 unchanged, 0 failed") {
		t.Fatalf("dry run = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dst, "copy", "a")); !os.IsNotExist(err) {
		t.Fatalf("dry run copied: %v", err)
	}
}

// readOnlyDir makes dir unwritable for the test and writable again after,
// so the temporary directory can be removed.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = f.Close()
		t.Skip("running with privileges that ignore directory permissions")
	}
}

// TestSyncCommandPartialFailure: copies that fail are printed to stderr
// with their cause, counted in the totals, and make the command fail; in
// JSON each failure carries its error and code, and nothing else is
// printed for it.
func TestSyncCommandPartialFailure(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{"a": "1"})
	readOnlyDir(t, filepath.Join(dst, "ro"))

	r := fsRun(t, "", "sync", loc(src, ""), loc(dst, "ro"))
	if r.code != exitFailed || !strings.Contains(r.stderr, "fs: copy a:") || !strings.Contains(r.stdout, "0 copied, 0 deleted, 0 unchanged, 1 failed") {
		t.Fatalf("sync into a read-only directory = %+v", r)
	}

	r = fsRun(t, "", "-json", "sync", loc(src, ""), loc(dst, "ro"))
	var out syncOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("json output %q: %v", r.stdout, err)
	}
	if r.code != exitFailed || r.stderr != "" || out.Failed != 1 || len(out.Items) != 1 || out.Items[0].Error == "" || out.Items[0].Code == "" {
		t.Fatalf("json sync into a read-only directory = %+v, %+v", r, out)
	}
}

// brokenWriter fails every write, as stdout closed by the reader does.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestSyncCommandOutputFailure: when the JSON result cannot be written,
// the command fails even though the sync itself succeeded, so a script
// never takes a missing report for success.
func TestSyncCommandOutputFailure(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{"a": "1"})
	var stderr strings.Builder
	code := run(t.Context(), []string{"-json", "sync", loc(src, ""), loc(dst, "")}, func(string) string { return "" }, strings.NewReader(""), brokenWriter{}, &stderr)
	if code != exitFailed {
		t.Fatalf("exit code %d with a broken stdout, want %d", code, exitFailed)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "a")); err != nil || string(b) != "1" {
		t.Fatalf("the sync itself did not happen: %q, %v", b, err)
	}
}

// TestSweepCommandFailures: an unknown flag is a usage error, a location
// that cannot be opened fails, and a dry run without -json prints each
// object it would delete.
func TestSweepCommandFailures(t *testing.T) {
	if r := fsRun(t, "", "sweep", "-bogus", "-older", "1h", unknownScheme+"#in"); r.code != exitUsage {
		t.Fatalf("sweep with an unknown flag = %+v", r)
	}
	if r := fsRun(t, "", "sweep", "-older", "1h", unknownScheme+"#in"); r.code != exitFailed || !strings.Contains(r.stderr, "nosuchscheme") {
		t.Fatalf("sweep of an unknown scheme = %+v", r)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"in/old": "o"})
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "in", "old"), old, old); err != nil {
		t.Fatal(err)
	}
	if r := fsRun(t, "", "sweep", "-older", "1h", "-dry-run", loc(dir, "in")); r.code != exitOK || r.stdout != "would delete in/old\n" {
		t.Fatalf("text dry run = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "in", "old")); err != nil {
		t.Fatalf("dry run deleted: %v", err)
	}
}
