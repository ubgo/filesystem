package acl_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
)

// TestPurgeScopeRemovesGrants: purging a scope deletes every grant of that
// scope, folder grants included (a file grant also goes with its file's
// own purge, so only a folder grant proves the scope-wide delete), and
// leaves another scope's grants alone.
func TestPurgeScopeRemovesGrants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, withFolders)
	f := e.file(t, "a.txt")
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"t", "other"} {
		dir, err := e.folders.Create(ctx, e.store, scope, folders.RootID, "Team")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.acl.Grant(ctx, e.store, scope, acl.Grant{Subject: "u", Kind: acl.OnFolder, Resource: dir.ID, Level: acl.Read}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.PurgeScope(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if list, err := e.acl.GrantsOf(ctx, e.store, "t", "u"); err != nil || len(list) != 0 {
		t.Errorf("grants of the purged scope = %+v, %v", list, err)
	}
	if list, err := e.acl.GrantsOf(ctx, e.store, "other", "u"); err != nil || len(list) != 1 {
		t.Errorf("grants of the other scope = %+v, %v", list, err)
	}
}

// TestWithClock: a grant's CreatedAt comes from the injected clock, in UTC.
func TestWithClock(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	e := newEnv(t, func(*folders.Plugin) acl.Option { return acl.WithClock(func() time.Time { return at }) })
	f := e.file(t, "a.txt")
	g, err := e.acl.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read})
	if err != nil || !g.CreatedAt.Equal(at) || g.CreatedAt.Location() != time.UTC {
		t.Fatalf("Grant = %+v, %v; want CreatedAt %v in UTC", g, err, at)
	}
}
