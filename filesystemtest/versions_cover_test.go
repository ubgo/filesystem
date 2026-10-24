package filesystemtest_test

import (
	"context"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// TestSuitePassesOnAVersionedDriver runs the whole suite on the memory
// driver keeping versions, so the VersionRead case runs end to end,
// including its refusals of a missing and an empty version id.
func TestSuitePassesOnAVersionedDriver(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("versioned", memory.New(memory.WithVersions()))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// failNthVersioned is failNth on a versioned memory driver that also
// counts (and so can fail) the storage-class and version calls, which
// failNth forwards uncounted.
type failNthVersioned struct{ *failNth }

func (f failNthVersioned) SetStorageClass(ctx context.Context, key, class string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.SetStorageClass(ctx, key, class)
}

func (f failNthVersioned) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Driver.Versions(ctx, key)
}

func (f failNthVersioned) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Driver.ReadVersion(ctx, key, id)
}

func (f failNthVersioned) RestoreVersion(ctx context.Context, key, id string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.RestoreVersion(ctx, key, id)
}

// TestVersionCasesReportEveryFailure is TestCasesReportEveryFailure for
// the cases whose calls failNth does not count: every driver call of
// StorageClass and VersionRead fails in turn, and the case must report it.
func TestVersionCasesReportEveryFailure(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "StorageClass" && c.Name != "VersionRead" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			for k := int64(1); ; k++ {
				drv := failNthVersioned{&failNth{Driver: memory.New(memory.WithVersions()), n: k}}
				d, err := filesystem.NewDisk("sweep", drv)
				if err != nil {
					t.Fatal(err)
				}
				if !c.Needs(d.Capabilities()) {
					t.Fatal("the versioned memory driver lacks the capability")
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

// shortHistory reports at most keep versions of a key, newest first, as a
// driver that drops history would.
type shortHistory struct {
	*memory.Driver
	keep int
}

func (s shortHistory) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	vs, err := s.Driver.Versions(ctx, key)
	return vs[:min(len(vs), s.keep)], err
}

// TestVersionReadCatchesLostHistory proves VersionRead fails a driver that
// reports fewer than the two versions it wrote, and one whose restore
// does not add a version.
func TestVersionReadCatchesLostHistory(t *testing.T) {
	var vr filesystemtest.Case
	for _, c := range filesystemtest.Cases() {
		if c.Name == "VersionRead" {
			vr = c
		}
	}
	for _, keep := range []int{1, 2} {
		d, err := filesystem.NewDisk("short", shortHistory{memory.New(memory.WithVersions()), keep})
		if err != nil {
			t.Fatal(err)
		}
		if err := vr.Check(t.Context(), d); err == nil {
			t.Errorf("VersionRead passed on a driver keeping %d versions", keep)
		}
	}
}

// classless is failNth declaring no StorageClass capability, so the
// refusal case runs.
type classless struct{ *failNth }

func (c classless) Capabilities() filesystem.Capabilities {
	caps := c.failNth.Capabilities()
	caps.StorageClass = false
	return caps
}

// TestStorageClassRefusalReportsAFailedWrite proves the refusal case
// reports the failure of the write it starts with instead of passing.
func TestStorageClassRefusalReportsAFailedWrite(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "StorageClassUnsupportedIsRefused" {
			continue
		}
		drv := classless{&failNth{Driver: memory.New(), n: 1}}
		d, err := filesystem.NewDisk("classless", drv)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Check(t.Context(), d); err == nil || !drv.fired.Load() {
			t.Fatalf("refusal case = %v after its write failed (fired %v), want the failure", err, drv.fired.Load())
		}
	}
}
