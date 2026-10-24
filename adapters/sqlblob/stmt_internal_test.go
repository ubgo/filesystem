package sqlblob

import (
	"context"
	"errors"
	"testing"

	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"

	"github.com/ubgo/filesystem"
)

// errMalformed is what malformedBuilder refuses with.
var errMalformed = errors.New("malformed statement")

// malformedBuilder is a statement sqlb would refuse to render.
type malformedBuilder struct{}

func (malformedBuilder) Build() (sqlb.Query, error) { return sqlb.Query{}, errMalformed }

// TestUnbuildableStatementsNeverRun pins that every way the driver runs a
// statement (exec, one, each) returns the build's refusal and touches no
// database: db is nil, so a helper that ran the statement anyway would
// panic. It covers a stand-in builder and a real one sqlb refuses (a
// select with no columns), so the refusal is sqlb's own, not only a fake's.
func TestUnbuildableStatementsNeverRun(t *testing.T) {
	ctx := context.Background()
	noColumns := sqlb.NewSelect(sqlb.SQLiteDialect{}).From(newColumns("", DefaultTable).table)
	for name, tc := range map[string]struct {
		st   builder
		want error
	}{
		"stand-in":   {malformedBuilder{}, errMalformed},
		"no columns": {noColumns, sqlb.ErrNoColumns},
	} {
		runs := map[string]func() error{
			"exec": func() error { _, err := exec(ctx, nil, tc.st); return err },
			"one":  func() error { return one(ctx, nil, tc.st) },
			"each": func() error {
				return each(ctx, nil, tc.st, func() error { t.Fatal("each scanned a row"); return nil })
			},
		}
		for run, fn := range runs {
			if err := fn(); !errors.Is(err, tc.want) {
				t.Errorf("%s via %s: %v, want %v", name, run, err, tc.want)
			}
		}
	}
}

// TestSchemaReportsAnUnbuildableTable pins that a CREATE TABLE sqlb refuses
// (here a column declared on another table, which New never produces) is
// CodeInvalidArgument carrying sqlb's reason, on every engine, from Schema
// and from Migrate, which then runs nothing (db is nil, so running a
// statement would panic).
func TestSchemaReportsAnUnbuildableTable(t *testing.T) {
	cols := newColumns("", DefaultTable)
	cols.table = sqlb.Table{Name: "other"}
	for _, dialect := range []sqlb.Dialect{sqlb.SQLiteDialect{}, sqlb.PostgresDialect{}, sqlb.MySQLDialect{}} {
		d := &Driver{dialect: dialect, engine: sqlb.DialectName(dialect), cols: cols}
		_, schemaErr := d.Schema()
		for call, err := range map[string]error{"Schema": schemaErr, "Migrate": d.Migrate(context.Background())} {
			if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !errors.Is(err, ddl.ErrForeignColumn) {
				t.Errorf("%s %s: %v, want CodeInvalidArgument wrapping ddl.ErrForeignColumn", d.engine, call, err)
			}
		}
	}
}
