package quota_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

type env struct {
	store  *records.Store
	plugin *quota.Plugin
	disk   *filesystem.Disk
	db     io.Closer
	faults *testdb.Faults
}

func newEnv(t *testing.T, limits quota.LimitFunc, opts ...quota.Option) (*env, func(q string)) {
	t.Helper()
	db, faults := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p, err := quota.New(limits, opts...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{store: s, plugin: p, disk: disk, db: db, faults: faults}, func(q string) { testdb.Exec(t, db, q) }
}

// add stores size bytes and records them in scope.
func (e *env) add(t *testing.T, scope, key string, size int) (records.File, error) {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), key, []byte(strings.Repeat("x", size)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: key})
}

func fixed(l quota.Limit) quota.LimitFunc {
	return func(context.Context, string) (quota.Limit, error) { return l, nil }
}

func usage(t *testing.T, e *env, scope string) quota.Usage {
	t.Helper()
	u, err := e.plugin.Usage(t.Context(), e.store, scope)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLimits(t *testing.T) {
	ctx := t.Context()
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 100, Files: 3}))
	a, err := e.add(t, "t1", "a", 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.add(t, "t1", "b", 50); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, e, "t1"); u.Bytes != 90 || u.Files != 2 {
		t.Errorf("usage = %+v", u)
	}
	_, err = e.add(t, "t1", "c", 20)
	if filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("over the byte limit = %v", err)
	}
	if u := usage(t, e, "t1"); u.Bytes != 90 {
		t.Errorf("refused upload charged: %+v", u)
	}
	if _, err := e.add(t, "t1", "d", 10); err != nil {
		t.Fatal(err)
	}
	_, err = e.add(t, "t1", "e", 0)
	if filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Errorf("over the file limit = %v", err)
	}
	// Other scopes are independent.
	if _, err := e.add(t, "t2", "f", 60); err != nil {
		t.Errorf("another scope = %v", err)
	}
	// Trash keeps the space; purge releases it.
	_ = e.store.Trash(ctx, "t1", a.ID)
	if u := usage(t, e, "t1"); u.Bytes != 100 {
		t.Errorf("trash released space: %+v", u)
	}
	if err := e.store.Purge(ctx, "t1", a.ID); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, e, "t1"); u.Bytes != 60 || u.Files != 2 {
		t.Errorf("after purge = %+v", u)
	}
	if u := usage(t, e, "never"); u != (quota.Usage{}) {
		t.Errorf("unknown scope = %+v", u)
	}
}

func TestFirstUploadOverTheLimit(t *testing.T) {
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 10}))
	_, err := e.add(t, "t", "big", 11)
	if filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Errorf("first upload over the limit = %v", err)
	}
	e2, _ := newEnv(t, fixed(quota.Limit{}))
	if _, err := e2.add(t, "t", "any-size", 5000); err != nil {
		t.Errorf("unlimited = %v", err)
	}
}

func TestRecount(t *testing.T) {
	ctx := t.Context()
	e, exec := newEnv(t, fixed(quota.Limit{}))
	_, _ = e.add(t, "t", "a", 10)
	_, _ = e.add(t, "t", "b", 5)
	exec("UPDATE " + quota.TableNameUsage + " SET " + quota.ColBytes + " = 999")
	u, err := e.plugin.Recount(ctx, e.store, "t")
	if err != nil || u.Bytes != 15 || u.Files != 2 {
		t.Fatalf("Recount = %+v, %v", u, err)
	}
	if got := usage(t, e, "t"); got != u {
		t.Errorf("stored usage = %+v", got)
	}
	if u, err := e.plugin.Recount(ctx, e.store, "empty"); err != nil || u != (quota.Usage{}) {
		t.Errorf("Recount of an empty scope = %+v, %v", u, err)
	}
}

func TestErrors(t *testing.T) {
	if _, err := quota.New(nil); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("New(nil) = %v", err)
	}
	boom := errors.New("plan service down")
	e, _ := newEnv(t, func(context.Context, string) (quota.Limit, error) { return quota.Limit{}, boom })
	if _, err := e.add(t, "t", "a", 1); !errors.Is(err, boom) {
		t.Errorf("limit lookup failure = %v", err)
	}
	if e.plugin.Name() != quota.PluginName {
		t.Error("name")
	}

	// The lookup fails only when a replacement grows the file.
	var down bool
	e, _ = newEnv(t, func(context.Context, string) (quota.Limit, error) {
		if down {
			return quota.Limit{}, boom
		}
		return quota.Limit{}, nil
	})
	f, _ := e.add(t, "t", "a", 1)
	down = true
	_, err := e.store.Replace(t.Context(), "t", f.ID, strings.NewReader("grown"), "", upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}})
	if !errors.Is(err, boom) {
		t.Errorf("limit lookup failure on replace = %v", err)
	}
}

