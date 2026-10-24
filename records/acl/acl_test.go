package acl_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

type env struct {
	store   *records.Store
	acl     *acl.Plugin
	folders *folders.Plugin
}

func newEnv(t *testing.T, opts ...func(*folders.Plugin) acl.Option) *env {
	t.Helper()
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	fp := folders.New()
	var aopts []acl.Option
	for _, o := range opts {
		aopts = append(aopts, o(fp))
	}
	ap := acl.New(aopts...)
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(fp, ap))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{store: s, acl: ap, folders: fp}
}

func withFolders(fp *folders.Plugin) acl.Option { return acl.WithHierarchy(fp) }

func (e *env) file(t *testing.T, name string) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), "t", "media", strings.NewReader(name), name, upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *env) can(t *testing.T, subjects []string, fileID string, level acl.Level) bool {
	t.Helper()
	ok, err := e.acl.Can(t.Context(), e.store, "t", subjects, fileID, level)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestFileGrants: a grant on a file allows its level and the ones below,
// for the subject or any subject the caller speaks for; revoking ends it.
func TestFileGrants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t, "a.txt")
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "group:editors", Kind: acl.OnFile, Resource: f.ID, Level: acl.Write}); err != nil {
		t.Fatal(err)
	}
	alice := []string{"user:alice", "group:editors"}
	if !e.can(t, alice, f.ID, acl.Read) || !e.can(t, alice, f.ID, acl.Write) || e.can(t, alice, f.ID, acl.Admin) {
		t.Fatal("a write grant's levels")
	}
	if e.can(t, []string{"user:bob"}, f.ID, acl.Read) || e.can(t, nil, f.ID, acl.Read) {
		t.Fatal("a subject without a grant, or none, was allowed")
	}
	// Granting again replaces the level.
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "group:editors", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read}); err != nil {
		t.Fatal(err)
	}
	if e.can(t, alice, f.ID, acl.Write) {
		t.Fatal("a regrant did not replace the level")
	}
	list, err := e.acl.Grants(ctx, e.store, "t", acl.OnFile, f.ID)
	if err != nil || len(list) != 1 || list[0].Level != acl.Read {
		t.Fatalf("Grants = %+v, %v", list, err)
	}
	if err := e.acl.Revoke(ctx, e.store, "t", acl.OnFile, f.ID, "group:editors"); err != nil {
		t.Fatal(err)
	}
	if e.can(t, alice, f.ID, acl.Read) {
		t.Fatal("a revoked grant still allows")
	}
	if err := e.acl.Revoke(ctx, e.store, "t", acl.OnFile, f.ID, "group:editors"); err != nil {
		t.Fatalf("revoking twice = %v", err)
	}
	for _, bad := range []acl.Grant{
		{Subject: "", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read},
		{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: "owner"},
		{Subject: "u", Kind: "bucket", Resource: f.ID, Level: acl.Read},
		{Subject: "u", Kind: acl.OnFolder, Resource: "fld_x", Level: acl.Read}, // no hierarchy
	} {
		if _, err := e.acl.Grant(ctx, e.store, "t", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: "fil_missing", Level: acl.Read}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a grant on an unknown file = %v", err)
	}
}

// TestFolderGrantsInherit: a grant on a folder reaches the files in it and
// in its subfolders, including files put there later, and not files
// outside it; moving a file out ends it.
func TestFolderGrantsInherit(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, withFolders)
	top, _ := e.folders.Create(ctx, e.store, "t", folders.RootID, "Team")
	sub, _ := e.folders.Create(ctx, e.store, "t", top.ID, "Specs")
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "user:bob", Kind: acl.OnFolder, Resource: top.ID, Level: acl.Admin}); err != nil {
		t.Fatal(err)
	}
	deep := e.file(t, "deep.txt")
	if err := e.folders.Put(ctx, e.store, "t", deep.ID, sub.ID); err != nil {
		t.Fatal(err)
	}
	outside := e.file(t, "outside.txt")
	bob := []string{"user:bob"}
	if !e.can(t, bob, deep.ID, acl.Admin) || e.can(t, bob, outside.ID, acl.Read) {
		t.Fatal("folder grant inheritance")
	}
	if ok, err := e.acl.CanFolder(ctx, e.store, "t", bob, sub.ID, acl.Write); err != nil || !ok {
		t.Fatalf("CanFolder on a subfolder = %v, %v", ok, err)
	}
	if err := e.folders.Put(ctx, e.store, "t", deep.ID, folders.RootID); err != nil {
		t.Fatal(err)
	}
	if e.can(t, bob, deep.ID, acl.Read) {
		t.Fatal("a file moved out kept the folder's grant")
	}
	mine, err := e.acl.GrantsOf(ctx, e.store, "t", "user:bob")
	if err != nil || len(mine) != 1 || mine[0].Resource != top.ID {
		t.Fatalf("GrantsOf = %+v, %v", mine, err)
	}
}

