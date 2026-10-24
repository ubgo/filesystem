package records_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// raceEnv is a Store whose disk runs race, once, right after the object
// of a change had its visibility set: after the change read the file and
// before its transaction locks the row, the window IfUnchanged's
// re-check under the lock exists for.
type raceEnv struct {
	store *records.Store
	disk  *filesystem.Disk
	race  func(ctx context.Context)
}

func newRaceEnv(t *testing.T) *raceEnv {
	t.Helper()
	e := &raceEnv{}
	intercept := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		if op.Kind == filesystem.OpSetVisibility && e.race != nil {
			race := e.race
			e.race = nil
			race(ctx)
		}
		return err
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(intercept))
	if err != nil {
		t.Fatal(err)
	}
	e.disk = disk
	mgr, _ := filesystem.NewManager(disk)
	if e.store, err = records.New(openDB(t), testDialect(), mgr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *raceEnv) public(t *testing.T) records.File {
	t.Helper()
	p := images
	p.Visibility = filesystem.VisibilityPublic
	f, err := e.store.Upload(t.Context(), "t", "media", reader(pngData), "a.png", p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *raceEnv) visibility(t *testing.T, key string) filesystem.Visibility {
	t.Helper()
	st, err := e.disk.Stat(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return st.Visibility
}

// TestIfUnchangedRechecksUnderLock: a replacement landing after
// SetVisibility, Trash or Restore read the file, while it was changing
// the object, is caught by the check under the row lock; the change is
// refused with ErrChanged and its object change undone.
func TestIfUnchangedRechecksUnderLock(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name    string
		trashed bool
		run     func(s *records.Store, f records.File) error
	}{
		{"set visibility", false, func(s *records.Store, f records.File) error {
			return s.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate, records.IfUnchanged(f.SHA256))
		}},
		{"trash", false, func(s *records.Store, f records.File) error {
			return s.Trash(ctx, "t", f.ID, records.IfUnchanged(f.SHA256))
		}},
		{"restore", true, func(s *records.Store, f records.File) error {
			return s.Restore(ctx, "t", f.ID, records.IfUnchanged(f.SHA256))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRaceEnv(t)
			f := e.public(t)
			if c.trashed {
				if err := e.store.Trash(ctx, "t", f.ID); err != nil {
					t.Fatal(err)
				}
				if f, _ = e.store.File(ctx, "t", f.ID); f.DeletedAt == nil {
					t.Fatal("not trashed")
				}
			}
			e.race = func(ctx context.Context) {
				if c.trashed {
					_ = e.store.Restore(ctx, "t", f.ID)
				}
				if _, err := e.store.Replace(ctx, "t", f.ID, reader(pngData+"-v2"), "", images); err != nil {
					t.Error(err)
				}
				if c.trashed {
					_ = e.store.Trash(ctx, "t", f.ID)
				}
			}
			err := c.run(e.store, f)
			if !errors.Is(err, records.ErrChanged) || filesystem.CodeOf(err) != filesystem.CodeConflict {
				t.Fatalf("a change racing a replacement = %v", err)
			}
			cur, _ := e.store.File(ctx, "t", f.ID)
			want := filesystem.VisibilityPublic
			if c.trashed {
				want = filesystem.VisibilityPrivate // the trash hides it
			}
			if (cur.DeletedAt != nil) != c.trashed || cur.Visibility != filesystem.VisibilityPublic || e.visibility(t, cur.Key) != want {
				t.Fatalf("the refused change left the file %+v, object %s", cur, e.visibility(t, cur.Key))
			}
		})
	}
}

// TestIfUnchangedOnPurge: a purge naming stale content is refused and
// purges nothing; IfTrashed is refused on any change but Purge.
func TestIfUnchangedOnPurge(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", images)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Replace(ctx, "t", f.ID, reader(pngData+"-v2"), "", images); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(ctx, "t", f.ID, records.IfUnchanged(f.SHA256)); !errors.Is(err, records.ErrChanged) {
		t.Fatalf("a stale purge = %v", err)
	}
	cur, err := e.store.File(ctx, "t", f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		e.store.Trash(ctx, "t", f.ID, records.IfTrashed(false)),
		e.store.Restore(ctx, "t", f.ID, records.IfTrashed(true)),
		e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic, records.IfTrashed(false)),
	} {
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	_, err = e.store.Replace(ctx, "t", f.ID, reader(pngData), "", images, records.IfTrashed(false))
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if err := e.store.Purge(ctx, "t", f.ID, records.IfUnchanged(cur.SHA256)); err != nil {
		t.Fatal(err)
	}
}
