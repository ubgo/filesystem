package filesystemtest_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// Faults of the directory and sweep-preview cases.
const (
	dirIgnoresKeys          fault = "dir-exists-ignores-keys"
	dirObjectIsDir          fault = "dir-exists-counts-object"
	makeDirNoop             fault = "make-dir-noop"
	makeDirOverObject       fault = "make-dir-over-object"
	dirSurvivesDeletePrefix fault = "dir-survives-delete-prefix"
	makeDirMarker           fault = "make-dir-writes-marker"
	findNoop                fault = "find-noop"
	findIgnoresCutoff       fault = "find-ignores-cutoff"
	findRemoves             fault = "find-removes"
	findWrongKey            fault = "find-wrong-key"
	// The faults of the case for drivers without Multipart hide it.
	findReportsObjects     fault = "find-reports-objects"
	findDeletesNoMultipart fault = "find-deletes-without-multipart"
	findFailsNoMultipart   fault = "find-fails-without-multipart"
)

func (f *faulty) MakeDir(ctx context.Context, dir string) error {
	switch {
	case f.is(makeDirNoop):
		return nil
	case f.is(makeDirOverObject):
		if _, err := f.Driver.Stat(ctx, dir); err == nil {
			return nil
		}
	case f.is(makeDirMarker):
		if _, err := f.Driver.Write(ctx, dir+"/.keep", strings.NewReader(""), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	return f.Driver.MakeDir(ctx, dir)
}

func (f *faulty) DirExists(ctx context.Context, dir string) (bool, error) {
	switch {
	case f.is(dirIgnoresKeys):
		// Blind to the keys stored under dir.
		return false, nil
	case f.is(dirObjectIsDir):
		if _, err := f.Driver.Stat(ctx, dir); err == nil {
			return true, nil
		}
	}
	return f.Driver.DirExists(ctx, dir)
}

func (f *faulty) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	switch {
	case f.is(findNoop):
		return nil, nil
	case f.is(findIgnoresCutoff):
		return f.Driver.FindAbandoned(ctx, before.Add(1000*time.Hour))
	case f.is(findRemoves):
		found, err := f.Driver.FindAbandoned(ctx, before)
		if err == nil {
			_, err = f.Driver.SweepAbandoned(ctx, before)
		}
		return found, err
	case f.is(findWrongKey):
		found, err := f.Driver.FindAbandoned(ctx, before)
		for i := range found {
			found[i].Key = "wrong/" + found[i].Key
		}
		return found, err
	case f.is(findReportsObjects):
		return []filesystem.Abandoned{{Kind: filesystem.AbandonedStaging, ID: "x", Key: "preview/kept.txt", LastActivity: before.Add(-time.Hour)}}, nil
	case f.is(findDeletesNoMultipart):
		return nil, f.Driver.DeletePrefix(ctx, "preview")
	case f.is(findFailsNoMultipart):
		return nil, errors.New("staging directory gone")
	}
	return f.Driver.FindAbandoned(ctx, before)
}

// failNth counts, and so can fail, the directory and preview calls too.
func (f *failNth) MakeDir(ctx context.Context, dir string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.MakeDir(ctx, dir)
}

func (f *failNth) DirExists(ctx context.Context, dir string) (bool, error) {
	if f.fail() {
		return false, errInjected
	}
	return f.Driver.DirExists(ctx, dir)
}

func (f *failNth) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Driver.FindAbandoned(ctx, before)
}

// withoutDirs is failNth declaring neither Dirs nor the preview, so the
// cases for object stores run against it.
type withoutDirs struct{ *failNth }

func (w withoutDirs) Capabilities() filesystem.Capabilities {
	c := w.failNth.Capabilities()
	c.Dirs, c.FindAbandoned = false, false
	return c
}

// TestDirCasesWithoutDirsReportEveryFailure is TestCasesReportEveryFailure
// for the directory cases on a driver without real directories.
func TestDirCasesWithoutDirsReportEveryFailure(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "DirExists" && c.Name != "MakeDirWithoutDirsWritesNothing" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			for k := int64(1); ; k++ {
				drv := withoutDirs{&failNth{Driver: memory.New(), n: k}}
				d, err := filesystem.NewDisk("nodirs", drv)
				if err != nil {
					t.Fatal(err)
				}
				if c.Needs != nil && !c.Needs(d.Capabilities()) {
					t.Fatal("the case does not apply to a driver without Dirs")
				}
				err = c.Check(t.Context(), d)
				if !drv.fired.Load() {
					if err != nil {
						t.Fatalf("case failed without a fault: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("case passed although driver call %d failed", k)
				}
			}
		})
	}
}

// TestDirCasesFailOnTheWrongDriver runs the cases written for one kind of
// driver against the other kind, where each must report a violation.
func TestDirCasesFailOnTheWrongDriver(t *testing.T) {
	for _, name := range []string{"MakeDirWithoutDirsWritesNothing", "FindAbandonedUnsupportedIsRefused"} {
		i := slices.IndexFunc(filesystemtest.Cases(), func(c filesystemtest.Case) bool { return c.Name == name })
		d, err := filesystem.NewDisk("mem", memory.New())
		if err != nil {
			t.Fatal(err)
		}
		if err := filesystemtest.Cases()[i].Check(t.Context(), d); err == nil {
			t.Errorf("%s passed on a driver with the capability", name)
		}
	}
}
