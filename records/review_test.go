package records_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// refuseCreate is a FileHooks plugin refusing the next file it is
// offered, as a quota at its limit does.
type refuseCreate struct{ on bool }

func (*refuseCreate) Name() string { return "refuse_create" }
func (r *refuseCreate) BeforeCreate(context.Context, *records.Store, *records.File) error {
	if r.on {
		return filesystem.Errorf(filesystem.CodeQuotaExceeded, "full")
	}
	return nil
}
func (*refuseCreate) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (*refuseCreate) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// TestContentAddressedUploads: with content-addressed keys a second upload
// of the same bytes lands on the first file's object; neither Dedup nor a
// refused or rolled-back upload may delete that object.
func TestContentAddressedUploads(t *testing.T) {
	ctx := t.Context()
	refuse := &refuseCreate{}
	e := newEnv(t, nil, records.WithPlugins(refuse))
	ca := images
	ca.Key, ca.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	a, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", ca)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.store.Upload(ctx, "t", "media", reader(pngData), "b.png", ca, records.Dedup())
	if err != nil {
		t.Fatal(err)
	}
	if b.Key != a.Key || !e.exists(t, a.Key) {
		t.Fatalf("Dedup on a content-addressed key: b=%q, object kept %v", b.Key, e.exists(t, a.Key))
	}
	refuse.on = true
	if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "c.png", ca); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("refused upload = %v", err)
	}
	if !e.exists(t, a.Key) {
		t.Fatal("a refused upload deleted the object two files name")
	}
	refuse.on = false
	boom := errors.New("later step failed")
	err = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "d.png", ca); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || !e.exists(t, a.Key) {
		t.Fatalf("a rolled-back upload: %v, object kept %v", err, e.exists(t, a.Key))
	}
	// Even with nothing naming it, a refused content-addressed upload's
	// object stays (another upload in flight may share its key); it is an
	// orphan SweepOrphans removes once older than its grace.
	refuse.on = true
	unique := pngData + "-unique"
	if _, err := e.store.Upload(ctx, "t", "media", reader(unique), "u.png", ca); err == nil {
		t.Fatal("not refused")
	}
	if n := e.objectCount(t); n != 2 {
		t.Fatalf("%d objects after a refused unique upload, want 2", n)
	}
	if n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("SweepOrphans = %d, %v; want the refused upload's object", n, err)
	}
}

// settleThen wraps a Settler to run then right after it settled, as a
// second upload interleaving with this one.
func settleThen(settle upload.Settler, then func()) upload.Settler {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) (upload.Result, error) {
		r, err := settle(ctx, disk, r)
		if then != nil {
			run := then
			then = nil
			run()
		}
		return r, err
	}
}

// refuseNamed is a FileHooks plugin refusing files of one name.
type refuseNamed struct{ name string }

func (*refuseNamed) Name() string { return "refuse_named" }
func (r *refuseNamed) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	if f.Filename == r.name {
		return filesystem.Errorf(filesystem.CodeQuotaExceeded, "full")
	}
	return nil
}
func (*refuseNamed) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (*refuseNamed) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// TestContentAddressedRace: two identical content-addressed uploads share
// one key. The second settles onto it and fails while the first has
// stored it but not yet recorded its row; the first must still record a
// file whose object exists.
func TestContentAddressedRace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithPlugins(&refuseNamed{name: "loser.png"}))
	key, settle := upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	loser := images
	loser.Key, loser.Settle = key, settle
	winner := loser
	winner.Settle = settleThen(settle, func() {
		if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "loser.png", loser); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
			t.Errorf("the interleaved upload = %v, want it refused", err)
		}
	})
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "winner.png", winner)
	if err != nil {
		t.Fatalf("the first upload lost its object to the failed one: %v", err)
	}
	if !e.exists(t, f.Key) {
		t.Fatal("the recorded file has no object")
	}
}

// TestReleaseKeepsObjectOfLowestSharer pins the locking read without a
// LIMIT: when the file releasing an object has the lowest ID of the rows
// naming it and its own row still names it, a read limited to one row
// would see only that row and delete the object the others name.
func TestReleaseKeepsObjectOfLowestSharer(t *testing.T) {
	ctx := t.Context()
	n := 0
	ids := records.WithIDs(func(prefix string) string { n++; return fmt.Sprintf("%s%03d", prefix, n) })
	e := newEnv(t, nil, ids)
	low := e.put(t, "t", "shared.png")
	if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "shared.png"}); err != nil {
		t.Fatal(err)
	}
	e.store.DeleteObjectAfterCommit(ctx, low) // a plugin releasing it while low's row still names it
	if !e.exists(t, "shared.png") {
		t.Fatal("released an object another file names")
	}
}

