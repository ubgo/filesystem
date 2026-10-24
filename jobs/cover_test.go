package jobs_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
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
	"github.com/ubgo/sqlb"
)

// faultEnv is a store on a database whose steps can be made to fail
// (recordstest.Faults), over a "hot" and a "cold" memory disk and an
// "exports" disk that signs links, all on one clock. It is the setup for
// the failure paths the happy-path envs cannot reach: a database step
// failing mid-run, a disk refusing one operation, a store with retention
// rules.
type faultEnv struct {
	now                time.Time
	hot, cold, exports *filesystem.Disk
	store              *records.Store
	faults             *recordstest.Faults
}

// faultEnvOptions customise newFaultEnv; every field is optional.
type faultEnvOptions struct {
	hot                               filesystem.Driver // nil means a memory driver
	hotInterceptors, coldInterceptors []filesystem.Interceptor
	exportsInterceptors               []filesystem.Interceptor
	exportsUnsigned                   bool // no URL signer on the exports disk
	storeOptions                      []records.Option
}

func newFaultEnv(t *testing.T, o faultEnvOptions) *faultEnv {
	t.Helper()
	e := &faultEnv{now: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return e.now }
	mk := func(name string, d filesystem.Driver, opts ...filesystem.Option) *filesystem.Disk {
		disk, err := filesystem.NewDisk(name, d, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return disk
	}
	hot := o.hot
	if hot == nil {
		hot = memory.New(memory.WithClock(clock))
	}
	e.hot = mk("hot", hot, filesystem.WithInterceptors(o.hotInterceptors...))
	e.cold = mk("cold", memory.New(memory.WithClock(clock)), filesystem.WithInterceptors(o.coldInterceptors...))
	exportOpts := []filesystem.Option{filesystem.WithInterceptors(o.exportsInterceptors...)}
	if !o.exportsUnsigned {
		signer, err := signedurl.New("https://files.example.com/s", []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}}, signedurl.WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		exportOpts = append(exportOpts, filesystem.WithURLSigner(signer))
	}
	e.exports = mk("exports", memory.New(memory.WithClock(clock)), exportOpts...)
	mgr, err := filesystem.NewManager(e.hot, e.cold, e.exports)
	if err != nil {
		t.Fatal(err)
	}
	db, faults := recordstest.OpenFaulty(t)
	e.faults = faults
	opts := append([]records.Option{records.WithClock(clock), records.WithOwnerTypes("post")}, o.storeOptions...)
	e.store, err = records.New(db, recordstest.Dialect(), mgr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

// putN uploads one text file per body to hot, as user-1, and returns
// them in ID order, the order Scrub and MigrateDisk walk them.
func (e *faultEnv) putN(t *testing.T, bodies ...string) []records.File {
	t.Helper()
	ctx := records.WithActor(t.Context(), "user-1")
	var files []records.File
	for i, b := range bodies {
		f, err := e.store.Upload(ctx, "acme", "hot", strings.NewReader(b), string(rune('a'+i))+".txt", anyFile)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b records.File) int { return strings.Compare(a.ID, b.ID) })
	return files
}

// stubCursor is a Cursor whose Load or Save fails with the error set.
type stubCursor struct {
	value            string
	loadErr, saveErr error
}

func (c *stubCursor) Load(context.Context) (string, error) { return c.value, c.loadErr }
func (c *stubCursor) Save(_ context.Context, v string) error {
	if c.saveErr != nil {
		return c.saveErr
	}
	c.value = v
	return nil
}

var (
	errLoad = errors.New("cursor store down on load")
	errSave = errors.New("cursor store down on save")
)

// failOn returns an interceptor that fails every op of kind with
// CodeUnavailable without calling the driver while *armed is true (always
// when armed is nil), first running before (when set), so a test can end
// the run's context at the moment of the failure.
func failOn(kind filesystem.OpKind, armed *bool, before func()) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind != kind || armed != nil && !*armed {
			return call(ctx)
		}
		if before != nil {
			before()
		}
		return filesystem.Errorf(filesystem.CodeUnavailable, "simulated %s failure", kind)
	}
}

