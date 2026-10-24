package variants_test

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

var transforms = map[string]imaging.Transform{
	"thumb":  {Mode: imaging.ModeFill, Width: 32, Height: 32, Format: imaging.FormatJPEG},
	"square": {Mode: imaging.ModeFit, Width: 50, Height: 50, Format: imaging.FormatPNG},
}

type env struct {
	db     *sql.DB
	disk   *filesystem.Disk
	store  *records.Store
	plugin *variants.Plugin
}

func newEnv(t *testing.T, drv filesystem.Driver, tr map[string]imaging.Transform) *env {
	t.Helper()
	db, _ := recordstest.Open(t)
	if drv == nil {
		drv = memory.New()
	}
	disk, _ := filesystem.NewDisk("media", drv, filesystem.WithPublicURL("https://cdn.test"), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	p, err := variants.New(tr, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, disk: disk, store: s, plugin: p}
}

type signer struct{}

func (signer) SignURL(_ context.Context, disk, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: o.Method, URL: "https://app.test/" + disk + "/" + key}, nil
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func (e *env) image(t *testing.T, key string, data []byte, v filesystem.Visibility) records.File {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), key, data, filesystem.WriteOptions{Visibility: v}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

func TestGenerateAndCache(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "photos/a.png", pngBytes(t, 200, 100), filesystem.VisibilityPrivate)
	v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if v.Width != 32 || v.Height != 32 || v.ContentType != "image/jpeg" || !strings.HasPrefix(v.Key, variants.KeyDir+"/"+f.ID+"/thumb-") || !strings.HasPrefix(v.ID, variants.IDPrefix) {
		t.Errorf("variant = %+v", v)
	}
	if ok, _ := e.disk.Exists(ctx, v.Key); !ok {
		t.Error("variant object missing")
	}
	again, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil || again.ID != v.ID {
		t.Errorf("second Get regenerated: %v / %v", again.ID, err)
	}
	sq, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	if sq.Width != 50 || sq.Height != 25 || sq.ContentType != "image/png" {
		t.Errorf("square = %+v", sq)
	}
	u, err := e.plugin.URL(ctx, e.store, "t", f.ID, "thumb", time.Minute)
	if err != nil || u != "https://app.test/media/"+v.Key {
		t.Errorf("private URL = %q, %v", u, err)
	}
	pubFile := e.image(t, "photos/pub.png", pngBytes(t, 40, 40), filesystem.VisibilityPublic)
	if pubFile.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("visibility not taken from the disk: %q", pubFile.Visibility)
	}
	if u, err := e.plugin.URL(ctx, e.store, "t", pubFile.ID, "thumb", time.Minute); err != nil || !strings.HasPrefix(u, "https://cdn.test/variants/") {
		t.Errorf("public URL = %q, %v", u, err)
	}
	if e.plugin.Name() != variants.PluginName {
		t.Error("name")
	}
}

func TestConcurrentFirstRequests(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 100, 100), filesystem.VisibilityPrivate)
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = v.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("concurrent requests returned different variants: %v", ids)
			break
		}
	}
}

func TestPurgeRemovesVariants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 80, 80), filesystem.VisibilityPrivate)
	v, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.disk.Exists(ctx, v.Key); ok {
		t.Error("variant object survived the purge")
	}
	var n int
	_ = e.db.QueryRow("SELECT COUNT(*) FROM " + variants.TableNameVariants).Scan(&n)
	if n != 0 {
		t.Errorf("%d variant rows survived the purge", n)
	}
}

// TestReplaceDropsVariants proves a replaced image's variants are made
// again from the new content.
func TestReplaceDropsVariants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 80, 80), filesystem.VisibilityPrivate)
	old, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	policy := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	if _, err := e.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 100, 20)), "", policy); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.disk.Exists(ctx, old.Key); ok {
		t.Error("the old variant's object survived the replacement")
	}
	fresh, err := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	if err != nil || fresh.ID == old.ID || fresh.Width != 50 || fresh.Height != 10 {
		t.Fatalf("variant after replace = %+v, %v (before: %dx%d)", fresh, err, old.Width, old.Height)
	}
	// A file made public or private gets its variants made again, with its
	// new visibility.
	public, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.disk.Exists(ctx, public.Key); ok {
		t.Error("a variant kept its old visibility")
	}
	again, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	if st, _ := e.disk.Stat(ctx, again.Key); st.Visibility != filesystem.VisibilityPublic {
		t.Errorf("regenerated variant visibility = %s", st.Visibility)
	}
	fresh = again

	// Other events leave variants alone.
	if err := e.plugin.OnEvent(ctx, e.store, records.Event{Kind: records.EventAttachmentCreated, Scope: "t", FileID: f.ID}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.disk.Exists(ctx, fresh.Key); !ok {
		t.Error("attaching dropped the variants")
	}
}

