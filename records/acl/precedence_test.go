package acl_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// TestMostSpecific pins R-I1: under MostSpecific an allow on a file
// reopens a deny on its folder ("contractor denied on /team, allowed on
// /team/brief.pdf") and a deny on a file closes an allow on its folder,
// while two grants at one distance keep deny-wins; the default plugin
// still refuses the reopening.
func TestMostSpecific(t *testing.T) {
	for _, specific := range []bool{true, false} {
		var opts []acl.Option
		if specific {
			opts = append(opts, acl.WithPrecedence(acl.MostSpecific))
		}
		d := newDenyEnv(t, opts...)
		d.grant(t, acl.Grant{Subject: "group:team", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Write})
		d.grant(t, acl.Grant{Subject: "user:contractor", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Read, Effect: acl.Deny})
		d.grant(t, acl.Grant{Subject: "user:contractor", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Read})
		contractor := []string{"user:contractor", "group:team"}
		if got := d.can(t, contractor, d.file.ID, acl.Read); got != specific {
			t.Fatalf("specific=%v: the file allow under a folder deny gave %v", specific, got)
		}
		// The file allow gives Read, not Write: the nearest grant that
		// applies to Write is the folder deny.
		if d.can(t, contractor, d.file.ID, acl.Write) {
			t.Fatalf("specific=%v: the contractor got Write", specific)
		}
		if ok, _ := d.acl.CanFolder(t.Context(), d.store, "t", contractor, d.sub.ID, acl.Read); ok {
			t.Fatalf("specific=%v: the folder deny opened the subfolder", specific)
		}

		member := []string{"user:ann", "group:team"}
		d.grant(t, acl.Grant{Subject: "user:ann", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Write, Effect: acl.Deny})
		if d.can(t, member, d.file.ID, acl.Write) {
			t.Fatalf("specific=%v: a file deny did not close the folder allow", specific)
		}
		// Same distance: an allow and a deny on the subfolder, deny wins.
		d.grant(t, acl.Grant{Subject: "user:bob", Kind: acl.OnFolder, Resource: d.sub.ID, Level: acl.Read})
		d.grant(t, acl.Grant{Subject: "group:qa", Kind: acl.OnFolder, Resource: d.sub.ID, Level: acl.Read, Effect: acl.Deny})
		if d.can(t, []string{"user:bob", "group:qa"}, d.file.ID, acl.Read) {
			t.Fatalf("specific=%v: an allow beat a deny at the same distance", specific)
		}
	}
}