// TestScrubRefusesWhatItCannotStart: an unknown disk, a cursor that
// cannot load and a database that cannot list each stop the run before
// any file is checked, and the cursor is left as it was.
func TestScrubRefusesWhatItCannotStart(t *testing.T) {
	ctx := t.Context()
	e := newFaultEnv(t, faultEnvOptions{})
	e.putN(t, "a")
	if _, err := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "nowhere", Cursor: &memCursor{}}).Run(ctx, e.now); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("scrub of an unknown disk: %v", err)
	}
	if _, err := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: &stubCursor{loadErr: errLoad}}).Run(ctx, e.now); !errors.Is(err, errLoad) {
		t.Fatalf("scrub with a cursor that cannot load: %v", err)
	}
	cursor := &memCursor{value: "before"}
	e.faults.Arm(1) // the listing query
	res, err := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: cursor}).Run(ctx, e.now)
	if !errors.Is(err, recordstest.ErrInjected) || res.Count != 0 || cursor.value != "before" {
		t.Fatalf("scrub with a failing listing: %+v, %v, cursor %q", res, err, cursor.value)
	}
}

// TestMigrateDiskRefusesWhatItCannotStart mirrors the scrub case for
// MigrateDisk: an unknown target disk, a cursor that cannot load and a
// failing listing stop it with nothing moved.
func TestMigrateDiskRefusesWhatItCannotStart(t *testing.T) {
	ctx := t.Context()
	e := newFaultEnv(t, faultEnvOptions{})
	files := e.putN(t, "a")
	if _, err := jobs.MigrateDisk(e.store, "hot", "nowhere", jobs.MigrateOptions{Cursor: &memCursor{}}).Run(ctx, e.now); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("migration to an unknown disk: %v", err)
	}
	if _, err := jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: &stubCursor{loadErr: errLoad}}).Run(ctx, e.now); !errors.Is(err, errLoad) {
		t.Fatalf("migration with a cursor that cannot load: %v", err)
	}
	e.faults.Arm(1)
	res, err := jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: &memCursor{}}).Run(ctx, e.now)
	if !errors.Is(err, recordstest.ErrInjected) || res.Count != 0 {
		t.Fatalf("migration with a failing listing: %+v, %v", res, err)
	}
	if cur, _ := e.store.File(ctx, "acme", files[0].ID); cur.Disk != "hot" {
		t.Fatalf("file moved to %s by a failed run", cur.Disk)
	}
}

// TestScrubFaultSweep fails every database step of a scrub run in turn:
// each run must return the injected error, never report a file as missing
// or changed, and never move the cursor past a file it did not check.
func TestScrubFaultSweep(t *testing.T) {
	ctx := t.Context()
	measure := newFaultEnv(t, faultEnvOptions{})
	measure.putN(t, "a", "b")
	measure.faults.Arm(0)
	if res, err := jobs.Scrub(measure.store, jobs.ScrubOptions{Disk: "hot", Cursor: &memCursor{}}).Run(ctx, measure.now); err != nil || res.Count != 2 {
		t.Fatalf("clean run %+v, %v", res, err)
	}
	steps := measure.faults.Steps()
	if steps < 3 {
		t.Fatalf("a scrub of two files took %d database steps; the sweep would prove nothing", steps)
	}
	for n := 1; n <= steps; n++ {
		e := newFaultEnv(t, faultEnvOptions{})
		files := e.putN(t, "a", "b")
		var reported []string
		report := func(f records.File) { reported = append(reported, f.ID) }
		cursor := &memCursor{}
		e.faults.Arm(n)
		_, err := jobs.Scrub(e.store, jobs.ScrubOptions{
			Disk: "hot", Cursor: cursor, OnMissing: report,
			OnMismatch: func(f records.File, _ int64, _ string) { report(f) },
		}).Run(ctx, e.now)
		if !errors.Is(err, recordstest.ErrInjected) || len(reported) != 0 {
			t.Fatalf("step %d: %v, reported %v", n, err, reported)
		}
		if cursor.value == files[1].ID {
			t.Fatalf("step %d: the cursor passed the file whose check failed", n)
		}
	}
}

