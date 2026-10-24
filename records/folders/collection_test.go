package folders_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records/folders"
)

// TestCollection: the folder collection lists and contains only the
// files directly in its folder, accepts a deposit into it, and refuses
// the root, which would share the whole scope.
func TestCollection(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	c := e.p.Collection()
	if c.Kind() != folders.KindFolder {
		t.Fatalf("Kind = %q", c.Kind())
	}
	docs := e.mkdir(t, folders.RootID, "docs")
	sub := e.mkdir(t, docs.ID, "sub")
	in, deeper, out := e.file(t, "in.txt"), e.file(t, "deeper.txt"), e.file(t, "out.txt")
	if err := e.p.Put(ctx, e.store, scope, in.ID, docs.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Put(ctx, e.store, scope, deeper.ID, sub.ID); err != nil {
		t.Fatal(err)
	}
	files, err := c.Files(ctx, e.store, scope, docs.ID, 10)
	if err != nil || len(files) != 1 || files[0].ID != in.ID {
		t.Fatalf("Files = %+v, %v", files, err)
	}
	for _, want := range []struct {
		id string
		in bool
	}{{in.ID, true}, {deeper.ID, false}, {out.ID, false}} {
		if got, err := c.Contains(ctx, e.store, scope, docs.ID, want.id); err != nil || got != want.in {
			t.Fatalf("Contains(%s) = %v, %v; want %v", want.id, got, err, want.in)
		}
	}
	if err := c.Accept(ctx, e.store, scope, docs.ID, out); err != nil {
		t.Fatal(err)
	}
	if got := e.filesIn(t, docs.ID); !strings.Contains(got, "out.txt") {
		t.Fatalf("after Accept the folder holds %q", got)
	}

	_, err = c.Files(ctx, e.store, scope, folders.RootID, 10)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = c.Contains(ctx, e.store, scope, folders.RootID, in.ID)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	wantCode(t, c.Accept(ctx, e.store, scope, folders.RootID, in), filesystem.CodeInvalidArgument)
}

// TestAncestors: a folder's chain runs from itself up to the top level,
// the root's is empty, and an unknown folder is CodeNotFound.
func TestAncestors(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a := e.mkdir(t, folders.RootID, "a")
	b := e.mkdir(t, a.ID, "b")
	got, err := e.p.Ancestors(ctx, e.store, scope, b.ID)
	if err != nil || strings.Join(got, ",") != b.ID+","+a.ID {
		t.Fatalf("Ancestors = %v, %v", got, err)
	}
	if got, err := e.p.Ancestors(ctx, e.store, scope, folders.RootID); err != nil || len(got) != 0 {
		t.Fatalf("Ancestors(root) = %v, %v", got, err)
	}
	_, err = e.p.Ancestors(ctx, e.store, scope, "fld_missing")
	wantCode(t, err, filesystem.CodeNotFound)
}
