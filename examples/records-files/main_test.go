package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// testStore returns a migrated Store over a fresh SQLite file.
func testStore(t *testing.T) *records.Store {
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
	s, err := newStore(t.Context(), db, disks)
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
		`created fil_ "Quarterly report.txt" text/plain; charset=utf-8 19 bytes private`,
		"by user:alice", "refused: rejected", "listed 2 files in 2 pages", "files over 10 bytes: 1",
		"view: " + signBase + "/media/exports/q3.txt?", "cd=attachment",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestCreateFileNeedsTheObject(t *testing.T) {
	s := testStore(t)
	_, err := s.CreateFile(t.Context(), records.NewFile{Scope: tenant, Disk: diskName, Key: "missing.txt"})
	if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("CreateFile of a missing object = %v, want not_found", err)
	}
}

func TestFilesAreScoped(t *testing.T) {
	s := testStore(t)
	f, err := recordExisting(t.Context(), s, "a.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.File(t.Context(), "other-tenant", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("File from another scope = %v, want not_found", err)
	}
	got, err := s.File(t.Context(), tenant, f.ID)
	if err != nil || got.Metadata["source"] != "export-job" {
		t.Fatalf("File = %+v, %v", got, err)
	}
}

func TestPublicFileGetsPublicURL(t *testing.T) {
	s := testStore(t)
	f, err := recordExisting(context.Background(), s, "pub.txt", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility = filesystem.VisibilityPublic
	view, _, err := links(t.Context(), s, f)
	if err != nil || view != cdnBase+"/pub.txt" {
		t.Fatalf("URL = %q, %v", view, err)
	}
}

func TestBadCursorIsInvalid(t *testing.T) {
	s := testStore(t)
	_, err := s.Files(t.Context(), records.FileQuery{Scope: tenant, Cursor: "not-a-cursor"})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Files with a bad cursor = %v", err)
	}
}

func TestExtras(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"id fil_… (36 chars) key exports/q4.txt",
		"uploaded by user:bob via ios app/3.2 from 203.0.113.7, created_at set true",
		`alt "Bar chart of revenue against costs" title "Q3 report" source "export-job"`,
		"dedup: two rows, same key true, 2 references", "purged one: object kept true",
		"content key under blobs/: true, shared true",
		"purge refused: filesystem: conflict: the file is still used: post 42 as cover (conflict)",
		"detached, then purged", "v1=true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestSourceIPDroppedByDefault pins the default: without WithSourceIP the
// IP in the context is not stored.
func TestSourceIPDroppedByDefault(t *testing.T) {
	s := testStore(t)
	ctx := requestContext(t.Context(), "user:bob", "203.0.113.7")
	f, err := recordExisting(ctx, s, "x.txt", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Source.IP != "" || f.Source.Channel != "ios" {
		t.Fatalf("Source = %+v, want the channel without the IP", f.Source)
	}
}

// TestTextLimits pins the refusal of a title over MaxTitleChars.
func TestTextLimits(t *testing.T) {
	s := testStore(t)
	f, err := recordExisting(t.Context(), s, "y.txt", []byte("y"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SetText(t.Context(), f.Scope, f.ID, records.Text{Title: strings.Repeat("t", records.MaxTitleChars+1)})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("SetText with a long title = %v, want invalid_argument", err)
	}
}

// TestUnembedRefusesNewerVersion pins that a row written by a newer
// library is not misread.
func TestUnembedRefusesNewerVersion(t *testing.T) {
	if _, err := records.Unembed([]byte(`{"v":99,"id":"fil_x"}`)); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("Unembed of v99 = %v, want unsupported", err)
	}
}

func TestSessionAndSort(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"session actor: uploaded by user:42",
		"no session: uploaded by share:lnk_1",
		"order name:      Agenda.txt(10) budget.txt(3) notes.txt(1)",
		"order name_desc: notes.txt(1) budget.txt(3) Agenda.txt(10)",
		"order largest:   Agenda.txt(10) budget.txt(3) notes.txt(1)",
		"order smallest:  notes.txt(1) budget.txt(3) Agenda.txt(10)",
		"name cursor with the largest order: invalid_argument",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestSessionNeedsActorFunc pins that the app's session type reaches
// UploadedBy only through WithActorFunc: the default Store records no
// actor for the same context.
func TestSessionNeedsActorFunc(t *testing.T) {
	s := testStore(t)
	ctx := withSession(t.Context(), session{UserID: "42"})
	f, err := s.Upload(ctx, sortScope, diskName, strings.NewReader("x"), "x.txt", notesPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if f.UploadedBy != "" {
		t.Fatalf("UploadedBy without WithActorFunc = %q, want empty", f.UploadedBy)
	}
}