// TestMigrateDiskFaultSweep fails every database step of a migration run
// in turn. A failure either stops the run (returned) or fails one file's
// move (OnFailed), and in both cases every file still reads back whole
// from the disk its row names.
func TestMigrateDiskFaultSweep(t *testing.T) {
	ctx := t.Context()
	measure := newFaultEnv(t, faultEnvOptions{})
	measure.putN(t, "a", "b")
	measure.faults.Arm(0)
	if res, err := jobs.MigrateDisk(measure.store, "hot", "cold", jobs.MigrateOptions{Cursor: &memCursor{}}).Run(ctx, measure.now); err != nil || res.Count != 2 {
		t.Fatalf("clean run %+v, %v", res, err)
	}
	steps := measure.faults.Steps()
	stopped := 0
	for n := 1; n <= steps; n++ {
		e := newFaultEnv(t, faultEnvOptions{})
		files := e.putN(t, "first", "second")
		var failed []error
		e.faults.Arm(n)
		_, err := jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: &memCursor{}, OnFailed: func(_ records.File, err error) { failed = append(failed, err) }}).Run(ctx, e.now)
		injected := errors.Is(err, recordstest.ErrInjected)
		for _, ferr := range failed {
			injected = injected || errors.Is(ferr, recordstest.ErrInjected)
		}
		if !injected {
			t.Fatalf("step %d: run %v, failed %v: the injected fault was lost", n, err, failed)
		}
		if err != nil {
			stopped++
		}
		e.faults.Arm(0)
		for i, f := range files {
			cur, ferr := e.store.File(ctx, "acme", f.ID)
			if ferr != nil {
				t.Fatalf("step %d: row %s: %v", n, f.ID, ferr)
			}
			d, _ := e.store.Disks().Disk(cur.Disk)
			if b, rerr := d.ReadAll(ctx, cur.Key); rerr != nil || string(b) != []string{"first", "second"}[i] && string(b) != []string{"first", "second"}[1-i] {
				t.Fatalf("step %d: file %s on %s reads %q, %v", n, f.ID, cur.Disk, b, rerr)
			}
		}
	}
	if stopped == 0 {
		t.Fatal("no fault stopped the run: the listing and row reads were never failed")
	}
}

// TestMigrateDiskSkipsPurgedAndTrashedFiles: a file purged after the
// batch was listed is skipped without a failure, and a trashed file is
// left on the old disk.
func TestMigrateDiskSkipsPurgedAndTrashedFiles(t *testing.T) {
	ctx := t.Context()
	e := newFaultEnv(t, faultEnvOptions{})
	files := e.putN(t, "a", "b", "c")
	if err := e.store.Trash(ctx, "acme", files[2].ID); err != nil {
		t.Fatal(err)
	}
	var failed []string
	job := jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{
		Cursor:   &memCursor{},
		OnMoved:  func(records.File) { _ = e.store.Purge(ctx, "acme", files[1].ID) },
		OnFailed: func(f records.File, _ error) { failed = append(failed, f.ID) },
	})
	res, err := job.Run(ctx, e.now)
	if err != nil || res.Count != 1 || len(failed) != 0 {
		t.Fatalf("run %+v, %v, failed %v", res, err, failed)
	}
	if _, err := e.store.File(ctx, "acme", files[1].ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("purged file: %v", err)
	}
	trashed, err := e.store.File(ctx, "acme", files[2].ID)
	if err != nil || trashed.Disk != "hot" {
		t.Fatalf("trashed file on %s, %v", trashed.Disk, err)
	}
}

