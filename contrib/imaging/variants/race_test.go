package variants_test

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// raceEnv is an env whose disk runs a hook right after each variant object
// is written: the moment between generating a variant and recording it,
// where a concurrent Trash, Replace or SetVisibility lands.
type raceEnv struct {
	*env
	afterVariantWrite atomic.Pointer[func()]
	// beforeVariantDelete runs before each variant object deletion.
	beforeVariantDelete atomic.Pointer[func()]
}

func newRaceEnv(t *testing.T) *raceEnv {
	t.Helper()
	db, _ := recordstest.Open(t)
	r := &raceEnv{}
	hook := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpDelete && strings.HasPrefix(op.Key, variants.KeyDir+"/") {
			if fn := r.beforeVariantDelete.Load(); fn != nil {
				(*fn)()
			}
		}
		err := call(ctx)
		if op.Kind == filesystem.OpWrite && strings.HasPrefix(op.Key, variants.KeyDir+"/") {
			if fn := r.afterVariantWrite.Load(); fn != nil {
				(*fn)()
			}
		}
		return err
	}
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(hook))
	mgr, _ := filesystem.NewManager(disk)
	p, err := variants.New(transforms, imaging.Options{})
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
	r.env = &env{db: db, disk: disk, store: s, plugin: p}
	return r
}

// once runs fn after the next variant write only.
func (r *raceEnv) once(fn func()) {
	var done atomic.Bool
	wrapped := func() {
		if done.CompareAndSwap(false, true) {
			fn()
		}
	}
	r.afterVariantWrite.Store(&wrapped)
}

func (r *raceEnv) variantObjects(t *testing.T) []string {
	t.Helper()
	var keys []string
	for e, err := range r.disk.List(t.Context(), variants.KeyDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

var anything = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}

// TestGetRacingTrashLeavesNothing pins that a Trash committing between a
// variant's generation and its recording leaves neither a row (which would
// outlive the drop until purge) nor an object (possibly public) behind.
func TestGetRacingTrashLeavesNothing(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPublic)
	r.once(func() {
		if err := r.store.Trash(ctx, "t", f.ID); err != nil {
			t.Error(err)
		}
	})
	_, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
	if n := r.rows(t); n != 0 {
		t.Errorf("%d variant rows outlived the trash", n)
	}
	if keys := r.variantObjects(t); len(keys) != 0 {
		t.Errorf("variant objects left behind: %v", keys)
	}
}

// TestGetRacingReplaceServesTheNewContent pins that a variant generated
// from content replaced meanwhile is not recorded: it is generated again
// from the new content.
func TestGetRacingReplaceServesTheNewContent(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 200, 100), filesystem.VisibilityPrivate)
	r.once(func() {
		if _, err := r.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 100, 200)), "", anything); err != nil {
			t.Error(err)
		}
	})
	v, err := r.plugin.Get(ctx, r.store, "t", f.ID, "square")
	if err != nil {
		t.Fatal(err)
	}
	if v.Width != 25 || v.Height != 50 {
		t.Errorf("variant is %dx%d, want 25x50 from the replacement", v.Width, v.Height)
	}
	if n := r.rows(t); n != 1 {
		t.Errorf("rows = %d", n)
	}
}

// TestGetRacingVisibilityChangeFollowsTheFile pins that a variant written
// with the visibility the file had when it was loaded is not recorded once
// the file is made private: no public thumbnail of a private file.
func TestGetRacingVisibilityChangeFollowsTheFile(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPublic)
	r.once(func() {
		if err := r.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate); err != nil {
			t.Error(err)
		}
	})
	v, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.disk.Stat(ctx, v.Key)
	if err != nil || e.Visibility != filesystem.VisibilityPrivate {
		t.Errorf("variant object = %+v, %v; want private", e, err)
	}
}

// TestGetGivesUpOnAFileThatKeepsChanging pins the bound on regeneration and
// that the last stale object is deleted.
func TestGetGivesUpOnAFileThatKeepsChanging(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	size := 40
	always := func() {
		size++
		if _, err := r.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, size, size)), "", anything); err != nil {
			t.Error(err)
		}
	}
	r.afterVariantWrite.Store(&always)
	_, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeUnavailable)
	r.afterVariantWrite.Store(nil)
	if n := r.rows(t); n != 0 {
		t.Errorf("rows = %d", n)
	}
	if keys := r.variantObjects(t); len(keys) != 0 {
		t.Errorf("variant objects left behind: %v", keys)
	}
	// Once the file settles, the variant is made.
	if _, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
}

