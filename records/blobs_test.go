package records_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

func (e *env) exists(t *testing.T, key string) bool {
	t.Helper()
	ok, err := e.disk.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (e *env) refs(t *testing.T, key string) int64 {
	t.Helper()
	n, err := e.store.References(t.Context(), "media", key)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDedupSharesOneObject: Upload with Dedup records a second file with
// the same content on the first one's object, deletes its own copy, and
// the object goes only with the last file naming it.
func TestDedupSharesOneObject(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	a, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", images, records.Dedup())
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.store.Upload(ctx, "t", "media", reader(pngData), "b.png", images, records.Dedup())
	if err != nil {
		t.Fatal(err)
	}
	if b.Key != a.Key || b.ID == a.ID || b.Filename != "b.png" || e.refs(t, a.Key) != 2 {
		t.Fatalf("dedup: a=%+v b=%+v refs=%d", a, b, e.refs(t, a.Key))
	}
	if objects := e.objectCount(t); objects != 1 {
		t.Fatalf("%d objects after a deduplicated upload, want 1", objects)
	}
	// Another scope never shares; neither does an upload without Dedup.
	other, err := e.store.Upload(ctx, "u", "media", reader(pngData), "c.png", images, records.Dedup())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := e.store.Upload(ctx, "t", "media", reader(pngData), "d.png", images)
	if err != nil {
		t.Fatal(err)
	}
	if other.Key == a.Key || plain.Key == a.Key {
		t.Fatalf("shared across scopes or without Dedup: %q %q", other.Key, plain.Key)
	}

	if err := e.store.Purge(ctx, "t", a.ID); err != nil {
		t.Fatal(err)
	}
	if !e.exists(t, b.Key) || e.refs(t, b.Key) != 1 {
		t.Fatal("purging one sharing file deleted the object the other names")
	}
	if err := e.store.Purge(ctx, "t", b.ID); err != nil {
		t.Fatal(err)
	}
	if e.exists(t, b.Key) {
		t.Fatal("the last reference was purged but the object stayed")
	}
}

func (e *env) objectCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

// TestSharedObjectAcrossChanges: replacing, and trashing, a file that
// shares its object leave the other file whole; an in-place visibility
// change is refused rather than reaching the other file.
func TestSharedObjectAcrossChanges(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	a := e.put(t, "t", "shared.png")
	b, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "shared.png"})
	if err != nil {
		t.Fatal(err)
	}
	err = e.store.SetVisibility(ctx, "t", b.ID, filesystem.VisibilityPublic)
	wantCode(t, err, filesystem.CodeConflict)
	if got, _ := e.store.File(ctx, "t", b.ID); got.Visibility != filesystem.VisibilityPrivate {
		t.Fatal("a refused visibility change reached the row")
	}
	if _, err := e.store.Replace(ctx, "t", a.ID, reader(pngData+"-v2"), "", images); err != nil {
		t.Fatal(err)
	}
	if !e.exists(t, "shared.png") {
		t.Fatal("replacing one sharing file deleted the other's object")
	}
	// No longer shared: the change goes through.
	if err := e.store.SetVisibility(ctx, "t", b.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(ctx, "t", b.ID); err != nil {
		t.Fatal(err)
	}
	if e.exists(t, "shared.png") {
		t.Fatal("the object outlived its last file")
	}
}

// TestSharedPublicObjectStaysPublicInTrash: trashing one of two public
// files sharing an object leaves it public for the live one; trashing the
// second hides it.
func TestSharedPublicObjectStaysPublicInTrash(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	if _, err := e.disk.WriteBytes(ctx, "pub.png", []byte(pngData), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 2 {
		f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "pub.png", Visibility: filesystem.VisibilityPublic})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}
	visibility := func() filesystem.Visibility {
		st, err := e.disk.Stat(ctx, "pub.png")
		if err != nil {
			t.Fatal(err)
		}
		return st.Visibility
	}
	if err := e.store.Trash(ctx, "t", ids[0]); err != nil {
		t.Fatal(err)
	}
	if visibility() != filesystem.VisibilityPublic {
		t.Fatal("trashing one sharing file hid the object of the live one")
	}
	if err := e.store.Trash(ctx, "t", ids[1]); err != nil {
		t.Fatal(err)
	}
	if visibility() != filesystem.VisibilityPrivate {
		t.Fatal("with every sharing file trashed the object stayed public")
	}
}

