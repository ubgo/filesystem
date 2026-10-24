package variants_test

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// usageLog is a records.UsageHooks plugin that records every hold and
// release, and can refuse either, as quota would.
type usageLog struct {
	mu                      sync.Mutex
	holds, releases         []records.HeldObject
	refuseHold, failRelease error
}

func (u *usageLog) Name() string { return "usage_log" }

func (u *usageLog) OnHold(_ context.Context, _ *records.Store, o records.HeldObject) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.refuseHold != nil {
		return u.refuseHold
	}
	u.holds = append(u.holds, o)
	return nil
}

func (u *usageLog) OnRelease(_ context.Context, _ *records.Store, o records.HeldObject) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failRelease != nil {
		return u.failRelease
	}
	u.releases = append(u.releases, o)
	return nil
}

// held is the net bytes the log saw held.
func (u *usageLog) held() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	var n int64
	for _, o := range u.holds {
		n += o.Size
	}
	for _, o := range u.releases {
		n -= o.Size
	}
	return n
}

func newUsageEnv(t *testing.T) (*env, *usageLog) {
	t.Helper()
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p, err := variants.New(transforms, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	u := &usageLog{}
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p, u))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, disk: disk, store: s, plugin: p}, u
}

// TestVariantsAreHeldAndReleased pins the usage accounting: a generated
// variant is held once with its size, a cached Get holds nothing, and
// every way a variant's row goes (replace, trash, prune, purge) releases
// it, so the net held bytes always equal HeldBytes.
func TestVariantsAreHeldAndReleased(t *testing.T) {
	ctx := t.Context()
	e, u := newUsageEnv(t)
	f := e.image(t, "a.png", pngBytes(t, 80, 80), filesystem.VisibilityPrivate)
	thumb, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	square, _ := e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	if len(u.holds) != 2 {
		t.Fatalf("holds = %+v", u.holds)
	}
	want := records.HeldObject{Plugin: variants.PluginName, Scope: "t", FileID: f.ID, Disk: "media", Key: thumb.Key, Size: thumb.Size}
	if u.holds[0] != want {
		t.Errorf("hold = %+v, want %+v", u.holds[0], want)
	}
	check := func(when string) {
		t.Helper()
		n, err := e.plugin.HeldBytes(ctx, e.store, "t")
		if err != nil || n != u.held() {
			t.Errorf("%s: HeldBytes = %d, %v; usage plugins saw %d", when, n, err, u.held())
		}
		if total, _ := e.store.HeldBytes(ctx, "t"); total != n {
			t.Errorf("%s: Store.HeldBytes = %d, want %d", when, total, n)
		}
	}
	check("after generation")
	if n, _ := e.plugin.HeldBytes(ctx, e.store, "t"); n != thumb.Size+square.Size {
		t.Errorf("HeldBytes = %d, want %d", n, thumb.Size+square.Size)
	}
	if n, _ := e.plugin.HeldBytes(ctx, e.store, "other"); n != 0 {
		t.Errorf("another scope holds %d", n)
	}

	if _, err := e.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 60, 60)), "", anything); err != nil {
		t.Fatal(err)
	}
	check("after replace")
	_, _ = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	check("after trash")
	if err := e.store.Restore(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	_, _ = e.plugin.Get(ctx, e.store, "t", f.ID, "square")
	p2, _ := variants.New(map[string]imaging.Transform{"thumb": transforms["thumb"]}, imaging.Options{})
	if n, err := p2.Prune(ctx, e.store); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	check("after prune")
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	check("after purge")
	if u.held() != 0 {
		t.Errorf("%d bytes still held after purge", u.held())
	}
}

// TestRefusedHoldLeavesNothing pins that a usage plugin's refusal (quota
// at its limit) fails the Get with its code and leaves no row and no
// object.
func TestRefusedHoldLeavesNothing(t *testing.T) {
	ctx := t.Context()
	e, u := newUsageEnv(t)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	u.refuseHold = filesystem.Errorf(filesystem.CodeQuotaExceeded, "over quota")
	_, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeQuotaExceeded)
	if n := e.rows(t); n != 0 {
		t.Errorf("rows = %d", n)
	}
	n := 0
	for _, err := range e.disk.List(ctx, variants.KeyDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 0 {
		t.Errorf("%d variant objects left", n)
	}
}

// TestFailedReleaseAbortsTheChange pins that a release error fails the
// change that dropped the variants, so the count is never half updated.
func TestFailedReleaseAbortsTheChange(t *testing.T) {
	ctx := t.Context()
	e, u := newUsageEnv(t)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	u.failRelease = filesystem.Errorf(filesystem.CodeUnavailable, "counter down")
	if err := e.store.Trash(ctx, "t", f.ID); err == nil {
		t.Fatal("trash succeeded with a failed release")
	}
	if n := e.rows(t); n != 1 {
		t.Errorf("rows = %d; the trash should have rolled back", n)
	}
	p2, _ := variants.New(map[string]imaging.Transform{"square": transforms["square"]}, imaging.Options{})
	if _, err := p2.Prune(ctx, e.store); err == nil {
		t.Error("prune succeeded with a failed release")
	}
	if n := e.rows(t); n != 1 {
		t.Errorf("rows = %d; the prune should have rolled back", n)
	}
}

func TestHeldBytesFailure(t *testing.T) {
	e, _ := newUsageEnv(t)
	_, _ = e.db.Exec("DROP TABLE " + variants.TableNameVariants)
	if _, err := e.plugin.HeldBytes(t.Context(), e.store, "t"); err == nil {
		t.Error("HeldBytes ignored a failed query")
	}
}
