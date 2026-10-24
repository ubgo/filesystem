package records_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// objectVisibility is the visibility f's object has on d.
func objectVisibility(t *testing.T, d *filesystem.Disk, key string) filesystem.Visibility {
	t.Helper()
	st, err := d.Stat(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return st.Visibility
}

// TestTrashHidesPublicFileInPlace: a trashed public file's object is
// private while it is trashed, and public again after Restore; a private
// file's object is not touched.
func TestTrashHidesPublicFileInPlace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	row, _ := e.store.File(ctx, "t", f.ID)
	if got := objectVisibility(t, e.disk, f.Key); got != filesystem.VisibilityPrivate || row.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("trashed: object %s, row %s; want a private object under a public row", got, row.Visibility)
	}
	if err := e.store.Restore(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if got := objectVisibility(t, e.disk, f.Key); got != filesystem.VisibilityPublic {
		t.Fatalf("restored: object %s, want public", got)
	}

	g := e.put(t, "t", "b.png") // private
	if err := e.store.Trash(ctx, "t", g.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Restore(ctx, "t", g.ID); err != nil {
		t.Fatal(err)
	}
	if got := objectVisibility(t, e.disk, g.Key); got != filesystem.VisibilityPrivate {
		t.Fatalf("a private file's object became %s", got)
	}

	// A rolled-back trash puts the object back.
	boom := errors.New("later step failed")
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := e.store.Trash(ctx, "t", f.ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || objectVisibility(t, e.disk, f.Key) != filesystem.VisibilityPublic {
		t.Fatalf("rolled-back trash: %v, object %s", err, objectVisibility(t, e.disk, f.Key))
	}

	// A bucket public as a whole (no per-object visibility) cannot hide one
	// object: the trash still works and leaves it.
	bucket, _ := filesystem.NewDisk("bucket", noVisibility{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	mgr, _ := filesystem.NewManager(bucket)
	plain, err := records.New(openDB(t), testDialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bucket.WriteBytes(ctx, "p.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	pf, err := plain.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "bucket", Key: "p.png", Visibility: filesystem.VisibilityPublic})
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Trash(ctx, "t", pf.ID); err != nil {
		t.Fatalf("trash on a disk without per-object visibility = %v", err)
	}
	if err := plain.Restore(ctx, "t", pf.ID); err != nil {
		t.Fatal(err)
	}

	// A visibility such a disk cannot give is refused, so the row never
	// claims what the object is not.
	privateOnly, _ := filesystem.NewDisk("vault", noVisibility{memory.New()})
	mgr2, _ := filesystem.NewManager(privateOnly)
	strict, _ := records.New(openDB(t), testDialect(), mgr2)
	_, _ = strict.Migrate(ctx)
	_, _ = privateOnly.WriteBytes(ctx, "q.png", []byte(pngData), filesystem.WriteOptions{})
	_, err = strict.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "vault", Key: "q.png", Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
}

// TestSetVisibilityOfTrashedFile: a trashed file's visibility changes in
// its row only, its object staying hidden, and Restore applies it.
func TestSetVisibilityOfTrashedFile(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png") // private
	_ = e.store.Trash(ctx, "t", f.ID)
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	row, _ := e.store.File(ctx, "t", f.ID)
	if row.Visibility != filesystem.VisibilityPublic || objectVisibility(t, e.disk, f.Key) != filesystem.VisibilityPrivate {
		t.Fatalf("row %s, object %s; want a public row over a hidden object", row.Visibility, objectVisibility(t, e.disk, f.Key))
	}
	if err := e.store.Restore(ctx, "t", f.ID); err != nil || objectVisibility(t, e.disk, f.Key) != filesystem.VisibilityPublic {
		t.Fatalf("restore = %v, object %s", err, objectVisibility(t, e.disk, f.Key))
	}
	_ = e.store.Trash(ctx, "t", f.ID)
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Restore(ctx, "t", f.ID); err != nil || objectVisibility(t, e.disk, f.Key) != filesystem.VisibilityPrivate {
		t.Fatalf("restore = %v, object %s", err, objectVisibility(t, e.disk, f.Key))
	}

	// Restore could not apply it on a disk without per-object visibility,
	// so the change is refused, as on a live file.
	plain := newEnv(t, noVisibility{memory.New()})
	g := plain.put(t, "t", "b.png")
	_ = plain.store.Trash(ctx, "t", g.ID)
	wantCode(t, plain.store.SetVisibility(ctx, "t", g.ID, filesystem.VisibilityPublic), filesystem.CodeUnsupported)

	// A rolled-back change of a trashed file leaves its row as it was.
	_ = e.store.Trash(ctx, "t", f.ID)
	boom := errors.New("later step failed")
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
			return err
		}
		return boom
	})
	if row, _ := e.store.File(ctx, "t", f.ID); !errors.Is(err, boom) || row.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("rolled back: %v, row %s", err, row.Visibility)
	}

	// The file's disk is gone from the manager.
	empty, _ := filesystem.NewManager()
	other, _ := records.New(e.db, testDialect(), empty)
	wantCode(t, other.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic), filesystem.CodeNotFound)
	_ = e.store.Restore(ctx, "t", f.ID)
	_ = e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic)
	wantCode(t, other.Trash(ctx, "t", f.ID), filesystem.CodeNotFound)
}

