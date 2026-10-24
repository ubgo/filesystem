package records_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestNewValidation(t *testing.T) {
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	if _, err := records.New(nil, testDialect(), mgr); err == nil {
		t.Error("nil db accepted")
	}
	if _, err := records.New(db, sqlb.ClickHouseDialect{}, mgr); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("ClickHouse = %v", err)
	}
	bad := []records.Option{
		records.WithIDs(nil),
		records.WithOwnerTypes("User"),
		records.WithPlugins(nil),
		records.WithPlugins(named("Bad Name")),
		records.WithPlugins(named(records.CoreOwner)),
		records.WithPlugins(named("p"), named("p")),
		records.WithObjectErrorHandler(nil),
	}
	for i, o := range bad {
		if _, err := records.New(db, testDialect(), mgr, o); err == nil {
			t.Errorf("bad option %d accepted", i)
		}
	}
	s, err := records.New(db, testDialect(), mgr)
	if err != nil || s.Dialect() == nil || s.Disks() != mgr {
		t.Fatalf("New = %v", err)
	}
}

type named string

func (n named) Name() string { return string(n) }

// migrator ships migrations; its statements can change between runs to
// prove the checksum check.
type migrator struct {
	named
	stmt  string
	owner string
	err   error
}

func (m *migrator) Migrations(engine string) ([]records.Migration, error) {
	if m.err != nil {
		return nil, m.err
	}
	owner := m.owner
	if owner == "" {
		owner = string(m.named)
	}
	stmt := strings.NewReplacer("{key}", records.TypesFor(engine).Key, "{int}", records.TypesFor(engine).Int).Replace(m.stmt)
	return []records.Migration{{Owner: owner, Name: "0001_plugin", Statements: []string{stmt}}}, nil
}

func TestMigrate(t *testing.T) {
	ctx := t.Context()
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	p := &migrator{named: "quota", stmt: "CREATE TABLE quota_usage (scope {key} PRIMARY KEY, bytes {int} NOT NULL)"}
	s, _ := records.New(db, testDialect(), mgr, records.WithPlugins(p, named("plain")))
	done, err := s.Migrate(ctx)
	if err != nil || strings.Join(done, ",") != "records/0001_files_and_attachments,records/0002_files_uploaded_by,records/0003_files_status,records/0004_files_object,records/0005_files_source,records/0006_files_expiry,records/0007_files_checksums,quota/0001_plugin" {
		t.Fatalf("first migrate = %v, %v", done, err)
	}
	if done, err := s.Migrate(ctx); err != nil || len(done) != 0 {
		t.Errorf("second migrate = %v, %v", done, err)
	}
	sqlText, err := s.MigrationSQL()
	if err != nil || !strings.Contains(sqlText, "CREATE TABLE IF NOT EXISTS") || !strings.Contains(sqlText, "fs_files") || !strings.Contains(sqlText, "-- quota 0001_plugin") {
		t.Errorf("MigrationSQL = %v\n%s", err, sqlText)
	}
	// Applied by hand, the script records its migrations in the ledger, so
	// Migrate then has nothing left to do.
	byHand := openDB(t)
	conn, err := byHand.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range strings.Split(sqlText, ";\n") {
		if st = strings.TrimSpace(st); st != "" {
			if _, err := conn.ExecContext(ctx, st); err != nil {
				t.Fatalf("%s: %v", st, err)
			}
		}
	}
	_ = conn.Close()
	fresh := &migrator{named: "quota", stmt: p.stmt}
	sh, _ := records.New(byHand, testDialect(), mgr, records.WithPlugins(fresh, named("plain")))
	if done, err := sh.Migrate(ctx); err != nil || len(done) != 0 {
		t.Fatalf("Migrate after the script = %v, %v", done, err)
	}
	p.stmt = "CREATE TABLE quota_usage (changed {int})"
	if _, err := s.Migrate(ctx); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Errorf("checksum drift = %v", err)
	}
	p.owner = "someone-else"
	if _, err := s.Migrate(ctx); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("foreign owner = %v", err)
	}
	if _, err := s.MigrationSQL(); err == nil {
		t.Error("MigrationSQL accepted a foreign owner")
	}
	p.owner, p.err = "", errors.New("cannot write DDL")
	if _, err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "cannot write DDL") {
		t.Errorf("plugin error = %v", err)
	}
	// A failing statement is reported and not recorded.
	db2 := openDB(t)
	broken := &migrator{named: "broken", stmt: "NOT SQL"}
	s2, _ := records.New(db2, testDialect(), mgr, records.WithPlugins(broken))
	if _, err := s2.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "broken/0001_plugin") {
		t.Errorf("bad statement = %v", err)
	}
	broken.stmt = "CREATE TABLE ok_now (x {int})"
	if done, err := s2.Migrate(ctx); err != nil || len(done) != 1 {
		t.Errorf("retry after a failed migration = %v, %v", done, err)
	}
	if got := records.TypesFor(records.EnginePostgres).Timestamp; got != "TIMESTAMPTZ" {
		t.Errorf("Postgres timestamp = %q", got)
	}
	m := records.Migration{Statements: []string{"a", "b"}}
	if m.Checksum() == (records.Migration{Statements: []string{"ab"}}).Checksum() {
		t.Error("checksum does not separate statements")
	}
}

