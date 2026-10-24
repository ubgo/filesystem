package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

func testStore(t *testing.T) *records.Store {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := newStore(t.Context(), db)
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
		"gallery: [1:beach.jpg 2:dunes.jpg 3:sunset.jpg]",
		"reordered: [1:sunset.jpg 2:beach.jpg 3:dunes.jpg]",
		"partial reorder: invalid_argument",
		"after detach: [1:sunset.jpg 2:beach.jpg]",
		"unknown owner type: invalid_argument",
		"second Attach to a single role: exists",
		"avatar is the new file: true; previous trashed: true",
		"shared file kept: true, still the post's cover: true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestDetachUnknownSucceeds(t *testing.T) {
	s := testStore(t)
	if err := s.Detach(t.Context(), tenant, "att_unknown"); err != nil {
		t.Fatalf("Detach of an unknown attachment = %v, want nil", err)
	}
}

func TestAttachTwiceIsExists(t *testing.T) {
	s := testStore(t)
	f, err := store(t.Context(), s, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: ownerPost, ID: "p"}
	if _, err := s.Attach(t.Context(), tenant, f.ID, post, roleGallery); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(t.Context(), tenant, f.ID, post, roleGallery); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("second Attach = %v, want exists", err)
	}
}

func TestTrashedFileCannotBeAttached(t *testing.T) {
	s := testStore(t)
	f, err := store(t.Context(), s, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trash(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Attach(t.Context(), tenant, f.ID, records.Owner{Type: ownerPost, ID: "p"}, roleGallery)
	if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Attach of a trashed file = %v, want not_found", err)
	}
}

// TestConcurrentSetAttachment leaves exactly one avatar whatever the
// interleaving: on SQLite the calls serialize, on Postgres and MySQL a
// loser fails and may retry.
func TestConcurrentSetAttachment(t *testing.T) {
	s := testStore(t)
	user := records.Owner{Type: ownerUser, ID: "u"}
	var files []records.File
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		f, err := store(t.Context(), s, name)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Go(func() { _, _ = setAvatar(t.Context(), s, user, f.ID) })
	}
	wg.Wait()
	list, err := s.Attachments(t.Context(), tenant, user, roleAvatar)
	if err != nil || len(list) != 1 {
		t.Fatalf("avatars = %d, %v; want exactly 1", len(list), err)
	}
}

func TestUnknownPreviousIsRefused(t *testing.T) {
	s := testStore(t)
	_, err := s.SetAttachment(t.Context(), tenant, "fil_x", records.Owner{Type: ownerUser, ID: "u"}, roleAvatar, records.Previous("delete"))
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("SetAttachment with an unknown Previous = %v", err)
	}
}
