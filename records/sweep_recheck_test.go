package records_test

import (
	"context"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// restorer restores another file the first time a purge reaches
// BeforePurge, inside that purge's transaction: a Restore that lands
// after a sweep selected both files and before it purges the second.
type restorer struct {
	scope string
	ids   []string
	done  bool
	got   string
}

func (*restorer) Name() string { return "restorer" }
func (*restorer) BeforeCreate(context.Context, *records.Store, *records.File) error {
	return nil
}
func (*restorer) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (r *restorer) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	if r.done {
		return nil
	}
	r.done = true
	for _, id := range r.ids {
		if id != f.ID {
			r.got = id
			return s.Restore(ctx, r.scope, id)
		}
	}
	return nil
}

// TestSweepSkipsAFileRestoredMidSweep: SweepTrash selects A and B, and B
// is restored while A is purged. B must survive, live, and not count.
func TestSweepSkipsAFileRestoredMidSweep(t *testing.T) {
	ctx := t.Context()
	plug := &restorer{scope: "acme"}
	e := newEnv(t, nil, records.WithPlugins(plug))
	a := e.put(t, "acme", "a.png")
	b := e.put(t, "acme", "b.png")
	plug.ids = []string{a.ID, b.ID}
	for _, f := range []records.File{a, b} {
		if err := e.store.Trash(ctx, "acme", f.ID); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.store.SweepTrash(ctx, e.now.Add(1<<40))
	if err != nil || n != 1 {
		t.Fatalf("SweepTrash = %d, %v; want 1 purged, no error", n, err)
	}
	restored, err := e.store.File(ctx, "acme", plug.got)
	if err != nil || restored.DeletedAt != nil {
		t.Fatalf("the file restored mid-sweep: %+v, %v", restored, err)
	}
	if ok, _ := e.disk.Exists(ctx, restored.Key); !ok {
		t.Fatal("the object of the restored file was deleted")
	}
	if _, err := e.store.File(ctx, "acme", other(plug.ids, plug.got)); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("the other file was not purged: %v", err)
	}
}

func other(ids []string, not string) string {
	if ids[0] == not {
		return ids[1]
	}
	return ids[0]
}
