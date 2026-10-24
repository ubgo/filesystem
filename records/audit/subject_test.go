package audit_test

import (
	"context"
	"database/sql"
	"maps"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// notes is a plugin that reports its own changes with Store.Emit, the
// way folders, tags and share do.
type notes struct{}

func (notes) Name() string { return "notes" }

const notePinned records.EventKind = "notes.pinned"

func (n notes) pin(ctx context.Context, s *records.Store, scope, fileID, subject string, detail map[string]string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return s.Emit(ctx, n, records.Event{Kind: notePinned, Scope: scope, FileID: fileID, Subject: subject, Detail: detail})
	})
}

// TestPluginEvents: a plugin's event is logged with its Subject and
// Detail, which read back as written; the Store's own events have
// neither.
func TestPluginEvents(t *testing.T) {
	ctx := audit.WithActor(t.Context(), "user:alice")
	e := newEnvWith(t, 0, []records.Plugin{notes{}})
	f := e.file(ctx, t, scope, "a.png")
	detail := map[string]string{"board": "brd_1", "note": `quotes " and, commas`}
	if err := (notes{}).pin(ctx, e.store, scope, f.ID, "pin_1", detail); err != nil {
		t.Fatal(err)
	}
	if err := (notes{}).pin(ctx, e.store, scope, "", "pin_2", nil); err != nil {
		t.Fatal(err)
	}
	page := e.entries(t, audit.Query{Scope: scope})
	if got := render(page); got != "user:alice notes.pinned; user:alice notes.pinned; user:alice file.created" {
		t.Fatalf("log = %s", got)
	}
	bare, pinned, created := page.Entries[0], page.Entries[1], page.Entries[2]
	if pinned.Subject != "pin_1" || pinned.FileID != f.ID || !maps.Equal(pinned.Detail, detail) {
		t.Fatalf("plugin entry = %+v", pinned)
	}
	if bare.Subject != "pin_2" || bare.FileID != "" || bare.Detail != nil {
		t.Fatalf("plugin entry without detail = %+v", bare)
	}
	if created.Subject != "" || created.Detail != nil {
		t.Fatalf("a Store event gained a subject or detail: %+v", created)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope, FileID: f.ID})); got != "user:alice notes.pinned; user:alice file.created" {
		t.Fatalf("history of the file = %s", got)
	}
}

// TestUpgradeAddsSubjectDetail migrates a log that predates the subject
// and detail columns and holds entries: they read back with neither, and
// new entries keep both.
func TestUpgradeAddsSubjectDetail(t *testing.T) {
	ctx := t.Context()
	e := newEnvWith(t, 0, []records.Plugin{notes{}})
	f := e.file(ctx, t, scope, "a.png")
	for _, col := range []string{audit.ColSubject, audit.ColDetail} {
		testdb.Exec(t, e.db, "ALTER TABLE "+audit.TableNameAudit+" DROP COLUMN "+col)
	}
	testdb.Exec(t, e.db, "DELETE FROM "+records.TableNameMigrations+" WHERE name = '0002_audit_subject_detail'")
	done, err := e.store.Migrate(ctx)
	if err != nil || strings.Join(done, ",") != "audit/0002_audit_subject_detail" {
		t.Fatalf("upgrade = %v, %v", done, err)
	}
	if err := (notes{}).pin(ctx, e.store, scope, f.ID, "pin_1", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	page := e.entries(t, audit.Query{Scope: scope})
	if len(page.Entries) != 2 || page.Entries[0].Detail["k"] != "v" || page.Entries[0].Subject != "pin_1" ||
		page.Entries[1].Subject != "" || page.Entries[1].Detail != nil {
		t.Fatalf("after the upgrade: %+v", page.Entries)
	}
}

// TestSubjectDetailRunsTwice runs the 0002 statements again after they
// applied, as after a crash before the ledger write. SQLite applies a
// migration and its ledger row in one transaction, so it never needs to.
func TestSubjectDetailRunsTwice(t *testing.T) {
	if testdb.Engine() == records.EngineSQLite {
		t.Skip("SQLite applies a migration and its ledger row in one transaction")
	}
	e := newEnv(t)
	ms, err := e.p.Migrations(testdb.Engine())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range ms[1].Statements {
		testdb.Exec(t, e.db, st)
	}
}

// TestMalformedDetail: a detail that is not a JSON object (a hand-edited
// row) reads back as nil instead of failing the page.
func TestMalformedDetail(t *testing.T) {
	ctx := t.Context()
	e := newEnvWith(t, 0, []records.Plugin{notes{}})
	if err := (notes{}).pin(ctx, e.store, scope, "", "pin_1", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	testdb.Exec(t, e.db, "UPDATE "+audit.TableNameAudit+" SET "+audit.ColDetail+" = 'not json'")
	page := e.entries(t, audit.Query{Scope: scope})
	if len(page.Entries) != 1 || page.Entries[0].Detail != nil || page.Entries[0].Subject != "pin_1" {
		t.Fatalf("an entry with a malformed detail = %+v", page.Entries)
	}
}