// failOn makes every op on table fail (SQLite triggers).
func failOn(op, table string) string {
	return "CREATE TRIGGER fail_" + op + " BEFORE " + op + " ON " + table + " BEGIN SELECT RAISE(FAIL, 'injected'); END"
}

func TestDatabaseFailures(t *testing.T) {
	testdb.OnlySQLite(t)
	ctx := t.Context()

	e, exec := newEnv(t, fixed(quota.Limit{}))
	_, _ = e.add(t, "t", "a", 1)
	exec(failOn("UPDATE", quota.TableNameUsage))
	if _, err := e.add(t, "t", "b", 1); err == nil {
		t.Error("charge failure ignored")
	}
	f, _ := e.store.Files(ctx, records.FileQuery{Scope: "t"})
	if err := e.store.Purge(ctx, "t", f.Files[0].ID); err == nil {
		t.Error("release failure ignored")
	}

	e, exec = newEnv(t, fixed(quota.Limit{}))
	exec(failOn("INSERT", quota.TableNameUsage))
	if _, err := e.add(t, "t", "a", 1); err == nil {
		t.Error("first-row insert failure ignored")
	}
	if _, err := e.plugin.Recount(ctx, e.store, "t"); err == nil {
		t.Error("recount insert failure ignored")
	}

	// A concurrent first upload won the INSERT: retryable.
	e, exec = newEnv(t, fixed(quota.Limit{}))
	exec("CREATE TRIGGER race BEFORE INSERT ON " + quota.TableNameUsage + " BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: fs_quota_usage.scope'); END")
	if _, err := e.add(t, "t", "a", 1); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("racing first upload = %v", err)
	}

	e, exec = newEnv(t, fixed(quota.Limit{}))
	_, _ = e.add(t, "t", "a", 1)
	exec(failOn("DELETE", quota.TableNameUsage))
	if _, err := e.plugin.Recount(ctx, e.store, "t"); err == nil {
		t.Error("recount delete failure ignored")
	}

	e, exec = newEnv(t, fixed(quota.Limit{}))
	exec("DROP TABLE " + quota.TableNameUsage)
	if _, err := e.add(t, "t", "a", 1); err == nil {
		t.Error("missing usage table ignored")
	}
	if _, err := e.plugin.Usage(ctx, e.store, "t"); err == nil {
		t.Error("usage read failure ignored")
	}
	exec("DROP TABLE " + records.TableNameAttachments)
	exec("DROP TABLE " + records.TableNameFiles)
	if _, err := e.plugin.Recount(ctx, e.store, "t"); err == nil {
		t.Error("recount read failure ignored")
	}
}

// TestFaultSweep fails every database step of each quota path in turn
// (an existing scope's charge, a first charge, a refused charge, a purge's
// credit): a run that fails with the injected fault leaves the usage as
// it was, and the run after the last step has the path's own outcome.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 100, Files: 10}))
	if _, err := e.add(t, "t1", "seed", 10); err != nil {
		t.Fatal(err)
	}
	purgeable, _ := e.add(t, "t1", "purgeable", 5)
	growing, _ := e.add(t, "t1", "growing", 2)
	anything := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	key := 0
	put := func(scope string, size int) func() error {
		return func() error {
			key++
			_, err := e.add(t, scope, "k"+strings.Repeat("x", key), size)
			return err
		}
	}
	ops := []struct {
		name string
		run  func() error
		want filesystem.Code
	}{
		{"charge", put("t1", 1), ""},
		{"first charge", put("t2", 1), ""},
		{"refused charge", put("t1", 90), filesystem.CodeQuotaExceeded},
		{"replace", func() error {
			_, err := e.store.Replace(ctx, "t1", growing.ID, strings.NewReader(strings.Repeat("z", 12)), "", anything)
			return err
		}, ""},
		{"refused replace", func() error {
			_, err := e.store.Replace(ctx, "t1", growing.ID, strings.NewReader(strings.Repeat("z", 95)), "", anything)
			return err
		}, filesystem.CodeQuotaExceeded},
		{"credit", func() error { return e.store.Purge(ctx, "t1", purgeable.ID) }, ""},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := [2]quota.Usage{usage(t, e, "t1"), usage(t, e, "t2")}
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if !errors.Is(err, testdb.ErrInjected) {
					if filesystem.CodeOf(err) != op.want {
						t.Fatalf("outcome = %v, want %q", err, op.want)
					}
					return
				}
				if after := [2]quota.Usage{usage(t, e, "t1"), usage(t, e, "t2")}; after != before {
					t.Fatalf("step %d failed but changed the usage: %+v -> %+v", n, before, after)
				}
			}
		})
	}
}

