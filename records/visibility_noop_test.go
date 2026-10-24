package records_test

import (
	"testing"

	"github.com/ubgo/filesystem"
)

// TestSetVisibilityToItsOwnValue: setting a file's visibility to the one
// it has rewrites the row's existing values, which MySQL counts as no row
// changed; it must still succeed, on every engine.
func TestSetVisibilityToItsOwnValue(t *testing.T) {
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.txt")
	for range 2 {
		if err := e.store.SetVisibility(t.Context(), "t", f.ID, filesystem.VisibilityPublic); err != nil {
			t.Fatalf("SetVisibility: %v", err)
		}
	}
	got, err := e.store.File(t.Context(), "t", f.ID)
	if err != nil || got.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("file = %+v, %v", got, err)
	}
}
