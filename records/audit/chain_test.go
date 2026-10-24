package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

var chainKey = []byte("kept-outside-the-database")

// chained is an env with the hash chain and three files in scope.
func chained(t *testing.T, opts ...audit.Option) *env {
	t.Helper()
	e := newEnv(t, append([]audit.Option{audit.WithHashChain(chainKey)}, opts...)...)
	for _, name := range []string{"a", "b", "c"} {
		e.file(t.Context(), t, scope, name)
	}
	return e
}

func mustVerify(t *testing.T, e *env, want int64) {
	t.Helper()
	if n, err := e.p.Verify(t.Context(), e.store, scope); err != nil || n != want {
		t.Fatalf("Verify = %d, %v; want %d entries", n, err, want)
	}
}

func mustBreak(t *testing.T, p *audit.Plugin, e *env, why string) {
	t.Helper()
	_, err := p.Verify(t.Context(), e.store, scope)
	if !errors.Is(err, audit.ErrChainBroken) || filesystem.CodeOf(err) != filesystem.CodeConflict || !strings.Contains(err.Error(), why) {
		t.Fatalf("Verify = %v; want a broken chain mentioning %q", err, why)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	for _, tc := range []struct{ name, sql, why string }{
		{"edited", "UPDATE fs_audit SET actor = 'someone-else' WHERE seq = 2", "entry 2"},
		{"deleted", "DELETE FROM fs_audit WHERE seq = 2", "expected entry 2"},
		{"truncated", "DELETE FROM fs_audit WHERE seq = 3", "head is entry 3"},
		{"reordered", "UPDATE fs_audit SET seq = CASE seq WHEN 1 THEN 2 WHEN 2 THEN 1 ELSE seq END", "does not follow"},
		{"inserted", "INSERT INTO fs_audit (id, scope, at, actor, kind, file_id, attachment_id, owner_type, owner_id, role, subject, detail) VALUES ('aud_x', 't', '2030-01-01 00:00:00', '', 'file.purged', '', '', '', '', '', '', '')", "unchained"},
		{"head deleted", "DELETE FROM fs_audit_chain", "no chain head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := chained(t)
			mustVerify(t, e, 3)
			testdb.Exec(t, e.db, tc.sql)
			mustBreak(t, e.p, e, tc.why)
		})
	}
}

// The key is part of every hash: a chain rebuilt without it, or checked
// with another, does not verify.
func TestVerifyNeedsTheKey(t *testing.T) {
	e := chained(t)
	mustBreak(t, audit.New(audit.WithHashChain([]byte("another key"))), e, "altered")
}

func TestPruneKeepsTheChainVerifiable(t *testing.T) {
	e := chained(t) // three entries, one second apart
	en, _ := e.p.Entries(t.Context(), e.store, audit.Query{Scope: scope})
	cutoff := en.Entries[0].At // the newest: keep it, prune the two before
	n, err := e.p.Prune(t.Context(), e.store, cutoff)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	mustVerify(t, e, 1)
	// The anchor catches an old entry deleted by anything but Prune.
	e.file(t.Context(), t, scope, "d")
	testdb.Exec(t, e.db, "DELETE FROM fs_audit WHERE seq = 3")
	mustBreak(t, e.p, e, "expected entry 3")
}

