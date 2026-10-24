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
)

// folderNames lists folders by name.
func folderNames(fs []folders.Folder) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return strings.Join(out, ",")
}

// TestFolderShortcuts: a folder shows in other folders through shortcuts
// while its parent stays its owner; obvious mistakes are refused; moving
// it where it has a shortcut removes that shortcut; deleting a folder
// removes the shortcuts in it and to it, and a folder of shortcuts only is
// empty.
func TestFolderShortcuts(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithFolderShortcuts())
	proj, b, c := e.mkdir(t, folders.RootID, "proj"), e.mkdir(t, folders.RootID, "b"), e.mkdir(t, folders.RootID, "c")
	sub := e.mkdir(t, proj.ID, "sub")
	must(t, e.p.AddFolderShortcut(ctx, e.store, scope, proj.ID, b.ID))
	must(t, e.p.AddFolderShortcut(ctx, e.store, scope, proj.ID, c.ID))
	must(t, e.p.AddFolderShortcut(ctx, e.store, scope, proj.ID, c.ID)) // again: nothing
	must(t, e.p.AddFolderShortcut(ctx, e.store, scope, sub.ID, folders.RootID))
	in, err := e.p.FolderShortcutsIn(ctx, e.store, scope, b.ID)
	if err != nil || folderNames(in) != "proj" {
		t.Fatalf("FolderShortcutsIn(b) = %v, %v", folderNames(in), err)
	}
	if kids, _ := e.p.Children(ctx, e.store, scope, b.ID); len(kids) != 0 {
		t.Fatalf("a shortcut was listed as a child: %v", folderNames(kids))
	}
	if anc, _ := e.p.Ancestors(ctx, e.store, scope, sub.ID); len(anc) != 2 || anc[1] != proj.ID {
		t.Fatalf("Ancestors(sub) = %v: a shortcut changed the parent chain", anc)
	}
	for name, into := range map[string]string{"parent": folders.RootID, "itself": proj.ID, "inside": sub.ID} {
		if err := e.p.AddFolderShortcut(ctx, e.store, scope, proj.ID, into); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := e.p.AddFolderShortcut(ctx, e.store, scope, "fld_none", b.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("unknown target = %v", err)
	}
	if err := e.p.AddFolderShortcut(ctx, e.store, scope, proj.ID, "fld_none"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("unknown folder = %v", err)
	}
	must(t, e.p.Move(ctx, e.store, scope, proj.ID, c.ID))
	of, err := e.p.FolderShortcutsOf(ctx, e.store, scope, proj.ID)
	if err != nil || len(of) != 1 || of[0] != b.ID {
		t.Fatalf("after moving proj into c: shortcuts in %v, %v", of, err)
	}
	must(t, e.p.Delete(ctx, e.store, scope, b.ID)) // holds only a shortcut: empty
	if of, _ := e.p.FolderShortcutsOf(ctx, e.store, scope, proj.ID); len(of) != 0 {
		t.Fatalf("deleting b left shortcuts in %v", of)
	}
	must(t, e.p.Delete(ctx, e.store, scope, sub.ID))
	if of, _ := e.p.FolderShortcutsOf(ctx, e.store, scope, sub.ID); len(of) != 0 {
		t.Fatalf("deleting sub left its shortcut in %v", of)
	}
}

// TestFolderShortcutGivesNoAccess: a grant on the folder a shortcut sits
// in reaches neither the linked folder nor its files.
func TestFolderShortcutGivesNoAccess(t *testing.T) {
	ctx := t.Context()
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	fp := folders.New(folders.WithFolderShortcuts())
	ap := acl.New(acl.WithHierarchy(fp))
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(fp, ap))
	must(t, err)
	_, err = s.Migrate(ctx)
	must(t, err)
	shared, err := fp.Create(ctx, s, scope, folders.RootID, "shared")
	must(t, err)
	private, err := fp.Create(ctx, s, scope, folders.RootID, "private")
	must(t, err)
	inShared, err := s.Upload(ctx, scope, "media", strings.NewReader("a"), "a.txt", anyFile)
	must(t, err)
	inPrivate, err := s.Upload(ctx, scope, "media", strings.NewReader("b"), "b.txt", anyFile)
	must(t, err)
	must(t, fp.Put(ctx, s, scope, inShared.ID, shared.ID))
	must(t, fp.Put(ctx, s, scope, inPrivate.ID, private.ID))
	must(t, fp.AddFolderShortcut(ctx, s, scope, private.ID, shared.ID))
	_, err = ap.Grant(ctx, s, scope, acl.Grant{Subject: "team", Kind: acl.OnFolder, Resource: shared.ID, Level: acl.Read})
	must(t, err)
	for f, want := range map[string]bool{inShared.ID: true, inPrivate.ID: false} {
		if ok, err := ap.Can(ctx, s, scope, []string{"team"}, f, acl.Read); err != nil || ok != want {
			t.Fatalf("Can(%s) = %v, %v; want %v", f, ok, err, want)
		}
	}
}

