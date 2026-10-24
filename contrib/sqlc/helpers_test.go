package sqlc_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// tables are the records tables the files describe.
var tables = []string{records.TableNameFiles, records.TableNameAttachments}

// newStore returns a migrated Store over a fresh database of the engine
// under test, and that database.
func newStore(t *testing.T) (*records.Store, *sql.DB, *filesystem.Disk) {
	t.Helper()
	return newStoreWith(t, nil)
}

// tablePrefix is the prefix of the prefixed Store in the naming tests.
const tablePrefix = "app_"

// prefixed is what records.WithTablePrefix(tablePrefix) does to a name.
func prefixed(name string) string { return tablePrefix + name }

// mapped is name under names, the identity when names is nil.
func mapped(names records.TableNamer, name string) string {
	if names == nil {
		return name
	}
	return names(name)
}

// newStoreWith is newStore with the Store's table and index names mapped
// by names (nil keeps the defaults).
func newStoreWith(t *testing.T, names records.TableNamer) (*records.Store, *sql.DB, *filesystem.Disk) {
	t.Helper()
	db, dialect := recordstest.Open(t)
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	opts := []records.Option{records.WithOwnerTypes("post")}
	if names != nil {
		opts = append(opts, records.WithTableNames(names))
	}
	s, err := records.New(db, dialect, mgr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, db, disk
}

// column is what the tests compare of a column: its declared type, its
// nullability and its default, as the engine reports them.
type column struct{ typ, nullable, def string }

// describe reads the columns and index names of the records tables from
// the engine's catalogue, each table looked up under its name mapped by
// names (nil: the default names) and reported under its default name.
func describe(t *testing.T, db *sql.DB, names records.TableNamer) (map[string]map[string]column, map[string][]string) {
	t.Helper()
	cols := map[string]map[string]column{}
	idx := map[string][]string{}
	for _, key := range tables {
		table := mapped(names, key)
		cols[key] = map[string]column{}
		var q string
		switch recordstest.Engine() {
		case recordstest.Postgres:
			q = "SELECT column_name, data_type, is_nullable, COALESCE(column_default, '') FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = '" + table + "'"
		case recordstest.MySQL:
			q = "SELECT column_name, column_type, is_nullable, COALESCE(column_default, '') FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '" + table + "'"
		default:
			q = "SELECT name, type, CASE WHEN \"notnull\" = 1 THEN 'NO' ELSE 'YES' END, COALESCE(dflt_value, '') FROM pragma_table_info('" + table + "')"
		}
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var c column
			if err := rows.Scan(&name, &c.typ, &c.nullable, &c.def); err != nil {
				t.Fatal(err)
			}
			cols[key][name] = c
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		switch recordstest.Engine() {
		case recordstest.Postgres:
			q = "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = '" + table + "' AND indexname NOT LIKE '%_pkey'"
		case recordstest.MySQL:
			q = "SELECT DISTINCT index_name FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = '" + table + "' AND index_name <> 'PRIMARY'"
		default:
			q = "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = '" + table + "' AND name NOT LIKE 'sqlite_autoindex_%'"
		}
		rows, err = db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			idx[key] = append(idx[key], name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(idx[key])
	}
	return cols, idx
}

// statements splits a .sql file into its statements, dropping comments.
func statements(file string) []string {
	var out []string
	for _, part := range strings.Split(file, ";") {
		var lines []string
		for _, line := range strings.Split(part, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
				lines = append(lines, line)
			}
		}
		if len(lines) > 0 {
			out = append(out, strings.Join(lines, "\n"))
		}
	}
	return out
}

func diff(a, b map[string]map[string]column) []string {
	var out []string
	for table, cols := range a {
		for name, c := range cols {
			if other, ok := b[table][name]; !ok {
				out = append(out, fmt.Sprintf("%s.%s only in the Store's table", table, name))
			} else if other != c {
				out = append(out, fmt.Sprintf("%s.%s: Store %+v, schema.sql %+v", table, name, c, other))
			}
		}
		for name := range b[table] {
			if _, ok := cols[name]; !ok {
				out = append(out, fmt.Sprintf("%s.%s only in schema.sql", table, name))
			}
		}
	}
	slices.Sort(out)
	return out
}