// TestMigrateDiskStopsWhenCancelledDuringAMove: a move that fails because
// the run's context ended stops the run (it is not a file failure for
// OnFailed), and the file stays whole on the old disk.
func TestMigrateDiskStopsWhenCancelledDuringAMove(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := newFaultEnv(t, faultEnvOptions{coldInterceptors: []filesystem.Interceptor{failOn(filesystem.OpWrite, nil, cancel)}})
	files := e.putN(t, "a")
	var failed []string
	cursor := &memCursor{value: ""}
	res, err := jobs.MigrateDisk(e.store, "hot", "cold", jobs.MigrateOptions{Cursor: cursor, OnFailed: func(f records.File, _ error) { failed = append(failed, f.ID) }}).Run(ctx, e.now)
	if err == nil || res.Count != 0 || len(failed) != 0 {
		t.Fatalf("cancelled move: %+v, %v, failed %v", res, err, failed)
	}
	cur, err := e.store.File(t.Context(), "acme", files[0].ID)
	if err != nil || cur.Disk != "hot" {
		t.Fatalf("file on %s, %v", cur.Disk, err)
	}
	if b, err := e.hot.ReadAll(t.Context(), cur.Key); err != nil || string(b) != "a" {
		t.Fatalf("old object reads %q, %v", b, err)
	}
}

// TestScrubSkipsFilesChangedSinceTheListing: a file purged or moved to
// another disk after the batch was listed is skipped, never reported as
// missing (its object is gone from the scrubbed disk, rightly).
func TestScrubSkipsFilesChangedSinceTheListing(t *testing.T) {
	ctx := t.Context()
	e := newFaultEnv(t, faultEnvOptions{})
	files := e.putN(t, "a", "b", "c")
	// The first file is rotten, so its report runs mid-batch, before the
	// other two are checked.
	if _, err := e.hot.WriteBytes(ctx, files[0].Key, []byte("A"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var mismatched, missing []string
	job := jobs.Scrub(e.store, jobs.ScrubOptions{
		Disk: "hot", Cursor: &memCursor{},
		OnMissing: func(f records.File) { missing = append(missing, f.ID) },
		OnMismatch: func(f records.File, _ int64, _ string) {
			mismatched = append(mismatched, f.ID)
			if err := e.store.Purge(ctx, "acme", files[1].ID); err != nil {
				t.Error(err)
			}
			if _, err := e.store.MoveFile(ctx, "acme", files[2].ID, "cold"); err != nil {
				t.Error(err)
			}
		},
	})
	res, err := job.Run(ctx, e.now)
	if err != nil || res.Count != 3 {
		t.Fatalf("run %+v, %v", res, err)
	}
	if !slices.Equal(mismatched, []string{files[0].ID}) || len(missing) != 0 {
		t.Fatalf("mismatched %v, missing %v", mismatched, missing)
	}
}

// TestScrubReportsAFailedStat: a Stat that fails for a reason other than
// not found goes to OnError and the run goes on; when the run's context
// ended with it, the run stops instead, and a cursor that cannot save
// then is reported alongside.
func TestScrubReportsAFailedStat(t *testing.T) {
	ctx := t.Context()
	statFails := false
	e := newFaultEnv(t, faultEnvOptions{hotInterceptors: []filesystem.Interceptor{failOn(filesystem.OpStat, &statFails, nil)}})
	files := e.putN(t, "a", "b")
	statFails = true
	var errored []string
	res, err := jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: &memCursor{}, OnError: func(f records.File, err error) {
		if filesystem.CodeOf(err) == filesystem.CodeUnavailable {
			errored = append(errored, f.ID)
		}
	}}).Run(ctx, e.now)
	if err != nil || res.Count != 2 || !slices.Equal(errored, []string{files[0].ID, files[1].ID}) {
		t.Fatalf("run %+v, %v, errored %v", res, err, errored)
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	statFails = false
	e = newFaultEnv(t, faultEnvOptions{hotInterceptors: []filesystem.Interceptor{failOn(filesystem.OpStat, &statFails, cancel)}})
	e.putN(t, "a")
	statFails = true
	errored = nil
	res, err = jobs.Scrub(e.store, jobs.ScrubOptions{Disk: "hot", Cursor: &stubCursor{saveErr: errSave}, OnError: func(f records.File, _ error) { errored = append(errored, f.ID) }}).Run(cctx, e.now)
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, errSave) || res.Count != 0 || len(errored) != 0 {
		t.Fatalf("cancelled run %+v, %v, errored %v", res, err, errored)
	}
}

