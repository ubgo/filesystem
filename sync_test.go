package filesystem_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func memDisk(t *testing.T, name string, objects map[string]string) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk(name, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range objects {
		if _, err := d.WriteBytes(t.Context(), k, []byte(v), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// contents is every object on d as key → body.
func contents(t *testing.T, d *filesystem.Disk) map[string]string {
	t.Helper()
	out := map[string]string{}
	for e, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		b, err := d.ReadAll(t.Context(), e.Key)
		if err != nil {
			t.Fatal(err)
		}
		out[e.Key] = string(b)
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestSync(t *testing.T) {
	ctx := t.Context()
	src := memDisk(t, "src", map[string]string{"photos/a.jpg": "aaa", "photos/b.jpg": "bbb", "photos/sub/c.jpg": "ccc", "other/x": "x"})
	dst := memDisk(t, "dst", map[string]string{
		"backup/a.jpg":  "aaa",     // unchanged
		"backup/b.jpg":  "old-bbb", // changed: copied again
		"backup/gone":   "stale",   // no source: deleted with Delete
		"backup2/keep":  "sibling", // outside the target directory
		"unrelated/key": "keep",
	})

	// A dry run reports every action and changes nothing.
	before := contents(t, dst)
	var planned []filesystem.SyncItem
	rep, err := filesystem.Sync(ctx, src, "photos", dst, "backup", filesystem.SyncOptions{Delete: true, DryRun: true, OnItem: func(it filesystem.SyncItem) { planned = append(planned, it) }})
	if err != nil {
		t.Fatal(err)
	}
	want := filesystem.SyncReport{Copied: 2, Deleted: 1, Unchanged: 1}
	if rep != want {
		t.Fatalf("dry run report %+v, want %+v", rep, want)
	}
	var got []string
	for _, it := range planned {
		got = append(got, string(it.Action)+" "+it.Key+" "+it.Target)
	}
	wantItems := []string{"copy photos/b.jpg backup/b.jpg", "copy photos/sub/c.jpg backup/sub/c.jpg", "delete backup/gone backup/gone"}
	if !slices.Equal(got, wantItems) {
		t.Fatalf("dry run items %q, want %q", got, wantItems)
	}
	if after := contents(t, dst); !equalMaps(before, after) {
		t.Fatalf("dry run changed the target: %v", after)
	}

	// Without Delete nothing is deleted.
	if rep, err := filesystem.Sync(ctx, src, "photos", dst, "backup", filesystem.SyncOptions{}); err != nil || rep.Copied != 2 || rep.Deleted != 0 {
		t.Fatalf("copy-only sync %+v, %v", rep, err)
	}
	if _, err := dst.Stat(ctx, "backup/gone"); err != nil {
		t.Fatalf("a sync without Delete deleted: %v", err)
	}

	// A second run with Delete copies nothing and deletes only inside
	// backup/: the sibling backup2/ and unrelated keys stay.
	rep, err = filesystem.Sync(ctx, src, "photos", dst, "backup", filesystem.SyncOptions{Delete: true})
	if err != nil || (rep != filesystem.SyncReport{Deleted: 1, Unchanged: 3}) {
		t.Fatalf("mirror sync %+v, %v", rep, err)
	}
	wantDst := map[string]string{"backup/a.jpg": "aaa", "backup/b.jpg": "bbb", "backup/sub/c.jpg": "ccc", "backup2/keep": "sibling", "unrelated/key": "keep"}
	if got := contents(t, dst); !equalMaps(got, wantDst) {
		t.Fatalf("target after sync %v, want %v", got, wantDst)
	}
}

func TestSyncRefusesOverlap(t *testing.T) {
	d := memDisk(t, "d", map[string]string{"a/b": "1"})
	for _, pair := range [][2]string{{"a", "a/copy"}, {"a/copy", "a"}, {"", "x"}} {
		if _, err := filesystem.Sync(t.Context(), d, pair[0], d, pair[1], filesystem.SyncOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("sync %q -> %q on one disk: %v", pair[0], pair[1], err)
		}
	}
}

func TestSyncSameEscapeHatch(t *testing.T) {
	src := memDisk(t, "src", map[string]string{"k": "new"})
	dst := memDisk(t, "dst", map[string]string{"k": "old"})
	rep, err := filesystem.Sync(t.Context(), src, "", dst, "", filesystem.SyncOptions{Same: func(_, _ filesystem.Entry) bool { return true }})
	if err != nil || rep.Unchanged != 1 || rep.Copied != 0 {
		t.Fatalf("custom Same: %+v, %v", rep, err)
	}
}

func TestSameContent(t *testing.T) {
	cases := []struct {
		a, b filesystem.Entry
		want bool
	}{
		{filesystem.Entry{Size: 1, ETag: "x"}, filesystem.Entry{Size: 2, ETag: "x"}, false},
		{filesystem.Entry{Size: 1, SHA256: "s", ETag: "x"}, filesystem.Entry{Size: 1, SHA256: "t", ETag: "x"}, false},
		{filesystem.Entry{Size: 1, SHA256: "s"}, filesystem.Entry{Size: 1, SHA256: "s"}, true},
		{filesystem.Entry{Size: 1, ETag: "x"}, filesystem.Entry{Size: 1, ETag: "x"}, true},
		{filesystem.Entry{Size: 1, ETag: "x"}, filesystem.Entry{Size: 1, ETag: "y"}, false},
		{filesystem.Entry{Size: 1}, filesystem.Entry{Size: 1}, false},
	}
	for i, c := range cases {
		if got := filesystem.SameContent(c.a, c.b); got != c.want {
			t.Errorf("case %d: SameContent = %v, want %v", i, got, c.want)
		}
	}
}

// TestSyncReportsFailures: on a cancelled context either the listing
// stops the sync with nothing done, or every action fails and is
// reported; nothing is ever reported as done.
func TestSyncReportsFailures(t *testing.T) {
	src := memDisk(t, "src", map[string]string{"a": "1", "b": "2"})
	dst := memDisk(t, "dst", nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var items []filesystem.SyncItem
	rep, err := filesystem.Sync(ctx, src, "", dst, "", filesystem.SyncOptions{OnItem: func(it filesystem.SyncItem) { items = append(items, it) }})
	if err == nil || rep.Copied != 0 {
		t.Fatalf("sync on a cancelled context: %+v, %v", rep, err)
	}
	for _, it := range items {
		if it.Err == nil {
			t.Fatalf("item %+v reported as done on a cancelled context", it)
		}
	}
}

// strayLister lists one extra key outside every directory it is asked
// for, as a misbehaving backend might.
type strayLister struct{ filesystem.Driver }

func (s strayLister) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }

func (s strayLister) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	p, err := s.Driver.List(ctx, dir, opts)
	if err == nil && opts.Cursor == "" {
		p.Entries = append(p.Entries, filesystem.Entry{Key: "elsewhere/precious", Size: 1})
	}
	return p, err
}

// TestSyncNeverDeletesOutsideTarget: even when the target's listing
// returns a key outside the target directory, Sync refuses to delete it.
func TestSyncNeverDeletesOutsideTarget(t *testing.T) {
	ctx := t.Context()
	mem := memory.New()
	dst, err := filesystem.NewDisk("dst", strayLister{mem})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mirror/a", "elsewhere/precious"} {
		if _, err := dst.WriteBytes(ctx, k, []byte("1"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	src := memDisk(t, "src", map[string]string{"a": "1"})
	rep, err := filesystem.Sync(ctx, src, "", dst, "mirror", filesystem.SyncOptions{Delete: true, Same: func(_, _ filesystem.Entry) bool { return true }})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || rep.Failed != 1 || rep.Deleted != 0 {
		t.Fatalf("sync with a stray key: %+v, %v", rep, err)
	}
	if ok, err := dst.Exists(ctx, "elsewhere/precious"); err != nil || !ok {
		t.Fatalf("the key outside the target was deleted: %v, %v", ok, err)
	}
}

// TestSyncChecksum: objects whose listings cannot be compared are copied
// again by default and recognised as equal with Checksum.
func TestSyncChecksum(t *testing.T) {
	ctx := t.Context()
	src := memDisk(t, "src", map[string]string{"a": "same", "b": "new!"})
	dst := memDisk(t, "dst", map[string]string{"a": "same", "b": "old!"})
	never := func(_, _ filesystem.Entry) bool { return false } // as for ETags that never match
	rep, err := filesystem.Sync(ctx, src, "", dst, "", filesystem.SyncOptions{Same: never, DryRun: true})
	if err != nil || rep.Copied != 2 {
		t.Fatalf("without Checksum: %+v, %v", rep, err)
	}
	rep, err = filesystem.Sync(ctx, src, "", dst, "", filesystem.SyncOptions{Same: never, Checksum: true})
	if err != nil || rep.Copied != 1 || rep.Unchanged != 1 {
		t.Fatalf("with Checksum: %+v, %v", rep, err)
	}
	sum, err := filesystem.ObjectSHA256(ctx, dst, "b")
	if err != nil || sum != "bdd1e524e5c90bee91a4f1ac4a087ca0012e36235ab24b5136d2a6388e7ad58b" {
		t.Fatalf("ObjectSHA256 = %q, %v", sum, err)
	}
}

// TestSyncRefusesOverlapThroughAScope: a scoped disk and the disk it was
// derived from are one backend, so syncing between overlapping
// directories through them is refused in either direction, with or
// without Delete, and the source survives.
func TestSyncRefusesOverlapThroughAScope(t *testing.T) {
	ctx := t.Context()
	root := memDisk(t, "root", map[string]string{"site/live/index.html": "<h1>", "site/live/app.js": "js"})
	live, err := root.Scope("live", "site/live")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		src    *filesystem.Disk
		srcDir string
		dst    *filesystem.Disk
		dstDir string
		delete bool
	}{
		{"scope into its parent's ancestor, with delete", live, "", root, "site", true},
		{"scope into its parent's ancestor", live, "", root, "site", false},
		{"parent into the scope", root, "site", live, "", false},
		{"parent dir into a dir inside the scope", root, "site/live", live, "copy", false},
	}
	for _, c := range cases {
		rep, err := filesystem.Sync(ctx, c.src, c.srcDir, c.dst, c.dstDir, filesystem.SyncOptions{Delete: c.delete})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || rep != (filesystem.SyncReport{}) {
			t.Errorf("%s: %+v, %v; want CodeInvalidArgument and nothing done", c.name, rep, err)
		}
	}
	if got := contents(t, root); len(got) != 2 {
		t.Fatalf("the source changed: %v", got)
	}
	// A sibling directory of the same backend is fine.
	if rep, err := filesystem.Sync(ctx, live, "", root, "backup", filesystem.SyncOptions{Delete: true}); err != nil || rep.Copied != 2 {
		t.Fatalf("sync to a sibling: %+v, %v", rep, err)
	}
}

// foldingDriver is a case-insensitive backend: it stores every key lower
// cased, as a case-insensitive file system or bucket would address it.
type foldingDriver struct{ inner filesystem.Driver }

func (f foldingDriver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{CaseInsensitive: true}
}
func (f foldingDriver) Stat(ctx context.Context, k string) (filesystem.Entry, error) {
	return f.inner.Stat(ctx, strings.ToLower(k))
}
func (f foldingDriver) Read(ctx context.Context, k string, r filesystem.ByteRange) (io.ReadCloser, error) {
	return f.inner.Read(ctx, strings.ToLower(k), r)
}
func (f foldingDriver) Write(ctx context.Context, k string, b io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	return f.inner.Write(ctx, strings.ToLower(k), b, o)
}
func (f foldingDriver) Delete(ctx context.Context, k string) error {
	return f.inner.Delete(ctx, strings.ToLower(k))
}
func (f foldingDriver) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	return f.inner.List(ctx, strings.ToLower(dir), o)
}

// TestSyncCaseCollisions: on a case-insensitive target, source keys that
// differ only in case are reported as conflicts and not copied, a key
// whose target differs only in case is matched, and Delete keeps it.
func TestSyncCaseCollisions(t *testing.T) {
	ctx := t.Context()
	src := memDisk(t, "src", map[string]string{"A.txt": "upper", "a.txt": "lower", "Readme.md": "r"})
	dst, err := filesystem.NewDisk("dst", foldingDriver{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.WriteBytes(ctx, "readme.md", []byte("r"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var conflicts []string
	rep, err := filesystem.Sync(ctx, src, "", dst, "", filesystem.SyncOptions{Delete: true, Checksum: true, OnItem: func(it filesystem.SyncItem) {
		if filesystem.CodeOf(it.Err) == filesystem.CodeConflict {
			conflicts = append(conflicts, it.Key)
		}
	}})
	if filesystem.CodeOf(err) != filesystem.CodeConflict || rep.Failed != 2 || rep.Copied != 0 || rep.Deleted != 0 || rep.Unchanged != 1 {
		t.Fatalf("sync = %+v, %v", rep, err)
	}
	slices.Sort(conflicts)
	if !slices.Equal(conflicts, []string{"A.txt", "a.txt"}) {
		t.Fatalf("conflicts %v", conflicts)
	}
	if got := contents(t, dst); !equalMaps(got, map[string]string{"readme.md": "r"}) {
		t.Fatalf("target %v", got)
	}
}

// TestSyncThroughMultiTargetDisks: a routed or mirrored disk reaches
// backends a plain unwrap does not, so overlaps through them are refused,
// Delete through them always, while a copy that cannot nest still works.
func TestSyncThroughMultiTargetDisks(t *testing.T) {
	ctx := t.Context()
	root := memDisk(t, "root", map[string]string{"site/live/index.html": "<h1>", "site/live/app.js": "js"})
	live, err := root.Scope("live", "site/live")
	if err != nil {
		t.Fatal(err)
	}
	route, err := filesystem.Route("route", []filesystem.Mount{{Prefix: "m", Disk: root}})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := memDisk(t, "elsewhere", nil)
	mirror, err := filesystem.Mirror("mirror", elsewhere, root) // root is a secondary
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		dst    *filesystem.Disk
		dstDir string
		delete bool
	}{
		{"route, with delete (the reviewer's case)", route, "m/site", true},
		{"route, copy only", route, "m/site", false},
		{"mirror whose secondary overlaps, with delete", mirror, "site", true},
		{"mirror whose secondary overlaps, copy only", mirror, "site", false},
		{"mirror, delete into a directory that does not nest", mirror, "backup", true},
	} {
		rep, err := filesystem.Sync(ctx, live, "", c.dst, c.dstDir, filesystem.SyncOptions{Delete: c.delete})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || rep != (filesystem.SyncReport{}) {
			t.Errorf("%s: %+v, %v; want CodeInvalidArgument and nothing done", c.name, rep, err)
		}
	}
	if got := contents(t, root); len(got) != 2 {
		t.Fatalf("the source changed: %v", got)
	}
	// Copies that cannot nest still work, through the route and the
	// mirror, and a plain scoped sync with Delete too.
	for _, c := range []struct {
		dst    *filesystem.Disk
		dstDir string
		delete bool
	}{{route, "m/backup", false}, {mirror, "backup", false}, {root, "backup2", true}} {
		if rep, err := filesystem.Sync(ctx, live, "", c.dst, c.dstDir, filesystem.SyncOptions{Delete: c.delete}); err != nil || rep.Copied != 2 {
			t.Fatalf("sync to %s:%s: %+v, %v", c.dst.Name(), c.dstDir, rep, err)
		}
	}
}