// TestFolderShortcutsNeedOption: without WithFolderShortcuts adding one is
// CodeUnsupported and nothing changes; removing still works.
func TestFolderShortcutsNeedOption(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithShortcuts())
	a, b := e.mkdir(t, folders.RootID, "a"), e.mkdir(t, folders.RootID, "b")
	before := e.snapshot(t)
	if err := e.p.AddFolderShortcut(ctx, e.store, scope, a.ID, b.ID); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("AddFolderShortcut without the option = %v", err)
	}
	must(t, e.p.RemoveFolderShortcut(ctx, e.store, scope, a.ID, b.ID))
	if after := e.snapshot(t); after != before {
		t.Fatalf("changed:\n%s\n%s", before, after)
	}
}

// TestFolderShortcutEventsRollBack: the folder shortcut events are emitted
// in the change's transaction, so a refused one rolls the change back.
func TestFolderShortcutEventsRollBack(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithFolderShortcuts())
	a, b := e.mkdir(t, folders.RootID, "a"), e.mkdir(t, folders.RootID, "b")
	e.events.refuse = folders.EventFolderShortcutAdded
	before := e.snapshot(t)
	if err := e.p.AddFolderShortcut(ctx, e.store, scope, a.ID, b.ID); err == nil || e.snapshot(t) != before {
		t.Fatalf("a refused folder_shortcut_added: %v", err)
	}
	e.events.refuse = ""
	must(t, e.p.AddFolderShortcut(ctx, e.store, scope, a.ID, b.ID))
	e.events.refuse = folders.EventFolderShortcutRemoved
	before = e.snapshot(t)
	if err := e.p.RemoveFolderShortcut(ctx, e.store, scope, a.ID, b.ID); err == nil || e.snapshot(t) != before {
		t.Fatalf("a refused folder_shortcut_removed: %v", err)
	}
}

// TestFolderShortcutFaultSweep fails every database step of adding,
// listing and removing a folder shortcut in turn: each failure is the
// injected fault, returned, and a failed add or remove changes nothing.
func TestFolderShortcutFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, folders.WithFolderShortcuts())
	proj, b := e.mkdir(t, folders.RootID, "proj"), e.mkdir(t, folders.RootID, "b")
	shortcutsOf := func() string {
		t.Helper()
		of, err := e.p.FolderShortcutsOf(ctx, e.store, scope, proj.ID)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(of, ",")
	}
	ops := []struct {
		name string
		run  func() error
	}{
		{"add", func() error { return e.p.AddFolderShortcut(ctx, e.store, scope, proj.ID, b.ID) }},
		{"in", func() error { _, err := e.p.FolderShortcutsIn(ctx, e.store, scope, b.ID); return err }},
		{"of", func() error { _, err := e.p.FolderShortcutsOf(ctx, e.store, scope, proj.ID); return err }},
		{"remove", func() error { return e.p.RemoveFolderShortcut(ctx, e.store, scope, proj.ID, b.ID) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := shortcutsOf()
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: err = %v, want the injected fault", n, err)
				}
				if after := shortcutsOf(); after != before {
					t.Fatalf("step %d failed but changed the shortcuts: %q -> %q", n, before, after)
				}
			}
		})
	}
	if got := shortcutsOf(); got != "" {
		t.Fatalf("after the sweep proj has shortcuts in %q", got)
	}
}
