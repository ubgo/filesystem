package records_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestStatus(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r))
	f := e.put(t, "t", "a.png")
	if f.Status != records.StatusReady || f.StatusDetail != "" {
		t.Fatalf("a new file is %q %q", f.Status, f.StatusDetail)
	}
	got, err := e.store.SetStatus(ctx, "t", f.ID, records.StatusProcessing, "thumbnails 0/3")
	if err != nil || got.Status != records.StatusProcessing || got.StatusDetail != "thumbnails 0/3" {
		t.Fatalf("SetStatus = %+v, %v", got, err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Status != records.StatusProcessing || read.StatusDetail != "thumbnails 0/3" {
		t.Fatalf("read back %q %q", read.Status, read.StatusDetail)
	}
	// The same status and detail again change nothing and emit nothing.
	before := len(r.events)
	if _, err := e.store.SetStatus(ctx, "t", f.ID, records.StatusProcessing, "thumbnails 0/3"); err != nil || len(r.events) != before {
		t.Fatalf("no-op SetStatus = %v, %d events", err, len(r.events)-before)
	}
	if _, err := e.store.SetStatus(ctx, "t", f.ID, records.StatusFailed, "thumbnail: unsupported format"); err != nil {
		t.Fatal(err)
	}
	last := r.events[len(r.events)-1]
	if last.Kind != records.EventFileStatusChanged || last.File == nil || last.File.Status != records.StatusFailed || last.File.StatusDetail != "thumbnail: unsupported format" {
		t.Fatalf("event = %+v (file %+v)", last, last.File)
	}

	// Filter by status.
	g := e.put(t, "t", "b.png")
	page, err := e.store.Files(ctx, records.FileQuery{Scope: "t", Filters: []sqlb.Predicate{sqlb.Eq(records.FileStatus, records.StatusFailed)}})
	if err != nil || len(page.Files) != 1 || page.Files[0].ID != f.ID {
		t.Fatalf("failed files = %v, %v", page.Files, err)
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Filters: []sqlb.Predicate{sqlb.Eq(records.FileStatus, records.StatusReady)}}); len(page.Files) != 1 || page.Files[0].ID != g.ID {
		t.Fatalf("ready files = %v", page.Files)
	}

	// Created as processing.
	if _, err := e.disk.WriteBytes(ctx, "c.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "c.png", Status: records.StatusProcessing, StatusDetail: "queued"})
	if err != nil || c.Status != records.StatusProcessing {
		t.Fatalf("CreateFile processing = %+v, %v", c, err)
	}
	if read, _ := e.store.File(ctx, "t", c.ID); read.Status != records.StatusProcessing || read.StatusDetail != "queued" {
		t.Fatalf("created status read back %q %q", read.Status, read.StatusDetail)
	}

	for _, bad := range []struct {
		st     records.Status
		detail string
	}{
		{"done", ""},
		{records.StatusFailed, "line\nbreak"},
		{records.StatusFailed, strings.Repeat("x", records.MaxStatusDetailChars+1)},
		{records.StatusFailed, "\xff"},
	} {
		_, err := e.store.SetStatus(ctx, "t", f.ID, bad.st, bad.detail)
		wantCode(t, err, filesystem.CodeInvalidArgument)
		_, err = e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "c.png", Status: bad.st, StatusDetail: bad.detail})
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	_, err = e.store.SetStatus(ctx, "t", "fil_nope", records.StatusReady, "")
	wantCode(t, err, filesystem.CodeNotFound)
	if len(records.StatusValues) != 4 {
		t.Fatal(records.StatusValues)
	}
	r.refuse = records.EventFileStatusChanged
	if _, err := e.store.SetStatus(ctx, "t", f.ID, records.StatusReady, ""); err == nil {
		t.Fatal("a refused event was ignored")
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Status != records.StatusFailed {
		t.Fatalf("a refused status change was kept: %q", read.Status)
	}
}

func TestStatusWriteFailure(t *testing.T) {
	onlySQLite(t)
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	exec(t, e.db, "CREATE TRIGGER fail BEFORE UPDATE OF "+records.ColFileStatus+" ON "+records.TableNameFiles+" BEGIN SELECT RAISE(FAIL, 'injected'); END")
	if _, err := e.store.SetStatus(t.Context(), "t", f.ID, records.StatusFailed, ""); err == nil {
		t.Fatal("a failed write was ignored")
	}
}

// TestEventsCarryTheFile: every file and attachment event carries the file
// as the change left it.
func TestEventsCarryTheFile(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r))
	post := records.Owner{Type: "post", ID: "p1"}
	f := e.put(t, "t", "a.png")
	att, _ := e.store.Attach(ctx, "t", f.ID, post, "gallery")
	_ = e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic)
	_, _ = e.store.Replace(ctx, "t", f.ID, reader(pngData+"-v2"), "new.png", images)
	_ = e.store.Trash(ctx, "t", f.ID)
	_ = e.store.Restore(ctx, "t", f.ID)
	_ = e.store.Detach(ctx, "t", att.ID)
	_ = e.store.Reorder(ctx, "t", post, "gallery", nil)
	_ = e.store.Purge(ctx, "t", f.ID)
	checks := map[records.EventKind]func(*records.File) bool{
		records.EventFileCreated:           func(x *records.File) bool { return x.Key == "a.png" },
		records.EventAttachmentCreated:     func(x *records.File) bool { return x.ID == f.ID },
		records.EventFileVisibilityChanged: func(x *records.File) bool { return x.Visibility == filesystem.VisibilityPublic },
		records.EventFileReplaced:          func(x *records.File) bool { return x.Filename == "new.png" },
		records.EventFileTrashed:           func(x *records.File) bool { return x.DeletedAt != nil },
		records.EventFileRestored:          func(x *records.File) bool { return x.DeletedAt == nil },
		records.EventAttachmentDeleted:     func(x *records.File) bool { return x.ID == f.ID },
		records.EventFilePurged:            func(x *records.File) bool { return x.ID == f.ID },
	}
	seen := 0
	for _, ev := range r.events {
		if ev.Kind == records.EventAttachmentsOrdered {
			if ev.File != nil {
				t.Fatal("reordered carries a file")
			}
			continue
		}
		check, ok := checks[ev.Kind]
		if !ok || ev.File == nil || !check(ev.File) {
			t.Fatalf("%s: file %+v", ev.Kind, ev.File)
		}
		seen++
	}
	if seen != len(checks) {
		t.Fatalf("%d events checked, want %d", seen, len(checks))
	}
}

