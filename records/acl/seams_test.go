package acl_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
)

// Levels of an application's own model, for the WithLevels tests: comment
// includes view, upload stands alone (a drop-box folder).
const (
	levelView    acl.Level = "view"
	levelComment acl.Level = "comment"
	levelUpload  acl.Level = "upload"
)

func ownLevels() acl.Option {
	return acl.WithLevels(
		acl.LevelDef{Level: levelView},
		acl.LevelDef{Level: levelComment, Includes: []acl.Level{levelView}},
		acl.LevelDef{Level: levelUpload},
	)
}

func with(opts ...acl.Option) func(*folders.Plugin) acl.Option {
	return func(*folders.Plugin) acl.Option {
		return func(p *acl.Plugin) {
			for _, o := range opts {
				o(p)
			}
		}
	}
}

func (e *env) grant(t *testing.T, g acl.Grant) {
	t.Helper()
	if _, err := e.acl.Grant(t.Context(), e.store, "t", g); err != nil {
		t.Fatal(err)
	}
}

// TestWithLevels: an application's levels replace the ladder; a grant gives
// the levels it includes and no other, and the default levels are unknown
// to such a plugin (Can refuses them rather than guessing).
func TestWithLevels(t *testing.T) {
	e := newEnv(t, with(ownLevels()))
	f := e.file(t, "a.txt")
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: levelComment})
	if !e.can(t, []string{"u"}, f.ID, levelView) || !e.can(t, []string{"u"}, f.ID, levelComment) {
		t.Fatal("a comment grant must give comment and view")
	}
	if e.can(t, []string{"u"}, f.ID, levelUpload) {
		t.Fatal("a comment grant gave upload, which it does not include")
	}
	if _, err := e.acl.Can(t.Context(), e.store, "t", []string{"u"}, f.ID, acl.Read); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a default level on a plugin with its own levels: %v", err)
	}
	if _, err := e.acl.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Admin}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("granting an undefined level: %v", err)
	}
	if got := e.acl.Levels(); len(got) != 3 || got[0] != levelComment || got[2] != levelView {
		t.Fatalf("Levels() = %v", got)
	}
}

// TestDefaultLevelsUnchanged: without WithLevels the ladder is read <
// write < admin, as before.
func TestDefaultLevelsUnchanged(t *testing.T) {
	e := newEnv(t)
	f := e.file(t, "a.txt")
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Write})
	if !e.can(t, []string{"u"}, f.ID, acl.Read) || !e.can(t, []string{"u"}, f.ID, acl.Write) || e.can(t, []string{"u"}, f.ID, acl.Admin) {
		t.Fatal("default ladder changed")
	}
	if got := e.acl.Levels(); len(got) != 3 || got[0] != acl.Admin || got[1] != acl.Read || got[2] != acl.Write {
		t.Fatalf("Levels() = %v", got)
	}
}

// TestInvalidOptionsFailClosed: a level including an undefined one, a
// duplicate, a bad name, a nil map, or a bad container kind leave a plugin
// that refuses every check and grant, and fails its migrations, instead of
// silently using the defaults.
func TestInvalidOptionsFailClosed(t *testing.T) {
	cases := map[string]acl.Option{
		"undefined include": acl.WithLevels(acl.LevelDef{Level: "a", Includes: []acl.Level{"b"}}),
		"duplicate":         acl.WithLevels(acl.LevelDef{Level: "a"}, acl.LevelDef{Level: "a"}),
		"bad name":          acl.WithLevels(acl.LevelDef{Level: "Bad Name"}),
		"no levels":         acl.WithLevels(),
		"nil level map":     acl.WithLevelFor(nil),
		"file kind":         acl.WithContainers(posts{kind: acl.OnFile}),
		"nil container":     acl.WithContainers(nil),
		"nil target map":    acl.WithLevelForTarget(nil),
		"nil precedence":    acl.WithPrecedence(nil),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			p := acl.New(opt)
			if _, err := p.Migrations(records.EngineSQLite); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
				t.Fatalf("Migrations: %v", err)
			}
			e := newEnv(t)
			f := e.file(t, "a.txt")
			if _, err := p.Can(t.Context(), e.store, "t", []string{"u"}, f.ID, acl.Read); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
				t.Fatalf("Can: %v", err)
			}
			if _, err := p.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
				t.Fatalf("Grant: %v", err)
			}
		})
	}
}

