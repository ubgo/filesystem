package acl_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/sqlb"
)

// denyEnv is an env with folders and WithDeny, a team folder holding a
// subfolder holding one file.
type denyEnv struct {
	*env
	team, sub folders.Folder
	file      records.File
}

func newDenyEnv(t *testing.T, opts ...acl.Option) *denyEnv {
	t.Helper()
	ctx := t.Context()
	e := newEnv(t, func(fp *folders.Plugin) acl.Option {
		return func(p *acl.Plugin) {
			acl.WithHierarchy(fp)(p)
			acl.WithDeny()(p)
			for _, o := range opts {
				o(p)
			}
		}
	})
	d := &denyEnv{env: e, file: e.file(t, "plan.txt")}
	var err error
	if d.team, err = e.folders.Create(ctx, e.store, "t", folders.RootID, "team"); err != nil {
		t.Fatal(err)
	}
	if d.sub, err = e.folders.Create(ctx, e.store, "t", d.team.ID, "q4"); err != nil {
		t.Fatal(err)
	}
	if err := e.folders.Put(ctx, e.store, "t", d.file.ID, d.sub.ID); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDenyBeatsInheritedAllow: everyone in the team folder may write,
// except the contractor, denied on the subfolder; the deny beats the
// allow inherited from above, for the file and for the folder check.
func TestDenyBeatsInheritedAllow(t *testing.T) {
	d := newDenyEnv(t)
	d.grant(t, acl.Grant{Subject: "group:team", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Write})
	d.grant(t, acl.Grant{Subject: "user:contractor", Kind: acl.OnFolder, Resource: d.sub.ID, Level: acl.Read, Effect: acl.Deny})
	member := []string{"user:ann", "group:team"}
	contractor := []string{"user:contractor", "group:team"}
	if !d.can(t, member, d.file.ID, acl.Write) {
		t.Fatal("a team member lost the inherited allow")
	}
	if d.can(t, contractor, d.file.ID, acl.Read) || d.can(t, contractor, d.file.ID, acl.Write) {
		t.Fatal("the deny on the subfolder did not beat the inherited allow")
	}
	ok, err := d.acl.CanFolder(t.Context(), d.store, "t", contractor, d.sub.ID, acl.Read)
	if err != nil || ok {
		t.Fatalf("CanFolder under a deny = %v, %v", ok, err)
	}
	if ok, _ := d.acl.CanFolder(t.Context(), d.store, "t", contractor, d.team.ID, acl.Read); !ok {
		t.Fatal("a deny below the team folder refused the team folder itself")
	}
}

// TestFolderDenyBeatsFileAllow: a deny on a folder is not reopened by an
// explicit allow on a file inside it.
func TestFolderDenyBeatsFileAllow(t *testing.T) {
	d := newDenyEnv(t)
	d.grant(t, acl.Grant{Subject: "user:bob", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Admin})
	if !d.can(t, []string{"user:bob"}, d.file.ID, acl.Admin) {
		t.Fatal("the file allow alone must allow")
	}
	d.grant(t, acl.Grant{Subject: "user:bob", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Read, Effect: acl.Deny})
	if d.can(t, []string{"user:bob"}, d.file.ID, acl.Read) {
		t.Fatal("a file allow reopened a folder deny")
	}
}

// TestDenyOfALevelKeepsTheLevelsBelow: denying Write refuses Write and
// Admin, which include it, and leaves Read.
func TestDenyOfALevelKeepsTheLevelsBelow(t *testing.T) {
	d := newDenyEnv(t)
	d.grant(t, acl.Grant{Subject: "user:eve", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Admin})
	d.grant(t, acl.Grant{Subject: "group:readonly", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Write, Effect: acl.Deny})
	eve := []string{"user:eve", "group:readonly"}
	if !d.can(t, eve, d.file.ID, acl.Read) || d.can(t, eve, d.file.ID, acl.Write) || d.can(t, eve, d.file.ID, acl.Admin) {
		t.Fatal("a deny of Write must refuse Write and Admin and leave Read")
	}
}

// TestExpiredDenyRefusesNothing: a deny with ExpiresAt stops refusing at
// that time, and the allows under it apply again.
func TestExpiredDenyRefusesNothing(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.UnixMicro())
	d := newDenyEnv(t, acl.WithClock(func() time.Time { return time.UnixMicro(clock.Load()) }))
	d.grant(t, acl.Grant{Subject: "user:ann", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Write})
	end := now.Add(time.Hour)
	d.grant(t, acl.Grant{Subject: "user:ann", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Read, Effect: acl.Deny, ExpiresAt: &end})
	if d.can(t, []string{"user:ann"}, d.file.ID, acl.Read) {
		t.Fatal("a live deny must refuse")
	}
	clock.Store(now.Add(2 * time.Hour).UnixMicro())
	if !d.can(t, []string{"user:ann"}, d.file.ID, acl.Write) {
		t.Fatal("an expired deny still refused")
	}
}

