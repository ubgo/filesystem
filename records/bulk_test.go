package records_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

func TestBulk(t *testing.T) {
	ctx := t.Context()
	h := &hooks{named: "legal"}
	e := newEnv(t, nil, records.WithPlugins(h))
	a, b, c := e.put(t, "t", "a.png"), e.put(t, "t", "b.png"), e.put(t, "t", "c.png")
	codes := func(rs []records.BulkResult) string {
		var out []string
		for _, r := range rs {
			out = append(out, string(filesystem.CodeOf(r.Err)))
		}
		return strings.Join(out, ",")
	}

	// Per-file results: an unknown ID fails alone, a duplicate counts once.
	rs, err := e.store.TrashMany(ctx, "t", []string{a.ID, "fil_nope", b.ID, a.ID})
	if err != nil || len(rs) != 3 || rs[0].ID != a.ID || codes(rs) != ",not_found," {
		t.Fatalf("TrashMany = %+v (%s), %v", rs, codes(rs), err)
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Trashed: true}); len(page.Files) != 2 {
		t.Fatalf("%d trashed", len(page.Files))
	}
	if rs, _ := e.store.RestoreMany(ctx, "t", []string{a.ID, b.ID}); codes(rs) != "," {
		t.Fatalf("RestoreMany = %s", codes(rs))
	}
	if rs, _ := e.store.SetVisibilityMany(ctx, "t", []string{a.ID, c.ID}, filesystem.VisibilityPublic); codes(rs) != "," {
		t.Fatalf("SetVisibilityMany = %s", codes(rs))
	}
	if f, _ := e.store.File(ctx, "t", c.ID); f.Visibility != filesystem.VisibilityPublic {
		t.Fatal("visibility not set")
	}
	// A plugin refusing one purge leaves the others purged.
	h.failPurge = errors.New("legal hold")
	rs, _ = e.store.PurgeMany(ctx, "t", []string{a.ID})
	if !errors.Is(rs[0].Err, h.failPurge) {
		t.Fatalf("held purge = %v", rs[0].Err)
	}
	h.failPurge = nil
	if rs, _ := e.store.PurgeMany(ctx, "t", []string{a.ID, b.ID}); codes(rs) != "," {
		t.Fatalf("PurgeMany = %s", codes(rs))
	}
	if ok, _ := e.disk.Exists(ctx, a.Key); ok {
		t.Fatal("a purged object survived")
	}

	// Refusals that change nothing.
	_, err = e.store.SetVisibilityMany(ctx, "t", []string{c.ID}, "secret")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.store.TrashMany(ctx, "t", make([]string, records.MaxBulk+1))
	wantCode(t, err, filesystem.CodeInvalidArgument)
	err = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		_, err := e.store.TrashMany(ctx, "t", []string{c.ID})
		return err
	})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if f, _ := e.store.File(ctx, "t", c.ID); f.DeletedAt != nil {
		t.Fatal("a refused bulk call trashed a file")
	}
	// A cancelled context reaches no file.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	rs, _ = e.store.TrashMany(cancelled, "t", []string{c.ID})
	if !errors.Is(rs[0].Err, context.Canceled) {
		t.Fatalf("cancelled = %v", rs[0].Err)
	}
	if rs, err := e.store.TrashMany(ctx, "t", nil); err != nil || len(rs) != 0 {
		t.Fatalf("no IDs = %v, %v", rs, err)
	}
}
