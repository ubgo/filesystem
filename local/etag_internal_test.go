package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

// noSysInfo is a FileInfo with no OS record behind it, as some fs.FS
// implementations and systems without inodes return.
type noSysInfo struct{ fs.FileInfo }

// Sys implements fs.FileInfo (whose signature it must match).
func (noSysInfo) Sys() any { return nil }

// stampless reports files without inode or change time.
type stampless struct{ fileRoot }

func (s stampless) Stat(name string) (fs.FileInfo, error) {
	info, err := s.fileRoot.Stat(name)
	if err != nil {
		return nil, err
	}
	return noSysInfo{info}, nil
}

// Without inodes and change times, versions cannot be told apart, so
// compare-and-swap is not declared.
func TestCompareAndSwapNeedsStamps(t *testing.T) {
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native}
	if _, err := d.init(stampless{osRoot{native}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if d.Capabilities().CompareAndSwap {
		t.Error("compare-and-swap declared without change times")
	}
	if _, _, ok := statStamp(noSysInfo{}); ok {
		t.Error("stamp reported for a FileInfo without an OS record")
	}
	// Changes still work; they just do not wait for the clock.
	if _, err := d.Write(t.Context(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Error(err)
	}
}

// Same-size writes in one modification-time tick (forced here, as one-
// second timestamps or a coarse kernel tick would give) still get ETags no
// earlier version had, so a stale IfMatch always loses.
func TestETagNeverRepeats(t *testing.T) {
	ctx := t.Context()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	tick := time.Unix(1_700_000_000, 0)
	seen := map[string]int{}
	var first string
	for i := range 50 {
		if _, err := d.Write(ctx, "k", strings.NewReader("same"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := d.SetModTime(ctx, "k", tick); err != nil {
			t.Fatal(err)
		}
		e, err := d.Stat(ctx, "k")
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[e.ETag]; dup {
			t.Fatalf("version %d repeats version %d's ETag %s", i, prev, e.ETag)
		}
		seen[e.ETag] = i
		if i == 0 {
			first = e.ETag
		}
	}
	_, err = d.Write(ctx, "k", strings.NewReader("lost"), filesystem.WriteOptions{IfMatch: first})
	if filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("stale IfMatch: %v", err)
	}
}

// slowAppendRoot pauses inside an append, after the file is open, so a
// write racing it has every chance to replace the file.
type slowAppendRoot struct {
	fileRoot
	opened chan struct{}
	once   *sync.Once
}

func (s slowAppendRoot) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	f, err := s.fileRoot.OpenFile(name, flag, perm)
	if err == nil && flag&os.O_APPEND != 0 {
		s.once.Do(func() { close(s.opened) })
		time.Sleep(20 * time.Millisecond)
	}
	return f, err
}

// A write racing an append waits for it: the appended bytes land in the
// version the append reports, never in a file the write already replaced.
func TestWriteWaitsForAppend(t *testing.T) {
	ctx := t.Context()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Write(ctx, "log", strings.NewReader("base"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	d.root = slowAppendRoot{d.root, opened, &sync.Once{}}
	var appended filesystem.Entry
	var appendErr error
	done := make(chan struct{})
	go func() {
		appended, appendErr = d.Append(ctx, "log", strings.NewReader("+line"), filesystem.AppendOptions{})
		close(done)
	}()
	<-opened
	if _, err := d.Write(ctx, "log", strings.NewReader("replaced"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	<-done
	if appendErr != nil || appended.Size != int64(len("base+line")) {
		t.Errorf("append reported %+v, %v: its bytes went to a replaced file", appended, appendErr)
	}
}

// coarseStamp reads stamps as a filesystem with a 10ms clock tick that
// reuses one inode for every file would: the worst case for telling
// versions apart.
func coarseStamp(info fs.FileInfo) (uint64, int64, bool) {
	_, ctime, ok := statStamp(info)
	return 1, ctime - ctime%int64(10*time.Millisecond), ok
}

func TestETagNeverRepeatsOnACoarseClock(t *testing.T) {
	ctx := t.Context()
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, stamp: coarseStamp}
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d.native = native
	if _, err := d.init(osRoot{native}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	tick := time.Unix(1_700_000_000, 0)
	seen := map[string]bool{}
	for range 5 {
		if _, err := d.Write(ctx, "k", strings.NewReader("same"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := d.SetModTime(ctx, "k", tick); err != nil {
			t.Fatal(err)
		}
		e, _ := d.Stat(ctx, "k")
		if seen[e.ETag] {
			t.Fatalf("ETag %s repeats", e.ETag)
		}
		seen[e.ETag] = true
	}
}

// chmodFails fails every mode change outside staging, where writes set
// their files' modes before committing them.
type chmodFails struct{ fileRoot }

func (c chmodFails) Chmod(name string, mode fs.FileMode) error {
	if strings.HasPrefix(name, tmpDir) {
		return c.fileRoot.Chmod(name, mode)
	}
	return errInjected
}

// A change time that will not move, or cannot be nudged, fails the change
// before it is made: the object keeps its old version rather than end up
// replaced by a change reported as failed.
func TestStuckClockChangesNothing(t *testing.T) {
	ctx := t.Context()
	frozen := func(fs.FileInfo) (uint64, int64, bool) { return 1, 1, true }
	old := time.Unix(1_600_000_000, 0)
	for name, c := range map[string]struct {
		wrap   func(fileRoot) fileRoot
		change func(d *Driver) error
		code   filesystem.Code
	}{
		"write": {func(r fileRoot) fileRoot { return r }, func(d *Driver) error {
			_, err := d.Write(ctx, "k", strings.NewReader("new"), filesystem.WriteOptions{})
			return err
		}, filesystem.CodeUnavailable},
		"append": {func(r fileRoot) fileRoot { return r }, func(d *Driver) error {
			_, err := d.Append(ctx, "k", strings.NewReader("new"), filesystem.AppendOptions{})
			return err
		}, filesystem.CodeUnavailable},
		"move onto": {func(r fileRoot) fileRoot { return r }, func(d *Driver) error {
			if err := d.root.WriteFile("src", []byte("new"), DefaultFilePrivate); err != nil {
				return err
			}
			_, err := d.Move(ctx, "src", "k")
			return err
		}, filesystem.CodeUnavailable},
		"set mod time, cannot nudge": {func(r fileRoot) fileRoot { return chmodFails{r} }, func(d *Driver) error {
			return d.SetModTime(ctx, "k", time.Now())
		}, filesystem.CodeUnknown},
		"set visibility": {func(r fileRoot) fileRoot { return r }, func(d *Driver) error {
			return d.SetVisibility(ctx, "k", filesystem.VisibilityPublic)
		}, filesystem.CodeUnavailable},
	} {
		native, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native, stamp: frozen, stampWait: 20 * time.Millisecond}
		if _, err := d.init(osRoot{native}); err != nil {
			t.Fatal(err)
		}
		if err := d.root.WriteFile("k", []byte("old"), DefaultFilePrivate); err != nil {
			t.Fatal(err)
		}
		if err := d.root.Chtimes("k", old, old); err != nil {
			t.Fatal(err)
		}
		d.root = c.wrap(d.root)
		if code := filesystem.CodeOf(c.change(d)); code != c.code {
			t.Errorf("%s: code %s, want %s", name, code, c.code)
		}
		info, err := d.root.Stat("k")
		if data, _ := d.root.ReadFile("k"); string(data) != "old" || err != nil || !info.ModTime().Equal(old) || info.Mode().Perm() != DefaultFilePrivate {
			t.Errorf("%s changed the object although it failed: %q %v", name, data, info)
		}
		_ = d.Close()
	}
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.SetModTime(ctx, StagingDir+"/tmp/x", time.Now()); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("set mod time in staging: %v", err)
	}
}

// A change waiting for a stuck change time stops when its context is
// cancelled, with the context's error, and changes nothing.
func TestPassHonoursContext(t *testing.T) {
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	frozen := func(fs.FileInfo) (uint64, int64, bool) { return 1, 1, true }
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native, stamp: frozen, stampWait: time.Minute}
	if _, err := d.init(osRoot{native}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.root.WriteFile("k", []byte("old"), DefaultFilePrivate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := d.SetVisibility(ctx, "k", filesystem.VisibilityPublic); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetVisibility under a stuck clock = %v", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("waited %s after the context ended", waited)
	}
	if info, err := d.root.Stat("k"); err != nil || info.Mode().Perm() != DefaultFilePrivate {
		t.Fatalf("the object changed: %v %v", info, err)
	}
}
