package sqlc_test

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/sqlc"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// TestSchemaMatchesStore creates the tables once with Store.Migrate and
// once from the engine's schema.sql, and compares what the engine reports
// of both: every column's type, nullability and default, and every index.
// A column the Store gains and schema.sql lacks fails it, so sqlc never
// generates code for a table that no longer exists in that shape.
func TestSchemaMatchesStore(t *testing.T) {
	schemaMatchesStore(t, nil)
}

// TestSchemaWithMatchesPrefixedStore: SchemaWith and the Store's mapping
// give the tables and indexes a Store built with WithTableNames creates.
func TestSchemaWithMatchesPrefixedStore(t *testing.T) {
	schemaMatchesStore(t, prefixed)
}

func schemaMatchesStore(t *testing.T, names records.TableNamer) {
	t.Helper()
	_, migrated, _ := newStoreWith(t, names)
	file, err := sqlc.SchemaWith(recordstest.Engine(), names)
	if err != nil {
		t.Fatal(err)
	}
	fromFile, _ := recordstest.Open(t)
	for _, st := range statements(file) {
		if _, err := fromFile.Exec(st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	wantCols, wantIdx := describe(t, migrated, names)
	gotCols, gotIdx := describe(t, fromFile, names)
	if d := diff(wantCols, gotCols); len(d) > 0 {
		t.Fatalf("%s/schema.sql differs from the Store's tables:\n%s", recordstest.Engine(), strings.Join(d, "\n"))
	}
	if !reflect.DeepEqual(wantIdx, gotIdx) {
		t.Fatalf("indexes: Store %v, schema.sql %v", wantIdx, gotIdx)
	}
	if len(wantCols[records.TableNameFiles]) == 0 || len(wantIdx[records.TableNameAttachments]) == 0 {
		t.Fatal("the catalogue reported nothing: the comparison proves nothing")
	}
}

// TestEngines: the directory names are the records engine names, and an
// unknown engine has no schema.
func TestEngines(t *testing.T) {
	if sqlc.EnginePostgres != records.EnginePostgres || sqlc.EngineMySQL != records.EngineMySQL || sqlc.EngineSQLite != records.EngineSQLite {
		t.Fatal("engine names drifted from records")
	}
	for _, e := range []string{sqlc.EnginePostgres, sqlc.EngineMySQL, sqlc.EngineSQLite} {
		s, err := sqlc.Schema(e)
		if err != nil || !strings.Contains(s, "CREATE TABLE fs_files") {
			t.Fatalf("Schema(%q) = %.40q, %v", e, s, err)
		}
	}
	if _, err := sqlc.Schema("oracle"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an unknown engine = %v", err)
	}
	for _, e := range []string{sqlc.EnginePostgres, sqlc.EngineMySQL, sqlc.EngineSQLite} {
		q, err := sqlc.Queries(e)
		if err != nil || !strings.Contains(q, "-- name: GetFile :one") || !strings.Contains(q, "-- name: ListLiveFiles :many") {
			t.Fatalf("Queries(%q) lacks the shared or the paged queries: %v", e, err)
		}
	}
	if _, err := sqlc.Queries("oracle"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("queries of an unknown engine = %v", err)
	}
}

// TestMapNames: every records name maps, nothing else does, the prefix
// is records', nil changes nothing, and FSWith maps only the .sql files.
func TestMapNames(t *testing.T) {
	if got := sqlc.MapNames("SELECT x FROM fs_files f JOIN fs_attachments a ON a.file_id = f.id -- fs_files", prefixed); got != "SELECT x FROM app_fs_files f JOIN app_fs_attachments a ON a.file_id = f.id -- app_fs_files" {
		t.Errorf("MapNames = %q", got)
	}
	if got := sqlc.MapNames("SELECT myfs_x, app_fs_files FROM t", prefixed); got != "SELECT myfs_x, app_fs_files FROM t" {
		t.Errorf("a word not starting with the prefix was mapped: %q", got)
	}
	if got := sqlc.MapNames("FROM fs_files", nil); got != "FROM fs_files" {
		t.Errorf("nil names changed the SQL: %q", got)
	}
	if !strings.HasPrefix(records.TableNameFiles, records.ObjectPrefix) || sqlc.MapNames(records.ObjectPrefix+"x", prefixed) != prefixed(records.ObjectPrefix+"x") {
		t.Error("sqlc's object prefix drifted from records.ObjectPrefix")
	}
	mfs, err := sqlc.FSWith(prefixed)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	err = fs.WalkDir(sqlc.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		orig, _ := fs.ReadFile(sqlc.FS(), p)
		got, err := fs.ReadFile(mfs, p)
		if err != nil {
			return err
		}
		n++
		want := string(orig)
		if strings.HasSuffix(p, ".sql") {
			want = sqlc.MapNames(want, prefixed)
			if !strings.Contains(string(got), "app_fs_") {
				t.Errorf("%s: no mapped name", p)
			}
		}
		if string(got) != want {
			t.Errorf("%s: FSWith differs from MapNames of FS", p)
		}
		return nil
	})
	if err != nil || n < 7 {
		t.Fatalf("walked %d files: %v", n, err)
	}
	if f, err := sqlc.FSWith(nil); err != nil || f != sqlc.FS() {
		t.Errorf("FSWith(nil) = %v, %v; want FS()", f, err)
	}
}