func TestMigrateOnClosedDB(t *testing.T) {
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	s, _ := records.New(db, testDialect(), mgr)
	_ = db.Close()
	if _, err := s.Migrate(t.Context()); err == nil {
		t.Error("migrate on a closed database succeeded")
	}
}

// hooks records lifecycle calls and can veto them.
type hooks struct {
	named
	calls         []string
	failCreate    error
	failAfter     error
	failPurge     error
	setVisibility filesystem.Visibility
}

func (h *hooks) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	h.calls = append(h.calls, "before:"+f.Key)
	if h.setVisibility != "" {
		f.Visibility = h.setVisibility
	}
	if f.Metadata == nil {
		f.Metadata = map[string]string{}
	}
	f.Metadata["hooked"] = "yes"
	return h.failCreate
}

func (h *hooks) AfterCreate(_ context.Context, _ *records.Store, f records.File) error {
	h.calls = append(h.calls, "after:"+f.Key)
	return h.failAfter
}

func (h *hooks) BeforePurge(_ context.Context, _ *records.Store, f records.File) error {
	h.calls = append(h.calls, "purge:"+f.Key)
	return h.failPurge
}

func TestCreateFile(t *testing.T) {
	ctx := t.Context()
	h := &hooks{named: "audit"}
	e := newEnv(t, nil, records.WithPlugins(h))
	f := e.put(t, "tenant-1", "a/photo.png")
	if f.Size != int64(len(pngData)) || f.ContentType != "image/png" || f.Visibility != filesystem.VisibilityPrivate ||
		f.Metadata["hooked"] != "yes" || !strings.HasPrefix(f.ID, records.IDPrefixFile) || f.Filename != "photo.png" {
		t.Errorf("file = %+v", f)
	}
	got, err := e.store.File(ctx, "tenant-1", f.ID)
	if err != nil || got.Metadata["hooked"] != "yes" || !got.CreatedAt.Equal(f.CreatedAt) {
		t.Errorf("read back = %+v, %v", got, err)
	}
	if strings.Join(h.calls, ",") != "before:a/photo.png,after:a/photo.png" {
		t.Errorf("hook calls = %v", h.calls)
	}
	_, err = e.store.File(ctx, "tenant-2", f.ID)
	wantCode(t, err, filesystem.CodeNotFound)

	cases := []struct {
		in   records.NewFile
		code filesystem.Code
	}{
		{records.NewFile{Disk: "nope", Key: "a/photo.png"}, filesystem.CodeNotFound},
		{records.NewFile{Disk: "media", Key: "../x"}, filesystem.CodeInvalidKey},
		{records.NewFile{Disk: "media", Key: "missing.png"}, filesystem.CodeNotFound},
		{records.NewFile{Disk: "media", Key: "a/photo.png", Visibility: "secret"}, filesystem.CodeInvalidArgument},
	}
	for _, c := range cases {
		_, err := e.store.CreateFile(ctx, c.in)
		wantCode(t, err, c.code)
	}

	// Explicit values win over the disk's; a hook can change visibility.
	h.setVisibility = filesystem.VisibilityPublic
	f2, err := e.store.CreateFile(ctx, records.NewFile{Disk: "media", Key: "a/photo.png", ContentType: "image/x-custom", Size: 7, Metadata: map[string]string{"k": "v"}})
	if err != nil || f2.ContentType != "image/x-custom" || f2.Size != 7 || f2.Visibility != filesystem.VisibilityPublic || f2.Metadata["k"] != "v" || f2.Filename != "photo.png" {
		t.Errorf("explicit file = %+v, %v", f2, err)
	}
	h.setVisibility = ""

	// A failing hook rolls the insert back.
	h.failAfter = errors.New("quota exceeded")
	_, err = e.store.CreateFile(ctx, records.NewFile{Scope: "s", Disk: "media", Key: "a/photo.png"})
	if !errors.Is(err, h.failAfter) {
		t.Errorf("after hook error = %v", err)
	}
	h.failAfter, h.failCreate = nil, errors.New("refused")
	_, err = e.store.CreateFile(ctx, records.NewFile{Scope: "s", Disk: "media", Key: "a/photo.png"})
	if !errors.Is(err, h.failCreate) {
		t.Errorf("before hook error = %v", err)
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "s"}); len(page.Files) != 0 {
		t.Errorf("rolled-back files listed: %d", len(page.Files))
	}
}

