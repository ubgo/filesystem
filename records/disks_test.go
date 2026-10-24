package records_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// renamer records the renames it is asked for and fails when fail is set.
type renamer struct {
	calls []string
	fail  error
}

func (r *renamer) Name() string { return "renamer" }

func (r *renamer) RenameDisk(_ context.Context, _ *records.Store, from, to string) error {
	if r.fail != nil {
		return r.fail
	}
	r.calls = append(r.calls, from+"->"+to)
	return nil
}

func TestRenameDisk(t *testing.T) {
	ctx := t.Context()
	// One store reached under two names, as during a rename.
	drv := memory.New()
	oldDisk, _ := filesystem.NewDisk("media", drv)
	newDisk, _ := filesystem.NewDisk("media-s3", drv)
	other, _ := filesystem.NewDisk("other", memory.New())
	mgr, _ := filesystem.NewManager(oldDisk, newDisk, other)
	r := &renamer{}
	db := openDB(t)
	s, err := records.New(db, testDialect(), mgr, records.WithPlugins(r))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	record := func(d *filesystem.Disk, scope, key string) records.File {
		t.Helper()
		if _, err := d.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: d.Name(), Key: key})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a, b, trashed, elsewhere := record(oldDisk, "t", "a.png"), record(oldDisk, "u", "b.png"), record(oldDisk, "t", "c.png"), record(other, "t", "d.png")
	if err := s.Trash(ctx, "t", trashed.ID); err != nil {
		t.Fatal(err)
	}

	// A plugin that fails rolls the whole rename back.
	r.fail = errors.New("refused")
	if _, err := s.RenameDisk(ctx, "media", "media-s3"); !errors.Is(err, r.fail) {
		t.Fatalf("refused rename = %v", err)
	}
	if f, _ := s.File(ctx, "t", a.ID); f.Disk != "media" {
		t.Fatalf("a rolled-back rename changed the row: %s", f.Disk)
	}
	r.fail = nil

	n, err := s.RenameDisk(ctx, "media", "media-s3")
	if err != nil || n != 3 {
		t.Fatalf("RenameDisk = %d, %v", n, err)
	}
	for _, f := range []records.File{a, b, trashed} {
		got, err := s.File(ctx, f.Scope, f.ID)
		if err != nil || got.Disk != "media-s3" {
			t.Fatalf("%s is on %q, %v", f.Key, got.Disk, err)
		}
		// The bytes are reached through the new name.
		if ok, err := newDisk.Exists(ctx, got.Key); !ok || err != nil {
			t.Fatalf("%s: %v", f.Key, err)
		}
	}
	if got, _ := s.File(ctx, "t", elsewhere.ID); got.Disk != "other" {
		t.Fatalf("a file of another disk moved to %q", got.Disk)
	}
	if len(r.calls) != 1 || r.calls[0] != "media->media-s3" {
		t.Fatalf("plugin calls %v", r.calls)
	}
	// The old name need not exist any more; nothing is left on it.
	if n, err := s.RenameDisk(ctx, "gone", "media-s3"); err != nil || n != 0 {
		t.Fatalf("rename from an unknown name = %d, %v", n, err)
	}
	for _, c := range [][2]string{{"", "media"}, {"media", ""}, {"media", "media"}} {
		_, err := s.RenameDisk(ctx, c[0], c[1])
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	_, err = s.RenameDisk(ctx, "media-s3", "nope")
	wantCode(t, err, filesystem.CodeNotFound)
	// A database failure is returned.
	exec(t, db, "DROP TABLE "+records.TableNameFiles)
	if _, err := s.RenameDisk(ctx, "media-s3", "media"); err == nil {
		t.Fatal("a failed update was ignored")
	}
}

func TestRenameDiskWithVisibilityDisks(t *testing.T) {
	ctx := t.Context()
	drv := memory.New()
	pub, _ := filesystem.NewDisk("public", drv)
	priv, _ := filesystem.NewDisk("private", memory.New())
	pub2, _ := filesystem.NewDisk("public-s3", drv)
	mgr, _ := filesystem.NewManager(pub, priv, pub2)
	db := openDB(t)
	build := func(public string) *records.Store {
		s, err := records.New(db, testDialect(), mgr, records.WithVisibilityDisks(public, "private"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := build("public")
	if _, err := pub.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "public", Key: "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	// The Store still routes public files to "public": refused.
	_, err = s.RenameDisk(ctx, "public", "public-s3")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if got, _ := s.File(ctx, "t", f.ID); got.Disk != "public" {
		t.Fatalf("a refused rename moved the file to %q", got.Disk)
	}
	// Rebuilt with the new name, the rename goes through.
	s = build("public-s3")
	if n, err := s.RenameDisk(ctx, "public", "public-s3"); err != nil || n != 1 {
		t.Fatalf("RenameDisk after the rebuild = %d, %v", n, err)
	}
}
