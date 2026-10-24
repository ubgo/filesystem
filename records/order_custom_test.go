package records_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// nameFold is a case-insensitive name order: LOWER(filename) as the
// database computes it, the cursor holding that value as it is.
func nameFold(dir sqlb.SortDirection) records.Ordering {
	return records.KeysetOrder(sqlb.Lower(records.FileFilename), dir,
		func(v string) (string, error) { return v, nil }, func(v string) string { return v })
}

// TestWithFileOrder pins R-C2: a Store's own order (a case-insensitive
// name sort) lists and pages in its key, cursors of another order are
// refused both ways, a parse that refuses a cursor fails the query, and
// the built-in name order is unchanged beside it.
func TestWithFileOrder(t *testing.T) {
	refuse := errors.New("no cursor")
	strict := records.KeysetOrder(sqlb.Lower(records.FileFilename), sqlb.SortAsc,
		func(string) (string, error) { return "", refuse }, func(v string) string { return v })
	e := newEnv(t, nil, records.WithFileOrder("name_ci", nameFold(sqlb.SortAsc)), records.WithFileOrder("name_ci_desc", nameFold(sqlb.SortDesc)),
		records.WithFileOrder("strict", strict))
	for _, name := range []string{"b.png", "A.png", "c.png", "B2.png"} {
		e.put(t, "t", name)
	}
	names := func(o records.FileOrder, limit int) []string {
		t.Helper()
		var out []string
		cursor := ""
		for range 10 {
			page, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: o, Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatalf("order %q: %v", o, err)
			}
			for _, f := range page.Files {
				out = append(out, f.Filename)
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		return out
	}
	folded := []string{"A.png", "b.png", "B2.png", "c.png"}
	for _, limit := range []int{0, 1, 3} {
		if got := names("name_ci", limit); !slices.Equal(got, folded) {
			t.Fatalf("name_ci limit %d: %v, want %v", limit, got, folded)
		}
		if got := names("name_ci_desc", limit); !slices.Equal(got, []string{"c.png", "B2.png", "b.png", "A.png"}) {
			t.Fatalf("name_ci_desc limit %d: %v", limit, got)
		}
	}
	// The built-in order still sorts in the database's collation.
	if got := names(records.OrderName, 1); engine() == records.EngineSQLite && !slices.Equal(got, []string{"A.png", "B2.png", "b.png", "c.png"}) {
		t.Fatalf("OrderName changed: %v", got)
	}
	custom, _ := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: "name_ci", Limit: 1})
	builtIn, _ := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: records.OrderName, Limit: 1})
	if _, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: records.OrderName, Cursor: custom.NextCursor}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a name_ci cursor under OrderName: %v", err)
	}
	if _, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: "name_ci", Cursor: builtIn.NextCursor}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an OrderName cursor under name_ci: %v", err)
	}
	first, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: "strict", Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("strict first page: %v %v", first, err)
	}
	if _, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: "strict", Cursor: first.NextCursor}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a cursor the parse refuses: %v", err)
	}
	// A Store without the option knows no such order.
	plain := newEnv(t, nil)
	if _, err := plain.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: "name_ci"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an order the Store never added: %v", err)
	}
}

// TestWithFileOrderRefusals: a bad name, a built-in one, a name added
// twice and an Ordering not from KeysetOrder fail New.
func TestWithFileOrderRefusals(t *testing.T) {
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	nilParse := records.KeysetOrder(records.FileFilename, sqlb.SortAsc, nil, func(v string) string { return v })
	cases := map[string][]records.Option{
		"bad name":   {records.WithFileOrder("Name CI", nameFold(sqlb.SortAsc))},
		"built in":   {records.WithFileOrder(records.OrderName, nameFold(sqlb.SortAsc))},
		"empty":      {records.WithFileOrder("", nameFold(sqlb.SortAsc))},
		"twice":      {records.WithFileOrder("ci", nameFold(sqlb.SortAsc)), records.WithFileOrder("ci", nameFold(sqlb.SortDesc))},
		"zero":       {records.WithFileOrder("ci", records.Ordering{})},
		"nil parse":  {records.WithFileOrder("ci", nilParse)},
		"separator":  {records.WithFileOrder("a|b", nameFold(sqlb.SortAsc))},
		"over 64":    {records.WithFileOrder(records.FileOrder(strings.Repeat("a", 65)), nameFold(sqlb.SortAsc))},
		"upper case": {records.WithFileOrder("CI", nameFold(sqlb.SortAsc))},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := records.New(db, testDialect(), mgr, opts...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
				t.Fatalf("New = %v", err)
			}
		})
	}
}
