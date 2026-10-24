package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/sqlc"
)

// TestRun pins every line the program prints: each reader leaves the
// trashed file out of the gallery and the listings, as the Store does.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sqlc gallery: 1 cover.txt 9 bytes",
		"sqlc gallery: 3 map.txt 7 bytes",
		"sqlc live files, 1 per page: map.txt, cover.txt",
		"sqlc totals: 3 files, 25 bytes (trash included)",
		"gorm gallery: 1 cover.txt 9 bytes",
		"gorm gallery: 3 map.txt 7 bytes",
		"gorm live file: map.txt, 1 attachment(s), records.File status ready",
		"gorm live file: cover.txt, 1 attachment(s), records.File status ready",
		"ent gallery: 1 cover.txt 9 bytes",
		"ent gallery: 3 map.txt 7 bytes",
		"ent delete refused: true",
		"ent rows after the refused delete: 3",
		"prefix: the Store writes app_fs_files",
		"prefix: gorm reads 1 live file(s) from app_fs_files, the default namer 2 from fs_files",
		"prefix: sqlc queries name app_fs_files: true, any fs_ name left: false",
		"prefix: ent schema table app_fs_files",
		"prefix: a mapping giving two names one table: invalid_argument",
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), strings.Join(want, "\n"))
	}
}

// queryName matches the sqlc annotation that names a query.
var queryName = regexp.MustCompile(`(?m)^-- name: (\w+) :`)

// TestGeneratedCoversEveryQuery fails when contrib/sqlc ships a query the
// committed fsdb package was not generated with: regenerate it (README.md
// says how) rather than hand-editing the generated files.
func TestGeneratedCoversEveryQuery(t *testing.T) {
	queries, err := sqlc.Queries(sqlc.EngineSQLite)
	if err != nil {
		t.Fatal(err)
	}
	var generated strings.Builder
	for _, f := range []string{"queries.sql.go", "pages.sql.go"} {
		b, err := os.ReadFile(filepath.Join("fsdb", f))
		if err != nil {
			t.Fatal(err)
		}
		generated.Write(b)
	}
	names := queryName.FindAllStringSubmatch(queries, -1)
	if len(names) == 0 {
		t.Fatal("contrib/sqlc returned no named queries")
	}
	for _, m := range names {
		if !strings.Contains(generated.String(), "func (q *Queries) "+m[1]+"(") {
			t.Errorf("fsdb has no method %s: regenerate it", m[1])
		}
	}
}
