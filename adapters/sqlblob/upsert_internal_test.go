package sqlblob

import (
	"strings"
	"testing"

	"github.com/ubgo/sqlb"
)

// TestUpsertIsSpelledPerEngine pins the one statement the driver spells per
// engine: Postgres and SQLite take ON CONFLICT ... DO UPDATE from EXCLUDED,
// MySQL takes ON DUPLICATE KEY UPDATE from the row alias. The unit run has
// only SQLite, so the other two spellings are checked here as text and run
// for real by the engines task.
func TestUpsertIsSpelledPerEngine(t *testing.T) {
	r := row{object: object{key: "k"}, data: []byte("x")}
	for dialect, want := range map[sqlb.Dialect][]string{
		sqlb.PostgresDialect{}: {`ON CONFLICT ("key") DO UPDATE SET`, `"data" = excluded."data"`, `"modified_ns" = excluded."modified_ns"`},
		sqlb.SQLiteDialect{}:   {`ON CONFLICT ("key") DO UPDATE SET`, `"data" = excluded."data"`, `"modified_ns" = excluded."modified_ns"`},
		sqlb.MySQLDialect{}:    {"AS `new` ON DUPLICATE KEY UPDATE", "`data` = `new`.`data`", "`modified_ns` = `new`.`modified_ns`"},
	} {
		d := &Driver{dialect: dialect, engine: sqlb.DialectName(dialect), cols: newColumns("", DefaultTable)}
		q, err := build(d.upsertStatement(r))
		if err != nil {
			t.Fatalf("%s: %v", d.engine, err)
		}
		for _, w := range want {
			if !strings.Contains(strings.ToLower(q.SQL), strings.ToLower(w)) { // keyword case differs per engine
				t.Errorf("%s: %s lacks %q", d.engine, q.SQL, w)
			}
		}
		if strings.Contains(q.SQL, "ON DUPLICATE KEY") != (d.engine == sqlb.DialectMySQL) {
			t.Errorf("%s: wrong upsert family: %s", d.engine, q.SQL)
		}
	}
}