func TestReplace(t *testing.T) {
	ctx := t.Context()
	e, exec := newEnv(t, fixed(quota.Limit{Bytes: 100}))
	f, err := e.add(t, "t1", "a", 40)
	if err != nil {
		t.Fatal(err)
	}
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	replace := func(size int) error {
		_, err := e.store.Replace(ctx, "t1", f.ID, strings.NewReader(strings.Repeat("y", size)), "", policy)
		return err
	}
	if err := replace(90); err != nil {
		t.Fatalf("growing within the limit: %v", err)
	}
	if u := usage(t, e, "t1"); u.Bytes != 90 || u.Files != 1 {
		t.Fatalf("after growing = %+v", u)
	}
	if err := replace(101); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("growing past the limit = %v", err)
	}
	if err := replace(10); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, e, "t1"); u.Bytes != 10 || u.Files != 1 {
		t.Fatalf("after shrinking = %+v", u)
	}
	if err := replace(10); err != nil {
		t.Fatalf("the same size: %v", err)
	}
	// A scope without a usage row (quota installed later) is left to Recount.
	exec("DELETE FROM " + quota.TableNameUsage)
	if err := replace(20); err != nil {
		t.Fatalf("without a usage row: %v", err)
	}
	if u := usage(t, e, "t1"); u.Bytes != 0 {
		t.Fatalf("a usage row appeared: %+v", u)
	}
}

// TestUsageNeverNegative: releasing a file the scope was never charged
// for (recorded before quota was installed) leaves usage at zero, not
// below it, whether purged or shrunk.
func TestUsageNeverNegative(t *testing.T) {
	ctx := t.Context()
	e, exec := newEnv(t, fixed(quota.Limit{Bytes: 100, Files: 10}))
	old, _ := e.add(t, "t", "old", 40)
	shrinking, _ := e.add(t, "t", "shrinking", 50)
	// As if both were recorded before quota: the scope was charged nothing.
	exec("UPDATE " + quota.TableNameUsage + " SET " + quota.ColBytes + " = 0, " + quota.ColFiles + " = 0")
	if err := e.store.Purge(ctx, "t", old.ID); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, e, "t"); u.Bytes != 0 || u.Files != 0 {
		t.Fatalf("after purging an uncharged file = %+v", u)
	}
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	if _, err := e.store.Replace(ctx, "t", shrinking.ID, strings.NewReader("y"), "", policy); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, e, "t"); u.Bytes != 0 {
		t.Fatalf("after shrinking an uncharged file = %+v", u)
	}
	// A charged file still releases exactly its size.
	f, _ := e.add(t, "t", "new", 30)
	_, _ = e.add(t, "t", "other", 20)
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, e, "t"); u.Bytes != 20 || u.Files != 1 {
		t.Fatalf("after purging a charged file = %+v", u)
	}
}

