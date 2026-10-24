package local_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/local"
)

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	drv, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("local", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, newDisk)
}
