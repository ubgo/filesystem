package folders_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/upload"
)

var anyFile = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// linkedIn lists the files with a shortcut in folder, by filename.
func (e *env) linkedIn(t *testing.T, folder string, trashed bool) string {
	t.Helper()
	page, err := e.p.Linked(t.Context(), e.store, folder, records.FileQuery{Scope: scope, Trashed: trashed})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range page.Files {
		out = append(out, f.Key)
	}
	return strings.Join(out, ",")
}

// TestShortcutsKeepTheHome: a file appears in two more folders through
// shortcuts while its home stays the owner; Put moves the home and
// leaves the shortcuts, and putting it home where it had a shortcut
// removes that shortcut.
func TestShortcutsKeepTheHome(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithShortcuts())
	home, b, c := e.mkdir(t, folders.RootID, "home"), e.mkdir(t, folders.RootID, "b"), e.mkdir(t, folders.RootID, "c")
	f := e.file(t, "f.txt")
	must(t, e.p.Put(ctx, e.store, scope, f.ID, home.ID))
	must(t, e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID))
	must(t, e.p.AddShortcut(ctx, e.store, scope, f.ID, c.ID))
	must(t, e.p.AddShortcut(ctx, e.store, scope, f.ID, c.ID)) // again: nothing
	if got := e.linkedIn(t, b.ID, false); got != "f.txt" {
		t.Fatalf("Linked(b) = %q", got)
	}
	if got := e.filesIn(t, b.ID); got != "" {
		t.Fatalf("a shortcut was listed as a home: %q", got)
	}
	if got, _ := e.p.FolderOf(ctx, e.store, scope, f.ID); got != home.ID {
		t.Fatalf("FolderOf = %q, want the home", got)
	}
	if err := e.p.AddShortcut(ctx, e.store, scope, f.ID, home.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a shortcut into the home = %v", err)
	}
	must(t, e.p.Put(ctx, e.store, scope, f.ID, c.ID))
	in, err := e.p.ShortcutsOf(ctx, e.store, scope, f.ID)
	if err != nil || len(in) != 1 || in[0] != b.ID {
		t.Fatalf("after putting the file home into c: shortcuts %v, %v", in, err)
	}
	must(t, e.p.RemoveShortcut(ctx, e.store, scope, f.ID, b.ID))
	if got := e.linkedIn(t, b.ID, false); got != "" {
		t.Fatalf("RemoveShortcut left %q", got)
	}
}

// TestShortcutsTrashPurgeDelete: a trashed file leaves the live Linked
// list and comes back on restore; deleting a folder removes the shortcuts
// in it (a folder of shortcuts only is empty) and never the file; purging
// the file removes its shortcuts.
func TestShortcutsTrashPurgeDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithShortcuts())
	b, c := e.mkdir(t, folders.RootID, "b"), e.mkdir(t, folders.RootID, "c")
	f := e.file(t, "f.txt")
	must(t, e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID))
	must(t, e.p.AddShortcut(ctx, e.store, scope, f.ID, c.ID))
	must(t, e.store.Trash(ctx, scope, f.ID))
	if e.linkedIn(t, b.ID, false) != "" || e.linkedIn(t, b.ID, true) != "f.txt" {
		t.Fatal("a trashed file's shortcut must show in the trash view only")
	}
	if err := e.p.AddShortcut(ctx, e.store, scope, f.ID, folders.RootID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a shortcut to a trashed file = %v", err)
	}
	must(t, e.store.Restore(ctx, scope, f.ID))
	if e.linkedIn(t, b.ID, false) != "f.txt" {
		t.Fatal("restoring did not bring the shortcut back")
	}
	must(t, e.p.Delete(ctx, e.store, scope, b.ID))
	if in, _ := e.p.ShortcutsOf(ctx, e.store, scope, f.ID); len(in) != 1 || in[0] != c.ID {
		t.Fatalf("deleting a folder left shortcuts %v", in)
	}
	if _, err := e.store.File(ctx, scope, f.ID); err != nil {
		t.Fatalf("deleting a folder of shortcuts touched the file: %v", err)
	}
	must(t, e.store.Trash(ctx, scope, f.ID))
	must(t, e.store.Purge(ctx, scope, f.ID))
	if in, _ := e.p.ShortcutsOf(ctx, e.store, scope, f.ID); len(in) != 0 {
		t.Fatalf("purging left shortcuts %v", in)
	}
}

