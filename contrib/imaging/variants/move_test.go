package variants_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// moveEnv has two disks on separate drivers: media, where files start,
// and archive, where MoveFile takes them.
type moveEnv struct {
	*env
	archive *filesystem.Disk
}

func newMoveEnv(t *testing.T, archiveOpts ...filesystem.Option) *moveEnv {
	t.Helper()
	db, _ := recordstest.Open(t)
	media, _ := filesystem.NewDisk("media", memory.New())
	archive, _ := filesystem.NewDisk("archive", memory.New(), archiveOpts...)
	mgr, _ := filesystem.NewManager(media, archive)
	p, _ := variants.New(transforms, imaging.Options{})
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &moveEnv{env: &env{db: db, disk: media, store: s, plugin: p}, archive: archive}
}

func (m *moveEnv) both(t *testing.T, id string) []variants.Variant {
	t.Helper()
	var out []variants.Variant
	for _, name := range []string{"thumb", "square"} {
		v, err := m.plugin.Get(t.Context(), m.store, "t", id, name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func exists(t *testing.T, d *filesystem.Disk, key string) bool {
	t.Helper()
	ok, err := d.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestMoveFileTakesTheVariants pins that Store.MoveFile carries the
// variants: rows on the new disk, objects copied there and deleted from
// the old one, and the same variants served afterwards.
func TestMoveFileTakesTheVariants(t *testing.T) {
	m := newMoveEnv(t)
	f := m.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	before := m.both(t, f.ID)
	if _, err := m.store.MoveFile(t.Context(), "t", f.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	after := m.both(t, f.ID)
	for i, v := range after {
		if v.ID != before[i].ID || v.Disk != "archive" || !exists(t, m.archive, v.Key) || exists(t, m.disk, v.Key) {
			t.Errorf("%s: %+v (before %+v)", v.Name, v, before[i])
		}
	}
	// Moving again is a no-op.
	if _, err := m.store.MoveFile(t.Context(), "t", f.ID, "archive"); err != nil {
		t.Errorf("second move = %v", err)
	}
}

// TestFailedMoveLeavesTheVariants pins the rollback: a copy that fails
// part-way leaves every row and object where it was and nothing on the
// target.
func TestFailedMoveLeavesTheVariants(t *testing.T) {
	failSquare := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite && strings.Contains(op.Key, "/square-") {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	}
	m := newMoveEnv(t, filesystem.WithInterceptors(failSquare))
	f := m.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	before := m.both(t, f.ID)
	if _, err := m.store.MoveFile(t.Context(), "t", f.ID, "archive"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("move = %v", err)
	}
	for _, v := range m.both(t, f.ID) {
		if v.Disk != "media" || !exists(t, m.disk, v.Key) || exists(t, m.archive, v.Key) {
			t.Errorf("%s after a failed move: %+v", v.Name, v)
		}
	}
	_ = before
}

// TestCancelledMoveRemovesTheCopies pins, through the plugin, the
// Store's promise that rollback work runs without the request's
// cancellation: the copies made before a cancelled move are removed.
func TestCancelledMoveRemovesTheCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelOnSquare := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite && strings.Contains(op.Key, "/square-") {
			cancel()
			return context.Canceled
		}
		return call(ctx)
	}
	m := newMoveEnv(t, filesystem.WithInterceptors(cancelOnSquare))
	f := m.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	m.both(t, f.ID) // variants are made on first request
	if _, err := m.store.MoveFile(ctx, "t", f.ID, "archive"); err == nil {
		t.Fatal("the cancelled move succeeded")
	}
	for _, v := range m.both(t, f.ID) {
		if exists(t, m.archive, v.Key) {
			t.Errorf("%s: the copy outlived the cancelled move", v.Name)
		}
	}
}

// TestMoveRefusesAChangedVariant pins the size check: an object that no
// longer matches its row fails the move rather than carrying it over.
func TestMoveRefusesAChangedVariant(t *testing.T) {
	m := newMoveEnv(t)
	f := m.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	vs := m.both(t, f.ID)
	if _, err := m.disk.WriteBytes(t.Context(), vs[0].Key, []byte("tampered"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.MoveFile(t.Context(), "t", f.ID, "archive"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("move = %v", err)
	}
	if exists(t, m.archive, vs[0].Key) {
		t.Error("the rolled-back copy stayed on the target")
	}
}

// TestMoveFailures covers a variant listing failure and a variant whose
// row names another disk (left alone).
func TestMoveFailures(t *testing.T) {
	recordstest.OnlySQLite(t) // edits rows directly
	m := newMoveEnv(t)
	f := m.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	vs := m.both(t, f.ID)
	if _, err := m.db.Exec("UPDATE "+variants.TableNameVariants+" SET disk = 'elsewhere' WHERE id = ?", vs[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.MoveFile(t.Context(), "t", f.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	var disk string
	_ = m.db.QueryRow("SELECT disk FROM "+variants.TableNameVariants+" WHERE id = ?", vs[1].ID).Scan(&disk)
	if disk != "elsewhere" || exists(t, m.archive, vs[1].Key) {
		t.Errorf("a variant on another disk was moved (row %q)", disk)
	}
	g := m.image(t, "b.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	m.both(t, g.ID)
	// Unknown disks, as a caller other than Store.MoveFile could pass.
	if err := m.plugin.MoveFileDisk(t.Context(), m.store, records.File{Scope: "t", ID: g.ID, Disk: "nope"}, "archive"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("unknown source disk = %v", err)
	}
	if err := m.plugin.MoveFileDisk(t.Context(), m.store, g, "nope"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("unknown target disk = %v", err)
	}
	// The row update failing rolls the move back.
	if _, err := m.db.Exec("CREATE TRIGGER no_move BEFORE UPDATE ON " + variants.TableNameVariants + " BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.MoveFile(t.Context(), "t", g.ID, "archive"); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("row update failure = %v", err)
	}
	_, _ = m.db.Exec("DROP TABLE " + variants.TableNameVariants)
	if _, err := m.store.MoveFile(t.Context(), "t", g.ID, "archive"); err == nil {
		t.Error("move ignored a listing failure")
	}
}
