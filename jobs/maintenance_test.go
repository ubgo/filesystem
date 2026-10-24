package jobs_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// twoDisks is a store over two memory disks, "hot" and "cold", and an
// "exports" disk that signs its links, all on one clock.
type twoDisks struct {
	now                time.Time
	exportWrites       int // writes to the exports disk
	hot, cold, exports *filesystem.Disk
	signer             *signedurl.Signer
	store              *records.Store
}

func newTwoDisks(t *testing.T, coldOpts ...filesystem.Option) *twoDisks {
	t.Helper()
	e := &twoDisks{now: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return e.now }
	var err error
	e.signer, err = signedurl.New("https://files.example.com/s", []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}}, signedurl.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, opts ...filesystem.Option) *filesystem.Disk {
		d, err := filesystem.NewDisk(name, memory.New(memory.WithClock(clock)), opts...)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	e.hot, e.cold, e.exports = mk("hot"), mk("cold", coldOpts...), mk("exports", filesystem.WithURLSigner(e.signer), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite {
			e.exportWrites++
		}
		return call(ctx)
	}))
	mgr, err := filesystem.NewManager(e.hot, e.cold, e.exports)
	if err != nil {
		t.Fatal(err)
	}
	db, dialect := recordstest.Open(t)
	e.store, err = records.New(db, dialect, mgr, records.WithClock(clock), records.WithOwnerTypes("post"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

var anyFile = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}}

func (e *twoDisks) put(t *testing.T, ctx context.Context, name, body string) records.File {
	t.Helper()
	f, err := e.store.Upload(ctx, "acme", "hot", strings.NewReader(body), name, anyFile)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// memCursor is the simplest Cursor: a second implementation beside
// jobs.ObjectCursor, as an application's own (a database row) would be.
type memCursor struct{ value string }

func (c *memCursor) Load(context.Context) (string, error)   { return c.value, nil }
func (c *memCursor) Save(_ context.Context, v string) error { c.value = v; return nil }

func TestScrub(t *testing.T) {
	ctx := t.Context()
	e := newTwoDisks(t)
	good := e.put(t, ctx, "good.txt", "good")
	gone := e.put(t, ctx, "gone.txt", "gone")
	rotten := e.put(t, ctx, "rotten.txt", "rotten")
	if err := e.hot.Delete(ctx, gone.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hot.WriteBytes(ctx, rotten.Key, []byte("r0tten"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	before := contentsOf(t, e.hot)

	var missing, mismatched []string
	cursor := jobs.ObjectCursor(e.cold, "_jobs/scrub-hot")
	job := jobs.Scrub(e.store, jobs.ScrubOptions{
		Disk: "hot", Cursor: cursor, Batch: 2,
		OnMissing:  func(f records.File) { missing = append(missing, f.ID) },
		OnMismatch: func(f records.File, size int64, sum string) { mismatched = append(mismatched, f.ID) },
	})
	// Two files per run: the first run stops after two and saves its
	// place, the second resumes there and ends the pass.
	res, err := job.Run(ctx, e.now)
	if err != nil || res.Count != 2 {
		t.Fatalf("first run %+v, %v", res, err)
	}
	if v, _ := cursor.Load(ctx); v == "" {
		t.Fatal("the first run saved no cursor")
	}
	res, err = job.Run(ctx, e.now)
	if err != nil || res.Count != 1 {
		t.Fatalf("second run %+v, %v", res, err)
	}
	if v, _ := cursor.Load(ctx); v != "" {
		t.Fatalf("a finished pass left cursor %q", v)
	}
	if !slices.Equal(missing, []string{gone.ID}) || !slices.Equal(mismatched, []string{rotten.ID}) {
		t.Fatalf("missing %v, mismatched %v; good file %s", missing, mismatched, good.ID)
	}
	// Scrubbing only reports: every object and row is still there.
	if after := contentsOf(t, e.hot); !mapsEqual(before, after) {
		t.Fatalf("scrub changed the disk: %v", after)
	}
	for _, f := range []records.File{good, gone, rotten} {
		if _, err := e.store.File(ctx, "acme", f.ID); err != nil {
			t.Fatalf("scrub removed row %s: %v", f.ID, err)
		}
	}
}

func TestScrubRateLimitAndResume(t *testing.T) {
	ctx := t.Context()
	e := newTwoDisks(t)
	for _, n := range []string{"a", "b", "c"} {
		e.put(t, ctx, n+".txt", n)
	}
	cursor := &memCursor{}
	interval := 20 * time.Millisecond
	job := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: cursor, Interval: interval})
	start := time.Now()
	if res, err := job.Run(ctx, e.now); err != nil || res.Count != 3 {
		t.Fatalf("run %+v, %v", res, err)
	}
	if took := time.Since(start); took < 3*interval {
		t.Fatalf("three objects at %s each took %s: not rate-limited", interval, took)
	}
	// A run cancelled during its pause saves the file it finished, and
	// the next run resumes after it.
	cctx, cancel := context.WithCancel(ctx)
	checked := 0
	job = jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: cursor, Interval: time.Hour, OnMissing: func(records.File) {}})
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res, err := job.Run(cctx, e.now)
	checked += int(res.Count)
	if err == nil || res.Count != 1 || cursor.value == "" {
		t.Fatalf("cancelled run %+v, %v, cursor %q", res, err, cursor.value)
	}
	job = jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: cursor})
	res, err = job.Run(ctx, e.now)
	if checked += int(res.Count); err != nil || checked != 3 {
		t.Fatalf("resumed run %+v, %v: %d checked in all", res, err, checked)
	}
	if _, err := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot"}).Run(ctx, e.now); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("scrub without a cursor: %v", err)
	}
}

