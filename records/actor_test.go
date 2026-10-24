package records_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestUploadedBy(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user:ana")
	e := newEnv(t, nil)
	_, _ = e.disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{})
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
	if err != nil || f.UploadedBy != "user:ana" {
		t.Fatalf("from the context = %q, %v", f.UploadedBy, err)
	}
	g, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png", UploadedBy: "import:legacy"})
	if err != nil || g.UploadedBy != "import:legacy" {
		t.Fatalf("explicit = %q, %v", g.UploadedBy, err)
	}
	if stored, _ := e.store.File(ctx, "t", f.ID); stored.UploadedBy != "user:ana" {
		t.Fatalf("stored = %q", stored.UploadedBy)
	}
	bob := records.WithActor(t.Context(), "user:bob")
	if r, err := e.store.Replace(bob, "t", f.ID, strings.NewReader(pngData+"2"), "", pngPolicy); err != nil || r.UploadedBy != "user:ana" {
		t.Fatalf("a replacement changed the uploader: %q, %v", r.UploadedBy, err)
	}
	if h := e.put(t, "t", "b.png"); h.UploadedBy != "" || records.ActorFrom(t.Context()) != "" {
		t.Fatalf("no actor = %q", h.UploadedBy)
	}
	_, err = e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png", UploadedBy: strings.Repeat("x", records.MaxActorBytes+1)})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Filters: []sqlb.Predicate{sqlb.Eq(records.FileUploadedBy, "user:ana")}})
	if len(page.Files) != 1 || page.Files[0].ID != f.ID {
		t.Fatalf("files by uploader = %+v", page.Files)
	}
}

// TestUpgradeAddsUploadedBy migrates a database that predates the column
// and holds files: they read back with no uploader.
func TestUpgradeAddsUploadedBy(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "old.png")
	exec(t, e.db, "ALTER TABLE "+records.TableNameFiles+" DROP COLUMN "+records.ColFileUploadedBy)
	exec(t, e.db, "DELETE FROM "+records.TableNameMigrations+" WHERE name = '0002_files_uploaded_by'")
	done, err := e.store.Migrate(ctx)
	if err != nil || strings.Join(done, ",") != "records/0002_files_uploaded_by" {
		t.Fatalf("upgrade = %v, %v", done, err)
	}
	if got, err := e.store.File(ctx, "t", f.ID); err != nil || got.UploadedBy != "" {
		t.Fatalf("an old file = %+v, %v", got, err)
	}
}