// TestTrashDropsVariants proves a trashed file's variants stop answering:
// their rows and objects go with the trash (a public thumbnail would
// otherwise stay reachable at its public URL), Get, URL and the handler
// refuse, and a restore makes them again.
func TestTrashDropsVariants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 80, 80), filesystem.VisibilityPublic)
	v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.disk.Exists(ctx, v.Key); ok {
		t.Error("a trashed file's variant object is still stored")
	}
	if n := e.rows(t); n != 0 {
		t.Errorf("%d variant rows survived the trash", n)
	}
	_, err = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.plugin.URL(ctx, e.store, "t", f.ID, "thumb", time.Minute)
	wantCode(t, err, filesystem.CodeNotFound)

	if err := e.store.Restore(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	back, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil || back.ID == v.ID {
		t.Fatalf("variant after restore = %+v, %v", back, err)
	}
	if ok, _ := e.disk.Exists(ctx, back.Key); !ok {
		t.Error("restored variant object missing")
	}
}

// TestTrashedRowNeverServed proves Get checks the file before the variant
// table: a row left from before the trash (generated concurrently with
// Trash, or by a version that kept variants of trashed files) is refused.
func TestTrashedRowNeverServed(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	// Trash through a Store without the plugin, so the variant row stays,
	// as one written concurrently with Trash would.
	mgr, _ := filesystem.NewManager(e.disk)
	bare, err := records.New(e.db, recordstest.Dialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if e.rows(t) != 1 {
		t.Fatal("the setup lost the variant row")
	}
	_, err = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
}

func (e *env) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM " + variants.TableNameVariants).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPrune(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 80, 80), filesystem.VisibilityPrivate)
	old, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	_, _ = e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	changed := map[string]imaging.Transform{"thumb": {Mode: imaging.ModeFill, Width: 48, Height: 48, Format: imaging.FormatJPEG}}
	p2, _ := variants.New(changed, imaging.Options{})
	n, err := p2.Prune(ctx, e.store)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if ok, _ := e.disk.Exists(ctx, old.Key); ok {
		t.Error("stale variant object kept")
	}
	fresh, err := p2.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil || fresh.Width != 48 {
		t.Errorf("regenerated = %+v, %v", fresh, err)
	}
	if n, _ := p2.Prune(ctx, e.store); n != 0 {
		t.Errorf("second prune removed %d", n)
	}
}

func TestRefusals(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 20, 20), filesystem.VisibilityPrivate)
	_, err := e.plugin.Get(ctx, e.store, "t", f.ID, "huge")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.plugin.Get(ctx, e.store, "t", "fil_missing", "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.plugin.URL(ctx, e.store, "t", "fil_missing", "thumb", time.Minute)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.plugin.URL(ctx, e.store, "t", f.ID, "huge", time.Minute)
	wantCode(t, err, filesystem.CodeNotFound)
	_ = e.store.Trash(ctx, "t", f.ID)
	_, err = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)

	_, _ = e.disk.WriteBytes(ctx, "doc.txt", []byte("hello"), filesystem.WriteOptions{})
	txt, _ := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "doc.txt"})
	_, err = e.plugin.Get(ctx, e.store, "t", txt.ID, "thumb")
	wantCode(t, err, filesystem.CodeRejected)

	// Declared an image, but the bytes are not one.
	_, _ = e.disk.WriteBytes(ctx, "fake.png", []byte("not an image"), filesystem.WriteOptions{ContentType: "image/png"})
	fake, _ := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "fake.png", ContentType: "image/png"})
	_, err = e.plugin.Get(ctx, e.store, "t", fake.ID, "thumb")
	wantCode(t, err, filesystem.CodeRejected)

	for _, bad := range []map[string]imaging.Transform{
		nil,
		{"Bad Name": transforms["thumb"]},
		{"ok": {Mode: "stretch", Width: 1, Height: 1, Format: imaging.FormatPNG}},
	} {
		if _, err := variants.New(bad, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%v) = %v", bad, err)
		}
	}
}