// TestCustomPrecedence: a precedence of the application's own sees every
// unexpired match of the asked subjects, nearest first with their depths,
// and decides: it can allow with no grant at all, refuse over an allow,
// and its error fails the check and the Authorizer without asking next.
func TestCustomPrecedence(t *testing.T) {
	c := posts{kind: "post", files: map[string][]string{}}
	var seen []acl.Match
	var decide func([]acl.Match) (bool, error)
	record := func(_ context.Context, level acl.Level, ms []acl.Match) (bool, error) {
		if level != acl.Write {
			t.Errorf("precedence asked for %q", level)
		}
		seen = slices.Clone(ms)
		return decide(ms)
	}
	d := newDenyEnv(t, acl.WithPrecedence(record), acl.WithContainers(c))
	c.files["42"] = []string{d.file.ID}
	d.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFolder, Resource: d.team.ID, Level: acl.Admin})
	d.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFolder, Resource: d.sub.ID, Level: acl.Read})
	d.grant(t, acl.Grant{Subject: "g", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Write, Effect: acl.Deny})
	d.grant(t, acl.Grant{Subject: "u", Kind: "post", Resource: "42", Level: acl.Write})
	d.grant(t, acl.Grant{Subject: "other", Kind: acl.OnFile, Resource: d.file.ID, Level: acl.Admin})

	decide = func([]acl.Match) (bool, error) { return false, nil }
	if d.can(t, []string{"u", "g"}, d.file.ID, acl.Write) {
		t.Fatal("a custom refusal over an admin allow was ignored")
	}
	type row struct {
		subject string
		depth   int
		applies bool
		denies  bool
	}
	var got []row
	for _, m := range seen {
		got = append(got, row{m.Grant.Subject, m.Depth, m.Applies, m.Denies()})
	}
	want := []row{{"g", 0, true, true}, {"u", 1, false, false}, {"u", 2, true, false}, {"u", acl.ContainerDepth, true, false}}
	if !slices.Equal(got, want) {
		t.Fatalf("matches %+v, want %+v", got, want)
	}

	decide = func([]acl.Match) (bool, error) { return true, nil }
	if !d.can(t, []string{"nobody"}, d.file.ID, acl.Write) || len(seen) != 0 {
		t.Fatalf("a custom allow with no grant: matches %v", seen)
	}

	boom := errors.New("policy engine down")
	decide = func([]acl.Match) (bool, error) { return true, boom }
	if ok, err := d.acl.Can(t.Context(), d.store, "t", []string{"u"}, d.file.ID, acl.Write); !errors.Is(err, boom) || ok {
		t.Fatalf("a failing precedence: %v %v", ok, err)
	}
	asked := false
	authz := acl.Authorizer(d.acl, d.store, func(*http.Request) []string { return []string{"u"} }, func(*http.Request, records.Action, records.ActionTarget) error {
		asked = true
		return nil
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := authz(r, records.ActionReplace, records.ActionTarget{Scope: "t", FileID: d.file.ID}); !errors.Is(err, boom) || asked {
		t.Fatalf("Authorizer with a failing precedence: %v (next asked %v)", err, asked)
	}
}

// TestDenyOverridesEquivalence: the default precedence decides every
// random set of grants on a file's path exactly as the rule it replaced
// (any applying allow, no applying deny), for every level.
func TestDenyOverridesEquivalence(t *testing.T) {
	d := newDenyEnv(t)
	rank := map[acl.Level]int{acl.Read: 0, acl.Write: 1, acl.Admin: 2}
	type spot struct {
		kind acl.ResourceKind
		id   string
	}
	spots := []spot{{acl.OnFile, d.file.ID}, {acl.OnFolder, d.sub.ID}, {acl.OnFolder, d.team.ID}}
	subjects := []string{"a", "b", "c"}
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 40 {
		for _, s := range spots {
			for _, sub := range subjects {
				if err := d.acl.Revoke(t.Context(), d.store, "t", s.kind, s.id, sub); err != nil {
					t.Fatal(err)
				}
			}
		}
		type given struct {
			level acl.Level
			deny  bool
		}
		var grants []given
		for _, s := range spots {
			for _, sub := range subjects {
				if rng.IntN(3) == 0 {
					continue
				}
				g := given{level: acl.LevelValues[rng.IntN(3)], deny: rng.IntN(3) == 0}
				effect := acl.Allow
				if g.deny {
					effect = acl.Deny
				}
				d.grant(t, acl.Grant{Subject: sub, Kind: s.kind, Resource: s.id, Level: g.level, Effect: effect})
				grants = append(grants, g)
			}
		}
		for _, want := range acl.LevelValues {
			allowed, denied := false, false
			for _, g := range grants {
				if g.deny {
					denied = denied || rank[want] >= rank[g.level]
				} else {
					allowed = allowed || rank[g.level] >= rank[want]
				}
			}
			if got := d.can(t, subjects, d.file.ID, want); got != (allowed && !denied) {
				t.Fatalf("round %d level %s: %v, the old rule %v (grants %+v)", round, want, got, allowed && !denied, grants)
			}
		}
	}
}

// TestWithLevelForTarget pins R-I3 and R-I4 at the plugin: a map that
// reads the target's requested change makes publishing need Admin and
// unpublishing only Write; a level it does not define refuses; and ok
// false leaves the action to next.
func TestWithLevelForTarget(t *testing.T) {
	levelFor := func(ctx context.Context, a records.Action, tg records.ActionTarget) (acl.Level, bool) {
		if ctx == nil {
			t.Error("no context")
		}
		switch {
		case a == records.ActionSetVisibility && tg.Change != nil && tg.Change.Visibility == filesystem.VisibilityPublic:
			return acl.Admin, true
		case a == records.ActionTrash && tg.File != nil && tg.File.Metadata["hold"] == "legal":
			return "nonexistent", true
		case a == records.ActionSetText:
			return "", false
		}
		return acl.LevelFor(a)
	}
	e := newEnv(t, with(acl.WithLevelForTarget(levelFor)))
	f := e.file(t, "w.txt")
	held := f
	held.Metadata = map[string]string{"hold": "legal"}
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Write})
	nextAsked := 0
	authz := acl.Authorizer(e.acl, e.store, func(*http.Request) []string { return []string{"u"} }, func(*http.Request, records.Action, records.ActionTarget) error {
		nextAsked++
		return filesystem.Errorf(filesystem.CodePermission, "next refuses")
	})
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	target := func(v filesystem.Visibility) records.ActionTarget {
		return records.ActionTarget{Scope: "t", FileID: f.ID, File: &f, Change: &records.Change{Visibility: v}}
	}
	if err := authz(r, records.ActionSetVisibility, target(filesystem.VisibilityPrivate)); err != nil {
		t.Fatalf("unpublishing with Write: %v", err)
	}
	if err := authz(r, records.ActionSetVisibility, target(filesystem.VisibilityPublic)); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("publishing with Write must be refused: %v", err)
	}
	before := nextAsked
	if err := authz(r, records.ActionTrash, records.ActionTarget{Scope: "t", FileID: f.ID, File: &held}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || nextAsked != before {
		t.Fatalf("an undefined level must refuse without next: %v", err)
	}
	if err := authz(r, records.ActionTrash, records.ActionTarget{Scope: "t", FileID: f.ID, File: &f}); err != nil {
		t.Fatalf("trash with Write: %v", err)
	}
	if err := authz(r, records.ActionSetText, records.ActionTarget{Scope: "t", FileID: f.ID, File: &f}); err == nil || nextAsked != before+1 {
		t.Fatalf("ok false must go to next: %v (next %d)", err, nextAsked-before)
	}
}

