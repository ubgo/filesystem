package records_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// mover is a FileDiskMover that records the moves it is asked for and
// fails when fail is set.
type mover struct {
	calls []string
	fail  error
}

func (m *mover) Name() string { return "mover" }

func (m *mover) MoveFileDisk(_ context.Context, _ *records.Store, f records.File, to string) error {
	if m.fail != nil {
		return m.fail
	}
	m.calls = append(m.calls, f.Key+":"+f.Disk+"->"+to)
	return nil
}

// moveEnv is a Store over two disks: "fast" (where uploads land) and
// "media" (where they are kept), each with its own store of bytes.
type moveEnv struct {
	store       *records.Store
	db          interface{ Close() error }
	fast, media *filesystem.Disk
}

func newMoveEnv(t *testing.T, mediaOpts []filesystem.Option, opts ...records.Option) *moveEnv {
	t.Helper()
	e := &moveEnv{}
	e.fast, _ = filesystem.NewDisk("fast", memory.New())
	e.media, _ = filesystem.NewDisk("media", memory.New(), mediaOpts...)
	plain, _ := filesystem.NewDisk("plain", plainDriver{})
	mgr, _ := filesystem.NewManager(e.fast, e.media, plain)
	db := openDB(t)
	e.db = db
	var err error
	if e.store, err = records.New(db, testDialect(), mgr, opts...); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *moveEnv) record(t *testing.T, disk *filesystem.Disk, key string, in records.NewFile) records.File {
	t.Helper()
	if _, err := disk.WriteBytes(t.Context(), key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	in.Scope, in.Disk, in.Key = "t", disk.Name(), key
	f, err := e.store.CreateFile(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMoveFile(t *testing.T) {
	ctx := t.Context()
	m, r := &mover{}, &recorder{}
	e := newMoveEnv(t, nil, records.WithPlugins(m, r))
	f := e.record(t, e.fast, "a.png", records.NewFile{})

	got, err := e.store.MoveFile(ctx, "t", f.ID, "media")
	if err != nil || got.Disk != "media" || got.Key != f.Key {
		t.Fatalf("MoveFile = %+v, %v", got, err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Disk != "media" {
		t.Fatalf("row on %q", read.Disk)
	}
	if b, err := e.media.ReadAll(ctx, f.Key); err != nil || string(b) != pngData {
		t.Fatalf("object on media = %q, %v", b, err)
	}
	if ok, _ := e.fast.Exists(ctx, f.Key); ok {
		t.Fatal("the old object was kept")
	}
	if len(m.calls) != 1 || m.calls[0] != "a.png:fast->media" {
		t.Fatalf("plugin calls %v", m.calls)
	}
	var moved []records.Event
	for _, ev := range r.events {
		if ev.Kind == records.EventFileMoved {
			moved = append(moved, ev)
		}
	}
	if len(moved) != 1 || moved[0].FileID != f.ID || moved[0].File == nil || moved[0].File.Disk != "media" ||
		moved[0].Detail[records.DetailFrom] != "fast" || moved[0].Detail[records.DetailTo] != "media" {
		t.Fatalf("file.moved events %+v", moved)
	}
	// Already there: nothing to do.
	if again, err := e.store.MoveFile(ctx, "t", f.ID, "media"); err != nil || again.Disk != "media" || len(m.calls) != 1 {
		t.Fatalf("again = %+v, %v, calls %v", again, err, m.calls)
	}

	// A copy left on the target by an earlier, interrupted move is
	// overwritten by the rerun.
	g := e.record(t, e.fast, "b.png", records.NewFile{})
	if _, err := e.media.WriteBytes(ctx, g.Key, []byte("partial"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.MoveFile(ctx, "t", g.ID, "media"); err != nil {
		t.Fatalf("rerun over a left copy: %v", err)
	}
	if b, _ := e.media.ReadAll(ctx, g.Key); string(b) != pngData {
		t.Fatalf("left copy not replaced: %q", b)
	}
}

func TestMoveFileRollsBack(t *testing.T) {
	ctx := t.Context()
	m := &mover{fail: errors.New("refused")}
	e := newMoveEnv(t, nil, records.WithPlugins(m))
	f := e.record(t, e.fast, "a.png", records.NewFile{})
	if _, err := e.store.MoveFile(ctx, "t", f.ID, "media"); !errors.Is(err, m.fail) {
		t.Fatalf("MoveFile = %v", err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Disk != "fast" {
		t.Fatalf("row moved to %q", read.Disk)
	}
	if ok, _ := e.fast.Exists(ctx, f.Key); !ok {
		t.Fatal("the original object went")
	}
	if ok, _ := e.media.Exists(ctx, f.Key); ok {
		t.Fatal("the copy was kept")
	}
}

// TestMoveFileChangedDuringCopy trashes the file while its bytes are
// copied: the move is refused and the copy deleted.
func TestMoveFileChangedDuringCopy(t *testing.T) {
	ctx := t.Context()
	var e *moveEnv
	var id string
	trashOnWrite := filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite && id != "" {
			_ = e.store.Trash(ctx, "t", id)
		}
		return call(ctx)
	})
	e = newMoveEnv(t, []filesystem.Option{trashOnWrite})
	f := e.record(t, e.fast, "a.png", records.NewFile{})
	id = f.ID
	_, err := e.store.MoveFile(ctx, "t", f.ID, "media")
	wantCode(t, err, filesystem.CodeUnavailable)
	if ok, _ := e.media.Exists(ctx, f.Key); ok {
		t.Fatal("the copy was kept")
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Disk != "fast" {
		t.Fatalf("row moved to %q", read.Disk)
	}
}

func TestMoveFileRefusals(t *testing.T) {
	ctx := t.Context()
	e := newMoveEnv(t, nil)
	f := e.record(t, e.fast, "a.png", records.NewFile{})

	_, err := e.store.MoveFile(ctx, "t", f.ID, "nowhere")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.store.MoveFile(ctx, "t", "fil_nope", "media")
	wantCode(t, err, filesystem.CodeNotFound)

	// Another file already uses the key on the target.
	if _, err := e.media.WriteBytes(ctx, f.Key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: "u", Disk: "media", Key: f.Key}); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.MoveFile(ctx, "t", f.ID, "media")
	wantCode(t, err, filesystem.CodeExists)

	// A public file cannot go to a disk whose objects are all private.
	pub := e.record(t, e.fast, "pub.png", records.NewFile{Visibility: filesystem.VisibilityPublic})
	_, err = e.store.MoveFile(ctx, "t", pub.ID, "plain")
	wantCode(t, err, filesystem.CodeUnsupported)

	// A copy that does not match the row is discarded.
	bad := e.record(t, e.fast, "bad.png", records.NewFile{Size: 3})
	_, err = e.store.MoveFile(ctx, "t", bad.ID, "media")
	wantCode(t, err, filesystem.CodeUnknown)
	if ok, _ := e.media.Exists(ctx, bad.Key); ok {
		t.Fatal("a bad copy was kept")
	}

	trashed := e.record(t, e.fast, "t.png", records.NewFile{})
	_ = e.store.Trash(ctx, "t", trashed.ID)
	_, err = e.store.MoveFile(ctx, "t", trashed.ID, "media")
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestMoveFileVisibilityDisks: with WithVisibilityDisks a file may only
// move onto the disk its visibility routes to, and never off it.
func TestMoveFileVisibilityDisks(t *testing.T) {
	ctx := t.Context()
	pubDisk, _ := filesystem.NewDisk("public", memory.New())
	privDisk, _ := filesystem.NewDisk("private", memory.New())
	staging, _ := filesystem.NewDisk("staging", memory.New())
	mgr, _ := filesystem.NewManager(pubDisk, privDisk, staging)
	s, err := records.New(openDB(t), testDialect(), mgr, records.WithVisibilityDisks("public", "private"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// RoutesVisibility reports the option, for handlers that take an
	// optional disk.
	plain, err := records.New(openDB(t), testDialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if !s.RoutesVisibility() || plain.RoutesVisibility() {
		t.Fatalf("RoutesVisibility = %v with WithVisibilityDisks, %v without", s.RoutesVisibility(), plain.RoutesVisibility())
	}
	if _, err := staging.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "staging", Key: "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MoveFile(ctx, "t", f.ID, "public") // a private file
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if got, err := s.MoveFile(ctx, "t", f.ID, "private"); err != nil || got.Disk != "private" {
		t.Fatalf("to its own disk = %+v, %v", got, err)
	}
	_, err = s.MoveFile(ctx, "t", f.ID, "staging") // off the routing
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestMoveFileSetsVisibility: a copy that lands with the target disk's
// default visibility is given the file's; a failure to do so deletes it.
func TestMoveFileSetsVisibility(t *testing.T) {
	ctx := t.Context()
	failSet := false
	interceptor := filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpSetVisibility && failSet {
			return filesystem.Errorf(filesystem.CodeUnavailable, "down")
		}
		return call(ctx)
	})
	e := newMoveEnv(t, []filesystem.Option{filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), interceptor})
	plain, _ := e.store.Disks().Disk("plain")
	f := e.record(t, plain, "a.png", records.NewFile{}) // private, from a disk that reports no visibility
	failSet = true
	_, err := e.store.MoveFile(ctx, "t", f.ID, "media")
	wantCode(t, err, filesystem.CodeUnavailable)
	if ok, _ := e.media.Exists(ctx, f.Key); ok {
		t.Fatal("a copy with the wrong visibility was kept")
	}
	failSet = false
	if _, err := e.store.MoveFile(ctx, "t", f.ID, "media"); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.media.Stat(ctx, f.Key); st.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("moved object is %s", st.Visibility)
	}
}

// TestMoveFileFailures: a missing source disk or object, and every
// database step failing in turn, leave the file where it was, whole.
func TestMoveFileFailures(t *testing.T) {
	ctx := t.Context()
	e := newMoveEnv(t, nil)
	gone := e.record(t, e.fast, "gone.png", records.NewFile{})
	_ = e.fast.Delete(ctx, gone.Key)
	_, err := e.store.MoveFile(ctx, "t", gone.ID, "media")
	wantCode(t, err, filesystem.CodeNotFound)

	// A row on a disk the Manager no longer has.
	db, faults := testdb.OpenFaulty(t)
	fast, _ := filesystem.NewDisk("fast", memory.New())
	media, _ := filesystem.NewDisk("media", memory.New())
	full, _ := filesystem.NewManager(fast, media)
	s, err := records.New(db, testDialect(), full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fast.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "fast", Key: "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	onlyMedia, _ := filesystem.NewManager(media)
	without, _ := records.New(db, testDialect(), onlyMedia)
	_, err = without.MoveFile(ctx, "t", f.ID, "media")
	wantCode(t, err, filesystem.CodeNotFound)

	// Every database step of a move failing in turn.
	if _, err := fast.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	probe, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "fast", Key: "b.png"})
	if err != nil {
		t.Fatal(err)
	}
	faults.Arm(0)
	if _, err := s.MoveFile(ctx, "t", probe.ID, "media"); err != nil {
		t.Fatal(err)
	}
	steps := faults.Steps()
	for n := 1; n <= steps; n++ {
		faults.Arm(n)
		_, err := s.MoveFile(ctx, "t", f.ID, "media")
		faults.Arm(0)
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("step %d: %v", n, err)
		}
		if read, _ := s.File(ctx, "t", f.ID); read.Disk != "fast" {
			t.Fatalf("step %d: row moved to %q", n, read.Disk)
		}
		if ok, _ := media.Exists(ctx, f.Key); ok {
			t.Fatalf("step %d: the copy was kept", n)
		}
		if ok, _ := fast.Exists(ctx, f.Key); !ok {
			t.Fatalf("step %d: the original went", n)
		}
	}
}
