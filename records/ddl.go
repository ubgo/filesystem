package records

import (
	"fmt"
	"strings"

	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
	"github.com/ubgo/sqlb/mysql"
	"github.com/ubgo/sqlb/pg"
	"github.com/ubgo/sqlb/sqlite"
)

// Column is a column definition for a migration: the typed column the
// queries use, spelled for the engine with sqlType (from TypesFor), so a
// table cannot be created under one name or type and read under another.
// Nullability follows the Go type: a pointer column is NULL, any other
// NOT NULL. Give a default with Default on the result.
func Column[T any](col sqlb.Expr[T], sqlType string) ddl.ColumnDef[T] {
	return ddl.Column(col).As(ddl.RawType[T](sqlType))
}

// IndexColumn is one column of an index, for every engine at once.
type IndexColumn struct {
	ref  ddl.ColumnRef
	pg   pg.IndexElement
	lite sqlite.IndexElement
	my   mysql.IndexElement
}

// Key makes an index column of col.
func Key[T any](col sqlb.Expr[T]) IndexColumn {
	return IndexColumn{ref: col, pg: pg.IndexKey(col), lite: sqlite.IndexKey(col), my: mysql.IndexKey(col)}
}

// KeyPrefix makes an index column of the first n characters of a text
// column on MySQL, whose index keys are capped at 3072 bytes (a Long
// column is 4096 bytes in utf8mb4), and of the whole column on Postgres
// and SQLite, which have no such cap. Lookups by the full value still use
// the index; MySQL rechecks the rest of the value on the rows it finds.
func KeyPrefix[T ~string](col sqlb.Expr[T], n int) IndexColumn {
	return IndexColumn{ref: col, pg: pg.IndexKey(col), lite: sqlite.IndexKey(col), my: mysql.IndexPrefix(col, n)}
}

// Index describes a non-primary index of a table.
type Index struct {
	// Name is unique in the database (Postgres and SQLite share one index
	// namespace per schema), so prefix it with the table's name.
	Name    string
	Unique  bool
	Columns []IndexColumn
}

// Table describes a table for CreateTable: its columns, its primary key
// and its indexes.
type Table struct {
	Table      sqlb.Table
	Columns    []ddl.ColumnSpec
	PrimaryKey []ddl.ColumnRef
	Indexes    []Index
}

// DDL collects the statements of one migration for one engine, built with
// github.com/ubgo/sqlb/ddl from the same typed columns the queries use.
// Every statement it writes can run again after a crash between it and
// the ledger write, which matters on MySQL, whose DDL commits at once: a
// table and a Postgres or SQLite index say IF NOT EXISTS, and a MySQL
// index or added column runs only when the catalogue lacks it. The first
// build error is kept and returned by Statements, so a migration function
// reads as a list of steps with one error check at the end.
type DDL struct {
	engine string
	stmts  []string
	err    error
}

// NewDDL starts the statements of a migration for engine (EnginePostgres,
// EngineMySQL or EngineSQLite).
func NewDDL(engine string) *DDL { return &DDL{engine: engine} }

// Statements returns the statements, or none and the first error a step
// met, so a migration is never half built.
func (d *DDL) Statements() ([]string, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.stmts, nil
}

// add appends a built script's statements, honouring SkipIf on MySQL.
func (d *DDL) add(s ddl.Script, err error) *DDL {
	if d.err != nil {
		return d
	}
	if err != nil {
		d.err = err
		return d
	}
	for _, st := range s {
		if st.SkipIf != "" {
			d.stmts = append(d.stmts, guarded(st.SQL, st.SkipIf)...)
			continue
		}
		d.stmts = append(d.stmts, st.SQL)
	}
	return d
}

// Raw appends statements as written, for what sqlb/ddl does not build (a
// data backfill between two schema steps). Keep them re-runnable.
func (d *DDL) Raw(stmts ...string) *DDL {
	if d.err == nil {
		d.stmts = append(d.stmts, stmts...)
	}
	return d
}

