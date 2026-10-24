package sqlc_test

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/sqlc"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// query is one named query of queries.sql.
type query struct{ name, kind, sql string }

var (
	nameLine = regexp.MustCompile(`(?m)^-- name: (\w+) :(\w+)$`)
	// argRef is a named parameter, or MySQL's bare LIMIT ? (pages.sql),
	// which sqlc names Limit and this test feeds page_size.
	argRef = regexp.MustCompile(`sqlc\.arg\((\w+)\)|LIMIT \?`)
)

// parse splits queries.sql into its named queries.
func parse(t *testing.T, file string) []query {
	t.Helper()
	locs := nameLine.FindAllStringSubmatchIndex(file, -1)
	var out []query
	for i, l := range locs {
		end := len(file)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		st := statements(file[l[1]:end])
		if len(st) != 1 {
			t.Fatalf("query %s has %d statements", file[l[2]:l[3]], len(st))
		}
		out = append(out, query{name: file[l[2]:l[3]], kind: file[l[4]:l[5]], sql: st[0]})
	}
	return out
}

// bind rewrites sqlc.arg(name) the way sqlc does for the engine under
// test ($1, $2… numbered by name on Postgres, ? per use elsewhere) and
// returns the statement with its arguments in order.
func bind(t *testing.T, q string, args map[string]any) (string, []any) {
	t.Helper()
	var out []any
	var names []string
	text := argRef.ReplaceAllStringFunc(q, func(ref string) string {
		name, prefix := argRef.FindStringSubmatch(ref)[1], ""
		if name == "" {
			name, prefix = "page_size", "LIMIT "
		}
		v, ok := args[name]
		if !ok {
			t.Fatalf("no value for sqlc.arg(%s)", name)
		}
		if recordstest.Engine() != recordstest.Postgres {
			out = append(out, v)
			return prefix + "?"
		}
		i := slices.Index(names, name)
		if i < 0 {
			names = append(names, name)
			out = append(out, v)
			i = len(names) - 1
		}
		return fmt.Sprintf("%s$%d", prefix, i+1)
	})
	return text, out
}

// TestQueries runs every query of queries.sql on tables the Store created,
// over rows it wrote, and checks each answer against the Store's own.
func TestQueries(t *testing.T) {
	queriesMatchStore(t, nil)
}

// TestQueriesWithPrefixedStore runs QueriesWith under the Store's mapping
// on a Store built with WithTableNames.
func TestQueriesWithPrefixedStore(t *testing.T) {
	queriesMatchStore(t, prefixed)
}

