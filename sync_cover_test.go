package filesystem_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// errSyncFault is the failure syncFaultDriver injects.
var errSyncFault = errors.New("injected sync fault")

// syncFaultDriver is a memory backend that fails the operations it is
// told to: every List, every Write, or reads of one key part-way through
// the body. Embedding memory.Driver keeps its capabilities and optional
// interfaces, so the Disk treats it as the real backend.
type syncFaultDriver struct {
	*memory.Driver
	failList  bool
	failWrite bool
	badRead   string
	extraDirs bool
}

func (f *syncFaultDriver) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	if f.failList {
		return filesystem.Page{}, filesystem.NewError(filesystem.CodeUnavailable, errSyncFault)
	}
	p, err := f.Driver.List(ctx, dir, o)
	if err == nil && f.extraDirs && o.Cursor == "" {
		// A backend breaking the contract that recursive listings hold
		// objects only.
		p.Entries = append(p.Entries, filesystem.Entry{Key: filesystem.JoinKey(dir, "phantom"), IsDir: true})
	}
	return p, err
}

func (f *syncFaultDriver) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	if f.failWrite {
		return filesystem.Entry{}, filesystem.NewError(filesystem.CodeUnavailable, errSyncFault)
	}
	return f.Driver.Write(ctx, key, body, o)
}

func (f *syncFaultDriver) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	if key == f.badRead {
		return io.NopCloser(io.MultiReader(strings.NewReader("par"), syncErrReader{})), nil
	}
	return f.Driver.Read(ctx, key, r)
}

// syncErrReader fails every read, as a connection dropped mid-body does.
type syncErrReader struct{}

func (syncErrReader) Read([]byte) (int, error) { return 0, errSyncFault }

