package filesystemtest

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// Fake is an in-memory Disk with test assertions: hand it to the code under test in place of a real
// disk, then assert on what was stored.
type Fake struct {
	*filesystem.Disk
	tb testing.TB
}

// NewFake returns an empty Fake disk named name, configured with opts like
// any Disk. It fails the test immediately if the options are invalid.
func NewFake(tb testing.TB, name string, opts ...filesystem.Option) *Fake {
	tb.Helper()
	d, err := filesystem.NewDisk(name, memory.New(), opts...)
	if err != nil {
		tb.Fatalf("filesystemtest: new fake disk: %v", err)
	}
	return &Fake{Disk: d, tb: tb}
}

// AssertExists fails the test unless key exists.
func (f *Fake) AssertExists(key string) {
	f.tb.Helper()
	ok, err := f.Exists(f.tb.Context(), key)
	if err != nil {
		f.tb.Errorf("filesystemtest: exists %q: %v", key, err)
		return
	}
	if !ok {
		f.tb.Errorf("filesystemtest: %s: expected %q to exist", f.Name(), key)
	}
}

// AssertMissing fails the test if key exists.
func (f *Fake) AssertMissing(key string) {
	f.tb.Helper()
	ok, err := f.Exists(f.tb.Context(), key)
	if err != nil {
		f.tb.Errorf("filesystemtest: exists %q: %v", key, err)
		return
	}
	if ok {
		f.tb.Errorf("filesystemtest: %s: expected %q to be missing", f.Name(), key)
	}
}

// AssertContent fails the test unless key holds exactly want.
func (f *Fake) AssertContent(key, want string) {
	f.tb.Helper()
	got, err := f.ReadAll(f.tb.Context(), key)
	if err != nil {
		f.tb.Errorf("filesystemtest: read %q: %v", key, err)
		return
	}
	if string(got) != want {
		f.tb.Errorf("filesystemtest: %s: %q holds %q, want %q", f.Name(), key, got, want)
	}
}

// AssertCount fails the test unless dir holds exactly n objects, counted
// recursively.
func (f *Fake) AssertCount(dir string, n int) {
	f.tb.Helper()
	got, err := keys(f.tb.Context(), f.Disk, dir, filesystem.ListOptions{Recursive: true})
	if err != nil {
		f.tb.Errorf("filesystemtest: list %q: %v", dir, err)
		return
	}
	if len(got) != n {
		f.tb.Errorf("filesystemtest: %s: %q holds %d objects %q, want %d", f.Name(), dir, len(got), got, n)
	}
}

// AssertEmpty fails the test unless the whole disk is empty.
func (f *Fake) AssertEmpty() {
	f.tb.Helper()
	f.AssertCount("", 0)
}
