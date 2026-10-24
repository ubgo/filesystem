// Package sqlc ships the records tables and read queries for sqlc
// (https://sqlc.dev), so an application that already uses sqlc reads
// fs_files and fs_attachments with generated, typed Go instead of
// hand-written SQL.
//
// For each engine there is a directory (postgres, mysql, sqlite) holding
// schema.sql, the tables exactly as records.Store.Migrate leaves them,
// pages.sql, the paginated listings (sqlc spells LIMIT differently per
// engine), and sqlc.yaml; queries.sql, shared by all three, holds the
// other read paths. The files are embedded (FS, Schema, Queries) so a
// tool can write them out, and they are plain files for copying.
//
// A Store built with records.WithTablePrefix or records.WithTableNames
// keeps its tables and indexes under other names, and sqlc bakes names in
// when it generates. FSWith returns the same project with every records
// name in schema.sql, pages.sql and queries.sql mapped by the Store's
// records.TableNamer; write it out and run sqlc generate on it.
// SchemaWith, QueriesWith and MapNames do the same for one file.
//
// Read only. Never create the tables from schema.sql (Store.Migrate does,
// and records its migration ledger), and never write fs_ rows through
// sqlc: the Store runs plugin hooks, emits events and keeps stored
// objects consistent with their rows, and a row written behind its back
// skips all of that. The package's tests fail when the Store's tables and
// a schema.sql differ, and run every query against rows the Store wrote.
package sqlc

import (
	"embed"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"testing/fstest"

	"github.com/ubgo/filesystem"
)

// Engine directory names, equal to records.EnginePostgres, EngineMySQL
// and EngineSQLite (the tests check it).
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
	EngineSQLite   = "sqlite"
)

// File names inside FS.
const (
	// QueriesFile is the read queries, at the root of FS.
	QueriesFile = "queries.sql"
	// SchemaFile, PagesFile and ConfigFile are in each engine's directory.
	SchemaFile = "schema.sql"
	PagesFile  = "pages.sql"
	ConfigFile = "sqlc.yaml"
)

//go:embed queries.sql postgres/*.sql postgres/*.yaml mysql/*.sql mysql/*.yaml sqlite/*.sql sqlite/*.yaml
var files embed.FS

// FS returns every file of the package: queries.sql at the root and
// <engine>/schema.sql and <engine>/sqlc.yaml, the layout sqlc.yaml
// expects, so writing FS out to a directory gives a working sqlc project.
func FS() fs.FS { return files }

// Schema returns the schema.sql of engine (EnginePostgres, EngineMySQL or
// EngineSQLite); any other engine is CodeInvalidArgument.
func Schema(engine string) (string, error) {
	if err := checkEngine(engine); err != nil {
		return "", err
	}
	b, err := files.ReadFile(engine + "/" + SchemaFile)
	return string(b), err
}

// Queries returns every read query of engine: queries.sql followed by the
// engine's pages.sql, the set its sqlc.yaml generates. Any other engine
// is CodeInvalidArgument.
func Queries(engine string) (string, error) {
	if err := checkEngine(engine); err != nil {
		return "", err
	}
	// Both files are embedded above, so neither read can fail in a built
	// binary; joining the errors still reports one rather than hiding it.
	shared, sharedErr := files.ReadFile(QueriesFile)
	pages, pagesErr := files.ReadFile(engine + "/" + PagesFile)
	return string(shared) + "\n" + string(pages), errors.Join(sharedErr, pagesErr)
}

// objectPrefix starts the name of every records table and index; it is
// records.ObjectPrefix (the tests check it), repeated here because this
// module imports the core module only.
const objectPrefix = "fs_"

// objectName finds the names a mapping applies to in the SQL files: whole
// words starting with objectPrefix, as records maps the SQL it renders.
var objectName = regexp.MustCompile(`\b` + objectPrefix + `[A-Za-z0-9_]*`)

// MapNames returns sql with every records table and index name (a word
// starting with "fs_") replaced by names(name), the Store's
// records.TableNamer (a records.TableNamer is assignable to the
// parameter). A nil names returns sql unchanged.
func MapNames(sql string, names func(name string) string) string {
	if names == nil {
		return sql
	}
	return objectName.ReplaceAllStringFunc(sql, names)
}

// SchemaWith is Schema with the names mapped by names (MapNames).
func SchemaWith(engine string, names func(name string) string) (string, error) {
	s, err := Schema(engine)
	return MapNames(s, names), err
}

// QueriesWith is Queries with the names mapped by names (MapNames).
func QueriesWith(engine string, names func(name string) string) (string, error) {
	q, err := Queries(engine)
	return MapNames(q, names), err
}

// FSWith returns FS with every .sql file's names mapped by names
// (MapNames), in the same layout, so writing it out and running sqlc
// generate gives queries over the tables of a Store built with
// records.WithTableNames or WithTablePrefix. sqlc names the generated
// models after the tables (app_fs_files gives AppFsFile); a rename in
// sqlc.yaml keeps the default names if you want them. A nil names
// returns FS unchanged.
func FSWith(names func(name string) string) (fs.FS, error) {
	if names == nil {
		return files, nil
	}
	out := fstest.MapFS{}
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		if path.Ext(p) == ".sql" {
			b = []byte(MapNames(string(b), names))
		}
		out[p] = &fstest.MapFile{Data: b, Mode: 0o644}
		return nil
	})
	return out, err
}

func checkEngine(engine string) error {
	switch engine {
	case EnginePostgres, EngineMySQL, EngineSQLite:
		return nil
	}
	return filesystem.Errorf(filesystem.CodeInvalidArgument, "sqlc: no files for engine %q", engine)
}