// failOnce makes the first write of *key to its disk fail after the bytes
// were stored, as a crash between the copy and the cut-over would leave
// it: an extra copy on the target, the row still on the source.
// key is read at each write, so a test can name it once it is known.
func failOnce(key *string) filesystem.Interceptor {
	failed := false
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		if err == nil && !failed && op.Kind == filesystem.OpWrite && op.Key == *key {
			failed = true
			return filesystem.Errorf(filesystem.CodeUnavailable, "simulated crash after the copy of %s", *key)
		}
		return err
	}
}

// TestMigrateDiskSurvivesACrash walks through a migration interrupted at
// each point it can be and checks after every step that each file still
// reads back whole from the disk its row names.
func TestMigrateDiskSurvivesACrash(t *testing.T) {
	ctx := t.Context()
	var crashKey string
	e := newTwoDisks(t, filesystem.WithInterceptors(failOnce(&crashKey)))
	bodies := map[string]string{}
	var files []records.File
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		f := e.put(t, ctx, n+".txt", "content of "+n)
		files = append(files, f)
		bodies[f.ID] = "content of " + n
	}
	// The job walks files in ID order.
	slices.SortFunc(files, func(a, b records.File) int { return strings.Compare(a.ID, b.ID) })
	allReadable := func(step string) {
		t.Helper()
		for _, f := range files {
			cur, err := e.store.File(ctx, "acme", f.ID)
			if err != nil {
				t.Fatalf("%s: row %s: %v", step, f.ID, err)
			}
			d, _ := e.store.Disks().Disk(cur.Disk)
			b, err := d.ReadAll(ctx, cur.Key)
			if err != nil || string(b) != bodies[f.ID] {
				t.Fatalf("%s: file %s on %s reads %q, %v", step, f.ID, cur.Disk, b, err)
			}
		}
	}

	// Step 1: a crash right after the copy of the third file (its row
	// still on hot, an extra copy on cold) and before its cut-over. The
	// cold disk fails that write once, as the crash would.
	crashKey = files[2].Key
	cursor := &memCursor{}
	var failed []string
	job := jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: cursor, Batch: 3, OnFailed: func(f records.File, err error) { failed = append(failed, f.ID) }})
	res, err := job.Run(ctx, e.now)
	if err != nil || res.Count != 2 || !slices.Equal(failed, []string{files[2].ID}) {
		t.Fatalf("run 1: %+v, %v, failed %v", res, err, failed)
	}
	allReadable("after the crash of one copy")

	// Step 2: the process dies between two files: the context ends after
	// the next move. The cursor keeps what was done.
	cctx, cancel := context.WithCancel(ctx)
	job = jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: cursor, OnMoved: func(records.File) { cancel() }})
	res, err = job.Run(cctx, e.now)
	if err == nil || res.Count != 1 {
		t.Fatalf("run 2: %+v, %v", res, err)
	}
	allReadable("after the interrupted run")

	// Step 3: reruns finish the pass, then a new pass retries the file
	// whose copy failed; the migration ends when a pass moves nothing.
	for i := 0; i < 3; i++ {
		job = jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: cursor})
		if _, err := job.Run(ctx, e.now); err != nil {
			t.Fatalf("rerun %d: %v", i, err)
		}
	}
	allReadable("after the reruns")
	for _, f := range files {
		cur, _ := e.store.File(ctx, "acme", f.ID)
		if cur.Disk != "cold" {
			t.Fatalf("file %s still on %s", f.ID, cur.Disk)
		}
		if ok, _ := e.hot.Exists(ctx, f.Key); ok {
			t.Fatalf("old object of %s left on hot", f.ID)
		}
	}
	if _, err := jobs.MigrateDisk(e.store, "hot", "hot", jobs.MigrateOptions{Cursor: cursor}).Run(ctx, e.now); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("migrating a disk to itself: %v", err)
	}
}