// TestConcurrentPurgesOfSharers: three files share one object; purging two
// of them at once, many times over, never deletes the object the third
// still names, and purging the last deletes it.
func TestConcurrentPurgesOfSharers(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	for round := range 10 {
		key := fmt.Sprintf("shared-%d.png", round)
		var ids []string
		for i := range 3 {
			var f records.File
			if i == 0 {
				f = e.put(t, "t", key)
			} else {
				var err error
				if f, err = e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: key}); err != nil {
					t.Fatal(err)
				}
			}
			ids = append(ids, f.ID)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range 2 {
			wg.Go(func() { errs[i] = purgeRetrying(ctx, e.store, ids[i]) })
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if !e.exists(t, key) {
			t.Fatalf("round %d: two concurrent purges deleted the object a third file names", round)
		}
		if err := e.store.Purge(ctx, "t", ids[2]); err != nil {
			t.Fatal(err)
		}
		if e.exists(t, key) {
			t.Fatalf("round %d: the last purge kept the object", round)
		}
	}
}

// purgeRetrying purges, retrying what the database reports as a
// deadlock or serialization failure, as an application would.
func purgeRetrying(ctx context.Context, s *records.Store, id string) error {
	var err error
	for range 5 {
		if err = s.Purge(ctx, "t", id); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// TestViewOnceIgnoresClock: once viewed, a view-once file stays refused
// even when the server's clock reads earlier than the view.
func TestViewOnceIgnoresClock(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "s.png", images, records.ViewOnce())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.store.Viewed(ctx, "t", f.ID); err != nil || !ok {
		t.Fatalf("first view = %v, %v", ok, err)
	}
	e.now = e.now.Add(-time.Hour) // another server, an hour behind
	if ok, err := e.store.Viewed(ctx, "t", f.ID); err != nil || ok {
		t.Fatalf("a second view on a slow clock = %v, %v", ok, err)
	}
}

// purgeOther purges a file once a detach it hears of commits, so the
// retention sweep meets a file that is gone by the time it gets to it.
type purgeOther struct {
	store *records.Store
	id    string
}

func (*purgeOther) Name() string { return "purge_other" }
func (p *purgeOther) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == records.EventAttachmentDeleted && p.id != "" {
		id := p.id
		p.id = ""
		s.AfterCommit(ctx, func(ctx context.Context) { _ = s.Purge(ctx, "t", id) })
	}
	return nil
}

// TestRetentionFileGone: a due attachment whose file was purged before
// the sweep reached it is skipped, not reported, and not an error.
func TestRetentionFileGone(t *testing.T) {
	ctx := t.Context()
	po := &purgeOther{}
	e := newEnv(t, nil, records.WithRetention(records.Retention{OwnerType: "post", MaxAge: time.Minute}), records.WithPlugins(po))
	first := e.put(t, "t", "first.png")
	second := e.put(t, "t", "second.png")
	for _, f := range []records.File{first, second} {
		if _, err := e.store.Attach(ctx, "t", f.ID, records.Owner{Type: "post", ID: "p"}, "chat"); err != nil {
			t.Fatal(err)
		}
	}
	po.id = second.ID
	rep, err := e.store.SweepRetention(ctx, e.now.Add(time.Hour), false)
	if err != nil || len(rep.Detached) != 1 || rep.Detached[0].FileID != first.ID {
		t.Fatalf("sweep with a file gone = %+v, %v", rep, err)
	}
}

// pauser holds the purge of one file inside its transaction, after its
// row was deleted, until released.
type pauser struct {
	id       string
	paused   chan struct{}
	released chan struct{}
}

func (*pauser) Name() string { return "pauser" }
func (p *pauser) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	if e.Kind == records.EventFilePurged && e.FileID == p.id {
		close(p.paused)
		<-p.released
	}
	return nil
}

// TestPurgeWaitsForSharer forces the interleaving the locking guards
// against: B's purge holds its row locked, A's purge then waits on it,
// and B commits. A must still see C, the third file naming the object,
// and keep the object; a locking read limited to one row would find
// none once B's row is gone (Postgres) and delete it.
func TestPurgeWaitsForSharer(t *testing.T) {
	ctx := t.Context()
	p := &pauser{paused: make(chan struct{}), released: make(chan struct{})}
	// IDs in creation order, so B's row comes first among A's sharers and
	// A's locking read meets it first.
	n := 0
	ids := records.WithIDs(func(prefix string) string { n++; return fmt.Sprintf("%s%03d", prefix, n) })
	e := newEnv(t, nil, records.WithPlugins(p), ids)
	a := e.put(t, "t", "shared.png")
	var others []records.File
	for range 2 {
		f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "shared.png"})
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, f)
	}
	b, c := others[0], others[1]
	p.id = b.ID
	bDone := make(chan error, 1)
	go func() { bDone <- e.store.Purge(ctx, "t", b.ID) }()
	<-p.paused
	aDone := make(chan error, 1)
	go func() { aDone <- e.store.Purge(ctx, "t", a.ID) }()
	time.Sleep(200 * time.Millisecond) // A reaches its locking read and waits on B
	close(p.released)
	if err := <-bDone; err != nil {
		t.Fatal(err)
	}
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	if !e.exists(t, "shared.png") {
		t.Fatal("the object was deleted while a third file still names it")
	}
	if err := e.store.Purge(ctx, "t", c.ID); err != nil || e.exists(t, "shared.png") {
		t.Fatalf("the last purge: %v, object kept %v", err, e.exists(t, "shared.png"))
	}
}

