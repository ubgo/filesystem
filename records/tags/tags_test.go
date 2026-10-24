package tags_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/tags"
)

const scope = "t"

type env struct {
	db     *sql.DB
	faults *testdb.Faults
	store  *records.Store
	disk   *filesystem.Disk
	p      *tags.Plugin
	events *recorder
}

// recorder logs the plugin's events of committed changes and fails the
// kind in refuse.
type recorder struct {
	log    []string
	refuse records.EventKind
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == r.refuse {
		return errors.New("refused")
	}
	if !strings.HasPrefix(string(e.Kind), tags.PluginName+".") {
		return nil
	}
	line := string(e.Kind) + " " + e.FileID + " " + e.Detail[tags.DetailTag]
	if from, ok := e.Detail[tags.DetailFrom]; ok {
		line += " from=" + from
	}
	s.AfterCommit(ctx, func(context.Context) { r.log = append(r.log, strings.TrimSpace(strings.ReplaceAll(line, "  ", " "))) })
	return nil
}

func newEnv(t *testing.T, opts ...tags.Option) *env {
	t.Helper()
	e := &env{p: tags.New(opts...), events: &recorder{}}
	e.db, e.faults = testdb.OpenFaulty(t)
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	var err error
	if e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithPlugins(e.p, e.events)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) file(t *testing.T, sc, name string) records.File {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), sc+"/"+name, []byte(name), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: sc, Disk: "media", Key: sc + "/" + name})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if filesystem.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func wantTags(t *testing.T, got []string, err error, want string) {
	t.Helper()
	if err != nil || strings.Join(got, ",") != want {
		t.Fatalf("tags = %q, %v; want %q", strings.Join(got, ","), err, want)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"Invoice": "invoice", "  Tax 2026 ": "tax 2026", "ÉTÉ": "été", strings.Repeat("A", tags.MaxTagChars): strings.Repeat("a", tags.MaxTagChars)} {
		if got, err := tags.DefaultNormalize(in); err != nil || got != want {
			t.Errorf("DefaultNormalize(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "   ", "a\tb", "\xff", strings.Repeat("a", tags.MaxTagChars+1)} {
		_, err := tags.DefaultNormalize(bad)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

func TestSetAddRemove(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t, scope, "a.pdf")

	got, err := e.p.Set(ctx, e.store, scope, f.ID, "Invoice", "tax", "invoice ")
	wantTags(t, got, err, "invoice,tax")
	got, err = e.p.Add(ctx, e.store, scope, f.ID, "TAX", "paid")
	wantTags(t, got, err, "invoice,paid,tax")
	got, err = e.p.Remove(ctx, e.store, scope, f.ID, "Invoice", "never-had")
	wantTags(t, got, err, "paid,tax")
	got, err = e.p.Tags(ctx, e.store, scope, f.ID)
	wantTags(t, got, err, "paid,tax")
	got, err = e.p.Set(ctx, e.store, scope, f.ID)
	wantTags(t, got, err, "")

	_, err = e.p.Add(ctx, e.store, scope, f.ID, "ok", "")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.p.Remove(ctx, e.store, scope, f.ID, "\x00")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.p.Set(ctx, e.store, scope, f.ID, "\x00")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	many := make([]string, tags.MaxPerFile+1)
	for i := range many {
		many[i] = fmt.Sprint("t", i)
	}
	_, err = e.p.Set(ctx, e.store, scope, f.ID, many...)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if got, err := e.p.Set(ctx, e.store, scope, f.ID, many[:tags.MaxPerFile]...); err != nil || len(got) != tags.MaxPerFile {
		t.Fatalf("MaxPerFile tags: %d, %v", len(got), err)
	}

	_, err = e.p.Add(ctx, e.store, scope, "fil_missing", "x")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.p.Tags(ctx, e.store, "other", f.ID)
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestFilesAndCounts(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b, c := e.file(t, scope, "a"), e.file(t, scope, "b"), e.file(t, scope, "c")
	o := e.file(t, "other", "o")
	_, _ = e.p.Set(ctx, e.store, scope, a.ID, "red", "big")
	_, _ = e.p.Set(ctx, e.store, scope, b.ID, "red")
	_, _ = e.p.Set(ctx, e.store, scope, c.ID, "red", "big", "old")
	_, _ = e.p.Set(ctx, e.store, "other", o.ID, "red")

	list := func(fq records.FileQuery, tt ...string) string {
		t.Helper()
		page, err := e.p.Files(ctx, e.store, fq, tt...)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range page.Files {
			out = append(out, f.ID)
		}
		return strings.Join(out, ",") + "|" + page.NextCursor
	}
	if got := list(records.FileQuery{Scope: scope}, "RED"); !strings.HasPrefix(got, c.ID+","+b.ID+","+a.ID+"|") {
		t.Fatalf("red = %s", got)
	}
	if got := list(records.FileQuery{Scope: scope}, "red", "big"); got != c.ID+","+a.ID+"|" {
		t.Fatalf("red and big = %s", got)
	}
	first, _ := e.p.Files(ctx, e.store, records.FileQuery{Scope: scope, Limit: 1}, "big")
	if got := list(records.FileQuery{Scope: scope, Limit: 1, Cursor: first.NextCursor}, "big"); got != a.ID+"|" {
		t.Fatalf("second page of big = %s", got)
	}
	_, err := e.p.Files(ctx, e.store, records.FileQuery{Scope: scope})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.p.Files(ctx, e.store, records.FileQuery{Scope: scope}, "")
	wantCode(t, err, filesystem.CodeInvalidArgument)

	if err := e.store.Trash(ctx, scope, c.ID); err != nil {
		t.Fatal(err)
	}
	counts, err := e.p.Counts(ctx, e.store, scope)
	if err != nil || fmt.Sprint(counts) != "[{red 2} {big 1}]" {
		t.Fatalf("counts = %v, %v", counts, err)
	}
}

func TestRenameAndDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b := e.file(t, scope, "a"), e.file(t, scope, "b")
	o := e.file(t, "other", "o")
	_, _ = e.p.Set(ctx, e.store, scope, a.ID, "colour")
	_, _ = e.p.Set(ctx, e.store, scope, b.ID, "colour", "color")
	_, _ = e.p.Set(ctx, e.store, "other", o.ID, "colour")

	if err := e.p.Rename(ctx, e.store, scope, "Colour", "color"); err != nil {
		t.Fatal(err)
	}
	got, err := e.p.Tags(ctx, e.store, scope, a.ID)
	wantTags(t, got, err, "color")
	got, err = e.p.Tags(ctx, e.store, scope, b.ID)
	wantTags(t, got, err, "color")
	got, err = e.p.Tags(ctx, e.store, "other", o.ID)
	wantTags(t, got, err, "colour")
	if err := e.p.Rename(ctx, e.store, scope, "color", "COLOR"); err != nil {
		t.Fatalf("renaming to the same tag: %v", err)
	}
	if err := e.p.Rename(ctx, e.store, scope, "nothing", "else"); err != nil {
		t.Fatalf("renaming an unused tag: %v", err)
	}
	wantCode(t, e.p.Rename(ctx, e.store, scope, "", "x"), filesystem.CodeInvalidArgument)
	wantCode(t, e.p.Rename(ctx, e.store, scope, "x", ""), filesystem.CodeInvalidArgument)

	if err := e.p.Delete(ctx, e.store, scope, "Color"); err != nil {
		t.Fatal(err)
	}
	got, err = e.p.Tags(ctx, e.store, scope, b.ID)
	wantTags(t, got, err, "")
	wantCode(t, e.p.Delete(ctx, e.store, scope, ""), filesystem.CodeInvalidArgument)
}

func TestPurgeAndNormalizer(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, tags.WithNormalize(tags.Check)) // case-sensitive tags
	f := e.file(t, scope, "a")
	got, err := e.p.Set(ctx, e.store, scope, f.ID, "Red", "red")
	wantTags(t, got, err, "Red,red")
	if err := e.store.Purge(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM " + tags.TableNameTags).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d tag rows left after purging, %v", n, err)
	}
}

func (e *env) snapshot(t *testing.T) string {
	t.Helper()
	rows, err := e.db.Query("SELECT " + tags.ColScope + ", " + tags.ColFileID + ", " + tags.ColTag + " FROM " + tags.TableNameTags + " ORDER BY 1, 2, 3")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var x, y, z string
		_ = rows.Scan(&x, &y, &z)
		fmt.Fprintf(&b, "%s,%s,%s;", x, y, z)
	}
	return b.String()
}

