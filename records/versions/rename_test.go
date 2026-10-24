package versions_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/versions"
)

// TestRenameDisk: Store.RenameDisk moves kept versions to the new disk
// name with their file, so they still read and restore; a version on
// another disk keeps its name.
func TestRenameDisk(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	shared := memory.New() // one backend under two names, as when a disk is renamed
	oldDisk, _ := filesystem.NewDisk("media", shared)
	newDisk, _ := filesystem.NewDisk("media-v2", shared)
	mgr, _ := filesystem.NewManager(oldDisk, newDisk)
	p, err := versions.New()
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, faults: faults, store: s, disk: oldDisk, p: p}
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")

	n, err := s.RenameDisk(ctx, "media", "media-v2")
	if err != nil || n != 1 {
		t.Fatalf("RenameDisk = %d, %v", n, err)
	}
	list, err := p.Versions(ctx, s, scope, f.ID)
	if err != nil || len(list) != 1 || list[0].Disk != "media-v2" {
		t.Fatalf("versions after the rename = %+v, %v", list, err)
	}
	if _, err := p.Restore(ctx, s, scope, list[0].ID); err != nil {
		t.Fatalf("restore from the renamed disk: %v", err)
	}
	if got := e.content(t, f.ID); got != "one" {
		t.Fatalf("restored content = %q", got)
	}
	if n, err := s.RenameDisk(ctx, "other", "media"); err != nil || n != 0 {
		t.Fatalf("renaming an unused name = %d, %v", n, err)
	}
	for _, v := range mustVersions(t, p, s, f.ID) {
		if v.Disk != "media-v2" {
			t.Fatalf("a version moved by an unrelated rename: %+v", v)
		}
	}
}

func mustVersions(t *testing.T, p *versions.Plugin, s *records.Store, fileID string) []versions.Version {
	t.Helper()
	list, err := p.Versions(t.Context(), s, scope, fileID)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