// TestWithLevelFor: a custom action map is what Authorizer asks: setting
// text needs only Read (a custom allow), changing visibility needs Admin
// (a custom deny of what a Write grant gave by default), an action mapped
// to an undefined level refuses rather than falling to next, and the
// default map is unchanged.
func TestWithLevelFor(t *testing.T) {
	levelFor := func(a records.Action) (acl.Level, bool) {
		switch a {
		case records.ActionSetText:
			return acl.Read, true
		case records.ActionSetVisibility:
			return acl.Admin, true
		case records.ActionReplace:
			return "nonexistent", true
		}
		return acl.LevelFor(a)
	}
	e := newEnv(t, with(acl.WithLevelFor(levelFor)))
	reader, writer := e.file(t, "r.txt"), e.file(t, "w.txt")
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: reader.ID, Level: acl.Read})
	e.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: writer.ID, Level: acl.Write})
	var nextCalls atomic.Int32
	next := func(*http.Request, records.Action, records.ActionTarget) error {
		nextCalls.Add(1)
		return filesystem.Errorf(filesystem.CodePermission, "next refuses")
	}
	authz := acl.Authorizer(e.acl, e.store, func(*http.Request) []string { return []string{"u"} }, next)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	at := func(f records.File) records.ActionTarget { return records.ActionTarget{Scope: "t", FileID: f.ID} }
	if err := authz(r, records.ActionSetText, at(reader)); err != nil {
		t.Fatalf("set_text with a read grant under the custom map: %v", err)
	}
	if err := authz(r, records.ActionSetVisibility, at(writer)); err == nil {
		t.Fatal("set_visibility with a write grant must be refused when it needs admin")
	}
	before := nextCalls.Load()
	if err := authz(r, records.ActionReplace, at(writer)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || nextCalls.Load() != before {
		t.Fatalf("an undefined level must refuse without asking next: %v (next called %d times)", err, nextCalls.Load()-before)
	}

	d := newEnv(t)
	w := d.file(t, "w.txt")
	d.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: w.ID, Level: acl.Write})
	def := acl.Authorizer(d.acl, d.store, func(*http.Request) []string { return []string{"u"} }, nil)
	if err := def(r, records.ActionSetVisibility, records.ActionTarget{Scope: "t", FileID: w.ID}); err != nil {
		t.Fatalf("default map: a write grant allows set_visibility: %v", err)
	}
	if err := def(r, records.ActionPurge, records.ActionTarget{Scope: "t", FileID: w.ID}); err == nil {
		t.Fatal("default map: a write grant must not purge")
	}
}

// posts is a Container: grants on a post reach the files it lists.
type posts struct {
	kind  acl.ResourceKind
	files map[string][]string // post ID -> file IDs
	ofErr error
}

func (p posts) Kind() acl.ResourceKind { return p.kind }

func (p posts) Of(_ context.Context, _ *records.Store, _, fileID string) ([]string, error) {
	if p.ofErr != nil {
		return nil, p.ofErr
	}
	var out []string
	for post, files := range p.files {
		for _, f := range files {
			if f == fileID {
				out = append(out, post)
			}
		}
	}
	return out, nil
}

func (p posts) Check(_ context.Context, _ *records.Store, _, id string) error {
	if _, ok := p.files[id]; !ok {
		return filesystem.Errorf(filesystem.CodeNotFound, "no post %q", id)
	}
	return nil
}

// TestContainers: a grant on a container reaches the files it holds and no
// other; Grant refuses a container that does not exist; a container that
// cannot answer makes Can fail, never allow.
func TestContainers(t *testing.T) {
	c := posts{kind: "post", files: map[string][]string{}}
	e := newEnv(t, with(acl.WithContainers(c)))
	in, out := e.file(t, "in.txt"), e.file(t, "out.txt")
	c.files["42"] = []string{in.ID}
	e.grant(t, acl.Grant{Subject: "u", Kind: "post", Resource: "42", Level: acl.Read})
	if !e.can(t, []string{"u"}, in.ID, acl.Read) {
		t.Fatal("a grant on the post must reach its file")
	}
	if e.can(t, []string{"u"}, out.ID, acl.Read) || e.can(t, []string{"u"}, in.ID, acl.Write) {
		t.Fatal("a grant on the post reached too far")
	}
	if _, err := e.acl.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "u", Kind: "post", Resource: "7", Level: acl.Read}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a grant on a missing post: %v", err)
	}
	if _, err := e.acl.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "u", Kind: "project", Resource: "1", Level: acl.Read}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a grant on an unknown kind: %v", err)
	}
	if _, err := e.acl.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "u", Kind: "post", Level: acl.Read}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a grant on a post without its ID: %v", err)
	}

	boom := errors.New("container down")
	f := newEnv(t, with(acl.WithContainers(posts{kind: "post", ofErr: boom})))
	g := f.file(t, "g.txt")
	f.grant(t, acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: g.ID, Level: acl.Read})
	if ok, err := f.acl.Can(t.Context(), f.store, "t", []string{"u"}, g.ID, acl.Read); !errors.Is(err, boom) || ok {
		t.Fatalf("a failing container must fail the check: %v %v", ok, err)
	}
}

