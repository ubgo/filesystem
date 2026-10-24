package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ifunchanged: alice saves: ok",
		"ifunchanged: bob saves the stale version: conflict (true)",
		"ifunchanged: bob reloads and saves: ok",
		"ifunchanged: trash alice's stale view: conflict",
		`Replace: same ID true, name "notes.txt", size 11, old object kept false`,
		`ReplaceObject: name "notes-v3.txt"; same key again: invalid_argument`,
		"SetVisibility in place: object is public, link " + cdnBase + "/media/docs/v3.txt",
		"routed upload: disk private, link " + signBase,
		"published: disk public, link " + cdnBase + "/public/docs/",
		"private copy left false",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func routedStore(t *testing.T) *records.Store {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := newRoutedStore(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReplaceKeepsPrivate replaces a private file under a policy that asks
// for public: the new content stays private.
func TestReplaceKeepsPrivate(t *testing.T) {
	s := routedStore(t)
	f, err := s.Upload(t.Context(), tenant, "", strings.NewReader("a"), "a.txt", textPolicy)
	if err != nil {
		t.Fatal(err)
	}
	p := textPolicy
	p.Visibility = filesystem.VisibilityPublic
	got, err := s.Replace(t.Context(), tenant, f.ID, strings.NewReader("bb"), "", p)
	if err != nil || got.Visibility != filesystem.VisibilityPrivate || got.Disk != privateDisk {
		t.Fatalf("Replace = %+v, %v", got, err)
	}
}

func TestReplaceTrashedIsNotFound(t *testing.T) {
	s := routedStore(t)
	f, err := s.Upload(t.Context(), tenant, "", strings.NewReader("a"), "a.txt", textPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trash(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceBody(t.Context(), s, f.ID, "b"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Replace of a trashed file = %v", err)
	}
}

func TestSetSameVisibilityIsNoop(t *testing.T) {
	s := routedStore(t)
	f, err := s.Upload(t.Context(), tenant, "", strings.NewReader("a"), "a.txt", textPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetVisibility(t.Context(), tenant, f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVisibility(t.Context(), tenant, f.ID, "secret"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("SetVisibility(secret) = %v", err)
	}
}