// mapHierarchy is a second Hierarchy: files and folders in maps, as an
// application's own tree would be.
type mapHierarchy struct {
	folderOf map[string]string
	parent   map[string]string
}

func (h mapHierarchy) FolderOf(_ context.Context, _ *records.Store, _, fileID string) (string, error) {
	return h.folderOf[fileID], nil
}

func (h mapHierarchy) Ancestors(_ context.Context, _ *records.Store, _, id string) ([]string, error) {
	var out []string
	for id != "" {
		if _, ok := h.parent[id]; !ok {
			return nil, filesystem.Errorf(filesystem.CodeNotFound, "no folder %q", id)
		}
		out = append(out, id)
		id = h.parent[id]
	}
	return out, nil
}

// TestOwnHierarchy: grants inherit through any Hierarchy, not only the
// folders plugin's.
func TestOwnHierarchy(t *testing.T) {
	ctx := t.Context()
	h := mapHierarchy{folderOf: map[string]string{}, parent: map[string]string{"albums": "", "2026": "albums"}}
	e := newEnv(t, func(*folders.Plugin) acl.Option { return acl.WithHierarchy(h) })
	f := e.file(t, "photo.txt")
	h.folderOf[f.ID] = "2026"
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "user:c", Kind: acl.OnFolder, Resource: "albums", Level: acl.Read}); err != nil {
		t.Fatal(err)
	}
	if !e.can(t, []string{"user:c"}, f.ID, acl.Read) {
		t.Fatal("a grant on an album did not reach a photo two levels down")
	}
}

// TestPurgeRemovesGrants: a purged file's grants go with it.
func TestPurgeRemovesGrants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t, "a.txt")
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := e.acl.GrantsOf(ctx, e.store, "t", "u"); err != nil || len(list) != 0 {
		t.Fatalf("grants after the purge = %+v, %v", list, err)
	}
}

// TestAuthorizer: the recordshttp helper allows a file action a grant
// covers and hands everything else to the application's own rule.
func TestAuthorizer(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t, "a.txt")
	if _, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "user:alice", Kind: acl.OnFile, Resource: f.ID, Level: acl.Write}); err != nil {
		t.Fatal(err)
	}
	var asked []recordshttp.Action
	denied := errors.New("not yours")
	next := func(_ *http.Request, a recordshttp.Action, _ recordshttp.Target) error {
		asked = append(asked, a)
		return denied
	}
	authorize := acl.Authorizer(e.acl, e.store, func(*http.Request) []string { return []string{"user:alice"} }, next)
	r := httptest.NewRequest("GET", "/", nil)
	target := recordshttp.Target{Scope: "t", FileID: f.ID, File: &f}
	for a, want := range map[recordshttp.Action]error{
		recordshttp.ActionRead: nil, recordshttp.ActionReplace: nil, recordshttp.ActionTrash: nil,
		recordshttp.ActionPurge: denied, recordshttp.ActionList: denied,
	} {
		if err := authorize(r, a, target); !errors.Is(err, want) && err != want {
			t.Errorf("%s = %v, want %v", a, err, want)
		}
	}
	if len(asked) != 2 {
		t.Fatalf("the application's rule was asked for %v", asked)
	}
	strict := acl.Authorizer(e.acl, e.store, func(*http.Request) []string { return nil }, nil)
	if err := strict(r, recordshttp.ActionRead, target); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("no grant and no next = %v", err)
	}
}
