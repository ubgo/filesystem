package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records/acl"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"user:alice               read  nda.txt  false",
		"user:alice+group:legal   read  nda.txt  true",
		"user:alice+group:legal   write nda.txt  true",
		"user:alice+group:legal   admin nda.txt  false",
		"user:bob                 read  memo.txt true",
		"user:bob                 write memo.txt false",
		"user:bob                 read  nda.txt  false",
		"shared with on Contracts: group:legal write; shared with bob: 1 grant(s)",
		"after revoke, bob reads memo.txt: false",
		"GET /files/{nda} as user:alice: 302", "GET /files/{nda} as user:bob: 403",
		"user:carol view    spec.txt true",
		"user:carol comment spec.txt true",
		"user:carol edit    spec.txt false",
		"user:dave  dropbox spec.txt true",
		"user:dave  view    spec.txt false",
		"grant on a missing project: not_found",
		"check for a level the model lacks: invalid_argument",
		"GET /files/{spec} as user:carol before expiry: 302",
		"after 73h0m0s, carol views spec.txt: false; pruned 1 expired grant(s)",
		"GET /files/{spec} as user:carol after expiry: 403",
		"deny: user:dana reads budget.txt: true", "deny: user:contractor reads roadmap.txt: true",
		"deny: user:contractor reads budget.txt: false", "deny: a deny without WithDeny: invalid_argument",
		"deny: stored denies still refuse without WithDeny: true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestFolderGrantNeedsHierarchy pins that a plugin without WithHierarchy
// refuses folder grants instead of storing grants that never match.
func TestFolderGrantNeedsHierarchy(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, _, _, err := newStore(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	flat := acl.New()
	if _, err := flat.Grant(t.Context(), s, tenant, acl.Grant{Subject: bob, Kind: acl.OnFolder, Resource: "fld_x", Level: acl.Read}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("folder grant without a hierarchy = %v, want invalid_argument", err)
	}
}

// TestCustomLevelsFailClosed pins that a level model that includes an
// undefined level makes the plugin refuse every check rather than fall
// back to the default ladder.
func TestCustomLevelsFailClosed(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	bad := acl.New(acl.WithLevels(acl.LevelDef{Level: edit, Includes: []acl.Level{view}}))
	s, err := storeWith(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	f, err := record(t.Context(), s, "a.txt", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Can(t.Context(), s, tenant, []string{carol}, f.ID, acl.Read); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Can on an invalid model = %v, want invalid_argument", err)
	}
}

// TestDenyBeatsAllow pins that the Finance deny is what refuses the
// contractor: their team grant and a file grant on the budget both allow,
// the deny wins over both, revoking it lets them in, and a deny past its
// ExpiresAt refuses nothing.
func TestDenyBeatsAllow(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, fp, ap, err := newDenyStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	teamFolder, err := fp.Create(ctx, s, tenant, "", "Team")
	if err != nil {
		t.Fatal(err)
	}
	finance, err := fp.Create(ctx, s, tenant, teamFolder.ID, "Finance")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := record(ctx, s, "budget.txt", "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := fp.Put(ctx, s, tenant, budget.ID, finance.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := everyoneButContractor(ctx, s, ap, teamFolder.ID, finance.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: contractor, Kind: acl.OnFile, Resource: budget.ID, Level: acl.Write}); err != nil {
		t.Fatal(err)
	}
	who := []string{contractor, team}
	check := func(p *acl.Plugin, want bool, label string) {
		t.Helper()
		ok, err := p.Can(ctx, s, tenant, who, budget.ID, acl.Read)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("%s: contractor reads budget = %t, want %t", label, ok, want)
		}
	}
	check(ap, false, "under the deny")
	later := acl.New(acl.WithHierarchy(fp), acl.WithDeny(), acl.WithClock(func() time.Time { return now.Add(auditWindow + time.Hour) }))
	check(later, true, "after the deny expired")
	if err := ap.Revoke(ctx, s, tenant, acl.OnFolder, finance.ID, contractor); err != nil {
		t.Fatal(err)
	}
	check(ap, true, "after revoking the deny")
}