func TestRetentionFloor(t *testing.T) {
	e := chained(t, audit.WithRetentionFloor(24*time.Hour))
	for _, prune := range []func(time.Time) (int64, error){
		func(c time.Time) (int64, error) { return e.p.Prune(t.Context(), e.store, c) },
		func(c time.Time) (int64, error) { return e.p.PruneScope(t.Context(), e.store, scope, c) },
	} {
		if n, err := prune(e.now.Add(time.Hour)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || n != 0 {
			t.Fatalf("prune inside the floor = %d, %v", n, err)
		}
	}
	mustVerify(t, e, 3)
	if _, err := e.p.Prune(t.Context(), e.store, e.now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("prune past the floor: %v", err)
	}
}

// failOnce fails the first change it hears of, after audit wrote its
// entry, so the change and its entry roll back.
type failOnce struct{ armed bool }

func (f *failOnce) Name() string { return "fail_once" }

func (f *failOnce) OnEvent(context.Context, *records.Store, records.Event) error {
	if f.armed {
		f.armed = false
		return errors.New("refused")
	}
	return nil
}

func TestChangesFeed(t *testing.T) {
	fail := &failOnce{}
	e := newEnvWith(t, time.Second, []records.Plugin{fail}, audit.WithHashChain(nil), audit.WithAccessLog(nil))
	ctx := t.Context()
	a := e.file(ctx, t, scope, "a")
	fail.armed = true
	if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: scope + "/a"}); err == nil {
		t.Fatal("the failing change committed")
	}
	if err := e.store.Served(ctx, records.Served{Kind: records.ServeLink, Scope: scope, FileID: a.ID, Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(ctx, scope, a.ID); err != nil {
		t.Fatal(err)
	}

	page, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope, Limit: 1})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Kind != records.EventFileCreated || page.Entries[0].Seq != 1 {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	// A new Plugin is a restarted process: the cursor still means the same.
	restarted := audit.New(audit.WithHashChain(nil))
	next, err := restarted.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope, Cursor: page.NextCursor})
	if err != nil || len(next.Entries) != 1 || next.Entries[0].Kind != records.EventFileTrashed {
		t.Fatalf("after cursor %q = %+v, %v", page.NextCursor, next, err)
	}
	// The rolled-back change gave its number back and the read is not a
	// change: 1 created, 2 read, 3 trashed.
	if next.Entries[0].Seq != 3 {
		t.Fatalf("trashed is entry %d, want 3: numbers must follow commits without gaps", next.Entries[0].Seq)
	}
	// Nothing new: the cursor stays.
	idle, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope, Cursor: next.NextCursor})
	if err != nil || len(idle.Entries) != 0 || idle.NextCursor != next.NextCursor {
		t.Fatalf("idle = %+v, %v", idle, err)
	}
	mustVerify(t, e, 3)

	// A cursor into pruned entries is refused, never silently skipped.
	if _, err := e.p.PruneScope(ctx, e.store, scope, e.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope, Cursor: "1"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("pruned cursor = %v", err)
	}
	if _, err := audit.New().Changes(ctx, e.store, audit.ChangeQuery{Scope: scope}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("Changes without the chain = %v", err)
	}
}

func TestAccessLog(t *testing.T) {
	read := records.Served{Kind: records.ServeShare, Scope: scope, FileID: "fil_1", Subject: "shr_1", Bytes: 42}
	for _, tc := range []struct {
		name string
		opts []audit.Option
		want string
	}{
		{"off by default", nil, ""},
		{"all", []audit.Option{audit.WithAccessLog(nil)}, "access.share 42 shr_1"},
		{"rate 1", []audit.Option{audit.WithAccessLog(audit.SampleRate(1))}, "access.share 42 shr_1"},
		{"rate 0", []audit.Option{audit.WithAccessLog(audit.SampleRate(0))}, ""},
		{"own sampler", []audit.Option{audit.WithAccessLog(func(sv records.Served) bool { return sv.Bytes > 100 })}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts...)
			if err := e.store.Served(audit.WithActor(t.Context(), "u1"), read); err != nil {
				t.Fatal(err)
			}
			page, _ := e.p.Entries(t.Context(), e.store, audit.Query{Scope: scope})
			var got string
			for _, en := range page.Entries {
				got = string(en.Kind) + " " + en.Detail[audit.DetailBytes] + " " + en.Subject
				if en.Actor != "u1" || en.FileID != "fil_1" {
					t.Fatalf("entry = %+v", en)
				}
			}
			if got != tc.want {
				t.Fatalf("logged %q, want %q", got, tc.want)
			}
		})
	}
}

// TestChainHeadIsAuthenticated: the reviewer's two attacks on the chain
// row. Cutting off the newest entry and pointing the head at the one
// before, or deleting the oldest entries and faking a prune anchor, leaves
// every remaining entry consistent; only the row's MAC catches them. A
// write on top of a rewritten row is refused, so it cannot seal the
// forgery.
func TestChainHeadIsAuthenticated(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"tail cut", "DELETE FROM fs_audit WHERE seq = 3; UPDATE fs_audit_chain SET seq = 2, head = (SELECT hash FROM fs_audit WHERE seq = 2)"},
		{"faked prune", "UPDATE fs_audit_chain SET anchor_seq = 2, anchor_hash = (SELECT hash FROM fs_audit WHERE seq = 2); DELETE FROM fs_audit WHERE seq <= 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := chained(t)
			for stmt := range strings.SplitSeq(tc.sql, "; ") {
				testdb.Exec(t, e.db, stmt)
			}
			mustBreak(t, e.p, e, "chain head was altered")
			if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: scope + "/a"}); !errors.Is(err, audit.ErrChainBroken) {
				t.Fatalf("a write on the rewritten chain = %v", err)
			}
		})
	}
}

// TestVerifyRefusesAPruneInsideTheFloor: a plugin holding the key but no
// floor prunes recent entries; the deployment's plugin, whose floor
// forbids that, refuses the chain.
func TestVerifyRefusesAPruneInsideTheFloor(t *testing.T) {
	e := chained(t)
	if _, err := e.p.Prune(t.Context(), e.store, e.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, e, 0)
	floored := audit.New(audit.WithHashChain(chainKey), audit.WithRetentionFloor(24*time.Hour))
	mustBreak(t, floored, e, "inside the retention floor")
}
