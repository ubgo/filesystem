package local_test

import (
	"net/url"
	"path/filepath"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
)

func TestOpener(t *testing.T) {
	dir := t.TempDir()
	for _, dsn := range []string{"file://" + dir, "file:" + filepath.Join(dir, "rel")} {
		u, _ := url.Parse(dsn)
		drv, err := local.Opener(t.Context(), u)
		if err != nil {
			t.Fatalf("Opener(%q): %v", dsn, err)
		}
		_ = drv.(*local.Driver).Close()
	}
	t.Chdir(t.TempDir())
	u, _ := url.Parse("file:uploads/relative")
	if u.Opaque == "" {
		t.Fatal("expected an opaque relative DSN")
	}
	drv, err := local.Opener(t.Context(), u)
	if err != nil {
		t.Fatalf("relative DSN: %v", err)
	}
	_ = drv.(*local.Driver).Close()
	for _, dsn := range []string{"file://host/x", "file:", "file:///x?unknown=1"} {
		u, _ := url.Parse(dsn)
		if _, err := local.Opener(t.Context(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Opener(%q) = %v", dsn, err)
		}
	}
}