// TestLevelsPerSubject pins R-I2: with WithLevelsPerSubject a subject
// holds levels that do not form a ladder side by side, and an allow
// beside a deny of another level; a grant replaces only its own level,
// RevokeLevel removes one, Revoke all; without the option a grant still
// replaces the subject's only grant, and the migration is not offered.
func TestLevelsPerSubject(t *testing.T) {
	e := newEnv(t, with(ownLevels(), acl.WithLevelsPerSubject(), acl.WithDeny()))
	f := e.file(t, "brief.pdf")
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelComment})
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelUpload})
	if !e.can(t, []string{"u"}, f.ID, levelView) || !e.can(t, []string{"u"}, f.ID, levelUpload) {
		t.Fatal("two levels of one subject do not both give access")
	}
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelUpload, Effect: acl.Deny})
	if e.can(t, []string{"u"}, f.ID, levelUpload) || !e.can(t, []string{"u"}, f.ID, levelComment) {
		t.Fatal("a deny of one level must leave the allow of another")
	}
	gs, err := e.acl.Grants(t.Context(), e.store, "t", acl.OnFile, f.ID)
	if err != nil || len(gs) != 2 {
		t.Fatalf("grants %+v, %v", gs, err)
	}
	if err := e.acl.RevokeLevel(t.Context(), e.store, "t", acl.OnFile, f.ID, "u", levelUpload); err != nil {
		t.Fatal(err)
	}
	if !e.can(t, []string{"u"}, f.ID, levelComment) {
		t.Fatal("RevokeLevel removed another level")
	}
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelUpload})
	if err := e.acl.Revoke(t.Context(), e.store, "t", acl.OnFile, f.ID, "u"); err != nil {
		t.Fatal(err)
	}
	if gs, _ := e.acl.Grants(t.Context(), e.store, "t", acl.OnFile, f.ID); len(gs) != 0 {
		t.Fatalf("Revoke left %+v", gs)
	}

	d := newEnv(t, with(ownLevels()))
	g := d.file(t, "g.pdf")
	d.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: g.ID, Level: levelComment})
	d.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: g.ID, Level: levelUpload})
	if d.can(t, []string{"u"}, g.ID, levelComment) {
		t.Fatal("without the option a second grant must replace the first")
	}
	names := func(p *acl.Plugin) []string {
		ms, err := p.Migrations(records.EngineSQLite)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			out = append(out, m.Name)
		}
		return out
	}
	if got := names(acl.New()); slices.Contains(got, "0004_grants_level_key") {
		t.Fatalf("the default plugin offers the level key migration: %v", got)
	}
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		ms, err := acl.New(acl.WithLevelsPerSubject()).Migrations(engine)
		if err != nil || ms[len(ms)-1].Name != "0004_grants_level_key" || len(ms[len(ms)-1].Statements) == 0 {
			t.Fatalf("%s: %v %v", engine, ms, err)
		}
	}
}

// TestLevelsPerSubjectUpgrade: turning the option on over a database the
// default plugin migrated keeps every grant and lets the subject add a
// second level.
func TestLevelsPerSubjectUpgrade(t *testing.T) {
	ctx := t.Context()
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	open := func(opts ...acl.Option) (*records.Store, *acl.Plugin) {
		p := acl.New(append([]acl.Option{ownLevels()}, opts...)...)
		s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(folders.New(), p))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s, p
	}
	s, p := open()
	e := &env{store: s, acl: p}
	f := e.file(t, "a.pdf")
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelComment})
	if _, err := p.Grant(ctx, s, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelUpload}); err != nil {
		t.Fatal(err)
	}
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelComment})

	s2, p2 := open(acl.WithLevelsPerSubject())
	e2 := &env{store: s2, acl: p2}
	if !e2.can(t, []string{"u"}, f.ID, levelComment) {
		t.Fatal("the grant made before the migration is gone")
	}
	e2.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelUpload})
	if !e2.can(t, []string{"u"}, f.ID, levelComment) || !e2.can(t, []string{"u"}, f.ID, levelUpload) {
		t.Fatal("after the migration a subject cannot hold two levels")
	}
}
