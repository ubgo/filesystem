package local

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

// coarseBy reads stamps as a filesystem whose change time has the given
// tick and that reuses one inode for every file: the worst case for
// telling versions apart.
func coarseBy(tick time.Duration) func(fs.FileInfo) (uint64, int64, bool) {
	return func(info fs.FileInfo) (uint64, int64, bool) {
		_, ctime, ok := statStamp(info)
		return 1, ctime - ctime%int64(tick), ok
	}
}

// stamped opens a driver on a new directory that reads stamps with stamp.
func stamped(t *testing.T, stamp func(fs.FileInfo) (uint64, int64, bool)) *Driver {
	t.Helper()
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native, stamp: stamp}
	if _, err := d.init(osRoot{native}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// startOfTick sleeps until a new tick of the given length has just begun,
// so the next few milliseconds of changes share one tick.
func startOfTick(tick time.Duration) {
	time.Sleep(tick - time.Duration(time.Now().UnixNano()%int64(tick)) + time.Millisecond)
}

// keyIn returns a key, made from prefix, whose lock is not stripe avoid.
func keyIn(d *Driver, prefix string, avoid int) string {
	for n := 0; ; n++ {
		if k := fmt.Sprintf("%s-%d", prefix, n); d.stripeOf(k) != avoid {
			return k
		}
	}
}

// Changes to unrelated keys in one change-time tick do not wait for the
// next: only earlier versions of the same key are a floor.
func TestUnrelatedKeysDoNotWaitForTheClock(t *testing.T) {
	const tick = 200 * time.Millisecond
	ctx := t.Context()
	d := stamped(t, coarseBy(tick))
	a := "a"
	b := keyIn(d, "b", d.stripeOf(a))
	startOfTick(tick)
	var ctimes []int64
	for _, k := range []string{a, b} {
		if _, err := d.Write(ctx, k, strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		info, err := d.root.Stat(k)
		if err != nil {
			t.Fatal(err)
		}
		_, ctime, _ := d.stamp(info)
		ctimes = append(ctimes, ctime)
	}
	if ctimes[0] != ctimes[1] {
		t.Errorf("the write of %s waited a tick for the write of %s", b, a)
	}
}

// A key written again in the tick its last version was removed in (by a
// delete, a prefix delete or a move away) never repeats the removed
// version's ETag, even with a reused inode, size and modification time.
func TestRemovedVersionsAreNotRepeated(t *testing.T) {
	const tick = 100 * time.Millisecond
	ctx := t.Context()
	mtime := time.Unix(1_700_000_000, 0)
	for name, remove := range map[string]func(d *Driver, key string) error{
		"delete":        func(d *Driver, key string) error { return d.Delete(ctx, key) },
		"delete prefix": func(d *Driver, key string) error { return d.DeletePrefix(ctx, path.Dir(key)) },
		"move away": func(d *Driver, key string) error {
			_, err := d.Move(ctx, key, "elsewhere")
			return err
		},
	} {
		d := stamped(t, coarseBy(tick))
		version := func(key string) string {
			if _, err := d.Write(ctx, key, strings.NewReader("same"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			// Behind the driver's back, as another tool could.
			if err := d.root.Chtimes(key, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			e, err := d.Stat(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			return e.ETag
		}
		startOfTick(tick)
		first := version("dir/k")
		if err := remove(d, "dir/k"); err != nil {
			t.Fatal(err)
		}
		if again := version("dir/k"); again == first {
			t.Errorf("%s: the new version repeats the removed one's ETag %s", name, first)
		}
	}
}

// blockingAppend holds an append to key, once its file is open, until
// release is closed.
type blockingAppend struct {
	fileRoot
	key     string
	opened  chan struct{}
	release chan struct{}
}

func (b blockingAppend) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	f, err := b.fileRoot.OpenFile(name, flag, perm)
	if err == nil && name == b.key && flag&os.O_APPEND != 0 {
		close(b.opened)
		<-b.release
	}
	return f, err
}

// A long append holds only its own key: writes to other keys go on.
func TestAppendHoldsOnlyItsKey(t *testing.T) {
	ctx := t.Context()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Write(ctx, "log", strings.NewReader("base"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	block := blockingAppend{d.root, "log", make(chan struct{}), make(chan struct{})}
	d.root = block
	appended := make(chan error, 1)
	go func() {
		_, err := d.Append(ctx, "log", strings.NewReader("+line"), filesystem.AppendOptions{})
		appended <- err
	}()
	select {
	case <-block.opened:
	case err := <-appended:
		t.Fatalf("append ended before opening its file: %v", err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := d.Write(ctx, keyIn(d, "other", d.stripeOf("log")), strings.NewReader("x"), filesystem.WriteOptions{})
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("a write to another key waited for the append")
		defer func() { <-wrote }()
	}
	close(block.release)
	if err := <-appended; err != nil {
		t.Error(err)
	}
}

// prunedParent removes the parent directory of a rename's target just
// before the rename, as a concurrent delete of the parent's last other key
// would: once when once is set, otherwise every time.
type prunedParent struct {
	fileRoot
	once *sync.Once
}

func (p prunedParent) Rename(oldname, newname string) error {
	prune := func() { _ = p.fileRoot.Remove(path.Dir(newname)) }
	if p.once != nil {
		p.once.Do(prune)
	} else {
		prune()
	}
	return p.fileRoot.Rename(oldname, newname)
}

// A parent directory pruned between its creation and the rename into it
// is created again, a bounded number of times; past that the write is
// CodeUnavailable (retry), keeping the cause.
func TestPlaceRecreatesAPrunedParent(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	d.root = prunedParent{d.root, &sync.Once{}}
	if _, err := d.Write(t.Context(), "dir/k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("write into a pruned parent: %v", err)
	}
	d.root = prunedParent{d.root, nil}
	if _, err := d.Write(t.Context(), "other/k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write into a parent pruned every time: %v", err)
	}
}

// Two changes locking the same stripes in opposite order never deadlock.
func TestLocksTakeStripesInOrder(t *testing.T) {
	d := &Driver{}
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, order := range [][]int{{3, 7}, {7, 3}} {
			wg.Go(func() {
				for range 2000 {
					d.lock(order...)()
				}
			})
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("locking two stripes in opposite orders deadlocked")
	}
}

// Keys that differ only in case name one file where the filesystem folds
// case, so they share a lock.
func TestCaseFoldedKeysShareALock(t *testing.T) {
	d := &Driver{caseInsensitive: true}
	if d.stripeOf("Report.PDF") != d.stripeOf("report.pdf") {
		t.Error("case-folded keys take different locks")
	}
}
