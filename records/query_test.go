package records_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestAttachedTo(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	post, other := records.Owner{Type: "post", ID: "p1"}, records.Owner{Type: "post", ID: "p2"}
	a, b, c, d := e.put(t, "t", "a.png"), e.put(t, "t", "b.png"), e.put(t, "t", "c.png"), e.put(t, "t", "d.png")
	_, _ = e.store.Attach(ctx, "t", a.ID, post, "gallery")
	_, _ = e.store.Attach(ctx, "t", b.ID, post, "cover")
	_, _ = e.store.Attach(ctx, "t", c.ID, other, "gallery")
	_ = d
	ids := func(filters ...sqlb.Predicate) string {
		t.Helper()
		page, err := e.store.Files(ctx, records.FileQuery{Scope: "t", Filters: filters})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range page.Files {
			out = append(out, f.Key)
		}
		return strings.Join(out, ",")
	}
	gallery, err := e.store.AttachedTo("t", post, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	any, _ := e.store.AttachedTo("t", post, "")
	if got := ids(gallery); got != "a.png" {
		t.Fatalf("gallery = %s", got)
	}
	if got := ids(any); got != "b.png,a.png" {
		t.Fatalf("any role = %s", got)
	}
	// Combined with another condition.
	if got := ids(any, sqlb.Eq(records.FileKey, "b.png")); got != "b.png" {
		t.Fatalf("combined = %s", got)
	}
	// Another scope's attachments never match.
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "u", Filters: []sqlb.Predicate{any}}); len(page.Files) != 0 {
		t.Fatal("matched in another scope")
	}
	for _, c := range []struct {
		o    records.Owner
		role string
	}{{records.Owner{Type: "nope", ID: "x"}, ""}, {post, "Bad Role"}, {records.Owner{Type: "post"}, "gallery"}} {
		_, err := e.store.AttachedTo("t", c.o, c.role)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

func TestAttachmentsPage(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	post := records.Owner{Type: "post", ID: "p1"}
	var want []string
	for i := range 7 {
		f := e.put(t, "t", fmt.Sprintf("%d.png", i))
		if _, err := e.store.Attach(ctx, "t", f.ID, post, "gallery"); err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			_ = e.store.Trash(ctx, "t", f.ID) // left out, like Attachments
			continue
		}
		want = append(want, f.Key)
	}
	var got []string
	cursor := ""
	pages := 0
	for {
		page, err := e.store.AttachmentsPage(ctx, records.AttachmentQuery{Scope: "t", Owner: post, Role: "gallery", Limit: 4, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, af := range page.Files {
			got = append(got, af.File.Key)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(want, ",") || pages != 2 {
		t.Fatalf("pages %d: %v, want %v", pages, got, want)
	}
	all, _ := e.store.Attachments(ctx, "t", post, "gallery")
	if len(all) != len(want) {
		t.Fatalf("Attachments = %d", len(all))
	}
	// The default page holds them all.
	if page, err := e.store.AttachmentsPage(ctx, records.AttachmentQuery{Scope: "t", Owner: post, Role: "gallery"}); err != nil || len(page.Files) != len(want) || page.NextCursor != "" {
		t.Fatalf("default page = %d, %q, %v", len(page.Files), page.NextCursor, err)
	}
	for _, aq := range []records.AttachmentQuery{
		{Scope: "t", Owner: post, Role: "gallery", Cursor: "!!"},
		{Scope: "t", Owner: post, Role: "gallery", Cursor: "eA"},   // base64 of "x": no separator
		{Scope: "t", Owner: post, Role: "gallery", Cursor: "YXx4"}, // "a|x": not a position
		{Scope: "t", Owner: post, Role: "gallery", Limit: records.MaxPageSize + 1},
		{Scope: "t", Owner: records.Owner{Type: "nope", ID: "x"}, Role: "gallery"},
	} {
		_, err := e.store.AttachmentsPage(ctx, aq)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	exec(t, e.db, "DROP TABLE "+records.TableNameAttachments)
	if _, err := e.store.AttachmentsPage(ctx, records.AttachmentQuery{Scope: "t", Owner: post, Role: "gallery"}); err == nil {
		t.Fatal("a failed query was ignored")
	}
}

func TestFacets(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	for _, k := range []string{"a.png", "b.png", "c.png"} {
		e.put(t, "t", k)
	}
	if _, err := e.disk.WriteBytes(ctx, "d.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	txt, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "d.txt", ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	pub := e.put(t, "t", "e.png")
	_ = e.store.SetVisibility(ctx, "t", pub.ID, filesystem.VisibilityPublic)
	trashed := e.put(t, "t", "f.png")
	_ = e.store.Trash(ctx, "t", trashed.ID)
	e.put(t, "u", "g.png")

	render := func(fc []records.FacetCount) string {
		var out []string
		for _, c := range fc {
			out = append(out, fmt.Sprintf("%s=%d/%d", c.Value, c.Files, c.Bytes))
		}
		return strings.Join(out, " ")
	}
	png := int64(len(pngData))
	got, err := e.store.Facets(ctx, records.FileQuery{Scope: "t"}, records.FacetContentType)
	if want := fmt.Sprintf("image/png=4/%d text/plain=1/5", 4*png); err != nil || render(got) != want {
		t.Fatalf("content type = %s, %v; want %s", render(got), err, want)
	}
	got, _ = e.store.Facets(ctx, records.FileQuery{Scope: "t"}, records.FacetVisibility)
	if want := fmt.Sprintf("private=4/%d public=1/%d", 3*png+5, png); render(got) != want {
		t.Fatalf("visibility = %s; want %s", render(got), want)
	}
	got, _ = e.store.Facets(ctx, records.FileQuery{Scope: "t", Trashed: true}, records.FacetContentType)
	if render(got) != fmt.Sprintf("image/png=1/%d", png) {
		t.Fatalf("trashed = %s", render(got))
	}
	got, _ = e.store.Facets(ctx, records.FileQuery{Scope: "t", Filters: []sqlb.Predicate{sqlb.Eq(records.FileID, txt.ID)}}, records.FacetContentType)
	if render(got) != "text/plain=1/5" {
		t.Fatalf("filtered = %s", render(got))
	}
	if got, err := e.store.Facets(ctx, records.FileQuery{Scope: "none"}, records.FacetContentType); err != nil || len(got) != 0 {
		t.Fatalf("empty scope = %v, %v", got, err)
	}
	for _, c := range []struct {
		fq records.FileQuery
		f  records.Facet
	}{
		{records.FileQuery{Scope: "t"}, "size"},
		{records.FileQuery{Scope: "t", Limit: 1}, records.FacetVisibility},
		{records.FileQuery{Scope: "t", Cursor: "x"}, records.FacetVisibility},
	} {
		_, err := e.store.Facets(ctx, c.fq, c.f)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	if len(records.FacetValues) != 2 {
		t.Fatal(records.FacetValues)
	}
	exec(t, e.db, "DROP TABLE "+records.TableNameFiles)
	if _, err := e.store.Facets(ctx, records.FileQuery{Scope: "t"}, records.FacetVisibility); err == nil {
		t.Fatal("a failed query was ignored")
	}
}
