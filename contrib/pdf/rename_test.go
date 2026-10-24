package pdf_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// renameEnv is a Store over "media", "cdn" and "search", plus "cdn2", a
// second name for cdn's storage: what a disk is renamed to must already
// reach its objects under the same keys.
func renameEnv(t *testing.T, p *pdf.Plugin) (*env, map[string]*filesystem.Disk) {
	t.Helper()
	media := newDisk(t)
	shared := memory.New()
	cdn, _ := filesystem.NewDisk("cdn", shared)
	cdn2, _ := filesystem.NewDisk("cdn2", shared)
	search, _ := filesystem.NewDisk("search", memory.New())
	mgr, err := filesystem.NewManager(media, cdn, cdn2, search)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{disk: media, store: store}, map[string]*filesystem.Disk{"media": media, "cdn2": cdn2, "search": search}
}

// TestRenameDisk_RewritesDerivativeDisks pins the DiskRenamer: after
// Store.RenameDisk, a preview WithLocation put on the renamed disk is
// found under the new name (PreviewLocation, the sweep, the purge), and
// the text, on another disk, keeps its own.
func TestRenameDisk_RewritesDerivativeDisks(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	loc := func(f records.File, name string) (string, string, error) {
		if name == pdf.DerivedPreview {
			return "cdn", "previews/" + f.ID + "-", nil
		}
		return "search", "texts/" + f.ID + "-", nil
	}
	e, disks := renameEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithText(), pdf.WithLocation(loc)))
	f := uploadDoc(t, e)
	_, pKey, _ := pdf.PreviewLocation(f)
	_, tKey, _ := pdf.TextLocation(f)
	if _, err := e.store.RenameDisk(t.Context(), "cdn", "cdn2"); err != nil {
		t.Fatal(err)
	}
	got := e.reread(t, f)
	if disk, k, ok := pdf.PreviewLocation(got); !ok || disk != "cdn2" || k != pKey || !existsOn(t, disks["cdn2"], k) {
		t.Fatalf("preview after the rename at %s:%s (%v)", disk, k, got.Metadata)
	}
	if disk, k, _ := pdf.TextLocation(got); disk != "search" || k != tKey {
		t.Fatalf("the text's disk changed: %s:%s", disk, k)
	}
	if n := sweepAll(t, e.store, "cdn2"); n != 0 {
		t.Fatalf("the sweep of the renamed disk deleted %d objects", n)
	}
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if existsOn(t, disks["cdn2"], pKey) || existsOn(t, disks["search"], tKey) {
		t.Fatal("a derivative outlived its purged document")
	}
}