// TestGrantExpiry: a grant with ExpiresAt gives its level until then and
// nothing after; one already past is refused; Prune deletes expired grants
// and keeps the others; grants without ExpiresAt never expire.
func TestGrantExpiry(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.UnixMicro())
	e := newEnv(t, with(acl.WithClock(func() time.Time { return time.UnixMicro(clock.Load()) })))
	f := e.file(t, "a.txt")
	friday := now.Add(time.Hour)
	e.grant(t, acl.Grant{Subject: "temp", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read, ExpiresAt: &friday})
	e.grant(t, acl.Grant{Subject: "perm", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read})
	if !e.can(t, []string{"temp"}, f.ID, acl.Read) {
		t.Fatal("an unexpired grant must allow")
	}
	gs, err := e.acl.Grants(t.Context(), e.store, "t", acl.OnFile, f.ID)
	if err != nil || len(gs) != 2 || gs[1].ExpiresAt == nil || !gs[1].ExpiresAt.Equal(friday) || gs[0].ExpiresAt != nil {
		t.Fatalf("Grants: %+v %v", gs, err)
	}
	clock.Store(now.Add(2 * time.Hour).UnixMicro())
	if e.can(t, []string{"temp"}, f.ID, acl.Read) {
		t.Fatal("an expired grant must give nothing")
	}
	if !e.can(t, []string{"perm"}, f.ID, acl.Read) {
		t.Fatal("a grant without ExpiresAt must not expire")
	}
	past := now
	if _, err := e.acl.Grant(t.Context(), e.store, "t", acl.Grant{Subject: "x", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read, ExpiresAt: &past}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a grant expiring in the past: %v", err)
	}
	n, err := e.acl.Prune(t.Context(), e.store, now.Add(90*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if gs, _ := e.acl.Grants(t.Context(), e.store, "t", acl.OnFile, f.ID); len(gs) != 1 || gs[0].Subject != "perm" {
		t.Fatalf("after Prune: %+v", gs)
	}
}

// TestNoHTTPLayerImport (C5): the plugin decides on records.Action, so no
// production file of it imports recordshttp. The scan is checked against a
// planted import so it cannot pass vacuously.
func TestNoHTTPLayerImport(t *testing.T) {
	const banned = "github.com/ubgo/filesystem/records/recordshttp"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		if importsPath(t, f, banned) {
			t.Errorf("%s imports %s; acl must depend on records only", f, banned)
		}
	}
	if scanned < 2 {
		t.Fatalf("scanned %d files", scanned)
	}
	planted := filepath.Join(t.TempDir(), "x.go")
	src := "package acl\n\nimport (\n\t\"net/http\"\n\n\t_ " + strconv.Quote(banned) + "\n)\n"
	if err := os.WriteFile(planted, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if !importsPath(t, planted, banned) {
		t.Fatal("the scan missed a planted multi-line import")
	}
}

func importsPath(t *testing.T, file, path string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == path {
			return true
		}
	}
	return false
}

// TestLevelInclusionGraph: a level reached by two paths (a diamond) and a
// cycle of inclusions resolve to each level's full set, once.
func TestLevelInclusionGraph(t *testing.T) {
	e := newEnv(t, with(acl.WithLevels(
		acl.LevelDef{Level: "base"},
		acl.LevelDef{Level: "left", Includes: []acl.Level{"base"}},
		acl.LevelDef{Level: "right", Includes: []acl.Level{"base"}},
		acl.LevelDef{Level: "top", Includes: []acl.Level{"left", "right"}},
		acl.LevelDef{Level: "ping", Includes: []acl.Level{"pong"}},
		acl.LevelDef{Level: "pong", Includes: []acl.Level{"ping"}},
	), acl.WithLevelFor(func(a records.Action) (acl.Level, bool) {
		if a == records.ActionRead {
			return "base", true
		}
		return "pong", true
	})))
	f := e.file(t, "a.txt")
	e.grant(t, acl.Grant{Subject: "top", Kind: acl.OnFile, Resource: f.ID, Level: "top"})
	e.grant(t, acl.Grant{Subject: "ping", Kind: acl.OnFile, Resource: f.ID, Level: "ping"})
	if !e.can(t, []string{"top"}, f.ID, "base") || !e.can(t, []string{"ping"}, f.ID, "pong") || e.can(t, []string{"ping"}, f.ID, "base") {
		t.Fatal("inclusion through a diamond or a cycle resolved wrong")
	}
}

// TestDefaultLevelsValid: Level.Valid knows the default ladder only.
func TestDefaultLevelsValid(t *testing.T) {
	for _, l := range acl.LevelValues {
		if !l.Valid() {
			t.Errorf("%q not valid", l)
		}
	}
	if acl.Level("comment").Valid() {
		t.Error("a level outside the defaults is valid")
	}
}
