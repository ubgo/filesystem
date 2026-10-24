package gdrive_test

import (
	"net/http"
	"testing"

	"github.com/ubgo/filesystem"
)

// countRemovals makes f record how removals arrive: permanent deletes and
// trash updates of a file.
func countRemovals(f *fake) (deletes, trashes *int) {
	deletes, trashes = new(int), new(int)
	f.on(func(_ http.ResponseWriter, r *http.Request) bool {
		switch r.Method {
		case http.MethodDelete:
			*deletes++
		case http.MethodPatch:
			if r.URL.Query().Get("addParents") == "" {
				*trashes++
			}
		}
		return false
	})
	return deletes, trashes
}

// TestTrash proves Config.Trash moves deleted files and prefixes to the
// trash, never deleting permanently, and that a deny on trashing fails the
// Delete; and that without it removals stay permanent.
func TestTrash(t *testing.T) {
	ctx := t.Context()
	f := newFake(t)
	cfg := config(f)
	cfg.Trash = true
	d, _ := filesystem.NewDisk("gdrive", newDriver(t, cfg))
	for _, k := range []string{"a.txt", "dir/b.txt"} {
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	deletes, trashes := countRemovals(f)
	if err := d.Delete(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeletePrefix(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if *deletes != 0 || *trashes != 2 {
		t.Fatalf("deletes %d, trashes %d; want 0 and 2", *deletes, *trashes)
	}
	f.mu.Lock()
	kept := 0
	for _, it := range f.items {
		if it.trashed {
			kept++
		}
	}
	f.mu.Unlock()
	if kept != 2 {
		t.Errorf("%d items in the trash, want 2", kept)
	}
	for _, k := range []string{"a.txt", "dir/b.txt"} {
		if ok, err := d.Exists(ctx, k); err != nil || ok {
			t.Errorf("%s still visible after trashing: %v, %v", k, ok, err)
		}
	}

	if _, err := d.WriteBytes(ctx, "c.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPatch {
			f.fail(w, http.StatusForbidden, "insufficientFilePermissions")
			return true
		}
		return false
	})
	if err := d.Delete(ctx, "c.txt"); err == nil {
		t.Error("a refused trash update reported success")
	}
}

// TestDeleteIsPermanentByDefault pins the default: Delete sends a
// permanent delete and nothing goes to the trash.
func TestDeleteIsPermanentByDefault(t *testing.T) {
	ctx := t.Context()
	f := newFake(t)
	d, _ := filesystem.NewDisk("gdrive", newDriver(t, config(f)))
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	deletes, trashes := countRemovals(f)
	if err := d.Delete(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if *deletes != 1 || *trashes != 0 {
		t.Fatalf("deletes %d, trashes %d; want 1 and 0", *deletes, *trashes)
	}
}