// TestReferenceLookupFailure: when the lookup of other references fails
// in a plugin's DeleteObjectAfterCommit, the object is kept and the
// failure reported, rather than risk deleting bytes another row needs.
// (A failed lookup inside Purge, Replace or a move fails the change; the
// fault sweeps of those operations cover it step by step.)
func TestReferenceLookupFailure(t *testing.T) {
	var reported []string
	h := newEnvOn(t, openDB(t), memory.New(), records.WithObjectErrorHandler(func(_ context.Context, f records.File, _ error) { reported = append(reported, f.Key) }))
	g := h.put(t, "t", "held.png")
	exec(t, h.db, "ALTER TABLE "+records.TableNameFiles+" RENAME TO fs_files_gone")
	h.store.DeleteObjectAfterCommit(t.Context(), g)
	if len(reported) != 1 || !h.exists(t, "held.png") {
		t.Fatalf("a failed lookup: reported %v, object kept %v", reported, h.exists(t, "held.png"))
	}
}

// TestPluginHeldObjectSurvivesPurge: an object a plugin still holds (a
// kept version naming the same key) is kept when the file is purged, and
// a plugin that cannot say fails the purge, changing nothing.
func TestPluginHeldObjectSurvivesPurge(t *testing.T) {
	ctx := t.Context()
	h := &holder{held: []string{"kept.png"}}
	e := newEnv(t, nil, records.WithPlugins(h))
	f := e.put(t, "t", "kept.png")
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if !e.exists(t, "kept.png") {
		t.Fatal("a purge deleted an object a plugin holds")
	}
	g := e.put(t, "t", "other.png")
	h.err = errors.New("cannot say")
	if err := e.store.Purge(ctx, "t", g.ID); !errors.Is(err, h.err) {
		t.Fatalf("purge with a failing holder = %v", err)
	}
	if _, err := e.store.File(ctx, "t", g.ID); err != nil || !e.exists(t, "other.png") {
		t.Fatalf("a failed purge changed something: %v", err)
	}
}

// TestNewPathsFaultSweep fails every database step of the new paths in
// turn: each failure is the injected fault, returned, never swallowed, and
// a later run succeeds.
func TestNewPathsFaultSweep(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, memory.New(), records.WithRetention(records.Retention{OwnerType: "post", MaxAge: time.Minute}))
	f := e.put(t, "t", "a.png")
	if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "dup.png", images); err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	if _, err := e.store.Attach(ctx, "t", e.put(t, "t", "r.png").ID, post, "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Attach(ctx, "t", e.put(t, "t", "s.png").ID, post, "chat"); err != nil {
		t.Fatal(err)
	}
	ops := []struct {
		name string
		run  func() error
	}{
		{"dedup upload", func() error {
			_, err := e.store.Upload(ctx, "t", "media", reader(pngData), "dup2.png", images, records.Dedup())
			return err
		}},
		{"usages", func() error {
			_, err := e.store.UsagesOf(ctx, records.UsageQuery{Scope: "t", FileID: f.ID})
			return err
		}},
		{"retention plan", func() error { _, err := e.store.SweepRetention(ctx, e.now.Add(time.Hour), true); return err }},
		{"retention", func() error {
			rep, err := e.store.SweepRetention(ctx, e.now.Add(time.Hour), false)
			if err == nil && len(rep.Detached) == 0 {
				t.Fatal("nothing was due")
			}
			return err
		}},
		{"viewed", func() error { _, err := e.store.Viewed(ctx, "t", f.ID); return err }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				faults.Arm(n)
				err := op.run()
				faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
			}
		})
	}
}

// canceller cancels its context once a detach it hears of commits.
type canceller struct{ cancel context.CancelFunc }

func (canceller) Name() string { return "canceller" }

func (c canceller) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == records.EventAttachmentDeleted {
		s.AfterCommit(ctx, func(context.Context) { c.cancel() })
	}
	return nil
}

// TestSweepRetentionCancelled: a context cancelled before or during the
// sweep stops it and is reported; what was done stays done.
func TestSweepRetentionCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	e := newEnv(t, nil, records.WithRetention(records.Retention{OwnerType: "post", MaxAge: time.Minute}), records.WithPlugins(canceller{cancel}))
	f := e.put(t, "t", "a.png")
	for _, id := range []string{"p1", "p2"} {
		if _, err := e.store.Attach(t.Context(), "t", f.ID, records.Owner{Type: "post", ID: id}, "chat"); err != nil {
			t.Fatal(err)
		}
	}
	// Both attachments of one file are due: the plan purges it once.
	plan, err := e.store.SweepRetention(t.Context(), e.now.Add(time.Hour), true)
	if err != nil || len(plan.Detached) != 2 || len(plan.Purged) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	rep, err := e.store.SweepRetention(ctx, e.now.Add(time.Hour), false)
	if !errors.Is(err, context.Canceled) || len(rep.Detached) != 1 {
		t.Fatalf("a sweep cancelled midway = %+v, %v", rep, err)
	}
	if _, err := e.store.SweepRetention(ctx, e.now.Add(time.Hour), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("a sweep cancelled before it starts = %v", err)
	}
}
