package fsent_test

import (
	"database/sql"
	"slices"
	"testing"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"

	"github.com/ubgo/filesystem"
	fsent "github.com/ubgo/filesystem/contrib/ent"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/sqlb"
)

// newStore returns a migrated Store over a fresh database of the engine
// under test, and that database.
func newStore(t *testing.T, opts ...records.Option) (*records.Store, *sql.DB) {
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
	s, err := records.New(db, dialect, mgr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

// columns reads a table's column names from the engine's catalogue.
func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	q := "SELECT name FROM pragma_table_info('" + table + "')"
	switch recordstest.Engine() {
	case recordstest.Postgres:
		q = "SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = '" + table + "'"
	case recordstest.MySQL:
		q = "SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '" + table + "'"
	}
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// indexes reads a table's index names, primary key left out, from the
// engine's catalogue.
func indexes(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	q := "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = '" + table + "' AND name NOT LIKE 'sqlite_autoindex_%'"
	switch recordstest.Engine() {
	case recordstest.Postgres:
		q = "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = '" + table + "' AND indexname NOT LIKE '%_pkey'"
	case recordstest.MySQL:
		q = "SELECT DISTINCT index_name FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = '" + table + "' AND index_name <> 'PRIMARY'"
	}
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func indexNames(list []ent.Index) []string {
	var out []string
	for _, i := range list {
		out = append(out, i.Descriptor().StorageKey)
	}
	slices.Sort(out)
	return out
}

func names(fields []ent.Field) []string {
	var out []string
	for _, f := range fields {
		d := f.Descriptor()
		name := d.Name
		if d.StorageKey != "" {
			name = d.StorageKey
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// TestFieldsMatchTables: every mixin field is a column of the Store's
// table and every column is a field, and the mixins' indexes are the
// table's, on the engine under test. A column or index a records
// migration adds fails it until the mixin has it.
func TestFieldsMatchTables(t *testing.T) {
	_, db := newStore(t)
	for table, fields := range map[string][]ent.Field{
		records.TableNameFiles:       fsent.FileMixin{}.Fields(),
		records.TableNameAttachments: fsent.AttachmentMixin{}.Fields(),
	} {
		cols := columns(t, db, table)
		if len(cols) == 0 {
			t.Fatalf("%s has no columns: the comparison proves nothing", table)
		}
		if got := names(fields); !slices.Equal(got, cols) {
			t.Errorf("%s: mixin fields %v, table columns %v", table, got, cols)
		}
	}
	for table, list := range map[string][]ent.Index{
		records.TableNameFiles:       fsent.FileMixin{}.Indexes(),
		records.TableNameAttachments: fsent.AttachmentMixin{}.Indexes(),
	} {
		if got, want := indexNames(list), indexes(t, db, table); !slices.Equal(got, want) {
			t.Errorf("%s: mixin indexes %v, table indexes %v", table, got, want)
		}
	}
}

// newFileStore is newStore on a SQLite file at path, which another
// process (the generated client) can open.
func newFileStore(t *testing.T, path string) (*records.Store, error) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return nil, err
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, mgr, records.WithOwnerTypes("post"))
	if err != nil {
		return nil, err
	}
	_, err = s.Migrate(t.Context())
	return s, err
}

// TestNamesFollowTheStore: mixins given the Store's table mapping name the
// tables and indexes a Store built with records.WithTablePrefix created,
// on the engine under test, and the zero value keeps the default names.
func TestNamesFollowTheStore(t *testing.T) {
	const prefix = "app_"
	_, db := newStore(t, records.WithTablePrefix(prefix))
	prefixed := func(name string) string { return prefix + name }
	for _, m := range []struct {
		table   string
		def     []schema.Annotation
		mapped  []schema.Annotation
		indexes []ent.Index
		fields  []ent.Field
	}{
		{records.TableNameFiles, fsent.FileMixin{}.Annotations(), fsent.FileMixin{Names: prefixed}.Annotations(), fsent.FileMixin{Names: prefixed}.Indexes(), fsent.FileMixin{}.Fields()},
		{records.TableNameAttachments, fsent.AttachmentMixin{}.Annotations(), fsent.AttachmentMixin{Names: prefixed}.Annotations(), fsent.AttachmentMixin{Names: prefixed}.Indexes(), fsent.AttachmentMixin{}.Fields()},
	} {
		if got := tableOf(t, m.def); got != m.table {
			t.Errorf("zero-value mixin names table %q, want %q", got, m.table)
		}
		table := tableOf(t, m.mapped)
		if table != prefix+m.table {
			t.Errorf("mapped mixin names table %q, want %q", table, prefix+m.table)
		}
		cols := columns(t, db, table)
		if len(cols) == 0 {
			t.Fatalf("%s has no columns: the Store did not create the table the mixin names", table)
		}
		if got := names(m.fields); !slices.Equal(got, cols) {
			t.Errorf("%s: mixin fields %v, table columns %v", table, got, cols)
		}
		if got, want := indexNames(m.indexes), indexes(t, db, table); !slices.Equal(got, want) {
			t.Errorf("%s: mixin indexes %v, table indexes %v", table, got, want)
		}
	}
}

// tableOf is the table an entsql annotation in list names.
func tableOf(t *testing.T, list []schema.Annotation) string {
	t.Helper()
	for _, a := range list {
		if e, ok := a.(entsql.Annotation); ok {
			return e.Table
		}
	}
	t.Fatal("no entsql annotation")
	return ""
}
