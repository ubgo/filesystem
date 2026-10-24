package folders_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/sqlb"
)

const scope = "t"

type env struct {
	db     *sql.DB
	faults *testdb.Faults
	store  *records.Store
	disk   *filesystem.Disk
	p      *folders.Plugin
	events *recorder
}

// recorder logs the events of committed changes and fails the kind in
// refuse.
type recorder struct {
	log    []string
	refuse records.EventKind
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == r.refuse {
		return errors.New("refused")
	}
	line := string(e.Kind) + " " + e.Subject + e.FileID
	for _, k := range []string{folders.DetailParent, folders.DetailFolder, folders.DetailName, folders.DetailFrom} {
		if v, ok := e.Detail[k]; ok {
			line += " " + k + "=" + v
		}
	}
	s.AfterCommit(ctx, func(context.Context) { r.log = append(r.log, line) })
	return nil
}

func newEnv(t *testing.T, opts ...folders.Option) *env {
	t.Helper()
	e := &env{events: &recorder{}}
	e.db, e.faults = testdb.OpenFaulty(t)
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	e.p = folders.New(append([]folders.Option{folders.WithClock(func() time.Time { return now })}, opts...)...)
	var err error
	if e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithPlugins(e.p, e.events)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) file(t *testing.T, name string) records.File {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), name, []byte(name), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: name})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *env) mkdir(t *testing.T, parent, name string) folders.Folder {
	t.Helper()
	f, err := e.p.Create(t.Context(), e.store, scope, parent, name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func names(fs []folders.Folder) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return strings.Join(out, "/")
}

func (e *env) filesIn(t *testing.T, folder string) string {
	t.Helper()
	page, err := e.p.Files(t.Context(), e.store, folder, records.FileQuery{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range page.Files {
		out = append(out, f.Filename)
	}
	return strings.Join(out, ",")
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if filesystem.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"a", "Photos 2026", "été", strings.Repeat("é", folders.MaxNameChars), "a.b", ".hidden"} {
		if err := folders.CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "tab\there", "nul\x00", " lead", "trail ", "\xff", strings.Repeat("a", folders.MaxNameChars+1)} {
		wantCode(t, folders.CheckName(bad), filesystem.CodeInvalidArgument)
	}
}

func TestTree(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	docs := e.mkdir(t, folders.RootID, "Docs")
	e.mkdir(t, folders.RootID, "Archive")
	tax := e.mkdir(t, docs.ID, "Tax")
	e.mkdir(t, docs.ID, "Archive") // the same name under another parent

	if !strings.HasPrefix(docs.ID, folders.IDPrefix) || docs.ParentID != folders.RootID || docs.Scope != scope {
		t.Fatalf("created %+v", docs)
	}
	top, err := e.p.Children(ctx, e.store, scope, folders.RootID)
	if err != nil || names(top) != "Archive/Docs" {
		t.Fatalf("top level = %q, %v", names(top), err)
	}
	path, err := e.p.Path(ctx, e.store, scope, tax.ID)
	if err != nil || names(path) != "Docs/Tax" {
		t.Fatalf("path = %q, %v", names(path), err)
	}
	if path, err := e.p.Path(ctx, e.store, scope, folders.RootID); err != nil || len(path) != 0 {
		t.Fatalf("root path = %v, %v", path, err)
	}

	_, err = e.p.Create(ctx, e.store, scope, folders.RootID, "Docs")
	wantCode(t, err, filesystem.CodeExists)
	_, err = e.p.Create(ctx, e.store, scope, "fld_missing", "x")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.p.Create(ctx, e.store, scope, folders.RootID, "a/b")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if _, err := e.p.Create(ctx, e.store, "other", folders.RootID, "Docs"); err != nil {
		t.Fatalf("the same name in another scope: %v", err)
	}
	_, err = e.p.Folder(ctx, e.store, "other", docs.ID)
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestDepth(t *testing.T) {
	e := newEnv(t)
	parent := folders.RootID
	var chain []folders.Folder
	for i := range folders.MaxDepth {
		f := e.mkdir(t, parent, fmt.Sprint("d", i))
		chain = append(chain, f)
		parent = f.ID
	}
	_, err := e.p.Create(t.Context(), e.store, scope, parent, "too deep")
	wantCode(t, err, filesystem.CodeInvalidArgument)

	// A two-level subtree fits under depth MaxDepth-2, not under MaxDepth-1.
	sub := e.mkdir(t, folders.RootID, "sub")
	e.mkdir(t, sub.ID, "leaf")
	if err := e.p.Move(t.Context(), e.store, scope, sub.ID, chain[folders.MaxDepth-3].ID); err != nil {
		t.Fatalf("move to the last level that fits: %v", err)
	}
	wantCode(t, e.p.Move(t.Context(), e.store, scope, sub.ID, chain[folders.MaxDepth-2].ID), filesystem.CodeInvalidArgument)
}

func TestRename(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a := e.mkdir(t, folders.RootID, "a")
	e.mkdir(t, folders.RootID, "b")
	if err := e.p.Rename(ctx, e.store, scope, a.ID, "c"); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Rename(ctx, e.store, scope, a.ID, "c"); err != nil {
		t.Fatalf("renaming to the current name: %v", err)
	}
	wantCode(t, e.p.Rename(ctx, e.store, scope, a.ID, "b"), filesystem.CodeExists)
	wantCode(t, e.p.Rename(ctx, e.store, scope, a.ID, ".."), filesystem.CodeInvalidArgument)
	wantCode(t, e.p.Rename(ctx, e.store, scope, "fld_missing", "x"), filesystem.CodeNotFound)
	if f, _ := e.p.Folder(ctx, e.store, scope, a.ID); f.Name != "c" {
		t.Fatalf("name = %q", f.Name)
	}
}

func TestMove(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a := e.mkdir(t, folders.RootID, "a")
	b := e.mkdir(t, a.ID, "b")
	c := e.mkdir(t, folders.RootID, "c")
	e.mkdir(t, c.ID, "b")

	wantCode(t, e.p.Move(ctx, e.store, scope, a.ID, a.ID), filesystem.CodeInvalidArgument)
	wantCode(t, e.p.Move(ctx, e.store, scope, a.ID, b.ID), filesystem.CodeInvalidArgument)
	wantCode(t, e.p.Move(ctx, e.store, scope, b.ID, c.ID), filesystem.CodeExists)
	wantCode(t, e.p.Move(ctx, e.store, scope, "fld_missing", c.ID), filesystem.CodeNotFound)
	wantCode(t, e.p.Move(ctx, e.store, scope, b.ID, "fld_missing"), filesystem.CodeNotFound)

	if err := e.p.Move(ctx, e.store, scope, a.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if path, _ := e.p.Path(ctx, e.store, scope, b.ID); names(path) != "c/a/b" {
		t.Fatalf("path after move = %q", names(path))
	}
	if err := e.p.Move(ctx, e.store, scope, b.ID, folders.RootID); err != nil {
		t.Fatal(err)
	}
	if top, _ := e.p.Children(ctx, e.store, scope, folders.RootID); names(top) != "b/c" {
		t.Fatalf("top level = %q", names(top))
	}
}

// TestCycle plants the cycle two racing moves could leave behind: every
// walk stops at MaxDepth instead of looping.
func TestCycle(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a := e.mkdir(t, folders.RootID, "a")
	b := e.mkdir(t, a.ID, "b")
	other := e.mkdir(t, folders.RootID, "other")
	testdb.Exec(t, e.db, "UPDATE "+folders.TableNameFolders+" SET "+folders.ColParentID+" = '"+b.ID+"' WHERE "+folders.ColID+" = '"+a.ID+"'")
	_, err := e.p.Path(ctx, e.store, scope, a.ID)
	wantCode(t, err, filesystem.CodeUnknown)
	wantCode(t, e.p.Move(ctx, e.store, scope, other.ID, a.ID), filesystem.CodeUnknown)
	wantCode(t, e.p.Move(ctx, e.store, scope, a.ID, other.ID), filesystem.CodeUnknown)
}

func TestFiles(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	docs := e.mkdir(t, folders.RootID, "Docs")
	tax := e.mkdir(t, docs.ID, "Tax")
	a, b := e.file(t, "a.txt"), e.file(t, "b.txt")
	e.file(t, "c.txt")

	if err := e.p.Put(ctx, e.store, scope, a.ID, docs.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Put(ctx, e.store, scope, b.ID, docs.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Put(ctx, e.store, scope, b.ID, tax.ID); err != nil {
		t.Fatalf("moving a placed file: %v", err)
	}
	if got := e.filesIn(t, docs.ID) + "|" + e.filesIn(t, tax.ID) + "|" + e.filesIn(t, folders.RootID); got != "a.txt|b.txt|c.txt" {
		t.Fatalf("docs|tax|root = %s", got)
	}
	if in, err := e.p.FolderOf(ctx, e.store, scope, b.ID); err != nil || in != tax.ID {
		t.Fatalf("FolderOf = %q, %v", in, err)
	}
	if err := e.p.Put(ctx, e.store, scope, b.ID, folders.RootID); err != nil {
		t.Fatal(err)
	}
	if in, err := e.p.FolderOf(ctx, e.store, scope, b.ID); err != nil || in != folders.RootID {
		t.Fatalf("FolderOf after moving to the root = %q, %v", in, err)
	}

	wantCode(t, e.p.Put(ctx, e.store, scope, "fil_missing", docs.ID), filesystem.CodeNotFound)
	wantCode(t, e.p.Put(ctx, e.store, scope, a.ID, "fld_missing"), filesystem.CodeNotFound)
	wantCode(t, e.p.Put(ctx, e.store, "other", a.ID, folders.RootID), filesystem.CodeNotFound)
	_, err := e.p.FolderOf(ctx, e.store, scope, "fil_missing")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.p.Files(ctx, e.store, "fld_missing", records.FileQuery{Scope: scope})
	wantCode(t, err, filesystem.CodeNotFound)

	// A purged file leaves its folder.
	if err := e.store.Purge(ctx, scope, a.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM " + folders.TableNamePlaces).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d places left after purging, %v", n, err)
	}
}

func TestDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	docs := e.mkdir(t, folders.RootID, "Docs")
	tax := e.mkdir(t, docs.ID, "Tax")
	f := e.file(t, "a.txt")
	_ = e.p.Put(ctx, e.store, scope, f.ID, tax.ID)

	wantCode(t, e.p.Delete(ctx, e.store, scope, docs.ID), filesystem.CodeInvalidArgument) // has a subfolder
	wantCode(t, e.p.Delete(ctx, e.store, scope, tax.ID), filesystem.CodeInvalidArgument)  // has a live file
	if err := e.store.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Delete(ctx, e.store, scope, tax.ID); err != nil {
		t.Fatalf("deleting a folder holding only trash: %v", err)
	}
	if err := e.store.Restore(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if in, _ := e.p.FolderOf(ctx, e.store, scope, f.ID); in != folders.RootID || e.filesIn(t, folders.RootID) != "a.txt" {
		t.Fatalf("a restored file is in %q, root lists %q", in, e.filesIn(t, folders.RootID))
	}
	if err := e.p.Delete(ctx, e.store, scope, docs.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.p.Delete(ctx, e.store, scope, docs.ID), filesystem.CodeNotFound)
}

// snapshot renders the folder, place and both shortcut tables, to prove a failed operation changed
// nothing.
func (e *env) snapshot(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT " + folders.ColID + ", " + folders.ColParentID + ", " + folders.ColName + " FROM " + folders.TableNameFolders + " ORDER BY 1",
		"SELECT " + folders.ColFileID + ", " + folders.ColFolderID + ", '' FROM " + folders.TableNamePlaces + " ORDER BY 1",
		"SELECT " + folders.ColFileID + ", " + folders.ColFolderID + ", '' FROM " + folders.TableNameShortcuts + " ORDER BY 1, 2",
		"SELECT " + folders.ColTargetID + ", " + folders.ColFolderID + ", 'f' FROM " + folders.TableNameFolderShortcuts + " ORDER BY 1, 2",
	} {
		rows, err := e.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var x, y, z string
			_ = rows.Scan(&x, &y, &z)
			fmt.Fprintf(&b, "%s,%s,%s;", x, y, z)
		}
		_ = rows.Close()
		b.WriteString("|")
	}
	return b.String()
}

// TestFaultSweep fails every database step of every operation in turn:
// each run must report the injected error and change nothing, and the run
// after the last step must succeed.
func TestFaultSweep(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	docs := e.mkdir(t, folders.RootID, "Docs")
	tax := e.mkdir(t, docs.ID, "Tax")
	empty := e.mkdir(t, folders.RootID, "Empty")
	trashed := e.file(t, "old.txt")
	_ = e.p.Put(ctx, e.store, scope, trashed.ID, empty.ID)
	_ = e.store.Trash(ctx, scope, trashed.ID)
	f := e.file(t, "a.txt")

	ops := []struct {
		name string
		run  func() error
	}{
		{"create", func() error { _, err := e.p.Create(ctx, e.store, scope, tax.ID, "New"); return err }},
		{"rename", func() error { return e.p.Rename(ctx, e.store, scope, tax.ID, "Taxes") }},
		{"rename to the same name", func() error { return e.p.Rename(ctx, e.store, scope, tax.ID, "Taxes") }},
		{"move", func() error { return e.p.Move(ctx, e.store, scope, tax.ID, folders.RootID) }},
		{"put", func() error { return e.p.Put(ctx, e.store, scope, f.ID, docs.ID) }},
		{"put in the root", func() error { return e.p.Put(ctx, e.store, scope, f.ID, folders.RootID) }},
		{"delete", func() error { return e.p.Delete(ctx, e.store, scope, empty.ID) }},
		{"read: path", func() error { _, err := e.p.Path(ctx, e.store, scope, tax.ID); return err }},
		{"read: children", func() error { _, err := e.p.Children(ctx, e.store, scope, folders.RootID); return err }},
		{"read: folder of", func() error { _, err := e.p.FolderOf(ctx, e.store, scope, f.ID); return err }},
		{"read: files", func() error {
			_, err := e.p.Files(ctx, e.store, docs.ID, records.FileQuery{Scope: scope})
			return err
		}},
		{"purge", func() error { return e.store.Purge(ctx, scope, trashed.ID) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := e.snapshot(t)
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					if n == 1 {
						t.Fatal("the operation took no database step")
					}
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: err = %v, want the injected fault", n, err)
				}
				if after := e.snapshot(t); after != before {
					t.Fatalf("step %d failed but changed the tables:\nbefore %s\nafter  %s", n, before, after)
				}
			}
		})
	}
}

func TestEvents(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a := e.mkdir(t, folders.RootID, "a")
	b := e.mkdir(t, folders.RootID, "b")
	f := e.file(t, "f.txt")
	steps := []error{
		e.p.Rename(ctx, e.store, scope, a.ID, "a2"),
		e.p.Rename(ctx, e.store, scope, a.ID, "a2"), // no change, no event
		e.p.Move(ctx, e.store, scope, a.ID, b.ID),
		e.p.Move(ctx, e.store, scope, a.ID, b.ID), // no change, no event
		e.p.Put(ctx, e.store, scope, f.ID, a.ID),
		e.p.Put(ctx, e.store, scope, f.ID, a.ID), // no change, no event
		e.p.Put(ctx, e.store, scope, f.ID, folders.RootID),
		e.p.Delete(ctx, e.store, scope, a.ID),
	}
	if err := errors.Join(steps...); err != nil {
		t.Fatal(err)
	}
	ids := strings.NewReplacer(a.ID, "A", b.ID, "B", f.ID, "F")
	want := []string{
		"folders.created A parent= name=a",
		"folders.created B parent= name=b",
		"file.created F",
		"folders.renamed A name=a2 from=a",
		"folders.moved A parent=B from=",
		"folders.file_put F folder=A from=",
		"folders.file_put F folder= from=A",
		"folders.deleted A parent=B name=a2",
	}
	if got := ids.Replace(strings.Join(e.events.log, "\n")); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}

	// A hook refusing the event rolls the change back.
	m := e.mkdir(t, folders.RootID, "m")
	for _, c := range []struct {
		kind records.EventKind
		run  func() error
	}{
		{folders.EventCreated, func() error { _, err := e.p.Create(ctx, e.store, scope, folders.RootID, "c"); return err }},
		{folders.EventRenamed, func() error { return e.p.Rename(ctx, e.store, scope, b.ID, "b2") }},
		{folders.EventMoved, func() error { return e.p.Move(ctx, e.store, scope, m.ID, b.ID) }},
		{folders.EventFilePut, func() error { return e.p.Put(ctx, e.store, scope, f.ID, b.ID) }},
		{folders.EventDeleted, func() error { return e.p.Delete(ctx, e.store, scope, b.ID) }},
	} {
		e.events.refuse = c.kind
		before := e.snapshot(t)
		if err := c.run(); err == nil {
			t.Fatalf("%s: a refused event did not fail the change", c.kind)
		}
		e.events.refuse = ""
		if after := e.snapshot(t); after != before {
			t.Fatalf("%s: the change was kept:\n%s\n%s", c.kind, before, after)
		}
	}
	// The five tree kinds are refused above; the two shortcut kinds in
	// TestShortcutEventsRollBack; the two folder shortcut kinds in
	// TestFolderShortcutEventsRollBack.
	if len(folders.EventKindValues) != 9 {
		t.Fatalf("EventKindValues = %v", folders.EventKindValues)
	}
}