// unreadable fails Read; readonly refuses writes after the original exists.
type unreadable struct{ *memory.Driver }

func (unreadable) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return nil, filesystem.ErrUnavailable
}

type writeOnce struct {
	*memory.Driver
	writes int
}

func (w *writeOnce) Write(ctx context.Context, k string, b io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	w.writes++
	if w.writes > 1 {
		return filesystem.Entry{}, filesystem.ErrPermission
	}
	return w.Driver.Write(ctx, k, b, o)
}

func TestStorageFailures(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, unreadable{memory.New()}, transforms)
	f := e.image(t, "a.png", pngBytes(t, 20, 20), filesystem.VisibilityPrivate)
	_, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeUnavailable)

	e = newEnv(t, &writeOnce{Driver: memory.New()}, transforms)
	f = e.image(t, "a.png", pngBytes(t, 20, 20), filesystem.VisibilityPrivate)
	_, err = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodePermission)

	// The file's disk is no longer configured.
	e = newEnv(t, nil, transforms)
	f = e.image(t, "a.png", pngBytes(t, 20, 20), filesystem.VisibilityPrivate)
	empty, _ := filesystem.NewManager()
	other, _ := records.New(e.db, recordstest.Dialect(), empty, records.WithPlugins(e.plugin))
	_, err = e.plugin.Get(ctx, other, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestDatabaseFailures(t *testing.T) {
	recordstest.OnlySQLite(t) // injects failures with SQLite triggers
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 20, 20), filesystem.VisibilityPrivate)
	if _, err := e.db.Exec("CREATE TRIGGER no_insert BEFORE INSERT ON " + variants.TableNameVariants + " BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("insert failure = %v", err)
	}
	_, _ = e.db.Exec("DROP TRIGGER no_insert")
	_, _ = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if _, err := e.db.Exec("CREATE TRIGGER no_delete BEFORE DELETE ON " + variants.TableNameVariants + " BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(ctx, "t", f.ID); err == nil {
		t.Error("variant delete failure ignored by purge")
	}
	p2, _ := variants.New(map[string]imaging.Transform{"other": transforms["square"]}, imaging.Options{})
	if _, err := p2.Prune(ctx, e.store); err == nil {
		t.Error("prune ignored a delete failure")
	}
	_, _ = e.db.Exec("DROP TABLE " + variants.TableNameVariants)
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); err == nil {
		t.Error("lookup failure ignored")
	}
	if err := e.store.Purge(ctx, "t", f.ID); err == nil {
		t.Error("purge ignored a variant listing failure")
	}
	if _, err := p2.Prune(ctx, e.store); err == nil {
		t.Error("prune ignored a listing failure")
	}
}