// TestDiscardKeepsNamedObject pins discardObject's row check: an upload
// whose key (chosen by the policy, no Settler) is an object a recorded
// file already names, refused after storing, leaves that object whole.
func TestDiscardKeepsNamedObject(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithPlugins(&refuseNamed{name: "again.png"}))
	if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "first.png", fixedKey("fixed.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "again.png", fixedKey("fixed.png")); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("the second upload = %v, want it refused", err)
	}
	if !e.exists(t, "fixed.png") {
		t.Fatal("a refused upload deleted the object a recorded file names")
	}
}

// fixedKey is images storing every upload at key.
func fixedKey(key string) upload.Policy {
	p := images
	p.Key = func(upload.Info) (string, error) { return key, nil }
	return p
}

// sweeper is a FileHooks plugin that runs SweepOrphans just before a new
// row is inserted, from inside its transaction, as a concurrent sweep
// that cannot see the uncommitted row would.
type sweeper struct {
	olderThan time.Time
	swept     int
}

func (*sweeper) Name() string { return "sweeper" }
func (w *sweeper) BeforeCreate(ctx context.Context, s *records.Store, _ *records.File) error {
	n, err := s.SweepOrphans(ctx, "media", "", w.olderThan)
	w.swept += n
	return err
}
func (*sweeper) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (*sweeper) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// TestSettledReuseOutlivesSweep: a content-addressed upload settling onto
// an old orphan (left by a failed upload) renews its age before its row
// commits, so a sweep running meanwhile spares it.
func TestSettledReuseOutlivesSweep(t *testing.T) {
	ctx := t.Context()
	diskNow := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) // long before the Store's clock
	w := &sweeper{}
	e := newEnv(t, memory.New(memory.WithClock(func() time.Time { return diskNow })), records.WithPlugins(w))
	key, settle := upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	p := images
	p.Key, p.Settle = key, settle
	orphan, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", p)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(ctx, "t", orphan.ID); err != nil { // the object goes with it
		t.Fatal(err)
	}
	if _, err := e.disk.WriteBytes(ctx, orphan.Key, []byte(pngData), filesystem.WriteOptions{}); err != nil { // an orphan, old
		t.Fatal(err)
	}
	w.olderThan = e.now.Add(-time.Hour) // the orphan's age is a month past it
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "b.png", p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Key != orphan.Key || !e.exists(t, f.Key) || w.swept != 0 {
		t.Fatalf("the reused orphan: key %q (want %q), exists %v, swept %d", f.Key, orphan.Key, e.exists(t, f.Key), w.swept)
	}
}

// TestSettledReuseWithoutSetModTime: on a disk that cannot set a
// modification time, renewing it rewrites the object in place, which
// leaves the same bytes under the same key and no aside copy.
func TestSettledReuseWithoutSetModTime(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, plainDriver{}, records.WithClock(time.Now)) // the disk's clock: the reuse is judged by age
	key, settle := upload.ContentAddressed("renew", upload.ContentAddressing{Shared: true})
	p := images
	p.Key, p.Settle = key, settle
	var before time.Time
	for i, name := range []string{"a.png", "b.png"} {
		f, err := e.store.Upload(ctx, "t", "media", reader(pngData), name, p)
		if err != nil {
			t.Fatal(err)
		}
		data, err := e.disk.ReadAll(ctx, f.Key)
		if err != nil || string(data) != pngData {
			t.Fatalf("%s after the renewal: %q, %v", name, data, err)
		}
		st, err := e.disk.Stat(ctx, f.Key)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && !st.ModTime.After(before) {
			t.Fatalf("reusing the object did not renew its age: %v, was %v", st.ModTime, before)
		}
		before = st.ModTime
	}
	for en, err := range e.disk.List(ctx, "renew", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(en.Key, ".renew-") {
			t.Fatalf("an aside copy was left: %s", en.Key)
		}
	}
}

// TestRenewOnlyOnDedupHit counts, through an interceptor, the operations a
// renewal takes: a fresh content-addressed upload does no copy and sets no
// modification time; a second upload of the same bytes, a dedup hit on
// the older object, renews it once.
func TestRenewOnlyOnDedupHit(t *testing.T) {
	ctx := t.Context()
	counts := map[filesystem.OpKind]int{}
	count := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		counts[op.Kind]++
		return call(ctx)
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(count))
	if err != nil {
		t.Fatal(err)
	}
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(openDB(t), testDialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, settle := upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	p := images
	p.Key, p.Settle = key, settle
	renewals := func() int { return counts[filesystem.OpSetModTime] + counts[filesystem.OpCopy] }
	if _, err := s.Upload(ctx, "t", "media", reader(pngData), "a.png", p); err != nil {
		t.Fatal(err)
	}
	if n := renewals(); n != 0 {
		t.Fatalf("a fresh upload renewed its object: %v", counts)
	}
	if _, err := s.Upload(ctx, "t", "media", reader(pngData), "b.png", p); err != nil {
		t.Fatal(err)
	}
	if n := renewals(); n != 1 {
		t.Fatalf("a dedup hit renewed %d times, want 1: %v", n, counts)
	}
}
