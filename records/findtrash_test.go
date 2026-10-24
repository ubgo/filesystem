package records_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
)

// TestFindTrash: the dry run reports exactly the files SweepTrash would
// purge, across scopes and in ID order, and changes nothing.
func TestFindTrash(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	a := e.put(t, "a", "a.png")
	b := e.put(t, "b", "b.png")
	young := e.put(t, "a", "young.png")
	live := e.put(t, "a", "live.png")
	_ = e.store.Trash(ctx, "a", a.ID)
	_ = e.store.Trash(ctx, "b", b.ID)
	cutoff := e.now.Add(500 * time.Millisecond)
	e.now = e.now.Add(time.Hour)
	_ = e.store.Trash(ctx, "a", young.ID)

	var found []string
	n, err := e.store.FindTrash(ctx, cutoff, func(f records.File) { found = append(found, f.ID) })
	if err != nil || n != 2 || len(found) != 2 {
		t.Fatalf("FindTrash = %d, %v, found %v", n, err, found)
	}
	want := map[string]bool{a.ID: true, b.ID: true}
	for _, id := range found {
		if !want[id] {
			t.Errorf("FindTrash found %s, want only %v", id, want)
		}
	}
	if found[0] > found[1] {
		t.Errorf("FindTrash order = %v, want ID order", found)
	}
	for _, f := range []records.File{a, b, young, live} {
		if _, err := e.store.File(ctx, f.Scope, f.ID); err != nil {
			t.Errorf("%s gone after a dry run: %v", f.Key, err)
		}
	}
	if swept, err := e.store.SweepTrash(ctx, cutoff); err != nil || swept != n {
		t.Errorf("SweepTrash = %d, %v; dry run found %d", swept, err, n)
	}
}