// TestFaultSweep fails every database step of every operation in turn:
// each run must report the injected error and change nothing.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b := e.file(t, scope, "a"), e.file(t, scope, "b")
	trashed := e.file(t, scope, "c")
	_, _ = e.p.Set(ctx, e.store, scope, a.ID, "x", "y")
	_, _ = e.p.Set(ctx, e.store, scope, b.ID, "x", "z")
	_, _ = e.p.Set(ctx, e.store, scope, trashed.ID, "x")
	_ = e.store.Trash(ctx, scope, trashed.ID)

	ops := []struct {
		name string
		run  func() error
	}{
		{"set", func() error { _, err := e.p.Set(ctx, e.store, scope, a.ID, "x", "new"); return err }},
		{"add", func() error { _, err := e.p.Add(ctx, e.store, scope, a.ID, "more"); return err }},
		{"remove", func() error { _, err := e.p.Remove(ctx, e.store, scope, a.ID, "more"); return err }},
		{"tags", func() error { _, err := e.p.Tags(ctx, e.store, scope, a.ID); return err }},
		{"files", func() error { _, err := e.p.Files(ctx, e.store, records.FileQuery{Scope: scope}, "x"); return err }},
		{"counts", func() error { _, err := e.p.Counts(ctx, e.store, scope); return err }},
		{"rename merging", func() error { return e.p.Rename(ctx, e.store, scope, "z", "x") }},
		{"delete", func() error { return e.p.Delete(ctx, e.store, scope, "y") }},
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
					t.Fatalf("step %d failed but changed the table:\nbefore %s\nafter  %s", n, before, after)
				}
			}
		})
	}
}

