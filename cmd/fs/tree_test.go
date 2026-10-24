package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// seedTree writes a/x.txt, a/sub/y.txt and the unrelated ab.txt (a sibling
// whose name starts with the source prefix) under dir.
func seedTree(t *testing.T, dir string) {
	t.Helper()
	for _, k := range []string{"a/x.txt", "a/sub/y.txt", "ab.txt"} {
		if r := fsRun(t, "body of "+k, "put", loc(dir, k)); r.code != exitOK {
			t.Fatalf("put %s = %+v", k, r)
		}
	}
}

func exists(dir, key string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	return err == nil
}

func TestCopyTreeAcrossDisks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedTree(t, src)
	r := fsRun(t, "", "cp", "-r", loc(src, "a"), loc(dst, "b"))
	if r.code != exitOK || !strings.Contains(r.stdout, "a/sub/y.txt -> b/sub/y.txt") {
		t.Fatalf("cp -r = %+v", r)
	}
	if r := fsRun(t, "", "cat", loc(dst, "b/sub/y.txt")); r.stdout != "body of a/sub/y.txt" {
		t.Errorf("copied object = %+v", r)
	}
	if !exists(src, "a/x.txt") || exists(dst, "b.txt") || exists(dst, "bb.txt") {
		t.Error("cp -r removed a source or copied a sibling outside the prefix")
	}
	// Into the root of another disk.
	if r := fsRun(t, "", "cp", "-r", loc(src, "a"), loc(dst, "")); r.code != exitOK || !exists(dst, "sub/y.txt") {
		t.Errorf("cp -r into the root = %+v", r)
	}
}

func TestMoveTreeWithinOneDisk(t *testing.T) {
	dir := t.TempDir()
	seedTree(t, dir)
	r := fsRun(t, "", "-json", "mv", "-r", loc(dir, "a"), loc(dir, "moved"))
	if r.code != exitOK {
		t.Fatalf("mv -r = %+v", r)
	}
	var out treeOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || len(out.Objects) != 2 || len(out.Failed) != 0 {
		t.Fatalf("mv -r json = %+v (%v)", out, err)
	}
	if exists(dir, "a/x.txt") || !exists(dir, "moved/x.txt") || !exists(dir, "moved/sub/y.txt") || !exists(dir, "ab.txt") {
		t.Error("mv -r did not move exactly the tree")
	}
}

// TestMoveTreeKeepsSourcesThatFailedToCopy pins that mv -r deletes a source
// object only after its own copy succeeded, carries on past a failure, and
// reports it: the target has a file where a directory is needed, so
// a/sub/y.txt cannot be written and must survive in the source.
func TestMoveTreeKeepsSourcesThatFailedToCopy(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedTree(t, src)
	if r := fsRun(t, "blocker", "put", loc(dst, "b/sub")); r.code != exitOK {
		t.Fatal(r)
	}
	r := fsRun(t, "", "mv", "-r", loc(src, "a"), loc(dst, "b"))
	if r.code != exitFailed || !strings.Contains(r.stderr, "a/sub/y.txt -> b/sub/y.txt") || !strings.Contains(r.stderr, "1 of 2 objects failed") {
		t.Fatalf("mv -r with one failure = %+v", r)
	}
	if !exists(src, "a/sub/y.txt") {
		t.Fatal("mv -r deleted a source object whose copy failed")
	}
	if exists(src, "a/x.txt") || !exists(dst, "b/x.txt") {
		t.Error("mv -r did not move the object that could be copied")
	}

	// The same failure as JSON: one document, no second error object.
	_ = fsRun(t, "", "put", loc(src, "a/x.txt"))
	r = fsRun(t, "", "-json", "mv", "-r", loc(src, "a"), loc(dst, "b"))
	var out treeOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || r.code != exitFailed {
		t.Fatalf("mv -r json = %+v (%v)", r, err)
	}
	if len(out.Failed) != 1 || out.Failed[0].From != "a/sub/y.txt" || out.Failed[0].Copied || out.Failed[0].Code == "" {
		t.Errorf("failed = %+v", out.Failed)
	}
}

// TestMoveTreeReportsUndeletedSources pins that a move whose copy landed but
// whose delete failed is reported as copied (the object is now in both
// places), not as a clean failure.
func TestMoveTreeReportsUndeletedSources(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedTree(t, src)
	r := fsRun(t, "", "-json", "mv", "-r", "file://"+src+"?readonly=true#a", loc(dst, "b"))
	var out treeOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || r.code != exitFailed {
		t.Fatalf("mv -r from read-only = %+v (%v)", r, err)
	}
	if len(out.Failed) != 2 || !out.Failed[0].Copied || out.Failed[0].Code != "read_only" {
		t.Errorf("failed = %+v", out.Failed)
	}
	if !exists(src, "a/x.txt") || !exists(dst, "b/x.txt") {
		t.Error("expected the object in both places")
	}
	r = fsRun(t, "", "mv", "-r", "file://"+src+"?readonly=true#a", loc(dst, "c"))
	if r.code != exitFailed || !strings.Contains(r.stderr, "copied, but the source was not deleted") {
		t.Errorf("mv -r from read-only, text = %+v", r)
	}
}