func TestCreateFileDuplicateID(t *testing.T) {
	e := newEnv(t, nil, records.WithIDs(func(prefix string) string { return prefix + "fixed" }))
	e.put(t, "", "a.png")
	_, err := e.store.CreateFile(t.Context(), records.NewFile{Disk: "media", Key: "a.png"})
	wantCode(t, err, filesystem.CodeExists)
}

func TestFilenameIsSafeAndBounded(t *testing.T) {
	e := newEnv(t, nil)
	_, _ = e.disk.WriteBytes(t.Context(), "k", []byte("x"), filesystem.WriteOptions{})
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Disk: "media", Key: "k", Filename: "../../" + strings.Repeat("ü", 300)})
	if err != nil || len(f.Filename) > 255 || len([]rune(f.Filename)) != 127 || strings.Contains(f.Filename, "/") {
		t.Errorf("filename = %q (%d runes), %v", f.Filename, len([]rune(f.Filename)), err)
	}
}

func TestUpload(t *testing.T) {
	ctx := t.Context()
	h := &hooks{named: "audit"}
	e := newEnv(t, nil, records.WithPlugins(h))
	f, err := e.store.Upload(ctx, "t1", "media", reader(pngData), "me.png", images)
	if err != nil || f.Filename != "me.png" || len(f.SHA256) != 64 {
		t.Fatalf("Upload = %+v, %v", f, err)
	}
	if ok, _ := e.disk.Exists(ctx, f.Key); !ok {
		t.Error("uploaded object missing")
	}
	_, err = e.store.Upload(ctx, "t1", "nope", reader(pngData), "x.png", images)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.store.Upload(ctx, "t1", "media", reader("<html>"), "x.png", images)
	wantCode(t, err, filesystem.CodeRejected)

	// The row fails: the object is removed again.
	h.failCreate = errors.New("refused")
	before := countObjects(t, e.disk)
	if _, err := e.store.Upload(ctx, "t1", "media", reader(pngData), "x.png", images); !errors.Is(err, h.failCreate) {
		t.Fatalf("Upload with a failing hook = %v", err)
	}
	if countObjects(t, e.disk) != before {
		t.Error("orphan object left after a failed insert")
	}
	h.failCreate = nil

	// An enclosing transaction rolls back: the object goes too.
	rollback := errors.New("request failed later")
	err = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Upload(ctx, "t1", "media", reader(pngData), "y.png", images); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || countObjects(t, e.disk) != before {
		t.Errorf("rollback left %d objects (want %d): %v", countObjects(t, e.disk), before, err)
	}
}