// TestRetentionDryRunListsDueAttachments: with a rule, the dry run plans
// the attachments past their age and detaches none of them.
func TestRetentionDryRunListsDueAttachments(t *testing.T) {
	ctx := t.Context()
	e := newFaultEnv(t, faultEnvOptions{storeOptions: []records.Option{records.WithRetention(records.Retention{OwnerType: "post", MaxAge: time.Hour})}})
	files := e.putN(t, "a")
	att, err := e.store.Attach(ctx, "acme", files[0].ID, records.Owner{Type: "post", ID: "p1"}, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	res, err := jobs.SweepRetention(e.store).DryRun(ctx, e.now.Add(2*time.Hour))
	if err != nil || res.Count != 1 || !slices.Equal(res.Planned, []string{att.ID}) {
		t.Fatalf("dry run %+v, %v", res, err)
	}
	if res, err = jobs.SweepRetention(e.store).DryRun(ctx, e.now.Add(2*time.Hour)); err != nil || res.Count != 1 {
		t.Fatalf("the dry run detached the attachment: second dry run %+v, %v", res, err)
	}
}

// exportsOf lists the keys stored on the exports disk.
func exportsOf(t *testing.T, e *faultEnv) []string {
	t.Helper()
	var keys []string
	for en, err := range e.exports.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, en.Key)
	}
	return keys
}

// TestExportSubjectRefusals: a directory that escapes the disk, an empty
// subject and a store that cannot list each refuse the export with
// nothing stored.
func TestExportSubjectRefusals(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user-1")
	e := newFaultEnv(t, faultEnvOptions{})
	e.putN(t, "a")
	sub := jobs.Subject{Scope: "acme", UploadedBy: "user-1"}
	if _, err := jobs.ExportSubject(ctx, e.store, e.exports, sub, jobs.ExportOptions{Dir: "../outside", TTL: time.Minute}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Fatalf("export outside the disk: %v", err)
	}
	if _, err := jobs.ExportSubject(ctx, e.store, e.exports, jobs.Subject{Scope: "acme"}, jobs.ExportOptions{Dir: "exports", TTL: time.Minute}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("export of an empty subject: %v", err)
	}
	unregistered := jobs.Subject{Scope: "acme", Owner: records.Owner{Type: "invoice", ID: "i1"}}
	if _, err := jobs.ExportSubject(ctx, e.store, e.exports, unregistered, jobs.ExportOptions{Dir: "exports", TTL: time.Minute}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("export of an unregistered owner type: %v", err)
	}
	e.faults.Arm(1)
	if _, err := jobs.ExportSubject(ctx, e.store, e.exports, sub, jobs.ExportOptions{Dir: "exports", TTL: time.Minute}); !errors.Is(err, recordstest.ErrInjected) {
		t.Fatalf("export with a failing listing: %v", err)
	}
	if keys := exportsOf(t, e); len(keys) != 0 {
		t.Fatalf("refused exports stored %v", keys)
	}
}

// TestExportSubjectCountsAFileOnce: a file the subject both uploaded and
// owns is exported once.
func TestExportSubjectCountsAFileOnce(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user-1")
	e := newFaultEnv(t, faultEnvOptions{})
	files := e.putN(t, "a")
	if _, err := e.store.Attach(ctx, "acme", files[0].ID, records.Owner{Type: "post", ID: "p1"}, "gallery"); err != nil {
		t.Fatal(err)
	}
	exp, err := jobs.ExportSubject(ctx, e.store, e.exports, jobs.Subject{Scope: "acme", UploadedBy: "user-1", Owner: records.Owner{Type: "post", ID: "p1"}}, jobs.ExportOptions{Dir: "exports", TTL: time.Minute})
	if err != nil || exp.Files != 1 || exp.Bytes != 1 {
		t.Fatalf("export %+v, %v", exp, err)
	}
	b, err := e.exports.ReadAll(ctx, exp.Key)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("archive holds %d entries, %v", len(zr.File), err)
	}
}

