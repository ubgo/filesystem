package versions_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records/versions"
)

// TestTrimScope: one call applies the age policy to every file of the
// scope with versions, across more than one page, leaves other scopes
// alone, and a file whose policy fails does not stop the rest.
func TestTrimScope(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var broken string
	e := policyEnv(t, &now, versions.WithKeepFunc(func(_ context.Context, req versions.KeepRequest) (versions.KeepPolicy, error) {
		if req.File.ID == broken {
			return versions.KeepPolicy{}, errPlan
		}
		return versions.KeepPolicy{Count: 10, MaxAge: time.Hour}, nil
	}))
	files := versions.TrimPageSize + 3
	ids := make([]string, files)
	for i := range files {
		f := e.upload(t, "one")
		e.replace(t, f.ID, "two")
		ids[i] = f.ID
	}
	other, err := e.store.Upload(ctx, "other", "media", strings.NewReader("one"), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Replace(ctx, "other", other.ID, strings.NewReader("two"), "", anything); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	broken = ids[7]
	n, err := e.p.TrimScope(ctx, e.store, scope)
	if n != files-1 || !errors.Is(err, errPlan) || !strings.Contains(err.Error(), broken) {
		t.Fatalf("TrimScope = %d, %v; want %d dropped and the broken file named", n, err, files-1)
	}
	for _, id := range []string{ids[0], ids[versions.TrimPageSize], ids[files-1]} {
		if got := e.history(t, id); got != "" {
			t.Fatalf("file %s kept %s", id, got)
		}
	}
	if got := e.history(t, broken); got != "1:one" {
		t.Fatalf("the failing file's history = %s", got)
	}
	list, err := e.p.Versions(ctx, e.store, "other", other.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("another scope was trimmed: %v, %v", list, err)
	}
	broken = ""
	if n, err := e.p.TrimScope(ctx, e.store, scope); n != 1 || err != nil {
		t.Fatalf("second run = %d, %v", n, err)
	}
	if n, err := e.p.TrimScope(ctx, e.store, scope); n != 0 || err != nil {
		t.Fatalf("third run = %d, %v", n, err)
	}
}