// TestTrashHidesPublicFileOnVisibilityDisks: with WithVisibilityDisks a
// trashed public file's object moves to the private disk, its row keeping
// its visibility, and Restore moves it back.
func TestTrashHidesPublicFileOnVisibilityDisks(t *testing.T) {
	ctx := t.Context()
	e := newSplit(t)
	public := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := e.store.Upload(ctx, "t", "", strings.NewReader(pngData), "a.png", public)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	row, _ := e.store.File(ctx, "t", f.ID)
	if row.Disk != "vault" || row.Visibility != filesystem.VisibilityPublic || keys(t, e.pub) != 0 || keys(t, e.priv) != 1 {
		t.Fatalf("trashed: %+v, public %d, private %d", row, keys(t, e.pub), keys(t, e.priv))
	}
	if err := e.store.Restore(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	row, _ = e.store.File(ctx, "t", f.ID)
	if row.Disk != "cdn" || keys(t, e.pub) != 1 || keys(t, e.priv) != 0 {
		t.Fatalf("restored: %+v, public %d, private %d", row, keys(t, e.pub), keys(t, e.priv))
	}
	if got, _ := e.pub.ReadAll(ctx, row.Key); string(got) != pngData {
		t.Fatalf("restored content = %q", got)
	}

	// A rolled-back trash leaves the file on the public disk, no copy behind.
	boom := errors.New("later step failed")
	err = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := e.store.Trash(ctx, "t", f.ID); err != nil {
			return err
		}
		return boom
	})
	row, _ = e.store.File(ctx, "t", f.ID)
	if !errors.Is(err, boom) || row.Disk != "cdn" || keys(t, e.pub) != 1 || keys(t, e.priv) != 0 {
		t.Fatalf("rolled-back trash: %v, %+v, public %d, private %d", err, row, keys(t, e.pub), keys(t, e.priv))
	}

	// A private file is already on the private disk.
	g, _ := e.store.Upload(ctx, "t", "", strings.NewReader(pngData), "b.png", pngPolicy)
	if err := e.store.Trash(ctx, "t", g.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.store.File(ctx, "t", g.ID); row.Disk != "vault" {
		t.Fatalf("a trashed private file moved: %+v", row)
	}
}

// TestTrashStorageSweep fails every storage operation of trashing and of
// restoring a public file on visibility disks: the file stays where it
// was, with nothing left on the other disk, and the run that succeeds
// leaves it where it belongs.
func TestTrashStorageSweep(t *testing.T) {
	ctx := t.Context()
	var failAt, seen int // failAt 0 never fails
	fail := filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		seen++
		if seen == failAt {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	})
	pub, _ := filesystem.NewDisk("cdn", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), fail)
	priv, _ := filesystem.NewDisk("vault", memory.New(), fail)
	mgr, _ := filesystem.NewManager(priv, pub)
	s, err := records.New(openDB(t), testDialect(), mgr, records.WithVisibilityDisks("cdn", "vault"),
		records.WithObjectErrorHandler(func(context.Context, records.File, error) {}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	public := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := s.Upload(ctx, "t", "", strings.NewReader(pngData), "a.png", public)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name     string
		run      func() error
		from, to *filesystem.Disk
	}{
		{"trash", func() error { return s.Trash(ctx, "t", f.ID) }, pub, priv},
		{"restore", func() error { return s.Restore(ctx, "t", f.ID) }, priv, pub},
	}
	for _, st := range steps {
		for n := 1; ; n++ {
			failAt, seen = n, 0
			err := st.run()
			failAt = 0
			row, _ := s.File(ctx, "t", f.ID)
			if err == nil {
				if row.Disk != st.to.Name() || keys(t, st.to) != 1 {
					t.Fatalf("%s: after it succeeded: %+v, %s has %d", st.name, row, st.to.Name(), keys(t, st.to))
				}
				// A failed deletion of the old copy after the commit is
				// reported, not undone; remove the leftover.
				_ = st.from.Delete(ctx, f.Key)
				break
			}
			if row.Disk != st.from.Name() || keys(t, st.from) != 1 || keys(t, st.to) != 0 {
				t.Fatalf("%s: storage failure %d: %+v, %s %d, %s %d", st.name, n, row, st.from.Name(), keys(t, st.from), st.to.Name(), keys(t, st.to))
			}
		}
	}
}

// TestTrashRefusedByDisk: when the disk refuses to make a public file's
// object private, the trash is refused and nothing changes.
func TestTrashRefusedByDisk(t *testing.T) {
	ctx := t.Context()
	refuse := false
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if refuse && op.Kind == filesystem.OpSetVisibility {
			return filesystem.Errorf(filesystem.CodeUnavailable, "refused")
		}
		return call(ctx)
	}))
	mgr, _ := filesystem.NewManager(disk)
	s, _ := records.New(openDB(t), testDialect(), mgr)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	f, _ := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
	refuse = true
	wantCode(t, s.Trash(ctx, "t", f.ID), filesystem.CodeUnavailable)
	if row, _ := s.File(ctx, "t", f.ID); row.DeletedAt != nil || objectVisibility(t, disk, f.Key) != filesystem.VisibilityPublic {
		t.Fatalf("a refused trash changed the file: %+v, object %s", row, objectVisibility(t, disk, f.Key))
	}
}

func TestPurgeIfTrashed(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	live, trashed := e.put(t, "t", "a.png"), e.put(t, "t", "b.png")
	_ = e.store.Trash(ctx, "t", trashed.ID)
	wantCode(t, e.store.Purge(ctx, "t", live.ID, records.IfTrashed(true)), filesystem.CodeUnavailable)
	wantCode(t, e.store.Purge(ctx, "t", trashed.ID, records.IfTrashed(false)), filesystem.CodeUnavailable)
	for _, f := range []records.File{live, trashed} {
		if _, err := e.store.File(ctx, "t", f.ID); err != nil {
			t.Fatalf("a refused purge removed %s: %v", f.Key, err)
		}
	}
	if err := e.store.Purge(ctx, "t", trashed.ID, records.IfTrashed(true)); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(ctx, "t", live.ID, records.IfTrashed(false)); err != nil {
		t.Fatal(err)
	}
}
