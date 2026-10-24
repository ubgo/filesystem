package records_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestUsagesOf lists where a file is used, newest first, page by page,
// and nothing of other files or scopes.
func TestUsagesOf(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "logo.png")
	other := e.put(t, "t", "other.png")
	var want []string
	for _, o := range []records.Owner{{Type: "post", ID: "p1"}, {Type: "post", ID: "p2"}, {Type: "user", ID: "u1"}} {
		a, err := e.store.Attach(ctx, "t", f.ID, o, "cover")
		if err != nil {
			t.Fatal(err)
		}
		want = append([]string{a.ID}, want...)
	}
	if _, err := e.store.Attach(ctx, "t", other.ID, records.Owner{Type: "post", ID: "p1"}, "cover"); err != nil {
		t.Fatal(err)
	}
	var got []string
	q := records.UsageQuery{Scope: "t", FileID: f.ID, Limit: 2}
	pages := 0
	for {
		page, err := e.store.UsagesOf(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, u := range page.Usages {
			if u.FileID != f.ID || u.Role != "cover" {
				t.Fatalf("usage of another file: %+v", u)
			}
			got = append(got, u.ID)
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if pages != 2 || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("usages %v in %d pages, want %v", got, pages, want)
	}
	if page, err := e.store.UsagesOf(ctx, records.UsageQuery{Scope: "u", FileID: f.ID}); err != nil || len(page.Usages) != 0 {
		t.Fatalf("another scope sees the usages: %+v, %v", page, err)
	}
	for _, bad := range []records.UsageQuery{{Scope: "t", FileID: f.ID, Limit: -1}, {Scope: "t", FileID: f.ID, Cursor: "!"}} {
		if _, err := e.store.UsagesOf(ctx, bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