// migrationGolden is the SQL the migrations render per engine, built by
// records.DDL from the typed columns. A change here changes the schema of
// every database that runs them: review it as a migration.
var migrationGolden = map[string][]string{
	records.EngineSQLite: {
		"0001_variants: CREATE TABLE IF NOT EXISTS \"fs_variants\" (\"id\" TEXT NOT NULL, \"scope\" TEXT NOT NULL, \"file_id\" TEXT NOT NULL, \"name\" TEXT NOT NULL, \"digest\" TEXT NOT NULL, \"disk\" TEXT NOT NULL, \"storage_key\" TEXT NOT NULL, \"content_type\" TEXT NOT NULL, \"size\" INTEGER NOT NULL, \"width\" INTEGER NOT NULL, \"height\" INTEGER NOT NULL, \"created_at\" TIMESTAMP NOT NULL, PRIMARY KEY (\"id\"))",
		"0001_variants: CREATE UNIQUE INDEX IF NOT EXISTS \"fs_variants_file_name_digest\" ON \"fs_variants\" (\"file_id\", \"name\", \"digest\")",
		"0002_variants_scope: CREATE INDEX IF NOT EXISTS \"fs_variants_scope\" ON \"fs_variants\" (\"scope\")",
	},
	records.EnginePostgres: {
		"0001_variants: CREATE TABLE IF NOT EXISTS \"fs_variants\" (\"id\" TEXT NOT NULL, \"scope\" TEXT NOT NULL, \"file_id\" TEXT NOT NULL, \"name\" TEXT NOT NULL, \"digest\" TEXT NOT NULL, \"disk\" TEXT NOT NULL, \"storage_key\" TEXT NOT NULL, \"content_type\" TEXT NOT NULL, \"size\" BIGINT NOT NULL, \"width\" BIGINT NOT NULL, \"height\" BIGINT NOT NULL, \"created_at\" TIMESTAMPTZ NOT NULL, PRIMARY KEY (\"id\"))",
		"0001_variants: CREATE UNIQUE INDEX IF NOT EXISTS \"fs_variants_file_name_digest\" ON \"fs_variants\" (\"file_id\", \"name\", \"digest\")",
		"0002_variants_scope: CREATE INDEX IF NOT EXISTS \"fs_variants_scope\" ON \"fs_variants\" (\"scope\")",
	},
	records.EngineMySQL: {
		"0001_variants: CREATE TABLE IF NOT EXISTS `fs_variants` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `name` VARCHAR(64) NOT NULL, `digest` VARCHAR(64) NOT NULL, `disk` VARCHAR(64) NOT NULL, `storage_key` VARCHAR(1024) NOT NULL, `content_type` VARCHAR(255) NOT NULL, `size` BIGINT NOT NULL, `width` BIGINT NOT NULL, `height` BIGINT NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `fs_variants_file_name_digest` UNIQUE (`file_id`, `name`, `digest`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`",
		// MySQL has no CREATE INDEX IF NOT EXISTS and commits DDL at once,
		// so the index is created only when the catalogue lacks it: a rerun
		// after a crash before the ledger write succeeds.
		"0002_variants_scope: SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'fs_variants' AND index_name = 'fs_variants_scope'), 'DO 0', 'CREATE INDEX `fs_variants_scope` ON `fs_variants` (`scope`)')",
		"0002_variants_scope: PREPARE fs_ddl FROM @fs_ddl",
		"0002_variants_scope: EXECUTE fs_ddl",
		"0002_variants_scope: DEALLOCATE PREPARE fs_ddl",
	},
}

func TestMigrationsPerEngine(t *testing.T) {
	p, _ := variants.New(transforms, imaging.Options{})
	for engine, want := range migrationGolden {
		ms, err := p.Migrations(engine)
		if err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		var got []string
		for _, m := range ms {
			if m.Owner != variants.PluginName {
				t.Errorf("%s: owner %q", engine, m.Owner)
			}
			for _, st := range m.Statements {
				got = append(got, m.Name+": "+st)
			}
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s migrations changed:\n%s", engine, strings.Join(got, "\n"))
		}
	}
}

// TestSweepOrphansKeepsVariants proves the plugin claims its objects, so
// records' orphan sweep deletes a real orphan but never a variant.
func TestSweepOrphansKeepsVariants(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.disk.WriteBytes(ctx, "orphan.bin", []byte("x"), filesystem.WriteOptions{})
	n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("SweepOrphans = %d, %v", n, err)
	}
	if ok, _ := e.disk.Exists(ctx, v.Key); !ok {
		t.Error("the sweep deleted a variant")
	}
	if ok, _ := e.disk.Exists(ctx, "orphan.bin"); ok {
		t.Error("the sweep kept the orphan")
	}

	owned, err := e.plugin.OwnedObjects(ctx, e.store, "media", nil)
	if err != nil || len(owned) != 0 {
		t.Errorf("no keys = %v, %v", owned, err)
	}
	if owned, _ := e.plugin.OwnedObjects(ctx, e.store, "other", []string{v.Key}); owned[v.Key] {
		t.Error("a variant claimed on another disk")
	}
	_, _ = e.db.Exec("DROP TABLE " + variants.TableNameVariants)
	if _, err := e.plugin.OwnedObjects(ctx, e.store, "media", []string{v.Key}); err == nil {
		t.Error("a lookup failure was ignored")
	}
}
