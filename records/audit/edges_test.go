package audit_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// TestAccessLogRefusesLongActor: a read whose actor is longer than the
// log keeps is refused, so a read is never logged under a cut name.
func TestAccessLogRefusesLongActor(t *testing.T) {
	long := strings.Repeat("a", audit.MaxActorBytes+1)
	e := newEnv(t, audit.WithAccessLog(nil), audit.WithActorFunc(func(context.Context) string { return long }))
	err := e.store.Served(t.Context(), records.Served{Kind: records.ServeLink, Scope: scope, FileID: "fil_1", Bytes: 1})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Served with a long actor = %v", err)
	}
	if n := e.count(t, audit.TableNameAudit); n != 0 {
		t.Fatalf("logged %d entries", n)
	}
}

// TestChangesRefusals: Changes refuses a limit outside [0, MaxPageSize]
// and a malformed cursor, and a scope without a chain has no changes yet
// (its cursor comes back unchanged).
func TestChangesRefusals(t *testing.T) {
	ctx := t.Context()
	e := chained(t)
	for _, limit := range []int{-1, audit.MaxPageSize + 1} {
		if _, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope, Limit: limit}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Changes(limit %d) = %v", limit, err)
		}
	}
	for _, cursor := range []string{"x", "-3"} {
		if _, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope, Cursor: cursor}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Changes(cursor %q) = %v", cursor, err)
		}
	}
	page, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: "nobody", Cursor: "7"})
	if err != nil || len(page.Entries) != 0 || page.NextCursor != "7" {
		t.Fatalf("Changes of a scope without a chain = %+v, %v", page, err)
	}
}

// TestChainlessScope: Verify and PruneScope of a scope that never logged
// anything check and delete nothing.
func TestChainlessScope(t *testing.T) {
	ctx := t.Context()
	e := chained(t)
	if n, err := e.p.Verify(ctx, e.store, "nobody"); err != nil || n != 0 {
		t.Fatalf("Verify of a scope without a chain = %d, %v", n, err)
	}
	if n, err := e.p.PruneScope(ctx, e.store, "nobody", e.now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("PruneScope of a scope without a chain = %d, %v", n, err)
	}
	mustVerify(t, e, 3)
}

// TestPruneRefusesABrokenChain: a prune does not move the anchor of a
// chain whose head row was rewritten, nor of one missing the entry that
// would become the anchor; both are ErrChainBroken and delete nothing.
func TestPruneRefusesABrokenChain(t *testing.T) {
	ctx := t.Context()
	e := chained(t)
	testdb.Exec(t, e.db, "UPDATE "+audit.TableNameChain+" SET "+audit.ColChainHead+" = 'forged'")
	if _, err := e.p.PruneScope(ctx, e.store, scope, e.now.Add(time.Hour)); !errors.Is(err, audit.ErrChainBroken) {
		t.Fatalf("PruneScope of an altered chain = %v", err)
	}
	if n := e.count(t, audit.TableNameAudit); n != 3 {
		t.Fatalf("the refused prune left %d of 3 entries", n)
	}

	e = chained(t)
	page := e.entries(t, audit.Query{Scope: scope})
	var third time.Time
	for _, en := range page.Entries {
		if en.Seq == 3 {
			third = en.At
		}
	}
	testdb.Exec(t, e.db, "DELETE FROM "+audit.TableNameAudit+" WHERE "+audit.ColSeq+" = 2")
	_, err := e.p.PruneScope(ctx, e.store, scope, third)
	if !errors.Is(err, audit.ErrChainBroken) || !strings.Contains(err.Error(), "entry 2 is missing") {
		t.Fatalf("PruneScope up to a missing entry = %v", err)
	}
	if n := e.count(t, audit.TableNameAudit); n != 2 {
		t.Fatalf("the refused prune left %d of 2 entries", n)
	}
}

// TestRacingFirstEntriesRetry: a first entry of the scope committing
// between link's update (no chain row yet) and its insert makes the insert
// a unique violation, reported as CodeUnavailable (retry), and the change
// it logs is rolled back with it. A SQLite trigger plays the other entry.
func TestRacingFirstEntriesRetry(t *testing.T) {
	testdb.OnlySQLite(t)
	e := newEnv(t, audit.WithHashChain(chainKey))
	testdb.Exec(t, e.db, "CREATE TRIGGER racer BEFORE INSERT ON "+audit.TableNameChain+" BEGIN "+
		"INSERT INTO "+audit.TableNameChain+" ("+strings.Join([]string{audit.ColChainScope, audit.ColChainSeq, audit.ColChainHead, audit.ColChainAnchorSeq,
		audit.ColChainAnchorHash, audit.ColChainSince, audit.ColChainAnchorAt, audit.ColChainMAC}, ", ")+") "+
		"VALUES (NEW."+audit.ColChainScope+", 1, 'h', 0, '', NEW."+audit.ColChainSince+", NEW."+audit.ColChainAnchorAt+", 'm'); END")
	if _, err := e.disk.WriteBytes(t.Context(), "a", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "a"})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("a racing first entry = %v, want CodeUnavailable", err)
	}
	if n := e.count(t, records.TableNameFiles); n != 0 {
		t.Fatalf("the change of the refused entry was kept: %d files", n)
	}
}

// TestChainFaultSweep fails every database step of the chain's
// operations in turn: each failure is the injected fault, returned, and
// the run after the sweep succeeds with the chain still verifiable.
func TestChainFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := chained(t)
	key := 0
	ops := []struct {
		name string
		run  func() error
	}{
		{"linked entry", func() error {
			key++
			name := "k" + strconv.Itoa(key)
			if _, err := e.disk.WriteBytes(ctx, name, []byte(name), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: name})
			return err
		}},
		{"first entry of a scope", func() error {
			key++
			name := "u" + strconv.Itoa(key)
			if _, err := e.disk.WriteBytes(ctx, name, []byte(name), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := e.store.CreateFile(ctx, records.NewFile{Scope: "u", Disk: "media", Key: name})
			return err
		}},
		{"verify", func() error { _, err := e.p.Verify(ctx, e.store, scope); return err }},
		{"verify a chainless scope", func() error { _, err := e.p.Verify(ctx, e.store, "nobody"); return err }},
		{"changes", func() error { _, err := e.p.Changes(ctx, e.store, audit.ChangeQuery{Scope: scope}); return err }},
		{"prune scope", func() error { _, err := e.p.PruneScope(ctx, e.store, scope, e.now); return err }},
		{"prune", func() error { _, err := e.p.Prune(ctx, e.store, e.now.Add(time.Hour)); return err }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
			}
		})
	}
	for _, sc := range []string{scope, "u"} {
		if _, err := e.p.Verify(ctx, e.store, sc); err != nil {
			t.Fatalf("Verify(%s) after the sweep = %v", sc, err)
		}
	}
}
