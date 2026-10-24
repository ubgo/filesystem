package audit_test

import (
	"context"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

type requestIDKey struct{}

// requestDetail is the application's WithEntryDetail hook: the request ID
// from ctx, plus a "board" key that collides with the notes plugin's own.
func requestDetail(ctx context.Context, e records.Event) map[string]string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	if id == "" {
		return nil
	}
	return map[string]string{"request_id": id, "kind_seen": string(e.Kind), "board": "forged"}
}

// TestEntryDetail: the hook's fields land in every entry's Detail, the
// event's own keys win a collision, a nil map adds nothing, and with the
// chain on the added fields are hashed.
func TestEntryDetail(t *testing.T) {
	e := newEnvWith(t, 0, []records.Plugin{notes{}}, audit.WithHashChain(chainKey), audit.WithEntryDetail(requestDetail))
	ctx := context.WithValue(t.Context(), requestIDKey{}, "req_42")
	f := e.file(ctx, t, scope, "a.png")
	if err := (notes{}).pin(ctx, e.store, scope, f.ID, "pin_1", map[string]string{"board": "brd_1"}); err != nil {
		t.Fatal(err)
	}
	e.file(t.Context(), t, scope, "b.png")
	page := e.entries(t, audit.Query{Scope: scope})
	bare, pinned, created := page.Entries[0], page.Entries[1], page.Entries[2]
	if created.Detail["request_id"] != "req_42" || created.Detail["kind_seen"] != string(records.EventFileCreated) {
		t.Fatalf("store event detail = %v", created.Detail)
	}
	if pinned.Detail["board"] != "brd_1" || pinned.Detail["request_id"] != "req_42" {
		t.Fatalf("plugin event detail = %v", pinned.Detail)
	}
	if bare.Detail != nil {
		t.Fatalf("a nil hook map added detail: %v", bare.Detail)
	}
	mustVerify(t, e, 3)
	testdb.Exec(t, e.db, `UPDATE fs_audit SET detail = REPLACE(detail, 'req_42', 'req_43') WHERE seq = 1`)
	mustBreak(t, e.p, e, "entry 1")
}