// TestStatusMigrationReruns replays 0003_files_status after it applied,
// as a crash between its DDL and its ledger row would make Migrate do on
// MySQL, whose DDL commits at once.
func TestStatusMigrationReruns(t *testing.T) {
	if engine() == records.EngineSQLite {
		t.Skip("SQLite applies a migration and its ledger row in one transaction")
	}
	e := newEnv(t, nil)
	exec(t, e.db, "DELETE FROM "+records.TableNameMigrations+" WHERE name = '0003_files_status'")
	done, err := e.store.Migrate(t.Context())
	if err != nil || strings.Join(done, ",") != "records/0003_files_status" {
		t.Fatalf("rerun = %v, %v", done, err)
	}
	f := e.put(t, "t", "a.png")
	if _, err := e.store.SetStatus(t.Context(), "t", f.ID, records.StatusFailed, "x"); err != nil {
		t.Fatal(err)
	}
}

// TestStatusIndex: migration 0003 creates the (scope, status) index on
// every engine.
func TestStatusIndex(t *testing.T) {
	e := newEnv(t, nil)
	q := map[string]string{
		records.EngineSQLite:   "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'fs_files_scope_status'",
		records.EnginePostgres: "SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'fs_files_scope_status'",
		records.EngineMySQL:    "SELECT COUNT(DISTINCT index_name) FROM information_schema.statistics WHERE table_schema = DATABASE() AND index_name = 'fs_files_scope_status'",
	}[engine()]
	var n int
	if err := e.db.QueryRow(q).Scan(&n); err != nil || n != 1 {
		t.Fatalf("index count = %d, %v", n, err)
	}
}
