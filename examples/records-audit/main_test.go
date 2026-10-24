package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
)

// testStore returns a migrated Store with au installed over a fresh SQLite file.
func testStore(t *testing.T, au *audit.Plugin) *records.Store {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	s, err := newStore(t.Context(), db, disks, au)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"history of the purged file (12 entries", "activity of user:alice: 13 entries in 5 pages",
		"activity after the rollback: 13 entries", "long actor: invalid_argument", "pruned 14 entries",
		"change feed: 1:file.created, 2:file.replaced, 4:file.trashed (reads left out)",
		"next poll from the stored cursor: 5:file.restored",
		"verify: 5 entries, err=<nil>",
		"after an edit with SQL: conflict, chain broken true",
		`rotated chain: 1:file.created key="" source="" "" ip=""`,
		`rotated chain: 2:file.replaced key="2026-10" source="ios" "app/3.2" ip=""`,
		"verify across the rotation: 2 entries, err=<nil>",
		"file.moved               actor=user:alice owner=/ role= from=media to=archive",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestEveryEventKindIsLogged(t *testing.T) {
	au := audit.New()
	s := testStore(t, au)
	if _, err := everyChange(t.Context(), s, actorAlice); err != nil {
		t.Fatal(err)
	}
	feed, _, err := activity(t.Context(), s, au, actorAlice)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []records.EventKind
	for _, e := range feed {
		kinds = append(kinds, e.Kind)
	}
	for _, k := range records.EventKindValues {
		if !slices.Contains(kinds, k) {
			t.Errorf("no %s entry in %v", k, kinds)
		}
	}
}

func TestActorFromOwnKey(t *testing.T) {
	au := newPluginWithOwnKey()
	s := testStore(t, au)
	ctx := context.WithValue(t.Context(), sessionUserKey{}, "bob")
	if _, err := s.Upload(ctx, tenant, diskName, strings.NewReader("x"), "x.txt", textPolicy); err != nil {
		t.Fatal(err)
	}
	page, err := au.Entries(t.Context(), s, audit.Query{Scope: tenant, Actor: "user:bob"})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("entries of user:bob = %+v, %v", page.Entries, err)
	}
}

func TestPruneKeepsRecentEntries(t *testing.T) {
	au := audit.New()
	s := testStore(t, au)
	if _, err := everyChange(t.Context(), s, actorAlice); err != nil {
		t.Fatal(err)
	}
	n, err := prune(t.Context(), s, au, time.Now())
	if err != nil || n != 0 {
		t.Fatalf("prune of recent entries = %d, %v", n, err)
	}
}

func TestBadLimitAndCursor(t *testing.T) {
	au := audit.New()
	s := testStore(t, au)
	for _, q := range []audit.Query{{Scope: tenant, Limit: audit.MaxPageSize + 1}, {Scope: tenant, Cursor: "x"}} {
		if _, err := au.Entries(t.Context(), s, q); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Entries(%+v) = %v, want invalid_argument", q, err)
		}
	}
}

// TestRotationNeedsTheOldKey pins that the keyring is really used to
// verify: a ring that dropped the pre-rotation key ("") cannot verify the
// entry chained under it, and Verify fails rather than skipping it.
func TestRotationNeedsTheOldKey(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "rotate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	before, err := newStore(ctx, db, disks, audit.New(audit.WithHashChain(oldChainKey)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.Upload(ctx, "rotating", diskName, strings.NewReader("v1"), "a.txt", feedPolicy); err != nil {
		t.Fatal(err)
	}
	newOnly := audit.New(audit.WithHashChainKeyring(audit.StaticKeyring{CurrentID: rotationKeyID, Keys: map[string][]byte{rotationKeyID: newChainKey}}))
	s, err := newStore(ctx, db, disks, newOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newOnly.Verify(ctx, s, "rotating"); err == nil {
		t.Fatal("Verify without the old key succeeded")
	}
	rotated := newRotatedPlugin()
	s, err = newStore(ctx, db, disks, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rotated.Verify(ctx, s, "rotating"); err != nil || n != 1 {
		t.Fatalf("Verify with the full ring = %d, %v", n, err)
	}
}