func TestExportSubject(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user-1")
	e := newTwoDisks(t)
	mine := e.put(t, ctx, "passwd", "first")
	// A row edited outside the Store may hold any name; the archive
	// still gets a safe one.
	if _, err := e.store.Exec(ctx, sqlb.NewUpdate(e.store.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileFilename, "../../etc/passwd")).Where(sqlb.Eq(records.FileID, mine.ID))); err != nil {
		t.Fatal(err)
	}
	tiered := e.put(t, ctx, "notes.txt", "second")
	if _, err := e.store.MoveFile(ctx, "acme", tiered.ID, "cold"); err != nil {
		t.Fatal(err)
	}
	other := e.put(t, records.WithActor(t.Context(), "user-2"), "other.txt", "not mine")
	if _, err := e.store.Attach(ctx, "acme", other.ID, records.Owner{Type: "post", ID: "p1"}, "gallery"); err != nil {
		t.Fatal(err)
	}

	ttl := 15 * time.Minute
	exp, err := jobs.ExportSubject(ctx, e.store, e.exports, jobs.Subject{Scope: "acme", UploadedBy: "user-1"}, jobs.ExportOptions{Dir: "exports", TTL: ttl, Filename: "my-data.zip"})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Files != 2 || exp.Bytes != int64(len("first")+len("second")) || !strings.HasPrefix(exp.Key, "exports/") {
		t.Fatalf("export %+v", exp)
	}

	// The link verifies now, and never after its TTL.
	u, err := url.Parse(exp.URL)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := e.signer.Verify("exports", exp.Key, filesystem.MethodGet, u.Query())
	if err != nil || !claims.ExpiresAt.Equal(e.now.Add(ttl)) || !strings.Contains(claims.ContentDisposition, "my-data.zip") {
		t.Fatalf("link claims %+v, %v", claims, err)
	}
	e.now = e.now.Add(ttl + time.Second)
	if _, err := e.signer.Verify("exports", exp.Key, filesystem.MethodGet, u.Query()); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("the link still works after its TTL: %v", err)
	}

	// The archive: the manifest, then each file under its ID with a safe
	// base name, from whichever disk holds it.
	raw, err := e.exports.ReadAll(ctx, exp.Key)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[zf.Name] = string(b)
	}
	if got[mine.ID+"/passwd"] != "first" || got[tiered.ID+"/notes.txt"] != "second" || len(got) != 3 {
		t.Fatalf("archive entries %v", got)
	}
	var manifest []struct{ ID, Path string }
	if err := json.Unmarshal([]byte(got[jobs.ExportManifest]), &manifest); err != nil || len(manifest) != 2 {
		t.Fatalf("manifest %q, %v", got[jobs.ExportManifest], err)
	}

	// An owner's files are a subject too.
	byOwner, err := jobs.ExportSubject(ctx, e.store, e.exports, jobs.Subject{Scope: "acme", Owner: records.Owner{Type: "post", ID: "p1"}}, jobs.ExportOptions{Dir: "exports", TTL: ttl})
	if err != nil || byOwner.Files != 1 {
		t.Fatalf("owner export %+v, %v", byOwner, err)
	}

	// The sweep scheduled on the export directory deletes expired
	// archives.
	sweep := jobs.UploadSweep(e.exports, "exports", ttl)
	if res, err := sweep.Run(ctx, e.now.Add(ttl+time.Minute)); err != nil || res.Count != 2 {
		t.Fatalf("export sweep %+v, %v", res, err)
	}
	if ok, _ := e.exports.Exists(ctx, exp.Key); ok {
		t.Fatal("the expired export was not swept")
	}

	// Refused before anything is written: a link that would not expire,
	// the root, no subject, too much.
	writes := e.exportWrites
	for name, tc := range map[string]struct {
		sub  jobs.Subject
		opts jobs.ExportOptions
		code filesystem.Code
	}{
		"no TTL":    {jobs.Subject{Scope: "acme", UploadedBy: "user-1"}, jobs.ExportOptions{Dir: "exports"}, filesystem.CodeInvalidArgument},
		"root":      {jobs.Subject{Scope: "acme", UploadedBy: "user-1"}, jobs.ExportOptions{TTL: ttl}, filesystem.CodeInvalidArgument},
		"nobody":    {jobs.Subject{Scope: "acme"}, jobs.ExportOptions{Dir: "exports", TTL: ttl}, filesystem.CodeInvalidArgument},
		"too large": {jobs.Subject{Scope: "acme", UploadedBy: "user-1"}, jobs.ExportOptions{Dir: "exports", TTL: ttl, MaxBytes: 3}, filesystem.CodeTooLarge},
		"too many":  {jobs.Subject{Scope: "acme", UploadedBy: "user-1"}, jobs.ExportOptions{Dir: "exports", TTL: ttl, MaxFiles: 1}, filesystem.CodeTooLarge},
	} {
		if _, err := jobs.ExportSubject(ctx, e.store, e.exports, tc.sub, tc.opts); filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
	if e.exportWrites != writes {
		t.Fatalf("refused exports wrote %d objects", e.exportWrites-writes)
	}
}