// TestShortcutGivesNoAccess: a folder grant reaches the file whose home
// the folder is, not a file linked into it by a shortcut.
func TestShortcutGivesNoAccess(t *testing.T) {
	ctx := t.Context()
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	fp := folders.New(folders.WithShortcuts())
	ap := acl.New(acl.WithHierarchy(fp))
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(fp, ap))
	must(t, err)
	_, err = s.Migrate(ctx)
	must(t, err)
	upload := func(name string) records.File {
		f, err := s.Upload(ctx, scope, "media", strings.NewReader(name), name, anyFile)
		must(t, err)
		return f
	}
	shared, err := fp.Create(ctx, s, scope, folders.RootID, "shared")
	must(t, err)
	homed, linked := upload("homed.txt"), upload("linked.txt")
	must(t, fp.Put(ctx, s, scope, homed.ID, shared.ID))
	must(t, fp.AddShortcut(ctx, s, scope, linked.ID, shared.ID))
	_, err = ap.Grant(ctx, s, scope, acl.Grant{Subject: "team", Kind: acl.OnFolder, Resource: shared.ID, Level: acl.Read})
	must(t, err)
	for f, want := range map[string]bool{homed.ID: true, linked.ID: false} {
		if ok, err := ap.Can(ctx, s, scope, []string{"team"}, f, acl.Read); err != nil || ok != want {
			t.Fatalf("Can(%s) = %v, %v; want %v", f, ok, err, want)
		}
	}
}

// TestShortcutsNeedOption: without WithShortcuts AddShortcut is
// CodeUnsupported and nothing changes; RemoveShortcut still works.
func TestShortcutsNeedOption(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	b := e.mkdir(t, folders.RootID, "b")
	f := e.file(t, "f.txt")
	before := e.snapshot(t)
	if err := e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("AddShortcut without the option = %v", err)
	}
	must(t, e.p.RemoveShortcut(ctx, e.store, scope, f.ID, b.ID))
	if after := e.snapshot(t); after != before {
		t.Fatalf("changed:\n%s\n%s", before, after)
	}
}

// TestShortcutEventsRollBack: the shortcut events are emitted in the
// change's transaction, so a hook refusing one rolls the change back.
func TestShortcutEventsRollBack(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithShortcuts())
	b := e.mkdir(t, folders.RootID, "b")
	f := e.file(t, "f.txt")
	e.events.refuse = folders.EventShortcutAdded
	before := e.snapshot(t)
	if err := e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID); err == nil || e.snapshot(t) != before {
		t.Fatalf("a refused shortcut_added: %v", err)
	}
	e.events.refuse = ""
	must(t, e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID))
	e.events.refuse = folders.EventShortcutRemoved
	before = e.snapshot(t)
	if err := e.p.RemoveShortcut(ctx, e.store, scope, f.ID, b.ID); err == nil || e.snapshot(t) != before {
		t.Fatalf("a refused shortcut_removed: %v", err)
	}
}

// TestShortcutFaultSweep fails every database step of adding, listing and
// removing shortcuts in turn: each failure is the injected fault,
// returned, never read as "no shortcut", and the run after the sweep
// succeeds.
func TestShortcutFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithShortcuts())
	home, b := e.mkdir(t, folders.RootID, "home"), e.mkdir(t, folders.RootID, "b")
	f := e.file(t, "f.txt")
	must(t, e.p.Put(ctx, e.store, scope, f.ID, home.ID))
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"add", func() error { return e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID) }},
		{"add again", func() error { return e.p.AddShortcut(ctx, e.store, scope, f.ID, b.ID) }},
		{"shortcuts of", func() error { _, err := e.p.ShortcutsOf(ctx, e.store, scope, f.ID); return err }},
		{"linked", func() error {
			_, err := e.p.Linked(ctx, e.store, b.ID, records.FileQuery{Scope: scope})
			return err
		}},
		{"remove", func() error { return e.p.RemoveShortcut(ctx, e.store, scope, f.ID, b.ID) }},
	} {
		for n := 1; ; n++ {
			e.faults.Arm(n)
			err := op.run()
			e.faults.Arm(0)
			if err == nil {
				break
			}
			if !errors.Is(err, testdb.ErrInjected) {
				t.Fatalf("%s step %d: %v", op.name, n, err)
			}
		}
	}
}

// TestShortcutUnknownFolder: a shortcut into, or a listing of, a folder
// that does not exist is CodeNotFound.
func TestShortcutUnknownFolder(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithShortcuts())
	f := e.file(t, "f.txt")
	if err := e.p.AddShortcut(ctx, e.store, scope, f.ID, "no-such-folder"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a shortcut into an unknown folder = %v", err)
	}
	if _, err := e.p.Linked(ctx, e.store, "no-such-folder", records.FileQuery{Scope: scope}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("Linked of an unknown folder = %v", err)
	}
}
