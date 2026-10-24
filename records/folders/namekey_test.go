package folders_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// keyedEnv is a Store with a folders plugin made with opts.
func keyedEnv(t *testing.T, opts ...folders.Option) (*records.Store, *folders.Plugin) {
	t.Helper()
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p := folders.New(opts...)
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, p
}

func mustCreate(t *testing.T, s *records.Store, p *folders.Plugin, parent, name string) folders.Folder {
	t.Helper()
	f, err := p.Create(t.Context(), s, scope, parent, name)
	if err != nil {
		t.Fatalf("Create %q: %v", name, err)
	}
	return f
}

// TestNameKeyFoldCase: with FoldCase, names that differ only in case clash
// on Create, Rename and Move; other names, and renaming a folder to a case
// variant of its own name, pass.
func TestNameKeyFoldCase(t *testing.T) {
	ctx := t.Context()
	s, p := keyedEnv(t, folders.WithNameKey(folders.FoldCase))
	docs := mustCreate(t, s, p, folders.RootID, "Docs")
	other := mustCreate(t, s, p, folders.RootID, "Other")
	inner := mustCreate(t, s, p, other.ID, "docs")

	if _, err := p.Create(ctx, s, scope, folders.RootID, "DOCS"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("Create DOCS = %v", err)
	}
	if err := p.Rename(ctx, s, scope, other.ID, "docs"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("Rename Other -> docs = %v", err)
	}
	if err := p.Move(ctx, s, scope, inner.ID, folders.RootID); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("Move docs beside Docs = %v", err)
	}
	// Allowed: a different name, and a folder's own case change.
	mustCreate(t, s, p, folders.RootID, "Docs 2")
	if err := p.Rename(ctx, s, scope, docs.ID, "DOCS"); err != nil {
		t.Fatalf("Rename Docs -> DOCS = %v", err)
	}
	if err := p.Move(ctx, s, scope, inner.ID, docs.ID); err != nil {
		t.Fatalf("Move docs into DOCS = %v", err)
	}
	got, _ := p.Folder(ctx, s, scope, docs.ID)
	if got.Name != "DOCS" {
		t.Fatalf("name stored as %q, want it as given", got.Name)
	}
}

// TestNameKeyCustom: any NameKey decides the clash, here one that ignores
// a trailing " (copy)".
func TestNameKeyCustom(t *testing.T) {
	s, p := keyedEnv(t, folders.WithNameKey(func(n string) string { return strings.TrimSuffix(n, " (copy)") }))
	mustCreate(t, s, p, folders.RootID, "Plans")
	if _, err := p.Create(t.Context(), s, scope, folders.RootID, "Plans (copy)"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("Create = %v", err)
	}
	mustCreate(t, s, p, folders.RootID, "plans") // case still counts under this key
}

// TestNameKeyDefault: without WithNameKey names compare byte for byte, as
// before: case variants live side by side, exact repeats clash.
func TestNameKeyDefault(t *testing.T) {
	s, p := keyedEnv(t)
	mustCreate(t, s, p, folders.RootID, "Docs")
	mustCreate(t, s, p, folders.RootID, "docs")
	if _, err := p.Create(t.Context(), s, scope, folders.RootID, "Docs"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("Create repeat = %v", err)
	}
}

// TestNameKeyFaults fails every database step of a create and a rename
// under WithNameKey in turn (the scope lock, the sibling read, the
// write): each failure is the injected fault, and the run after it
// succeeds.
func TestNameKeyFaults(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p := folders.New(folders.WithNameKey(folders.FoldCase))
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	docs := mustCreate(t, s, p, folders.RootID, "Docs")
	mustCreate(t, s, p, folders.RootID, "Other")
	for name, op := range map[string]func() error{
		"create": func() error { _, err := p.Create(ctx, s, scope, folders.RootID, "New"); return err },
		"rename": func() error { return p.Rename(ctx, s, scope, docs.ID, "Papers") },
	} {
		for n := 1; ; n++ {
			faults.Arm(n)
			err := op()
			faults.Arm(0)
			if err == nil {
				break
			}
			if !errors.Is(err, testdb.ErrInjected) {
				t.Fatalf("%s step %d: %v", name, n, err)
			}
		}
	}
}