// TestExceededNamesTheLimit: the refusal names the limit the upload
// passes, not both.
func TestExceededNamesTheLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit quota.Limit
		size  int
		want  string
	}{
		{"bytes", quota.Limit{Bytes: 100, Files: 10}, 70, "limit of 100 bytes"},
		{"files", quota.Limit{Bytes: 1000, Files: 1}, 1, "limit of 1 files"},
		{"both", quota.Limit{Bytes: 100, Files: 1}, 70, "limits of 100 bytes and 1 files"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newEnv(t, fixed(c.limit))
			if _, err := e.add(t, "t", "first", 50); err != nil {
				t.Fatal(err)
			}
			_, err := e.add(t, "t", "second", c.size)
			if filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded || !strings.HasSuffix(err.Error(), c.want) && !strings.Contains(err.Error(), c.want+" (") {
				t.Fatalf("err = %v, want it to name %q", err, c.want)
			}
			other := " files"
			if c.name == "files" {
				other = " bytes"
			}
			if c.name != "both" && strings.Contains(err.Error(), other) {
				t.Fatalf("err = %v names %s, which was not passed", err, other)
			}
		})
	}
	// The first upload of a scope, over the limit before any usage row.
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 10, Files: 5}))
	_, err := e.add(t, "t", "big", 11)
	if err == nil || !strings.HasSuffix(err.Error(), "limit of 10 bytes") {
		t.Fatalf("first upload = %v", err)
	}
	// Growth by replacement passes only the byte limit.
	f, _ := e.add(t, "t", "small", 5)
	_, err = e.store.Replace(t.Context(), "t", f.ID, strings.NewReader(strings.Repeat("y", 20)), "", upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}})
	if err == nil || !strings.HasSuffix(err.Error(), "limit of 10 bytes") {
		t.Fatalf("growth = %v", err)
	}
}

func TestPurgeScope(t *testing.T) {
	ctx := t.Context()
	e, _ := newEnv(t, fixed(quota.Limit{}))
	for _, k := range []string{"a", "b"} {
		if _, err := e.add(t, "t1", k, 10); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.add(t, "t2", "c", 10); err != nil {
		t.Fatal(err)
	}
	rows := func(scope string) int64 {
		var n int64
		q := sqlb.NewSelect(e.store.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &n)).From(quota.Table).Where(sqlb.Eq(quota.Scope, scope))
		if err := e.store.One(ctx, q); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n, err := e.store.PurgeScope(ctx, "t1"); err != nil || n != 2 {
		t.Fatalf("PurgeScope = %d, %v", n, err)
	}
	if rows("t1") != 0 || rows("t2") != 1 {
		t.Fatalf("usage rows left: t1 %d, t2 %d", rows("t1"), rows("t2"))
	}
	if u := usage(t, e, "t2"); u.Bytes != 10 || u.Files != 1 {
		t.Fatalf("another scope's usage changed: %+v", u)
	}
}

// txCounter is a HeldCounter that records whether it was asked inside a
// transaction.
type txCounter struct {
	inTx []bool
	fail error
}

func (c *txCounter) Name() string { return "txcounter" }
func (c *txCounter) HeldBytes(ctx context.Context, s *records.Store, _ string) (int64, error) {
	_, ok := s.Conn(ctx).(*sql.Tx)
	c.inTx = append(c.inTx, ok)
	return 7, c.fail
}

// TestRecountReadsInsideItsTransaction: the sums Recount writes back are
// read in the transaction that writes them, after the usage row is
// locked, so no upload's charge can fall between the two.
func TestRecountReadsInsideItsTransaction(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p, err := quota.New(fixed(quota.Limit{}), quota.WithHeldObjects())
	if err != nil {
		t.Fatal(err)
	}
	c := &txCounter{}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p, c))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := p.Recount(ctx, s, "t")
	if err != nil || u.Bytes != 7 || len(c.inTx) != 1 || !c.inTx[0] {
		t.Fatalf("Recount = %+v, %v; held asked in a transaction: %v", u, err, c.inTx)
	}
	c.fail = errors.New("counter down")
	if _, err := p.Recount(ctx, s, "t"); !errors.Is(err, c.fail) {
		t.Fatalf("Recount with a failing counter = %v", err)
	}
	if got := usage(t, &env{store: s, plugin: p}, "t"); got.Bytes != 7 {
		t.Fatalf("a failed recount changed the usage: %+v", got)
	}
	// Every database step failing in turn fails the recount cleanly.
	c.fail = nil
	faults.Arm(0)
	if _, err := p.Recount(ctx, s, "t"); err != nil {
		t.Fatal(err)
	}
	steps := faults.Steps()
	for n := 1; n <= steps; n++ {
		faults.Arm(n)
		_, err := p.Recount(ctx, s, "t")
		faults.Arm(0)
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("step %d: %v", n, err)
		}
		if got := usage(t, &env{store: s, plugin: p}, "t"); got.Bytes != 7 {
			t.Fatalf("step %d failed but changed the usage: %+v", n, got)
		}
	}
}