func countObjects(t *testing.T, d *filesystem.Disk) int {
	t.Helper()
	n := 0
	for _, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func TestFileBySHA256(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	first, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", images)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := e.store.Upload(ctx, "t", "media", reader(pngData), "b.png", images)
	other, _ := e.store.Upload(ctx, "t", "media", reader(pngData+"-other"), "c.png", images)
	_, _ = e.store.Upload(ctx, "u", "media", reader(pngData+"-u"), "d.png", images)

	got, err := e.store.FileBySHA256(ctx, "t", strings.ToUpper(first.SHA256))
	if err != nil || got.ID != second.ID {
		t.Fatalf("FileBySHA256 = %s, %v; want the newest, %s", got.ID, err, second.ID)
	}
	if got, err := e.store.FileBySHA256(ctx, "t", other.SHA256); err != nil || got.ID != other.ID {
		t.Fatalf("other = %s, %v", got.ID, err)
	}
	// Trashed files are left out; when none is left it is not found.
	_ = e.store.Trash(ctx, "t", second.ID)
	if got, _ := e.store.FileBySHA256(ctx, "t", first.SHA256); got.ID != first.ID {
		t.Fatalf("after trashing the newest = %s, want %s", got.ID, first.ID)
	}
	_ = e.store.Trash(ctx, "t", first.ID)
	_, err = e.store.FileBySHA256(ctx, "t", first.SHA256)
	wantCode(t, err, filesystem.CodeNotFound)
	// Another scope's content is never found.
	_, err = e.store.FileBySHA256(ctx, "u", other.SHA256)
	wantCode(t, err, filesystem.CodeNotFound)
	// Not a digest: "" would otherwise match files recorded without one.
	for _, sum := range []string{"", strings.Repeat("g", 64), first.SHA256[:63]} {
		_, err = e.store.FileBySHA256(ctx, "t", sum)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	// A database failure is returned, not mistaken for not-found.
	exec(t, e.db, "DROP TABLE "+records.TableNameFiles)
	if _, err := e.store.FileBySHA256(ctx, "t", first.SHA256); err == nil || filesystem.CodeOf(err) == filesystem.CodeNotFound {
		t.Fatalf("after dropping the table: %v", err)
	}
}

func TestFilesPagination(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	var ids []string
	for i := range 5 {
		ids = append(ids, e.put(t, "t", fmt.Sprintf("f%d.png", i)).ID)
	}
	e.put(t, "other", "o.png")
	var got []string
	cursor := ""
	pages := 0
	for {
		page, err := e.store.Files(ctx, records.FileQuery{Scope: "t", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, f := range page.Files {
			got = append(got, f.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if strings.Join(got, ",") != strings.Join(want, ",") || pages != 3 {
		t.Errorf("pages %d, order %v, want %v", pages, got, want)
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t"}); len(page.Files) != 5 || page.NextCursor != "" {
		t.Errorf("default page = %d files, cursor %q", len(page.Files), page.NextCursor)
	}
	filtered, err := e.store.Files(ctx, records.FileQuery{Scope: "t", Limit: 1, Filters: []sqlb.Predicate{sqlb.In(records.FileID, ids[1], ids[3])}})
	if err != nil || len(filtered.Files) != 1 || filtered.Files[0].ID != ids[3] {
		t.Fatalf("filtered first page = %+v, %v", filtered, err)
	}
	filtered, err = e.store.Files(ctx, records.FileQuery{Scope: "t", Limit: 1, Cursor: filtered.NextCursor, Filters: []sqlb.Predicate{sqlb.In(records.FileID, ids[1], ids[3])}})
	if err != nil || len(filtered.Files) != 1 || filtered.Files[0].ID != ids[1] || filtered.NextCursor != "" {
		t.Fatalf("filtered second page = %+v, %v", filtered, err)
	}
	for _, q := range []records.FileQuery{{Limit: -1}, {Limit: records.MaxPageSize + 1}, {Cursor: "!!"}, {Cursor: "bm9zZXA"}, {Cursor: "eHw"}, {Cursor: "MTIzfA"}} {
		_, err := e.store.Files(ctx, q)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

func TestTrashRestorePurge(t *testing.T) {
	ctx := t.Context()
	h := &hooks{named: "audit"}
	e := newEnv(t, nil, records.WithPlugins(h))
	f := e.put(t, "t", "doc.png")
	user := records.Owner{Type: "user", ID: "u1"}
	if _, err := e.store.Attach(ctx, "t", f.ID, user, "avatar"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Errorf("trashing twice: %v", err)
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t"}); len(page.Files) != 0 {
		t.Error("trashed file listed as live")
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Trashed: true}); len(page.Files) != 1 || page.Files[0].DeletedAt == nil {
		t.Error("trashed file not listed in the trash")
	}
	if att, _ := e.store.Attachments(ctx, "t", user, "avatar"); len(att) != 0 {
		t.Error("trashed file still attached in listings")
	}
	if err := e.store.Restore(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if att, _ := e.store.Attachments(ctx, "t", user, "avatar"); len(att) != 1 {
		t.Error("restore lost the attachment")
	}
	wantCode(t, e.store.Trash(ctx, "t", "fil_missing"), filesystem.CodeNotFound)

	h.failPurge = errors.New("legal hold")
	if err := e.store.Purge(ctx, "t", f.ID); !errors.Is(err, h.failPurge) {
		t.Errorf("vetoed purge = %v", err)
	}
	h.failPurge = nil
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.File(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Error("purged row remains")
	}
	if ok, _ := e.disk.Exists(ctx, f.Key); ok {
		t.Error("purged object remains")
	}
	wantCode(t, e.store.Purge(ctx, "t", f.ID), filesystem.CodeNotFound)
}

func TestPurgeDeletesObjectsOnlyAfterCommit(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	rollback := errors.New("undo")
	_ = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := e.store.Purge(ctx, "t", f.ID); err != nil {
			return err
		}
		if ok, _ := e.disk.Exists(ctx, f.Key); !ok {
			t.Error("object deleted before the commit")
		}
		return rollback
	})
	if _, err := e.store.File(ctx, "t", f.ID); err != nil {
		t.Error("rolled-back purge lost the row")
	}
	if ok, _ := e.disk.Exists(ctx, f.Key); !ok {
		t.Error("rolled-back purge deleted the object")
	}
}

func TestPurgeObjectFailures(t *testing.T) {
	ctx := t.Context()
	var handled []string
	e := newEnv(t, undeletable{memory.New()}, records.WithObjectErrorHandler(func(_ context.Context, f records.File, err error) {
		handled = append(handled, f.Key+":"+string(filesystem.CodeOf(err)))
	}))
	f := e.put(t, "t", "a.png")
	if err := e.store.Purge(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("object failure outside a transaction = %v", err)
	}
	g := e.put(t, "t", "b.png")
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return e.store.Purge(ctx, "t", g.ID) })
	if err != nil || strings.Join(handled, ",") != "b.png:permission" {
		t.Errorf("object failure inside a transaction = %v, handled %v", err, handled)
	}
	// Without a handler the failure inside a transaction is dropped.
	quiet := newEnv(t, undeletable{memory.New()})
	h := quiet.put(t, "t", "c.png")
	if err := quiet.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return quiet.store.Purge(ctx, "t", h.ID) }); err != nil {
		t.Errorf("default handler = %v", err)
	}
}

func TestPurgeWithMissingDisk(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	_, _ = e.disk.WriteBytes(ctx, "x.png", []byte(pngData), filesystem.WriteOptions{})
	// Record a file on a disk the next store's manager does not have.
	f, _ := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "x.png"})
	empty, _ := filesystem.NewManager()
	other, _ := records.New(e.db, testDialect(), empty)
	wantCode(t, other.Purge(ctx, "t", f.ID), filesystem.CodeNotFound)
	_, err := other.URL(ctx, f, time.Minute)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = other.DownloadURL(ctx, f, time.Minute)
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestSweepTrash(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	old := e.put(t, "a", "old.png")
	young := e.put(t, "b", "young.png")
	live := e.put(t, "a", "live.png")
	_ = e.store.Trash(ctx, "a", old.ID)
	cutoff := e.now.Add(500 * time.Millisecond)
	e.now = e.now.Add(time.Hour)
	_ = e.store.Trash(ctx, "b", young.ID)
	n, err := e.store.SweepTrash(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("SweepTrash = %d, %v", n, err)
	}
	for _, f := range []records.File{young, live} {
		if _, err := e.store.File(ctx, f.Scope, f.ID); err != nil {
			t.Errorf("%s swept: %v", f.Key, err)
		}
	}
	// An object that cannot be deleted goes to the handler; its row is
	// purged all the same.
	var leftovers []string
	bad := newEnv(t, undeletable{memory.New()}, records.WithObjectErrorHandler(func(_ context.Context, f records.File, _ error) {
		leftovers = append(leftovers, f.Key)
	}))
	g := bad.put(t, "a", "g.png")
	_ = bad.store.Trash(ctx, "a", g.ID)
	if n, err := bad.store.SweepTrash(ctx, bad.now.Add(time.Hour)); n != 1 || err != nil || len(leftovers) != 1 || leftovers[0] != "g.png" {
		t.Errorf("sweep with an undeletable object = %d, %v, leftovers %v", n, err, leftovers)
	}
}

// refuser is a plugin refusing to purge one file, as a legal hold does;
// with cancel set, it cancels the sweep's context instead.
type refuser struct {
	id     string
	cancel func()
}

func (*refuser) Name() string                                                      { return "hold" }
func (*refuser) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }
func (*refuser) AfterCreate(context.Context, *records.Store, records.File) error   { return nil }
func (r *refuser) BeforePurge(_ context.Context, _ *records.Store, f records.File) error {
	if r.cancel != nil {
		r.cancel()
	}
	if f.ID == r.id {
		return filesystem.Errorf(filesystem.CodePermission, "on legal hold")
	}
	return nil
}

// TestSweepTrashContinuesPastFailures: a file that cannot be purged does
// not hold back the others, wherever it comes in the sweep, and the
// error names it.
func TestSweepTrashContinuesPastFailures(t *testing.T) {
	ctx := t.Context()
	for i := range 3 {
		hold := &refuser{}
		e := newEnv(t, nil, records.WithPlugins(hold))
		var files []records.File
		for _, k := range []string{"a.png", "b.png", "c.png"} {
			f := e.put(t, "t", k)
			_ = e.store.Trash(ctx, "t", f.ID)
			files = append(files, f)
		}
		held := files[i]
		hold.id = held.ID
		n, err := e.store.SweepTrash(ctx, e.now.Add(time.Hour))
		if n != 2 || filesystem.CodeOf(err) != filesystem.CodePermission || !strings.Contains(err.Error(), held.ID) {
			t.Fatalf("held %s: SweepTrash = %d, %v", held.Key, n, err)
		}
		page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Trashed: true})
		if len(page.Files) != 1 || page.Files[0].ID != held.ID {
			t.Fatalf("held %s: left in the trash %+v", held.Key, page.Files)
		}
	}

	// A cancelled context stops the sweep.
	cancelled, cancel := context.WithCancel(ctx)
	e := newEnv(t, nil, records.WithPlugins(&refuser{cancel: cancel}))
	for _, k := range []string{"a.png", "b.png"} {
		f := e.put(t, "t", k)
		_ = e.store.Trash(ctx, "t", f.ID)
	}
	if n, err := e.store.SweepTrash(cancelled, e.now.Add(time.Hour)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sweep = %d, %v", n, err)
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Trashed: true}); len(page.Files) != 2 {
		t.Fatalf("a cancelled sweep purged: %+v", page.Files)
	}
}