// CreateTable creates t if it does not exist. On MySQL its indexes are
// declared inside the CREATE TABLE (MySQL has no CREATE INDEX IF NOT
// EXISTS), and the table is InnoDB with utf8mb4 and the byte-exact
// utf8mb4_bin collation, so names compare byte for byte as on the other
// engines; on Postgres and SQLite each index is a CREATE INDEX IF NOT
// EXISTS after the table.
func (d *DDL) CreateTable(t Table) *DDL {
	if len(t.Columns) == 0 {
		return d.fail(fmt.Errorf("records: table %s has no column", t.Table.Name))
	}
	switch d.engine {
	case EngineMySQL:
		var clauses []mysql.TableClause
		if len(t.PrimaryKey) > 0 {
			clauses = append(clauses, mysql.PrimaryKey(t.PrimaryKey[0], t.PrimaryKey[1:]...))
		}
		for _, ix := range t.Indexes {
			if len(ix.Columns) == 0 {
				return d.fail(errNoIndexColumn(ix))
			}
			refs := indexRefs(ix)
			if ix.Unique {
				clauses = append(clauses, mysql.Unique(mysql.TableConstraint{Table: t.Table, Name: ix.Name}, refs[0], refs[1:]...))
			} else {
				clauses = append(clauses, mysql.TableIndex(mysql.Index{Name: ix.Name}, refs[0], refs[1:]...))
			}
		}
		clauses = append(clauses, mysql.InnoDB(), mysql.DefaultCharset(mysqlCharset), mysql.TableCollate(mysqlCollation))
		return d.add(mysql.CreateTable(t.Table).IfNotExists().Column(t.Columns[0], t.Columns[1:]...).Clause(clauses...).Build())
	case EnginePostgres:
		q := pg.CreateTable(t.Table).IfNotExists().Column(t.Columns[0], t.Columns[1:]...)
		if len(t.PrimaryKey) > 0 {
			q = q.Clause(pg.PrimaryKey(t.PrimaryKey[0], t.PrimaryKey[1:]...))
		}
		d.add(q.Build())
	default:
		q := sqlite.CreateTable(t.Table).IfNotExists().Column(t.Columns[0], t.Columns[1:]...)
		if len(t.PrimaryKey) > 0 {
			q = q.Clause(sqlite.PrimaryKey(t.PrimaryKey[0], t.PrimaryKey[1:]...))
		}
		d.add(q.Build())
	}
	for _, ix := range t.Indexes {
		d.CreateIndex(t.Table, ix)
	}
	return d
}

// CreateIndex creates an index on an existing table if it does not
// exist: CREATE INDEX IF NOT EXISTS on Postgres and SQLite, and on MySQL,
// which has none, a CREATE INDEX that runs only when the catalogue lacks
// the index (sqlb/ddl's SkipIf, run by a guard: see guarded).
func (d *DDL) CreateIndex(t sqlb.Table, ix Index) *DDL {
	if len(ix.Columns) == 0 {
		return d.fail(errNoIndexColumn(ix))
	}
	switch d.engine {
	case EngineMySQL:
		var els []mysql.IndexElement
		for _, c := range ix.Columns {
			els = append(els, c.my)
		}
		q := mysql.CreateIndex(mysql.Index{Name: ix.Name}, t)
		if ix.Unique {
			q = q.Unique()
		}
		return d.add(q.On(els[0], els[1:]...).IfNotExists().Build())
	case EnginePostgres:
		var els []pg.IndexElement
		for _, c := range ix.Columns {
			els = append(els, c.pg)
		}
		q := pg.CreateIndex(pg.Index{Name: ix.Name}, t).IfNotExists()
		if ix.Unique {
			q = q.Unique()
		}
		return d.add(q.On(els[0], els[1:]...).Build())
	}
	var els []sqlite.IndexElement
	for _, c := range ix.Columns {
		els = append(els, c.lite)
	}
	q := sqlite.CreateIndex(sqlite.Index{Name: ix.Name}, t).IfNotExists()
	if ix.Unique {
		q = q.Unique()
	}
	return d.add(q.On(els[0], els[1:]...).Build())
}

