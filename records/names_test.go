package records_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/records/tags"
	"github.com/ubgo/filesystem/records/uploadsessions"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/records/webhooks"
	"github.com/ubgo/sqlb"
)

// testPrefix is the prefix the prefixed Stores of these tests use.
const testPrefix = "app_"

// allPlugins returns one of every plugin of the module, configured as
// the golden migrations render them.
func allPlugins(t *testing.T) []records.Plugin {
	t.Helper()
	q, err := quota.New(func(context.Context, string) (quota.Limit, error) {
		return quota.Limit{Bytes: 1 << 20, Files: 100}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := versions.New()
	if err != nil {
		t.Fatal(err)
	}
	wh, err := webhooks.New()
	if err != nil {
		t.Fatal(err)
	}
	return []records.Plugin{acl.New(), audit.New(), egress.New(), folders.New(), locks.New(), q, share.New(), tags.New(), uploadsessions.New(), v, wh}
}

// storeOn is a migrated Store with every plugin on db, over mgr, with opts.
func storeOn(t *testing.T, db *sql.DB, mgr *filesystem.Manager, plugins []records.Plugin, opts ...records.Option) *records.Store {
	t.Helper()
	s, err := records.New(db, testDialect(), mgr, append([]records.Option{records.WithPlugins(plugins...)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTablePrefixRunsEveryPlugin: a Store with WithTablePrefix migrates
// every plugin's tables under the prefix and runs each plugin's writes and
// reads against them, beside a default Store in the same database that
// sees none of its rows: two Stores in one database.
func TestTablePrefixRunsEveryPlugin(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	plain := storeOn(t, db, mgr, allPlugins(t))
	plugins := allPlugins(t)
	s := storeOn(t, db, mgr, plugins, records.WithTablePrefix(testPrefix))
	a, au, eg, fd, lk, sh, tg, ver := plugins[0].(*acl.Plugin), plugins[1].(*audit.Plugin), plugins[2].(*egress.Plugin),
		plugins[3].(*folders.Plugin), plugins[4].(*locks.Plugin), plugins[6].(*share.Plugin), plugins[7].(*tags.Plugin), plugins[9].(*versions.Plugin)

	f, err := s.Upload(ctx, "t", "media", strings.NewReader("one"), "a.txt", textPolicy)
	must(t, err)
	_, err = a.Grant(ctx, s, "t", acl.Grant{Subject: "user:a", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read})
	must(t, err)
	ok, err := a.Can(ctx, s, "t", []string{"user:a"}, f.ID, acl.Read)
	must(t, err)
	if !ok {
		t.Fatal("the grant under the prefix is not seen")
	}
	dir, err := fd.Create(ctx, s, "t", folders.RootID, "docs")
	must(t, err)
	must(t, fd.Put(ctx, s, "t", f.ID, dir.ID))
	_, err = lk.Lock(ctx, s, "t", f.ID, "user:a", time.Minute)
	must(t, err)
	_, err = sh.Create(ctx, s, "t", f.ID, share.Options{TTL: time.Hour})
	must(t, err)
	_, err = tg.Add(ctx, s, "t", f.ID, "red")
	must(t, err)
	must(t, s.Served(ctx, records.Served{Kind: records.ServeStream, Scope: "t", FileID: f.ID, Bytes: 3}))
	must(t, lk.Unlock(ctx, s, "t", f.ID, "user:a"))
	_, err = s.Replace(ctx, "t", f.ID, strings.NewReader("two"), "a.txt", textPolicy)
	must(t, err)
	kept, err := ver.Versions(ctx, s, "t", f.ID)
	must(t, err)
	if len(kept) != 1 {
		t.Fatalf("versions under the prefix = %v", kept)
	}
	trail, err := au.Entries(ctx, s, audit.Query{Scope: "t"})
	must(t, err)
	if len(trail.Entries) == 0 {
		t.Fatal("no audit entries under the prefix")
	}
	if days, err := eg.Days(ctx, s, "t", f.CreatedAt.AddDate(0, 0, -1), f.CreatedAt.AddDate(0, 0, 1)); err != nil || len(days) != 1 {
		t.Fatalf("egress under the prefix = %v, %v", days, err)
	}

	page, err := plain.Files(ctx, records.FileQuery{Scope: "t"})
	must(t, err)
	if len(page.Files) != 0 {
		t.Fatalf("the default Store sees the prefixed Store's files: %v", page.Files)
	}
	if _, err := plain.File(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("default Store File = %v, want CodeNotFound", err)
	}

	if engine() == testdb.SQLite {
		names := catalogue(t, db)
		for _, n := range names {
			if strings.HasPrefix(n, records.ObjectPrefix) && !slices.Contains(names, testPrefix+n) {
				t.Errorf("%s has no prefixed twin", n)
			}
		}
	}
}

// catalogue lists the tables and indexes of a SQLite database.
func catalogue(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "SELECT name FROM sqlite_master WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestTableNamesFunction: WithTableNames maps every name through the
// application's function, Store.TableName answers it for hand-written SQL,
// and the ledger follows.
func TestTableNamesFunction(t *testing.T) {
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	rename := func(name string) string { return "storage_" + strings.TrimPrefix(name, records.ObjectPrefix) }
	s := storeOn(t, db, mgr, nil, records.WithTableNames(rename))
	if got := s.TableName(records.TableNameFiles); got != "storage_files" {
		t.Fatalf("TableName = %q", got)
	}
	if got := s.TableName("other"); got != "other" {
		t.Fatalf("TableName of an application table = %q, want it unchanged", got)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+s.TableName(records.TableNameMigrations)).Scan(&n); err != nil || n == 0 {
		t.Fatalf("the ledger under its mapped name: %d, %v", n, err)
	}
	plain, err := records.New(db, testDialect(), mgr)
	must(t, err)
	if got := plain.TableName(records.TableNameFiles); got != records.TableNameFiles {
		t.Fatalf("default TableName = %q", got)
	}
}

// TestTableNamesRefused: a prefix or a mapping that would not make valid,
// distinct identifiers is refused before any table is created.
func TestTableNamesRefused(t *testing.T) {
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	for _, prefix := range []string{"", "9a_", "App_", "a-b", strings.Repeat("a", records.MaxTablePrefixBytes+1)} {
		if _, err := records.New(db, testDialect(), mgr, records.WithTablePrefix(prefix)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("WithTablePrefix(%q) = %v, want CodeInvalidArgument", prefix, err)
		}
	}
	if _, err := records.New(db, testDialect(), mgr, records.WithTableNames(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("WithTableNames(nil) = %v", err)
	}
	for name, fn := range map[string]records.TableNamer{
		"not injective":  func(string) string { return "files" },
		"not identifier": func(n string) string { return n + "-x" },
		"too long":       func(n string) string { return strings.Repeat("x", records.MaxObjectNameBytes) + n },
	} {
		s, err := records.New(db, testDialect(), mgr, records.WithTableNames(fn), records.WithPlugins(allPlugins(t)...))
		must(t, err)
		if _, err := s.Migrate(t.Context()); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: Migrate = %v, want CodeInvalidArgument", name, err)
		}
		if _, err := s.MigrationSQL(); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: MigrationSQL = %v, want CodeInvalidArgument", name, err)
		}
	}
	if engine() == testdb.SQLite {
		if names := catalogue(t, db); len(names) != 0 {
			t.Fatalf("a refused mapping created %v", names)
		}
	}
}

// objectName is a word of rendered SQL that WithTableNames maps.
var objectName = regexp.MustCompile(`\b` + records.ObjectPrefix + `[A-Za-z0-9_]*`)

// createdName is the name a statement creates: a CREATE TABLE or CREATE
// INDEX, or an index or unique constraint MySQL declares inside its
// CREATE TABLE.
var createdName = regexp.MustCompile("(?:CREATE (?:UNIQUE )?(?:TABLE|INDEX) (?:IF NOT EXISTS )?|, INDEX |, CONSTRAINT )[\"`](" + records.ObjectPrefix + "[A-Za-z0-9_]*)[\"`]")

// TestObjectPrefixInvariant pins what WithTableNames relies on: in the
// default migrations of every engine, every word starting with
// ObjectPrefix names a table or an index the migrations create (or the
// MySQL guards' prepared statement), so a mapping renames objects and
// nothing else.
func TestObjectPrefixInvariant(t *testing.T) {
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		body, err := os.ReadFile(filepath.Join("testdata", "migrations_"+engine+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		script := string(body)
		created := map[string]bool{records.ObjectPrefix + "ddl": true}
		for _, m := range createdName.FindAllStringSubmatch(script, -1) {
			created[m[1]] = true
		}
		if len(created) < 20 {
			t.Fatalf("%s: found only %d created names; the pattern is broken", engine, len(created))
		}
		for _, w := range objectName.FindAllString(script, -1) {
			if !created[w] {
				t.Errorf("%s: %q starts with %q but names no table or index", engine, w, records.ObjectPrefix)
			}
		}
	}
}

// TestTablePrefixGolden renders every migration under WithTablePrefix on
// every engine and compares it with its golden file, as
// TestMigrationSQLGolden does for the default names.
func TestTablePrefixGolden(t *testing.T) {
	for _, d := range []sqlb.Dialect{sqlb.SQLiteDialect{}, sqlb.PostgresDialect{}, sqlb.MySQLDialect{}} {
		engine := sqlb.DialectName(d)
		t.Run(engine, func(t *testing.T) {
			got := renderMigrations(t, d, records.WithTablePrefix(testPrefix))
			if strings.Contains(strings.ReplaceAll(got, testPrefix+records.ObjectPrefix, ""), "\""+records.ObjectPrefix) {
				t.Fatal("a quoted default name is left in the prefixed migrations")
			}
			compareGolden(t, filepath.Join("testdata", "migrations_"+engine+"_prefixed.sql"), got)
		})
	}
}
