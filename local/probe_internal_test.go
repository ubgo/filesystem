package local

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// caseRoot answers Stat of a lower-case staging name with err, as a
// case-sensitive filesystem (fs.ErrNotExist) or a failing one would.
type caseRoot struct {
	fileRoot
	err error
}

func (c caseRoot) Stat(name string) (fs.FileInfo, error) {
	if name == strings.ToLower(name) && strings.HasPrefix(name, tmpDir) {
		return nil, c.err
	}
	return c.fileRoot.Stat(name)
}

func TestProbeCaseOutcomes(t *testing.T) {
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	root := osRoot{native}
	if err := root.MkdirAll(tmpDir, DefaultDirMode); err != nil {
		t.Fatal(err)
	}
	if folds, err := probeCase(caseRoot{root, fs.ErrNotExist}); err != nil || folds {
		t.Errorf("case-sensitive filesystem: folds=%v, %v", folds, err)
	}
	if _, err := probeCase(caseRoot{root, errInjected}); !errors.Is(err, errInjected) {
		t.Errorf("failing stat: %v", err)
	}
	// The probe leaves nothing in staging.
	if left, _ := os.ReadDir(native.Name() + "/" + tmpDir); len(left) != 0 {
		t.Errorf("probe left %d files", len(left))
	}
}
