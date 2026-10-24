package local_test

import (
	"mime"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/sniff"
)

// custom is an application's own extension map, the model WithContentType
// lets a user bring instead of the host's MIME table.
func custom(key string) string {
	if key == "scan.dcm" {
		return "application/dicom"
	}
	return ""
}

func statType(t *testing.T, key string, opts ...local.Option) string {
	t.Helper()
	drv, err := local.New(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("l", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.WriteBytes(t.Context(), key, []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Stat(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	page, err := d.ListPage(t.Context(), "", filesystem.ListOptions{})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ContentType != e.ContentType {
		t.Fatalf("List disagrees with Stat: %+v %v", page, err)
	}
	return e.ContentType
}

func TestWithContentType(t *testing.T) {
	if got := statType(t, "scan.dcm", local.WithContentType(custom)); got != "application/dicom" {
		t.Errorf("custom map: %q", got)
	}
	if got := statType(t, "photo.png", local.WithContentType(custom)); got != "" {
		t.Errorf("custom map must decide alone, got %q", got)
	}
	if got := statType(t, "photo.png", local.WithContentType(sniff.TypeByExtension)); got != "image/png" {
		t.Errorf("sniff table: %q", got)
	}
	// Default unchanged: the host's table, nil option included.
	want := mime.TypeByExtension(".css")
	if got := statType(t, "site.css"); got != want {
		t.Errorf("default: %q, want the host's %q", got, want)
	}
	if got := statType(t, "site.css", local.WithContentType(nil)); got != want {
		t.Errorf("nil option: %q, want the host's %q", got, want)
	}
}