// TestGetRacingPurge pins that a purge landing mid-generation fails the Get
// with CodeNotFound and leaves no object.
func TestGetRacingPurge(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	r.once(func() {
		if err := r.store.Purge(ctx, "t", f.ID); err != nil {
			t.Error(err)
		}
	})
	_, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
	if keys := r.variantObjects(t); len(keys) != 0 {
		t.Errorf("variant objects left behind: %v", keys)
	}
}

// TestLosingRequestReturnsTheWinner pins the unique-violation path: another
// request records the same variant while this one is between writing and
// recording, so this one returns the winner's row and keeps the object,
// which is the winner's too.
func TestLosingRequestReturnsTheWinner(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	var winner variants.Variant
	r.once(func() {
		var err error
		if winner, err = r.plugin.Get(ctx, r.store, "t", f.ID, "thumb"); err != nil {
			t.Error(err)
		}
	})
	v, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	if err != nil || v.ID != winner.ID {
		t.Fatalf("loser got %+v, %v; want the winner %s", v, err, winner.ID)
	}
	if _, err := r.disk.Stat(ctx, v.Key); err != nil {
		t.Errorf("the winner's object is gone: %v", err)
	}
	// The loser wrote a key of its own; it is deleted, not left orphaned.
	if keys := r.variantObjects(t); len(keys) != 1 || keys[0] != v.Key {
		t.Errorf("variant objects = %v, want only the winner's %s", keys, v.Key)
	}
}

// TestStaleRequestKeepsAWinnersObject pins that discarding a stale variant
// never deletes the object a recorded variant points at: the file is made
// public and another request records the fresh variant before this one
// checks, and only the stale attempt's own object goes.
func TestStaleRequestKeepsAWinnersObject(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 200, 100), filesystem.VisibilityPrivate)
	r.once(func() {
		if err := r.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
			t.Error(err)
		}
		if _, err := r.plugin.Get(ctx, r.store, "t", f.ID, "square"); err != nil {
			t.Error(err)
		}
	})
	v, err := r.plugin.Get(ctx, r.store, "t", f.ID, "square")
	if err != nil {
		t.Fatalf("Get = %+v, %v", v, err)
	}
	if st, err := r.disk.Stat(ctx, v.Key); err != nil || st.Visibility != filesystem.VisibilityPublic {
		t.Errorf("the recorded variant's object = %+v, %v; want it kept, public", st, err)
	}
}

// TestFileGoneBeforeRetry pins that a file purged after a stale attempt
// ends the Get with the lookup's CodeNotFound instead of another attempt.
func TestFileGoneBeforeRetry(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	r.once(func() {
		if _, err := r.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 50, 50)), "", anything); err != nil {
			t.Error(err)
		}
	})
	var purged atomic.Bool
	purge := func() {
		if purged.CompareAndSwap(false, true) {
			if err := r.store.Purge(ctx, "t", f.ID); err != nil {
				t.Error(err)
			}
		}
	}
	r.beforeVariantDelete.Store(&purge)
	_, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestRowLockFailure pins that a failure to lock the file's row fails the
// Get and deletes the object it wrote.
func TestRowLockFailure(t *testing.T) {
	recordstest.OnlySQLite(t) // injects failures with SQLite triggers
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	if _, err := r.db.Exec("CREATE TRIGGER no_update BEFORE UPDATE ON " + records.TableFiles.Name + " BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb"); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("lock failure = %v", err)
	}
	if keys := r.variantObjects(t); len(keys) != 0 {
		t.Errorf("variant objects left behind: %v", keys)
	}
}

// TestGetBetweenDropAndDeleteKeepsItsObject pins that a variant generated
// after a drop committed but before the drop's deferred object deletion
// ran is not deleted by it: every generation writes a key of its own, so
// a deferred deletion only ever reaches the objects of the rows it
// dropped. (Found in review: with keys derived from the file's state, the
// Get after SetVisibility rewrote the dropped key and lost its object.)
func TestGetBetweenDropAndDeleteKeepsItsObject(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPublic)
	if _, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb"); err != nil {
		t.Fatal(err)
	}
	var fired atomic.Bool
	fn := func() {
		if fired.CompareAndSwap(false, true) {
			if _, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb"); err != nil {
				t.Error(err)
			}
		}
	}
	r.beforeVariantDelete.Store(&fn)
	if err := r.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	r.beforeVariantDelete.Store(nil)
	if !fired.Load() {
		t.Fatal("the setup never ran a Get between the drop and its deletion")
	}
	v, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.disk.Stat(ctx, v.Key); err != nil {
		t.Errorf("recorded variant %s has no object: %v", v.Key, err)
	}
}