func contentsOf(t *testing.T, d *filesystem.Disk) map[string]string {
	t.Helper()
	out := map[string]string{}
	for en, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		b, err := d.ReadAll(t.Context(), en.Key)
		if err != nil {
			t.Fatal(err)
		}
		out[en.Key] = string(b)
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// TestScrubGoesOnPastAnUnreadableObject: an object that cannot be read
// (as one in an archive tier) is reported and passed, and the cursor
// moves on, so it never holds the scrub back.
func TestScrubGoesOnPastAnUnreadableObject(t *testing.T) {
	ctx := t.Context()
	var archived string
	e := newTwoDisks(t, filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpRead && op.Key == archived {
			return filesystem.Errorf(filesystem.CodeUnavailable, "object is archived")
		}
		return call(ctx)
	}))
	var files []records.File
	for _, n := range []string{"a", "b", "c"} {
		f, err := e.store.Upload(ctx, "acme", "cold", strings.NewReader(n), n+".txt", anyFile)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b records.File) int { return strings.Compare(a.ID, b.ID) })
	archived = files[0].Key
	cursor := &memCursor{}
	var failed []string
	job := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "cold", Cursor: cursor, Batch: 2, OnError: func(f records.File, err error) {
		if filesystem.CodeOf(err) == filesystem.CodeUnavailable {
			failed = append(failed, f.ID)
		}
	}})
	res, err := job.Run(ctx, e.now)
	if err != nil || res.Count != 2 || cursor.value != files[1].ID {
		t.Fatalf("run past the archived object: %+v, %v, cursor %q", res, err, cursor.value)
	}
	if !slices.Equal(failed, []string{files[0].ID}) {
		t.Fatalf("reported %v", failed)
	}
	if res, err := job.Run(ctx, e.now); err != nil || res.Count != 1 || cursor.value != "" {
		t.Fatalf("second run: %+v, %v, cursor %q", res, err, cursor.value)
	}
}

// TestExportIsPrivateOnAPublicDisk: on a disk whose objects are public by
// default, the archive is still written private.
func TestExportIsPrivateOnAPublicDisk(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user-1")
	e := newTwoDisks(t)
	e.put(t, ctx, "a.txt", "a")
	public, err := filesystem.NewDisk("public", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), filesystem.WithURLSigner(e.signer))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := jobs.ExportSubject(ctx, e.store, public, jobs.Subject{Scope: "acme", UploadedBy: "user-1"}, jobs.ExportOptions{Dir: "exports", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if en, err := public.Stat(ctx, exp.Key); err != nil || en.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("archive visibility %q, %v", en.Visibility, err)
	}
}