func syncFaultDisk(t *testing.T, name string, f *syncFaultDriver, objects map[string]string) *filesystem.Disk {
	t.Helper()
	f.Driver = memory.New()
	for k, v := range objects {
		if _, err := f.Driver.Write(t.Context(), k, strings.NewReader(v), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := filesystem.NewDisk(name, f)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestSyncRefusesInvalidDirs: a traversal in either directory is refused
// before anything is listed or copied.
func TestSyncRefusesInvalidDirs(t *testing.T) {
	src := memDisk(t, "src", map[string]string{"a": "1"})
	dst := memDisk(t, "dst", nil)
	for _, dirs := range [][2]string{{"../up", ""}, {"", "../up"}} {
		rep, err := filesystem.Sync(t.Context(), src, dirs[0], dst, dirs[1], filesystem.SyncOptions{})
		if err == nil || rep != (filesystem.SyncReport{}) {
			t.Errorf("sync %q -> %q: %+v, %v; want an error and nothing done", dirs[0], dirs[1], rep, err)
		}
	}
	if got := contents(t, dst); len(got) != 0 {
		t.Fatalf("target changed: %v", got)
	}
}

// TestSyncStopsOnTargetListingFailure: a target that cannot be listed
// stops the sync with nothing copied, even though the source listed fine.
func TestSyncStopsOnTargetListingFailure(t *testing.T) {
	src := memDisk(t, "src", map[string]string{"a": "1"})
	f := &syncFaultDriver{failList: true}
	dst := syncFaultDisk(t, "dst", f, nil)
	rep, err := filesystem.Sync(t.Context(), src, "", dst, "", filesystem.SyncOptions{})
	if !errors.Is(err, errSyncFault) || rep != (filesystem.SyncReport{}) {
		t.Fatalf("sync = %+v, %v; want the listing error and nothing done", rep, err)
	}
	f.failList = false
	if got := contents(t, dst); len(got) != 0 {
		t.Fatalf("target changed: %v", got)
	}
}

// TestSyncCountsFailedCopies: each failed copy is reported with its cause,
// counted, and the rest of the run goes on; the error names the count and
// keeps the first cause's code.
func TestSyncCountsFailedCopies(t *testing.T) {
	src := memDisk(t, "src", map[string]string{"a": "1", "b": "2"})
	dst := syncFaultDisk(t, "dst", &syncFaultDriver{failWrite: true}, nil)
	var failed []string
	rep, err := filesystem.Sync(t.Context(), src, "", dst, "", filesystem.SyncOptions{OnItem: func(it filesystem.SyncItem) {
		if errors.Is(it.Err, errSyncFault) {
			failed = append(failed, it.Key)
		}
	}})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || !strings.Contains(err.Error(), "2 objects failed") {
		t.Fatalf("sync error = %v", err)
	}
	if rep.Failed != 2 || rep.Copied != 0 || len(failed) != 2 {
		t.Fatalf("sync = %+v, reported failures %v", rep, failed)
	}
}

// TestSyncChecksumReadFailureCopies: with Checksum, a source object that
// cannot be read counts as different, never as unchanged, so the run
// tries to copy it (which here fails too and is reported).
func TestSyncChecksumReadFailureCopies(t *testing.T) {
	src := syncFaultDisk(t, "src", &syncFaultDriver{badRead: "a"}, map[string]string{"a": "abc"})
	dst := memDisk(t, "dst", map[string]string{"a": "abc"})
	never := func(_, _ filesystem.Entry) bool { return false }
	rep, err := filesystem.Sync(t.Context(), src, "", dst, "", filesystem.SyncOptions{Same: never, Checksum: true})
	if err == nil || rep.Unchanged != 0 || rep.Failed != 1 {
		t.Fatalf("sync = %+v, %v; want the unreadable object treated as different", rep, err)
	}
}

// TestObjectSHA256Failures: a missing object and a body that fails
// part-way both return an error, never a digest of partial content.
func TestObjectSHA256Failures(t *testing.T) {
	d := syncFaultDisk(t, "d", &syncFaultDriver{badRead: "torn"}, map[string]string{"torn": "abc"})
	if sum, err := filesystem.ObjectSHA256(t.Context(), d, "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound || sum != "" {
		t.Fatalf("missing object: %q, %v", sum, err)
	}
	if sum, err := filesystem.ObjectSHA256(t.Context(), d, "torn"); !errors.Is(err, errSyncFault) || sum != "" {
		t.Fatalf("torn body: %q, %v", sum, err)
	}
}

// TestSyncSkipsDirectoryEntries: a listing that (against the Driver
// contract) yields directory entries does not make Sync copy or delete a
// directory.
func TestSyncSkipsDirectoryEntries(t *testing.T) {
	src := syncFaultDisk(t, "src", &syncFaultDriver{extraDirs: true}, map[string]string{"a": "1"})
	dst := syncFaultDisk(t, "dst", &syncFaultDriver{extraDirs: true}, map[string]string{"stale": "x"})
	var items []filesystem.SyncItem
	rep, err := filesystem.Sync(t.Context(), src, "", dst, "", filesystem.SyncOptions{Delete: true, OnItem: func(it filesystem.SyncItem) { items = append(items, it) }})
	if err != nil || rep.Copied != 1 || rep.Deleted != 1 || len(items) != 2 {
		t.Fatalf("sync = %+v, %v, items %+v", rep, err, items)
	}
	for _, it := range items {
		if strings.HasSuffix(it.Key, "phantom") {
			t.Fatalf("acted on a directory entry: %+v", it)
		}
	}
}

// TestSyncRefusesOverlapThroughRouteMounts: a route reaches its mounts
// whether the directory is the mount point itself or contains it.
func TestSyncRefusesOverlapThroughRouteMounts(t *testing.T) {
	root := memDisk(t, "root", map[string]string{"x": "1"})
	other := memDisk(t, "other", nil)
	route, err := filesystem.Route("route", []filesystem.Mount{{Prefix: "m", Disk: root}, {Prefix: "o", Disk: other}})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"m", ""} { // the mount point; above it
		rep, err := filesystem.Sync(t.Context(), root, "", route, dir, filesystem.SyncOptions{})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || rep != (filesystem.SyncReport{}) {
			t.Errorf("route dir %q: %+v, %v; want CodeInvalidArgument", dir, rep, err)
		}
	}
	// A directory under the other mount reaches only that mount.
	if rep, err := filesystem.Sync(t.Context(), root, "", route, "o/backup", filesystem.SyncOptions{}); err != nil || rep.Copied != 1 {
		t.Fatalf("sync into the other mount: %+v, %v", rep, err)
	}
}

// TestSyncRefusesOverlapThroughWrappers: overlay, cached and failover
// disks reach every disk under them, so syncing a disk into one that
// wraps it is refused, and Delete through any of them always is.
func TestSyncRefusesOverlapThroughWrappers(t *testing.T) {
	root := memDisk(t, "root", map[string]string{"site/a": "1"})
	live, err := root.Scope("live", "site")
	if err != nil {
		t.Fatal(err)
	}
	spare := memDisk(t, "spare", nil)
	overlayUpper, err := filesystem.Overlay("overlay-upper", root, spare)
	if err != nil {
		t.Fatal(err)
	}
	overlayLower, err := filesystem.Overlay("overlay-lower", spare, root)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := filesystem.CachedRead("cached", root, memDisk(t, "cache", nil), filesystem.CacheOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cachedOver, err := filesystem.CachedRead("cached-over", spare, root, filesystem.CacheOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	failPrimary, err := filesystem.Failover("failover-primary", root, []*filesystem.Disk{spare}, filesystem.FailoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	failSecondary, err := filesystem.Failover("failover-secondary", spare, []*filesystem.Disk{root}, filesystem.FailoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, dst := range []*filesystem.Disk{overlayUpper, overlayLower, cached, cachedOver, failPrimary, failSecondary} {
		for _, del := range []bool{false, true} {
			rep, err := filesystem.Sync(t.Context(), live, "", dst, "site/copy", filesystem.SyncOptions{Delete: del})
			if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || rep != (filesystem.SyncReport{}) {
				t.Errorf("%s (delete %v): %+v, %v; want CodeInvalidArgument", dst.Name(), del, rep, err)
			}
		}
	}
	// Delete is refused even where the directories cannot nest.
	if _, err := filesystem.Sync(t.Context(), spare, "", failSecondary, "elsewhere", filesystem.SyncOptions{Delete: true}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("delete through a failover disk: %v", err)
	}
	if got := contents(t, root); len(got) != 1 {
		t.Fatalf("the source changed: %v", got)
	}
}

// TestSyncBoundsWrapperChains: a chain of wrappers deeper than the walk
// follows is treated as reaching anything, so Delete across it is refused
// rather than trusted.
func TestSyncBoundsWrapperChains(t *testing.T) {
	root := memDisk(t, "root", map[string]string{"a": "1"})
	deep := root
	for i := range 70 {
		var err error
		if deep, err = deep.Scope("deep", "s"); err != nil {
			t.Fatalf("scope %d: %v", i, err)
		}
	}
	other := memDisk(t, "other", nil)
	if _, err := filesystem.Sync(t.Context(), other, "", deep, "", filesystem.SyncOptions{Delete: true}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("delete across a 70-deep chain: %v", err)
	}
}

// syncSliceDriver is a backend whose type cannot be compared with ==
// (it holds a slice), so Sync cannot tell two values apart.
type syncSliceDriver struct {
	*memory.Driver
	tags []string
}

// TestSyncTreatsIncomparableDriversAsOne: two disks over drivers of one
// incomparable type are taken as one backend, so nested directories are
// refused: a wrong refusal is recoverable, a deleted source is not.
func TestSyncTreatsIncomparableDriversAsOne(t *testing.T) {
	a, err := filesystem.NewDisk("a", syncSliceDriver{Driver: memory.New(), tags: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := filesystem.NewDisk("b", syncSliceDriver{Driver: memory.New(), tags: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Sync(t.Context(), a, "", b, "copy", filesystem.SyncOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("nested sync between incomparable drivers: %v", err)
	}
	// Different types are always different backends.
	if _, err := filesystem.Sync(t.Context(), a, "", memDisk(t, "m", nil), "copy", filesystem.SyncOptions{}); err != nil {
		t.Fatalf("sync to another type: %v", err)
	}
}