func TestURLs(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "pic.png")
	if u, err := e.store.URL(ctx, f, time.Minute); err != nil || !strings.HasPrefix(u, "https://app.test/files/media/pic.png") {
		t.Errorf("private URL = %q, %v", u, err)
	}
	f.Visibility = filesystem.VisibilityPublic
	if u, err := e.store.URL(ctx, f, time.Minute); err != nil || u != "https://cdn.test/pic.png" {
		t.Errorf("public URL = %q, %v", u, err)
	}
	f.Filename = "Résumé \"final\".pdf"
	u, err := e.store.DownloadURL(ctx, f, time.Minute)
	if err != nil || !strings.Contains(u, `attachment; filename="R_sum_ _final_.pdf"; filename*=UTF-8''R%C3%A9sum%C3%A9%20%22final%22.pdf`) {
		t.Errorf("download URL = %q, %v", u, err)
	}
	// A disk without signing or public URLs.
	plainDisk, _ := filesystem.NewDisk("plain", memory.New())
	mgr, _ := filesystem.NewManager(plainDisk)
	s, _ := records.New(e.db, testDialect(), mgr)
	p := records.File{Disk: "plain", Key: "k", Visibility: filesystem.VisibilityPublic}
	_, err = s.URL(ctx, p, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = s.DownloadURL(ctx, p, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestTxAndHelpers(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	ran := false
	e.store.AfterCommit(ctx, func(context.Context) { ran = true })
	if !ran {
		t.Error("AfterCommit outside a transaction did not run at once")
	}
	e.store.OnRollback(ctx, func(context.Context) { t.Error("OnRollback ran outside a transaction") })
	err := e.store.Tx(ctx, func(ctx context.Context, outer *sql.Tx) error {
		return e.store.Tx(ctx, func(ctx context.Context, inner *sql.Tx) error {
			if inner != outer {
				t.Error("nested Tx did not join")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.db.Close()
	if err := e.store.Tx(ctx, func(context.Context, *sql.Tx) error { return nil }); err == nil {
		t.Error("Tx on a closed database succeeded")
	}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("UNIQUE constraint failed: fs_files.id"), true},
		{errors.New("Error 1062 (23000): Duplicate entry"), true},
		{pgErr{"23505"}, true},
		{pgErr{"23503"}, false},
		{errors.New("other"), false},
	} {
		if got := records.IsUniqueViolation(c.err); got != c.want {
			t.Errorf("IsUniqueViolation(%v) = %v", c.err, got)
		}
	}
}

type pgErr struct{ state string }

func (p pgErr) Error() string    { return "pg error " + p.state }
func (p pgErr) SQLState() string { return p.state }

// mysqlLock is the lock name records uses on MySQL, spelled out because
// the test package cannot see it.
const mysqlLock = "CONCAT('ubgo_fs_migrate_', LEFT(SHA1(COALESCE(DATABASE(), '')), 32))"

// holdMySQLLock takes db's migration lock on a connection of its own until
// the returned release runs.
func holdMySQLLock(t *testing.T, db *sql.DB) (release func()) {
	t.Helper()
	holder, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := holder.QueryRowContext(t.Context(), "SELECT GET_LOCK("+mysqlLock+", 0)").Scan(&got); err != nil || got != 1 {
		t.Fatalf("take lock = %d, %v", got, err)
	}
	return func() {
		_, _ = holder.ExecContext(t.Context(), "SELECT RELEASE_LOCK("+mysqlLock+")")
		_ = holder.Close()
	}
}

// TestMigrationLockHeldElsewhere proves that two instances migrating one
// MySQL database at once do not both run: the second fails fast with
// CodeUnavailable while the first holds the lock. A lock held on another
// database of the same server does not get in the way.
func TestMigrationLockHeldElsewhere(t *testing.T) {
	if engine() != records.EngineMySQL {
		t.Skip("the bounded lock wait is MySQL's")
	}
	ctx := t.Context()
	db := openDB(t)
	mgr, _ := filesystem.NewManager()
	s, _ := records.New(db, testDialect(), mgr, records.WithMigrationLockWait(0))

	releaseOther := holdMySQLLock(t, openDB(t))
	defer releaseOther()
	release := holdMySQLLock(t, db)
	if _, err := s.Migrate(ctx); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("migrate while another instance holds the lock = %v", err)
	}
	release()
	if _, err := s.Migrate(ctx); err != nil {
		t.Errorf("migrate after release, with another database's lock held = %v", err)
	}
}

func TestMigrationLockWaitOption(t *testing.T) {
	mgr, _ := filesystem.NewManager()
	if _, err := records.New(openDB(t), testDialect(), mgr, records.WithMigrationLockWait(-time.Second)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("negative wait = %v", err)
	}
}