// DropIndex drops the index named name from t if it exists: DROP INDEX
// IF EXISTS on Postgres and SQLite, and on MySQL, which has none, a DROP
// INDEX that runs only when the catalogue still has the index (the same
// guard CreateIndex uses). A migration that widens a unique key creates
// the new index first and drops the old one after, so the table is never
// left without one.
func (d *DDL) DropIndex(t sqlb.Table, name string) *DDL {
	switch d.engine {
	case EngineMySQL:
		return d.add(mysql.DropIndex(mysql.Index{Name: name}, t).IfExists().Build())
	case EnginePostgres:
		return d.add(pg.DropIndex(pg.Index{Name: name}).IfExists().Build())
	}
	return d.add(sqlite.DropIndex(sqlite.Index{Name: name}).IfExists().Build())
}

// AddColumn adds a column to an existing table, re-runnably: Postgres
// says ADD COLUMN IF NOT EXISTS; SQLite's DDL is transactional, so a
// migration never half applies there; MySQL has no ADD COLUMN IF NOT
// EXISTS, so its ALTER runs only when information_schema lacks the column.
// A NOT NULL column needs a default (Default), for the existing rows.
func (d *DDL) AddColumn(t sqlb.Table, c ddl.ColumnSpec) *DDL {
	switch d.engine {
	case EngineMySQL:
		s, err := mysql.AlterTable(t).Clause(mysql.AddColumn(c)).Build()
		if err == nil {
			// sqlb/ddl writes no SkipIf for a MySQL ADD COLUMN (MySQL has no
			// IF NOT EXISTS for it, and ddl/not-built.md leaves repeatability
			// to the runner), so this guard query is the module's own.
			for i := range s {
				s[i].SkipIf = "SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '" +
					t.Name + "' AND column_name = '" + c.ColumnName() + "'"
			}
		}
		return d.add(s, err)
	case EnginePostgres:
		return d.add(pg.AlterTable(t).Clause(pg.AddColumnIfNotExists(c)).Build())
	}
	return d.add(sqlite.AlterTable(t).Action(ddl.AddColumn(c)).Build())
}

// ModifyColumn changes a MySQL column's definition in place (MODIFY), for
// a step MySQL needs and the other engines do not (a TEXT column made NOT
// NULL after a backfill). It writes nothing on the other engines.
func (d *DDL) ModifyColumn(t sqlb.Table, c ddl.ColumnSpec) *DDL {
	if d.engine != EngineMySQL {
		return d
	}
	return d.add(mysql.AlterTable(t).Clause(mysql.ModifyColumn(c)).Build())
}

func (d *DDL) fail(err error) *DDL {
	if d.err == nil {
		d.err = err
	}
	return d
}

func errNoIndexColumn(ix Index) error {
	return fmt.Errorf("records: index %s has no column", ix.Name)
}

func indexRefs(ix Index) []ddl.ColumnRef {
	refs := make([]ddl.ColumnRef, len(ix.Columns))
	for i, c := range ix.Columns {
		refs[i] = c.ref
	}
	return refs
}

// MySQL table options: the byte-exact collation, so a name compares byte
// for byte as it does on Postgres and SQLite.
const (
	mysqlCharset   = "utf8mb4"
	mysqlCollation = "utf8mb4_bin"
)

// guarded runs a MySQL statement only when skipIf returns no row. sqlb/ddl
// records the condition as Statement.SkipIf and leaves running it to the
// runner; the Store's migrations are plain statements, so the condition
// is folded into the statements themselves with PREPARE, which MySQL
// accepts for DDL. The statement is quoted as a string literal.
func guarded(stmt, skipIf string) []string {
	quoted := "'" + strings.ReplaceAll(stmt, "'", "''") + "'"
	return []string{
		"SET @fs_ddl = IF(EXISTS(" + skipIf + "), 'DO 0', " + quoted + ")",
		"PREPARE fs_ddl FROM @fs_ddl",
		"EXECUTE fs_ddl",
		"DEALLOCATE PREPARE fs_ddl",
	}
}
