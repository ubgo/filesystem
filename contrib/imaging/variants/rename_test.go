package variants_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// renameEnv is an env whose Manager also has a disk "archive" over the
// same driver, so objects are reachable under both names.
func renameEnv(t *testing.T) (*env, *records.Store) {
	t.Helper()
	e := newEnv(t, nil, transforms)
	drv := memory.New()
	media, _ := filesystem.NewDisk("media", drv)
	archive, _ := filesystem.NewDisk("archive", drv)
	mgr, _ := filesystem.NewManager(media, archive)
	p, err := variants.New(transforms, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(e.db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	e.disk, e.store, e.plugin = media, s, p
	return e, s
}

func (e *env) variantDisks(t *testing.T) map[string]int {
	t.Helper()
	rows, err := e.db.Query("SELECT disk FROM " + variants.TableNameVariants)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out[d]++
	}
	return out
}

// TestRenameDiskMovesVariantRows pins that Store.RenameDisk carries the
// variant rows with the files, and that the variants still resolve.
func TestRenameDiskMovesVariantRows(t *testing.T) {
	ctx := t.Context()
	e, s := renameEnv(t)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	v, err := e.plugin.Get(ctx, s, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Get(ctx, s, "t", f.ID, "square"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameDisk(ctx, "media", "archive"); err != nil {
		t.Fatal(err)
	}
	if got := e.variantDisks(t); got["archive"] != 2 || got["media"] != 0 {
		t.Errorf("variant disks = %v", got)
	}
	again, err := e.plugin.Get(ctx, s, "t", f.ID, "thumb")
	if err != nil || again.ID != v.ID || again.Disk != "archive" {
		t.Errorf("Get after rename = %+v, %v", again, err)
	}
}

// TestRenameDiskFailureRollsBack pins that a failing variant update undoes
// the whole rename, the files' rows included.
func TestRenameDiskFailureRollsBack(t *testing.T) {
	recordstest.OnlySQLite(t) // injects failures with SQLite triggers
	ctx := t.Context()
	e, s := renameEnv(t)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	if _, err := e.plugin.Get(ctx, s, "t", f.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec("CREATE TRIGGER no_rename BEFORE UPDATE ON " + variants.TableNameVariants + " BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameDisk(ctx, "media", "archive"); err == nil {
		t.Fatal("rename succeeded with a failing variant update")
	}
	if got, err := s.File(ctx, "t", f.ID); err != nil || got.Disk != "media" {
		t.Errorf("file after a failed rename = %+v, %v", got, err)
	}
	if got := e.variantDisks(t); got["media"] != 1 {
		t.Errorf("variant disks = %v", got)
	}
}