func TestEvents(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b := e.file(t, scope, "a"), e.file(t, scope, "b")
	_, err1 := e.p.Set(ctx, e.store, scope, a.ID, "Red", "blue")
	_, err2 := e.p.Add(ctx, e.store, scope, a.ID, "red") // already there: no event
	_, err3 := e.p.Set(ctx, e.store, scope, a.ID, "red", "green")
	_, err4 := e.p.Add(ctx, e.store, scope, b.ID, "green")
	err5 := e.p.Rename(ctx, e.store, scope, "green", "lime")
	err6 := e.p.Rename(ctx, e.store, scope, "nothing", "else") // no file has it: no event
	err7 := e.p.Delete(ctx, e.store, scope, "lime")
	err8 := e.p.Delete(ctx, e.store, scope, "lime") // gone already: no event
	if err := errors.Join(err1, err2, err3, err4, err5, err6, err7, err8); err != nil {
		t.Fatal(err)
	}
	ids := strings.NewReplacer(a.ID, "A", b.ID, "B")
	want := []string{
		"tags.added A red", "tags.added A blue",
		"tags.removed A blue", "tags.added A green",
		"tags.added B green",
		"tags.renamed lime from=green",
		"tags.deleted lime",
	}
	if got := ids.Replace(strings.Join(e.events.log, "; ")); got != strings.Join(want, "; ") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "; "))
	}

	// A hook refusing the event rolls the change back.
	for _, c := range []struct {
		kind records.EventKind
		run  func() error
	}{
		{tags.EventAdded, func() error { _, err := e.p.Add(ctx, e.store, scope, b.ID, "new"); return err }},
		{tags.EventRemoved, func() error { _, err := e.p.Remove(ctx, e.store, scope, a.ID, "red"); return err }},
		{tags.EventRenamed, func() error { return e.p.Rename(ctx, e.store, scope, "red", "pink") }},
		{tags.EventDeleted, func() error { return e.p.Delete(ctx, e.store, scope, "red") }},
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
	if len(tags.EventKindValues) != 4 {
		t.Fatalf("EventKindValues = %v", tags.EventKindValues)
	}
}

func TestPurgeScope(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b := e.file(t, scope, "a"), e.file(t, "u", "b")
	_, err1 := e.p.Set(ctx, e.store, scope, a.ID, "red")
	_, err2 := e.p.Set(ctx, e.store, "u", b.ID, "red")
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	// The plugin's own scope purge removes tag rows of the scope.
	if err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return e.p.PurgeScope(ctx, e.store, scope) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.p.Tags(ctx, e.store, scope, a.ID); len(got) != 0 {
		t.Fatalf("tags left: %v", got)
	}
	if got, _ := e.p.Tags(ctx, e.store, "u", b.ID); len(got) != 1 {
		t.Fatalf("another scope lost its tags: %v", got)
	}
	if n, err := e.store.PurgeScope(ctx, scope); err != nil || n != 1 {
		t.Fatalf("PurgeScope = %d, %v", n, err)
	}
}