// TestDenyListedAndReplaced: Grants shows a deny's effect; a subject has
// one grant per resource, so an allow given where it had a deny replaces
// it.
func TestDenyListedAndReplaced(t *testing.T) {
	d := newDenyEnv(t)
	d.grant(t, acl.Grant{Subject: "user:x", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Read, Effect: acl.Deny})
	gs, err := d.acl.Grants(t.Context(), d.store, "t", acl.OnFile, d.file.ID)
	if err != nil || len(gs) != 1 || gs[0].Effect != acl.Deny {
		t.Fatalf("Grants = %+v, %v", gs, err)
	}
	d.grant(t, acl.Grant{Subject: "user:x", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Read})
	if !d.can(t, []string{"user:x"}, d.file.ID, acl.Read) {
		t.Fatal("an allow did not replace the subject's deny")
	}
}

// TestDenyNeedsWithDeny: without WithDeny a deny grant is refused and an
// unknown effect always is; a default grant reads back as Allow, and a
// deny stored while WithDeny was on is still honoured by a Plugin built
// without it (turning the option off never turns a refusal into access).
func TestDenyNeedsWithDeny(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, withFolders)
	f := e.file(t, "a.txt")
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read, Effect: acl.Deny}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a deny without WithDeny = %v", err)
	}
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read, Effect: "maybe"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an unknown effect = %v", err)
	}
	g, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read})
	if err != nil || g.Effect != acl.Allow {
		t.Fatalf("a default grant = %+v, %v", g, err)
	}
	if !e.can(t, []string{"u"}, f.ID, acl.Read) {
		t.Fatal("the default allow changed")
	}
	// A deny given by a WithDeny plugin on the same tables.
	withDeny := acl.New(acl.WithHierarchy(e.folders), acl.WithDeny())
	if _, err := withDeny.Grant(ctx, e.store, "t", acl.Grant{Subject: "g", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read, Effect: acl.Deny}); err != nil {
		t.Fatal(err)
	}
	if e.can(t, []string{"u", "g"}, f.ID, acl.Read) {
		t.Fatal("a Plugin without WithDeny ignored a stored deny")
	}
}

// TestDenyOfAnUndefinedLevelRefusesEverything: a deny stored with a level
// the plugin no longer defines (its levels changed since) refuses every
// level for its subject, failing closed, and leaves other subjects alone.
func TestDenyOfAnUndefinedLevelRefusesEverything(t *testing.T) {
	d := newDenyEnv(t)
	d.grant(t, acl.Grant{Subject: "group:team", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Admin})
	ins := sqlb.NewInsert(d.store.Dialect()).Into(acl.Table).Row(
		sqlb.Set(acl.ID, "grt_retired"), sqlb.Set(acl.Scope, "t"), sqlb.Set(acl.Subject, "user:old"), sqlb.Set(acl.Kind, acl.OnFolder),
		sqlb.Set(acl.Resource, d.team.ID), sqlb.Set(acl.LevelCol, acl.Level("retired")), sqlb.Set(acl.CreatedAt, time.Now().UTC()),
		sqlb.Set(acl.EffectCol, acl.Deny))
	if _, err := d.store.Exec(t.Context(), ins); err != nil {
		t.Fatal(err)
	}
	if d.can(t, []string{"user:old", "group:team"}, d.file.ID, acl.Read) {
		t.Fatal("a deny of an undefined level let a read through")
	}
	if !d.can(t, []string{"user:ann", "group:team"}, d.file.ID, acl.Admin) {
		t.Fatal("the undefined deny refused another subject")
	}
}