func queriesMatchStore(t *testing.T, names records.TableNamer) {
	t.Helper()
	ctx := t.Context()
	s, db, disk := newStoreWith(t, names)
	var ids []string
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	for _, body := range []string{"one", "two", "three", "two"} {
		f, err := s.Upload(records.WithActor(ctx, "user:alice"), "t", disk.Name(), strings.NewReader(body), body+".txt", policy)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	for _, id := range []string{ids[2], ids[0], ids[3]} {
		if _, err := s.Attach(ctx, "t", id, post, "gallery"); err != nil {
			t.Fatal(err)
		}
	}
	// Trashed after it was attached: its attachment is left out of listings.
	if err := s.Trash(ctx, "t", ids[3]); err != nil {
		t.Fatal(err)
	}
	page, err := s.Files(ctx, records.FileQuery{Scope: "t", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := page.Files[1]
	two, err := s.File(ctx, "t", ids[1])
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{
		"scope": "t", "id": ids[1], "page_size": 2, "after_created_at": first.CreatedAt, "after_id": first.ID,
		"sha256": two.SHA256, "owner_type": post.Type, "owner_id": post.ID, "role": "gallery", "file_id": ids[0],
	}
	// want is each query's first column per row, as the Store sees it.
	want := map[string][]string{
		"GetFile":              {ids[1]},
		"ListLiveFiles":        {page.Files[0].ID, page.Files[1].ID},
		"ListLiveFilesAfter":   {ids[0]},
		"ListFilesBySHA256":    {ids[1]}, // ids[3] has the same content but is trashed
		"ScopeTotals":          {"4"},
		"ListOwnerAttachments": nil, // checked below: attachment IDs of ids[2], ids[0]
		"ListFileAttachments":  nil,
	}
	attached, err := s.Attachments(ctx, "t", post, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attached {
		want["ListOwnerAttachments"] = append(want["ListOwnerAttachments"], a.Attachment.ID)
		if a.File.ID == ids[0] {
			want["ListFileAttachments"] = []string{a.Attachment.ID}
		}
	}
	file, err := sqlc.QueriesWith(recordstest.Engine(), names)
	if err != nil {
		t.Fatal(err)
	}
	queries := parse(t, file)
	var seen []string
	for _, q := range queries {
		seen = append(seen, q.name)
		text, qargs := bind(t, q.sql, args)
		got := firstColumn(t, db, text, qargs)
		if q.kind == "one" && len(got) != 1 {
			t.Errorf("%s is :one but returned %d rows", q.name, len(got))
		}
		if w, ok := want[q.name]; !ok {
			t.Errorf("%s has no expected answer in this test", q.name)
		} else if !slices.Equal(got, w) {
			t.Errorf("%s = %v, the Store says %v", q.name, got, w)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("queries.sql has %v, the test expects %d queries", seen, len(want))
	}
}

// firstColumn runs a query and returns the first column of every row as
// text.
func firstColumn(t *testing.T, db *sql.DB, q string, args []any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		dst := make([]any, len(cols))
		var first any
		dst[0] = &first
		for i := 1; i < len(dst); i++ {
			var skip any
			dst[i] = &skip
		}
		if err := rows.Scan(dst...); err != nil {
			t.Fatal(err)
		}
		switch v := first.(type) {
		case []byte:
			out = append(out, string(v))
		case time.Time:
			out = append(out, v.UTC().String())
		default:
			out = append(out, fmt.Sprint(v))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBindPlaceholders pins the rewrite the query test relies on.
func TestBindPlaceholders(t *testing.T) {
	text, args := bind(t, "a = sqlc.arg(x) AND b = sqlc.arg(y) OR c = sqlc.arg(x) LIMIT ?", map[string]any{"x": 1, "y": 2, "page_size": 3})
	want, wantArgs := "a = ? AND b = ? OR c = ? LIMIT ?", []any{1, 2, 1, 3}
	if recordstest.Engine() == recordstest.Postgres {
		want, wantArgs = "a = $1 AND b = $2 OR c = $1 LIMIT $3", []any{1, 2, 3}
	}
	if text != want || !slices.Equal(args, wantArgs) || strings.Contains(text, "sqlc") {
		t.Fatalf("bind = %q %v", text, args)
	}
}

// fileQueries are the queries that return whole fs_files rows: each must
// select every column of the table, so a column the Store adds (checksums,
// say) reaches sqlc's generated row type.
var fileQueries = []string{"GetFile", "ListFilesBySHA256", "ListLiveFiles", "ListLiveFilesAfter"}

// TestFileQueriesSelectEveryColumn pins that every whole-row query names
// each fs_files column the Store creates, and that the checksums an upload
// asked for read back through GetFile exactly as the Store stored them.
func TestFileQueriesSelectEveryColumn(t *testing.T) {
	ctx := t.Context()
	s, db, disk := newStore(t)
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}, Checksums: []filesystem.Checksum{{Name: "md5", New: md5.New}}}
	f, err := s.Upload(ctx, "t", disk.Name(), strings.NewReader("hello"), "a.txt", policy)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := describe(t, db, nil)
	var table []string
	for name := range cols[records.TableNameFiles] {
		table = append(table, name)
	}
	slices.Sort(table)
	file, err := sqlc.Queries(recordstest.Engine())
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"scope": "t", "id": f.ID, "sha256": f.SHA256, "page_size": 1, "after_created_at": f.CreatedAt, "after_id": f.ID}
	byName := map[string]query{}
	for _, q := range parse(t, file) {
		byName[q.name] = q
	}
	for _, name := range fileQueries {
		q, ok := byName[name]
		if !ok {
			t.Fatalf("%s is missing from the queries", name)
		}
		text, qargs := bind(t, q.sql, args)
		rows, err := db.Query(text, qargs...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := rows.Columns()
		_ = rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		if !slices.Equal(got, table) {
			t.Errorf("%s selects %v, fs_files has %v", name, got, table)
		}
	}
	text, qargs := bind(t, byName["GetFile"].sql, args)
	var stored, read sql.NullString
	if err := db.QueryRow("SELECT checksums FROM "+records.TableNameFiles+" WHERE id = "+placeholder(1), f.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(text, qargs...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	names, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("GetFile returned no row")
	}
	dst := make([]any, len(names))
	for i, n := range names {
		if n == "checksums" {
			dst[i] = &read
			continue
		}
		var skip any
		dst[i] = &skip
	}
	if err := rows.Scan(dst...); err != nil {
		t.Fatal(err)
	}
	if !stored.Valid || read != stored || !strings.Contains(read.String, md5Hex("hello")) {
		t.Fatalf("GetFile checksums %v, the Store stored %v", read, stored)
	}
}

// placeholder is the engine's n-th bind parameter.
func placeholder(n int) string {
	if recordstest.Engine() == recordstest.Postgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// md5Hex is the lowercase hex MD5 of s.
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
