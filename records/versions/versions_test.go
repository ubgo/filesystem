package versions_test

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
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/upload"
)

const scope = "t"

var anything = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

type env struct {
	db     *sql.DB
	faults *testdb.Faults
	store  *records.Store
	disk   *filesystem.Disk
	p      *versions.Plugin
}

func newEnv(t *testing.T, keep int) *env {
	t.Helper()
	e := &env{}
	e.db, e.faults = testdb.OpenFaulty(t)
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var err error
	e.p, err = versions.New(versions.WithKeep(keep), versions.WithClock(func() time.Time { now = now.Add(time.Minute); return now }))
	if err != nil {
		t.Fatal(err)
	}
	if e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithPlugins(e.p)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) upload(t *testing.T, body string) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), scope, "media", strings.NewReader(body), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *env) replace(t *testing.T, id, body string) records.File {
	t.Helper()
	f, err := e.store.Replace(t.Context(), scope, id, strings.NewReader(body), "", anything)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *env) content(t *testing.T, id string) string {
	t.Helper()
	f, err := e.store.File(t.Context(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := e.disk.ReadAll(t.Context(), f.Key)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (e *env) objects(t *testing.T) int {
	t.Helper()
	page, err := e.disk.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Entries)
}

// history renders a file's versions as "number:content", newest first.
func (e *env) history(t *testing.T, id string) string {
	t.Helper()
	list, err := e.p.Versions(t.Context(), e.store, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range list {
		data, err := e.disk.ReadAll(t.Context(), v.Key)
		if err != nil {
			t.Fatalf("version %d's object: %v", v.Number, err)
		}
		out = append(out, fmt.Sprintf("%d:%s", v.Number, data))
	}
	return strings.Join(out, ",")
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if filesystem.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func TestNew(t *testing.T) {
	if _, err := versions.New(versions.WithKeep(0)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("keep 0 = %v", err)
	}
	p, err := versions.New()
	if err != nil || p.Name() != versions.PluginName {
		t.Fatal(err)
	}
}

func TestHistory(t *testing.T) {
	e := newEnv(t, 2)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	if got := e.history(t, f.ID); got != "1:one" {
		t.Fatalf("history = %s", got)
	}
	e.replace(t, f.ID, "three")
	e.replace(t, f.ID, "four") // the cap of 2 drops version 1
	if got := e.history(t, f.ID); got != "3:three,2:two" || e.content(t, f.ID) != "four" {
		t.Fatalf("history = %s, content %q", got, e.content(t, f.ID))
	}
	if n := e.objects(t); n != 3 {
		t.Fatalf("%d objects, want the current one and 2 versions", n)
	}
	list, _ := e.p.Versions(t.Context(), e.store, scope, f.ID)
	v := list[0]
	if !strings.HasPrefix(v.ID, versions.IDPrefix) || v.FileID != f.ID || v.Filename != "doc.txt" || v.Size != 5 || v.ContentType == "" || v.ReplacedAt.IsZero() {
		t.Fatalf("version = %+v", v)
	}
	if other, _ := e.p.Versions(t.Context(), e.store, "other", f.ID); len(other) != 0 {
		t.Fatal("versions leak across scopes")
	}
}

func TestRestore(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)

	restored, err := e.p.Restore(ctx, e.store, scope, list[0].ID)
	if err != nil || restored.ID != f.ID {
		t.Fatalf("Restore = %+v, %v", restored, err)
	}
	if got := e.content(t, f.ID) + "|" + e.history(t, f.ID); got != "one|2:two,1:one" {
		t.Fatalf("content|history = %s", got)
	}

	_, err = e.p.Restore(ctx, e.store, scope, "ver_missing")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.p.Restore(ctx, e.store, "other", list[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)

	before := e.objects(t)
	if err := e.store.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.p.Restore(ctx, e.store, scope, list[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)
	if n := e.objects(t); n != before {
		t.Fatalf("a failed restore left its copy: %d objects, was %d", n, before)
	}

	// A version on a disk the store no longer has.
	empty, _ := filesystem.NewManager()
	other, _ := records.New(e.db, testdb.Dialect(), empty, records.WithPlugins(e.p))
	_, err = e.p.Restore(ctx, other, scope, list[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)
	// And one whose object is gone.
	_ = e.disk.Delete(ctx, list[0].Key)
	if err := e.store.Restore(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.p.Restore(ctx, e.store, scope, list[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestRestoreRollback proves a restore inside a transaction that rolls
// back leaves the file and the disk as they were.
func TestRestoreRollback(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)
	before := e.objects(t)
	boom := errors.New("later step failed")
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.p.Restore(ctx, e.store, scope, list[0].ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || e.content(t, f.ID) != "two" || e.objects(t) != before || e.history(t, f.ID) != "1:one" {
		t.Fatalf("after rollback: err %v, content %q, %d objects (was %d), history %s", err, e.content(t, f.ID), e.objects(t), before, e.history(t, f.ID))
	}
}

func TestDeleteAndPurge(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	e.replace(t, f.ID, "three")
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)

	if err := e.p.Delete(ctx, e.store, scope, list[1].ID); err != nil {
		t.Fatal(err)
	}
	if got := e.history(t, f.ID); got != "2:two" || e.objects(t) != 2 {
		t.Fatalf("after deleting version 1: %s, %d objects", got, e.objects(t))
	}
	wantCode(t, e.p.Delete(ctx, e.store, scope, list[1].ID), filesystem.CodeNotFound)

	if err := e.store.Purge(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.history(t, f.ID); got != "" || e.objects(t) != 0 {
		t.Fatalf("after purging: %q, %d objects", got, e.objects(t))
	}
}

// state renders the version rows, the files' keys and the object count.
func (e *env) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT " + versions.ColID + ", " + versions.ColKey + " FROM " + versions.TableNameVersions + " ORDER BY 1",
		"SELECT " + records.ColFileID + ", " + records.ColFileKey + " FROM " + records.TableNameFiles + " ORDER BY 1",
	} {
		rows, err := e.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var x, y string
			_ = rows.Scan(&x, &y)
			fmt.Fprintf(&b, "%s=%s;", x, y)
		}
		_ = rows.Close()
		b.WriteString("|")
	}
	fmt.Fprintf(&b, "objects=%d", e.objects(t))
	for en, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		st, _ := e.disk.Stat(t.Context(), en.Key)
		fmt.Fprintf(&b, ";%s=%s", en.Key, st.Visibility)
	}
	return b.String()
}

// TestFaultSweep fails every database step of every operation in turn:
// rows and objects must be as they were each time.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 2)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	e.replace(t, f.ID, "three") // at the cap: the next replacement drops one
	g := e.upload(t, "other")
	e.replace(t, g.ID, "other two")
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)

	ops := []struct {
		name string
		run  func() error
	}{
		{"replace dropping the oldest", func() error {
			_, err := e.store.Replace(ctx, scope, f.ID, strings.NewReader("four"), "", anything)
			return err
		}},
		{"restore", func() error { _, err := e.p.Restore(ctx, e.store, scope, list[0].ID); return err }},
		{"delete", func() error {
			current, err := e.p.Versions(ctx, e.store, scope, f.ID) // restoring at the cap dropped some
			if err != nil {
				return err
			}
			return e.p.Delete(ctx, e.store, scope, current[len(current)-1].ID)
		}},
		{"set visibility", func() error { return e.store.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPublic) }},
		{"purge", func() error { return e.store.Purge(ctx, scope, g.ID) }},
		{"trim", func() error { _, err := e.p.Trim(ctx, e.store, scope, f.ID); return err }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := e.state(t)
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: err = %v", n, err)
				}
				if after := e.state(t); after != before {
					t.Fatalf("step %d failed but changed the store:\nbefore %s\nafter  %s", n, before, after)
				}
			}
		})
	}
}

func TestPurgeScope(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "v1")
	e.replace(t, f.ID, "v2")
	e.replace(t, f.ID, "v3")
	// Versions left over without going through the file's purge are
	// dropped by the plugin's scope purge itself, objects included.
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return e.p.PurgeScope(ctx, e.store, scope) })
	if err != nil {
		t.Fatal(err)
	}
	if h := e.history(t, f.ID); h != "" || e.objects(t) != 1 {
		t.Fatalf("after the plugin's scope purge: history %q, %d objects", h, e.objects(t))
	}
	// Through the Store, the file and its new versions all go.
	e.replace(t, f.ID, "v4")
	if n, err := e.store.PurgeScope(ctx, scope); err != nil || n != 1 || e.objects(t) != 0 {
		t.Fatalf("PurgeScope = %d, %v, %d objects", n, err, e.objects(t))
	}
	exec := func(q string) {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec("DROP TABLE " + versions.TableNameVersions)
	if err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return e.p.PurgeScope(ctx, e.store, scope) }); err == nil {
		t.Fatal("a failed listing was ignored")
	}
}