// TestRacingMovesCannotCycle holds the transaction of one move (A into B)
// open while a second (B into A) runs. The scope lock makes the second
// wait and then see the first, so it is refused as a move into itself;
// without it, on Postgres and MySQL both commit and the tree has a cycle.
// (SQLite has one writer, so there the second waits either way.)
func TestRacingMovesCannotCycle(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b := e.mkdir(t, folders.RootID, "a"), e.mkdir(t, folders.RootID, "b")
	held, release := make(chan struct{}), make(chan struct{})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
			if err := e.p.Move(ctx, e.store, scope, a.ID, b.ID); err != nil {
				close(held)
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	go func() { second <- e.p.Move(ctx, e.store, scope, b.ID, a.ID) }()
	// Without the lock the second move finishes at once; give it the time.
	var secondErr error
	finished := false
	select {
	case secondErr = <-second:
		finished = true
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first move: %v", err)
	}
	if !finished {
		secondErr = <-second
	}
	wantCode(t, secondErr, filesystem.CodeInvalidArgument)
	path, err := e.p.Path(ctx, e.store, scope, a.ID)
	if err != nil || names(path) != "b/a" {
		t.Fatalf("Path(a) = %q, %v", names(path), err)
	}
}

func TestPurgeScope(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	docs := e.mkdir(t, folders.RootID, "docs")
	e.mkdir(t, docs.ID, "tax")
	f := e.file(t, "a.txt")
	if err := e.p.Put(ctx, e.store, scope, f.ID, docs.ID); err != nil {
		t.Fatal(err)
	}
	other, err := e.p.Create(ctx, e.store, "u", folders.RootID, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.PurgeScope(ctx, scope); err != nil || n != 1 {
		t.Fatalf("PurgeScope = %d, %v", n, err)
	}
	count := func(table string) int {
		var n int
		if err := e.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + folders.ColScope + " = '" + scope + "'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{folders.TableNameFolders, folders.TableNamePlaces, folders.TableNameLocks} {
		if n := count(table); n != 0 {
			t.Errorf("%s keeps %d rows of the scope", table, n)
		}
	}
	if _, err := e.p.Folder(ctx, e.store, "u", other.ID); err != nil {
		t.Fatalf("another scope's folder went: %v", err)
	}
	// A failure is returned.
	testdb.Exec(t, e.db, "DROP TABLE "+folders.TableNameLocks)
	if _, err := e.store.PurgeScope(ctx, scope); err == nil {
		t.Fatal("a failed delete was ignored")
	}
}

// TestRenameOfAVanishedFolder stages a Delete that lands between Rename's
// read and its write: a SQLite trigger makes the update change no row, as
// a deleted folder would. Rename reports not-found and emits nothing.
func TestRenameOfAVanishedFolder(t *testing.T) {
	testdb.OnlySQLite(t)
	ctx := t.Context()
	e := newEnv(t)
	a := e.mkdir(t, folders.RootID, "a")
	e.events.log = nil
	testdb.Exec(t, e.db, "CREATE TRIGGER gone BEFORE UPDATE OF "+folders.ColName+" ON "+folders.TableNameFolders+" BEGIN SELECT RAISE(IGNORE); END")
	wantCode(t, e.p.Rename(ctx, e.store, scope, a.ID, "b"), filesystem.CodeNotFound)
	if len(e.events.log) != 0 {
		t.Fatalf("events for a rename that did nothing: %v", e.events.log)
	}
}

func TestFolderFacet(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	docs := e.mkdir(t, folders.RootID, "docs")
	a, b, c, d := e.file(t, "a.txt"), e.file(t, "bb.txt"), e.file(t, "ccc.txt"), e.file(t, "d.txt")
	_ = e.p.Put(ctx, e.store, scope, a.ID, docs.ID)
	_ = e.p.Put(ctx, e.store, scope, b.ID, docs.ID)
	_ = e.store.Trash(ctx, scope, d.ID)
	_ = c
	render := func(fc []records.FacetCount) string {
		var out []string
		for _, x := range fc {
			name := "root"
			if x.Value == docs.ID {
				name = "docs"
			}
			out = append(out, fmt.Sprintf("%s=%d/%d", name, x.Files, x.Bytes))
		}
		return strings.Join(out, " ")
	}
	got, err := e.p.FolderFacet(ctx, e.store, records.FileQuery{Scope: scope})
	if err != nil || render(got) != "docs=2/11 root=1/7" {
		t.Fatalf("FolderFacet = %s, %v", render(got), err)
	}
	got, _ = e.p.FolderFacet(ctx, e.store, records.FileQuery{Scope: scope, Trashed: true})
	if render(got) != "root=1/5" {
		t.Fatalf("trashed = %s", render(got))
	}
	got, _ = e.p.FolderFacet(ctx, e.store, records.FileQuery{Scope: scope, Filters: []sqlb.Predicate{sqlb.Eq(records.FileID, a.ID)}})
	if render(got) != "docs=1/5" {
		t.Fatalf("filtered = %s", render(got))
	}
	_, err = e.p.FolderFacet(ctx, e.store, records.FileQuery{Scope: scope, Limit: 1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	testdb.Exec(t, e.db, "DROP TABLE "+folders.TableNamePlaces)
	if _, err := e.p.FolderFacet(ctx, e.store, records.FileQuery{Scope: scope}); err == nil {
		t.Fatal("a failed query was ignored")
	}
}
