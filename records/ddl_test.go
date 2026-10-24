package records_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/records/tags"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// TestEveryMigrationReruns applies every migration of the core and of
// every plugin, then runs each one's statements again, as Migrate would
// after a crash between a migration's DDL and its ledger row on MySQL,
// whose DDL commits at once: none may fail. (SQLite applies a migration
// and its ledger row in one transaction, so it never reruns one.)
func TestEveryMigrationReruns(t *testing.T) {
	if engine() == records.EngineSQLite {
		t.Skip("SQLite applies a migration and its ledger row in one transaction")
	}
	q, _ := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{}, nil })
	v, _ := versions.New()
	e := newEnv(t, nil, records.WithPlugins(acl.New(), audit.New(), folders.New(), locks.New(), q, share.New(), tags.New(), v))
	ms, err := e.store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		for _, st := range m.Statements {
			if _, err := e.db.Exec(st); err != nil {
				t.Fatalf("%s/%s again: %s: %v", m.Owner, m.Name, st, err)
			}
		}
	}
	e.put(t, "t", "a.png")
}

// TestDDLAddColumnAndIndex adds a typed column and an index to an existing
// table on the engine under test, twice where the engine reruns them.
func TestDDLAddColumnAndIndex(t *testing.T) {
	e := newEnv(t, nil)
	extra := sqlb.Col[int64](records.TableFiles, "extra")
	tt := records.TypesFor(engine())
	st, err := records.NewDDL(engine()).
		AddColumn(records.TableFiles, records.Column(extra, tt.Int).Default(0)).
		CreateIndex(records.TableFiles, records.Index{Name: "fs_files_extra", Columns: []records.IndexColumn{records.Key(records.FileScope), records.Key(extra)}}).
		Statements()
	if err != nil {
		t.Fatal(err)
	}
	runs := 2
	if engine() == records.EngineSQLite {
		runs = 1 // SQLite has no ADD COLUMN IF NOT EXISTS; its migrations never rerun
	}
	for range runs {
		for _, s := range st {
			exec(t, e.db, s)
		}
	}
	f := e.put(t, "t", "a.png")
	var n int64
	q := sqlb.NewSelect(testDialect()).Columns(sqlb.Bind(extra, &n)).From(records.TableFiles).Where(sqlb.Eq(records.FileID, f.ID))
	if err := e.store.One(t.Context(), q); err != nil || n != 0 {
		t.Fatalf("extra = %d, %v", n, err)
	}
}

// TestDDLRefusals: a table without columns, an index without columns and a
// statement sqlb/ddl refuses are errors, and the first error is the one
// returned, later steps adding nothing.
func TestDDLRefusals(t *testing.T) {
	other := sqlb.Table{Name: "other"}
	foreign := sqlb.Col[string](other, "x")
	for _, eng := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		tt := records.TypesFor(eng)
		cases := map[string]*records.DDL{
			"no columns": records.NewDDL(eng).CreateTable(records.Table{Table: records.TableFiles}),
			"index without columns in a table": records.NewDDL(eng).CreateTable(records.Table{
				Table: records.TableFiles, Columns: []ddl.ColumnSpec{records.Column(records.FileID, tt.Key)},
				Indexes: []records.Index{{Name: "empty"}},
			}),
			"index without columns": records.NewDDL(eng).CreateIndex(records.TableFiles, records.Index{Name: "empty"}),
			"a column of another table": records.NewDDL(eng).CreateTable(records.Table{
				Table: records.TableFiles, Columns: []ddl.ColumnSpec{records.Column(foreign, tt.Key)},
			}),
		}
		for name, d := range cases {
			st, err := d.Raw("SELECT 1").AddColumn(records.TableFiles, records.Column(records.FileID, tt.Key)).Statements()
			if err == nil || len(st) != 0 {
				t.Errorf("%s %s: %v, %v", eng, name, st, err)
			}
		}
		// The first error stays the one returned.
		first := records.NewDDL(eng).CreateTable(records.Table{Table: records.TableFiles}).CreateIndex(records.TableFiles, records.Index{Name: "x"})
		if _, err := first.Statements(); err == nil || !strings.Contains(err.Error(), "no column") || strings.Contains(err.Error(), "index x") {
			t.Errorf("%s first error = %v", eng, err)
		}
	}
	// MODIFY is MySQL's; elsewhere it writes nothing.
	for _, eng := range []string{records.EngineSQLite, records.EnginePostgres} {
		if st, err := records.NewDDL(eng).ModifyColumn(records.TableFiles, records.Column(records.FileID, "TEXT")).Statements(); err != nil || len(st) != 0 {
			t.Errorf("%s ModifyColumn = %v, %v", eng, st, err)
		}
	}
	if st, err := records.NewDDL(records.EngineMySQL).ModifyColumn(records.TableFiles, records.Column(records.FileID, "VARCHAR(191)")).Statements(); err != nil || len(st) != 1 {
		t.Errorf("mysql ModifyColumn = %v, %v", st, err)
	}
	// A unique index on an existing table, guarded on MySQL.
	for _, eng := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		st, err := records.NewDDL(eng).CreateIndex(records.TableFiles, records.Index{Name: "u", Unique: true, Columns: []records.IndexColumn{records.Key(records.FileKey)}}).Statements()
		if err != nil || len(st) == 0 || !strings.Contains(strings.Join(st, " "), "UNIQUE INDEX") {
			t.Errorf("%s unique index = %v, %v", eng, st, err)
		}
	}
}