func TestTreeDryRunChangesNothing(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedTree(t, src)
	r := fsRun(t, "", "mv", "-r", "-dry-run", loc(src, "a"), loc(dst, "b"))
	if r.code != exitOK || !strings.Contains(r.stdout, "would move a/x.txt -> b/x.txt") {
		t.Fatalf("mv -r -dry-run = %+v", r)
	}
	r = fsRun(t, "", "-json", "cp", "-r", "-dry-run", loc(src, "a"), loc(dst, "b"))
	var out treeOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || !out.DryRun || len(out.Objects) != 2 {
		t.Fatalf("cp -r -dry-run json = %+v (%v)", r, err)
	}
	if !exists(src, "a/x.txt") || exists(dst, "b/x.txt") {
		t.Error("-dry-run changed something")
	}
}

func TestTreeRefusals(t *testing.T) {
	dir := t.TempDir()
	seedTree(t, dir)
	for _, target := range []string{"a", "a/inner"} {
		r := fsRun(t, "", "cp", "-r", loc(dir, "a"), loc(dir, target))
		if r.code != exitFailed || !strings.Contains(r.stderr, "inside source") {
			t.Errorf("cp -r into itself (%q) = %+v", target, r)
		}
	}
	if r := fsRun(t, "", "cp", "-r", loc(dir, ""), loc(dir, "copy")); r.code != exitFailed || !strings.Contains(r.stderr, "inside source") {
		t.Errorf("cp -r of the root onto the same disk = %+v", r)
	}
	if r := fsRun(t, "", "-json", "cp", "-r", loc(dir, "none"), loc(t.TempDir(), "b")); r.code != exitFailed || !strings.Contains(r.stdout, `"not_found"`) {
		t.Errorf("cp -r of an empty prefix = %+v", r)
	}
	ro := "file://" + dir + "?readonly=true#"
	if r := fsRun(t, "", "-json", "mv", "-r", ro+"a", ro+"b"); r.code != exitFailed || !strings.Contains(r.stdout, `"read_only"`) || !exists(dir, "a/x.txt") {
		t.Errorf("mv -r on a read-only disk = %+v", r)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "a", "sub")
		_ = os.Chmod(locked, 0o000)
		r := fsRun(t, "", "cp", "-r", loc(dir, "a"), loc(t.TempDir(), "b"))
		_ = os.Chmod(locked, 0o700)
		if r.code != exitFailed {
			t.Errorf("cp -r over an unreadable directory = %+v", r)
		}
	}
	if r := fsRun(t, "", "cp", "-dry-run", loc(dir, "a/x.txt"), loc(dir, "b")); r.code != exitUsage {
		t.Errorf("-dry-run without -r = %+v", r)
	}
	for _, args := range [][]string{{"cp", "-r", loc(dir, "../a"), loc(dir, "b")}, {"cp", "-r", loc(dir, "a"), loc(dir, "../b")}, {"cp", "-r", "nope://x#a", loc(dir, "b")}, {"cp", "-r", loc(dir, "a"), "nope://x#b"}} {
		if r := fsRun(t, "", args...); r.code != exitFailed {
			t.Errorf("%v = %+v", args, r)
		}
	}
}

func TestRebaseAndWithin(t *testing.T) {
	cases := []struct{ key, src, dst, want string }{
		{"a/x", "a", "b", "b/x"},
		{"a/s/x", "a", "", "s/x"},
		{"x", "", "b", "b/x"},
		{"x", "", "", "x"},
	}
	for _, c := range cases {
		if got := rebase(c.key, c.src, c.dst); got != c.want {
			t.Errorf("rebase(%q, %q, %q) = %q, want %q", c.key, c.src, c.dst, got, c.want)
		}
	}
	if within("ab", "a") || !within("a/b", "a") || !within("a", "a") || !within("x", "") {
		t.Error("within")
	}
}

func TestCopyTreeWithinOneDisk(t *testing.T) {
	dir := t.TempDir()
	seedTree(t, dir)
	if r := fsRun(t, "", "cp", "-r", loc(dir, "a"), loc(dir, "c")); r.code != exitOK || !exists(dir, "a/x.txt") || !exists(dir, "c/sub/y.txt") {
		t.Errorf("cp -r within one disk = %+v", r)
	}
}

// noRename is a memory driver without the Move capability, so Disk.Move
// falls back to Copy then Delete.
type noRename struct{ *memory.Driver }

func (d noRename) Capabilities() filesystem.Capabilities {
	c := d.Driver.Capabilities()
	c.Move = false
	return c
}

// TestTransferOneReportsAMoveThatCopied pins the copied flag on one disk:
// when Disk.Move is Copy then Delete and the delete fails, the target was
// written and the report must say the object is now in both places.
func TestTransferOneReportsAMoveThatCopied(t *testing.T) {
	ctx := t.Context()
	failDelete := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpDelete {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	}
	d, err := filesystem.NewDisk("d", noRename{memory.New()}, filesystem.WithInterceptors(failDelete))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	copied, err := transferOne(ctx, d, d, "a", "b", true)
	if err == nil || !copied {
		t.Errorf("transferOne = %v, %v; want copied with an error", copied, err)
	}
	copied, err = transferOne(ctx, d, d, "missing", "c", true)
	if err == nil || copied {
		t.Errorf("transferOne of a missing key = %v, %v; want not copied", copied, err)
	}
}