// TestExportSubjectStorageFailures: when the archive cannot be stored, or
// its link cannot be signed, the export fails and leaves no archive.
func TestExportSubjectStorageFailures(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user-1")
	sub := jobs.Subject{Scope: "acme", UploadedBy: "user-1"}
	opts := jobs.ExportOptions{Dir: "exports", TTL: time.Minute}

	e := newFaultEnv(t, faultEnvOptions{exportsInterceptors: []filesystem.Interceptor{failOn(filesystem.OpWrite, nil, nil)}})
	e.putN(t, "a")
	if _, err := jobs.ExportSubject(ctx, e.store, e.exports, sub, opts); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("export to a failing disk: %v", err)
	}

	e = newFaultEnv(t, faultEnvOptions{exportsUnsigned: true})
	e.putN(t, "a")
	if _, err := jobs.ExportSubject(ctx, e.store, e.exports, sub, opts); err == nil {
		t.Fatal("export to a disk that cannot sign links succeeded")
	}
	if keys := exportsOf(t, e); len(keys) != 0 {
		t.Fatalf("an export without a link left %v", keys)
	}
}

// brokenBody is a memory driver whose reads fail after the first byte, as
// a connection dropped mid-object would.
type brokenBody struct{ *memory.Driver }

func (b brokenBody) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := b.Driver.Read(ctx, key, r)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 1), errReader{}), rc}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, filesystem.Errorf(filesystem.CodeUnavailable, "connection dropped")
}

// TestExportSubjectSourceFailures: a file whose object cannot be found,
// read whole or matched to its row, or whose row names an unknown disk,
// fails the export with nothing stored.
func TestExportSubjectSourceFailures(t *testing.T) {
	ctx := records.WithActor(t.Context(), "user-1")
	sub := jobs.Subject{Scope: "acme", UploadedBy: "user-1"}
	opts := jobs.ExportOptions{Dir: "exports", TTL: time.Minute}
	cases := []struct {
		name    string
		hot     filesystem.Driver
		corrupt func(t *testing.T, e *faultEnv, f records.File)
		code    filesystem.Code
	}{
		{name: "object missing", code: filesystem.CodeNotFound, corrupt: func(t *testing.T, e *faultEnv, f records.File) {
			if err := e.hot.Delete(ctx, f.Key); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "object shorter than its row", code: filesystem.CodeUnavailable, corrupt: func(t *testing.T, e *faultEnv, f records.File) {
			if _, err := e.hot.WriteBytes(ctx, f.Key, []byte("ab"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "row on an unknown disk", code: filesystem.CodeNotFound, corrupt: func(t *testing.T, e *faultEnv, f records.File) {
			if _, err := e.store.Exec(ctx, sqlb.NewUpdate(e.store.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileDisk, "gone")).Where(sqlb.Eq(records.FileID, f.ID))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "read fails mid-object", code: filesystem.CodeUnavailable, hot: brokenBody{memory.New()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newFaultEnv(t, faultEnvOptions{hot: c.hot})
			files := e.putN(t, "abcdef")
			if c.corrupt != nil {
				c.corrupt(t, e, files[0])
			}
			if _, err := jobs.ExportSubject(ctx, e.store, e.exports, sub, opts); filesystem.CodeOf(err) != c.code {
				t.Fatalf("export: %v, want code %s", err, c.code)
			}
			if keys := exportsOf(t, e); len(keys) != 0 {
				t.Fatalf("a failed export stored %v", keys)
			}
		})
	}
}
