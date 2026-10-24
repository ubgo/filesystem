package records_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// splitEnv is a Store over a public and a private disk.
type splitEnv struct {
	store     *records.Store
	pub, priv *filesystem.Disk
	db        *sql.DB
}

func newSplit(t *testing.T, opts ...records.Option) *splitEnv {
	t.Helper()
	e := &splitEnv{db: openDB(t)}
	e.pub, _ = filesystem.NewDisk("cdn", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	e.priv, _ = filesystem.NewDisk("vault", memory.New())
	mgr, _ := filesystem.NewManager(e.priv, e.pub)
	var err error
	e.store, err = records.New(e.db, testDialect(), mgr, append([]records.Option{records.WithVisibilityDisks("cdn", "vault")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func keys(t *testing.T, d *filesystem.Disk) int { t.Helper(); return countObjects(t, d) }

func TestVisibilityDisks(t *testing.T) {
	ctx := t.Context()
	e := newSplit(t)
	public := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := e.store.Upload(ctx, "t", "", strings.NewReader(pngData), "a.png", public)
	if err != nil || f.Disk != "cdn" || f.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("public upload = %+v, %v", f, err)
	}
	g, err := e.store.Upload(ctx, "t", "", strings.NewReader(pngData), "b.png", pngPolicy) // no visibility: private
	if err != nil || g.Disk != "vault" {
		t.Fatalf("private upload = %+v, %v", g, err)
	}

	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	moved, _ := e.store.File(ctx, "t", f.ID)
	if moved.Disk != "vault" || moved.Visibility != filesystem.VisibilityPrivate || keys(t, e.pub) != 0 || keys(t, e.priv) != 2 {
		t.Fatalf("after making it private: %+v, public %d, private %d", moved, keys(t, e.pub), keys(t, e.priv))
	}
	if got, _ := e.priv.ReadAll(ctx, moved.Key); string(got) != pngData {
		t.Fatalf("moved content = %q", got)
	}
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatalf("the current visibility again: %v", err)
	}
	wantCode(t, e.store.SetVisibility(ctx, "t", f.ID, "secret"), filesystem.CodeInvalidArgument)
	wantCode(t, e.store.SetVisibility(ctx, "t", "fil_missing", filesystem.VisibilityPublic), filesystem.CodeNotFound)

	// A rolled-back move leaves the file where it was and no copy behind.
	boom := errors.New("later step failed")
	err = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := e.store.SetVisibility(ctx, "t", g.ID, filesystem.VisibilityPublic); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || keys(t, e.pub) != 0 {
		t.Fatalf("rolled-back move: %v, public %d", err, keys(t, e.pub))
	}
	if still, _ := e.store.File(ctx, "t", g.ID); still.Disk != "vault" {
		t.Fatalf("row after rollback = %+v", still)
	}
}

func TestVisibilityInPlace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil) // memory stores per-object visibility
	f := e.put(t, "t", "a.png")
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	row, _ := e.store.File(ctx, "t", f.ID)
	obj, _ := e.disk.Stat(ctx, f.Key)
	if row.Visibility != filesystem.VisibilityPublic || obj.Visibility != filesystem.VisibilityPublic || row.Disk != f.Disk {
		t.Fatalf("row %s, object %s", row.Visibility, obj.Visibility)
	}
	if _, err := e.store.Upload(ctx, "t", "", strings.NewReader(pngData), "x.png", pngPolicy); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an upload without a disk and without routing = %v", err)
	}

	plain := newEnv(t, noVisibility{memory.New()})
	g := plain.put(t, "t", "b.png")
	wantCode(t, plain.store.SetVisibility(ctx, "t", g.ID, filesystem.VisibilityPublic), filesystem.CodeUnsupported)
	if row, _ := plain.store.File(ctx, "t", g.ID); row.Visibility != g.Visibility {
		t.Fatal("a refused change reached the row")
	}
}

func TestVisibilityDisksConfig(t *testing.T) {
	mgr, _ := filesystem.NewManager()
	for _, o := range []records.Option{records.WithVisibilityDisks("", "b"), records.WithVisibilityDisks("a", "a"), records.WithVisibilityDisks("a", "b")} {
		if _, err := records.New(openDB(t), testDialect(), mgr, o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("config accepted: %v", err)
		}
	}
}

// noVisibility is the memory driver without per-object visibility.
type noVisibility struct{ *memory.Driver }

func (n noVisibility) Capabilities() filesystem.Capabilities {
	c := n.Driver.Capabilities()
	c.SetVisibility = false
	return c
}

// TestVisibilityStorageSweep fails every storage operation of a move
// between the public and private disks: the file stays where it was, with
// nothing left on the other disk.
func TestVisibilityStorageSweep(t *testing.T) {
	ctx := t.Context()
	var failAt, seen int // failAt 0 never fails
	fail := filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		seen++
		if seen == failAt {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	})
	pub, _ := filesystem.NewDisk("cdn", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), fail)
	priv, _ := filesystem.NewDisk("vault", memory.New(), fail)
	mgr, _ := filesystem.NewManager(priv, pub)
	var orphans []string
	s, err := records.New(openDB(t), testDialect(), mgr, records.WithVisibilityDisks("cdn", "vault"),
		records.WithObjectErrorHandler(func(_ context.Context, f records.File, _ error) { orphans = append(orphans, f.Disk+":"+f.Key) }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := s.Upload(ctx, "t", "", strings.NewReader(pngData), "a.png", pngPolicy)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; ; n++ {
		failAt, seen = n, 0
		err := s.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic)
		failAt = 0
		row, _ := s.File(ctx, "t", f.ID)
		if err == nil {
			// The first run that succeeds is the one whose injected failure
			// hit the old object's deletion after the commit: the move
			// stands, and the leftover is reported, not hidden.
			if row.Disk != "cdn" || keys(t, pub) != 1 || keys(t, priv) != 1 || len(orphans) != 1 || orphans[0] != "vault:"+f.Key {
				t.Fatalf("after the move: %+v, public %d, private %d, orphans %v", row, keys(t, pub), keys(t, priv), orphans)
			}
			return
		}
		if row.Disk != "vault" || keys(t, pub) != 0 || keys(t, priv) != 1 {
			t.Fatalf("storage failure %d: row %+v, public %d, private %d", n, row, keys(t, pub), keys(t, priv))
		}
	}
}

// TestVisibilityMissingDisk changes a file whose disk the store no longer
// has, with and without routing.
func TestVisibilityMissingDisk(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	empty, _ := filesystem.NewManager()
	plain, _ := records.New(e.db, testDialect(), empty)
	wantCode(t, plain.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic), filesystem.CodeNotFound)
	pub, _ := filesystem.NewDisk("cdn", memory.New())
	priv, _ := filesystem.NewDisk("vault", memory.New())
	mgr, _ := filesystem.NewManager(pub, priv)
	routed, _ := records.New(e.db, testDialect(), mgr, records.WithVisibilityDisks("cdn", "vault"))
	wantCode(t, routed.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic), filesystem.CodeNotFound)
}
