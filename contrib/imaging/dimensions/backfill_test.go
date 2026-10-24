package dimensions_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// legacy is a database whose files were recorded by a Store without the
// plugin (before), then reopened with it installed (after).
type legacy struct {
	disk          *filesystem.Disk
	before, after *records.Store
	// onRead, when set, runs once after the next disk read.
	onRead atomic.Pointer[func()]
	// reads counts disk reads.
	reads atomic.Int32
}

func newLegacy(t *testing.T) *legacy {
	t.Helper()
	db, _ := recordstest.Open(t)
	l := &legacy{}
	hook := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		if op.Kind == filesystem.OpRead {
			l.reads.Add(1)
			if fn := l.onRead.Swap(nil); fn != nil {
				(*fn)()
			}
		}
		return err
	}
	l.disk, _ = filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(hook))
	mgr, _ := filesystem.NewManager(l.disk)
	var err error
	if l.before, err = records.New(db, recordstest.Dialect(), mgr); err != nil {
		t.Fatal(err)
	}
	if _, err := l.before.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l.after, err = records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(dimensions.New())); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *legacy) record(t *testing.T, s *records.Store, key string, data []byte, meta map[string]string) records.File {
	t.Helper()
	if _, err := l.disk.WriteBytes(t.Context(), key, data, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: key, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (l *legacy) file(t *testing.T, id string) records.File {
	t.Helper()
	f, err := l.after.File(t.Context(), "t", id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBackfillMeasuresOlderFilesInBatches(t *testing.T) {
	l := newLegacy(t)
	ctx := t.Context()
	var ids []string
	for i, size := range [][2]int{{10, 20}, {30, 40}, {50, 60}} {
		f := l.record(t, l.before, "img"+string(rune('a'+i))+".png", pngOf(t, size[0], size[1]), map[string]string{"alt": "kept"})
		if _, _, ok := dimensions.Of(f); ok {
			t.Fatal("recorded with dimensions before the plugin")
		}
		ids = append(ids, f.ID)
	}
	text := l.record(t, l.before, "notes.txt", []byte("plain"), nil)
	broken := l.record(t, l.before, "broken.png", []byte("\x89PNG\r\n\x1a\nnot a header"), nil)
	present := l.record(t, l.after, "new.png", pngOf(t, 7, 8), nil)

	// In batches of two: every call resumes where the last one stopped.
	var total dimensions.BackfillResult
	cursor, calls := "", 0
	for {
		res, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t", Cursor: cursor, MaxFiles: 2, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		calls++
		total.Measured += res.Measured
		total.Present += res.Present
		total.Unreadable += res.Unreadable
		if cursor = res.NextCursor; cursor == "" {
			break
		}
		if calls > 10 {
			t.Fatal("backfill never finished")
		}
	}
	if total.Measured != 3 || total.Present != 1 || total.Unreadable != 1 || calls != 3 {
		t.Fatalf("total = %+v over %d calls", total, calls)
	}
	for i, want := range [][2]int{{10, 20}, {30, 40}, {50, 60}} {
		f := l.file(t, ids[i])
		if w, h, ok := dimensions.Of(f); !ok || w != want[0] || h != want[1] || f.Metadata["alt"] != "kept" {
			t.Errorf("file %d = %v", i, f.Metadata)
		}
	}
	if f := l.file(t, text.ID); f.Metadata != nil {
		t.Errorf("a text file was touched: %v", f.Metadata)
	}
	if _, _, ok := dimensions.Of(l.file(t, broken.ID)); ok {
		t.Error("an unreadable header got dimensions")
	}
	if w, _, _ := dimensions.Of(l.file(t, present.ID)); w != 7 {
		t.Error("an already measured file changed")
	}

	// Run twice: nothing left to measure, and only the file still without
	// dimensions (the unreadable one) is read again.
	l.reads.Store(0)
	res, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t"})
	if err != nil || res.Measured != 0 || res.Present != 4 || res.Unreadable != 1 || res.NextCursor != "" {
		t.Errorf("second run = %+v, %v", res, err)
	}
	if n := l.reads.Load(); n != 1 {
		t.Errorf("second run read %d objects, want 1", n)
	}
}

// TestBackfillNeverOverwritesAConcurrentReplace pins the compare-and-set:
// the file is replaced between the listing and the write, so the measured
// (old) dimensions must not land on the new content.
func TestBackfillNeverOverwritesAConcurrentReplace(t *testing.T) {
	l := newLegacy(t)
	ctx := t.Context()
	f := l.record(t, l.before, "a.png", pngOf(t, 10, 20), nil)
	replace := func() {
		anything := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}
		if _, err := l.after.Replace(ctx, "t", f.ID, bytes.NewReader(pngOf(t, 300, 400)), "", anything); err != nil {
			t.Error(err)
		}
	}
	l.onRead.Store(&replace)
	res, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t"})
	if err != nil || res.Changed != 1 || res.Measured != 0 {
		t.Fatalf("backfill = %+v, %v", res, err)
	}
	if w, h, _ := dimensions.Of(l.file(t, f.ID)); w != 300 || h != 400 {
		t.Errorf("dimensions = %dx%d, want the replacement's 300x400", w, h)
	}
}

func TestBackfillFailuresAndTrash(t *testing.T) {
	l := newLegacy(t)
	ctx := t.Context()
	gone := l.record(t, l.before, "gone.png", pngOf(t, 5, 5), nil)
	if err := l.disk.Delete(ctx, "gone.png"); err != nil {
		t.Fatal(err)
	}
	trashed := l.record(t, l.before, "trashed.png", pngOf(t, 6, 6), nil)
	if err := l.before.Trash(ctx, "t", trashed.ID); err != nil {
		t.Fatal(err)
	}
	res, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t"})
	if err != nil || len(res.Failed) != 1 || res.Failed[0].FileID != gone.ID || filesystem.CodeOf(res.Failed[0].Err) != filesystem.CodeNotFound {
		t.Fatalf("missing object = %+v, %v", res, err)
	}
	res, err = dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t", Trashed: true})
	if err != nil || res.Measured != 1 {
		t.Fatalf("trashed = %+v, %v", res, err)
	}
	if _, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t", Cursor: "bogus"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("bad cursor = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dimensions.Backfill(cancelled, l.after, dimensions.BackfillOptions{Scope: "t"}); err == nil {
		t.Error("a cancelled context did not stop the backfill")
	}
}

// TestBackfillStopsWhenTheContextEnds pins that a cancelled run returns
// its error with the progress so far, both between files and when the
// cancellation reaches the write.
func TestBackfillStopsWhenTheContextEnds(t *testing.T) {
	l := newLegacy(t)
	l.record(t, l.before, "a.png", pngOf(t, 5, 5), nil)
	l.record(t, l.before, "broken.png", []byte("\x89PNG\r\n\x1a\nnot a header"), nil)

	// Newest first: broken.png is read, then the context ends before a.png.
	ctx, cancel := context.WithCancel(t.Context())
	stop := func() { cancel() }
	l.onRead.Store(&stop)
	res, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t"})
	if err == nil || res.Unreadable != 1 || res.NextCursor == "" {
		t.Fatalf("cancelled between files = %+v, %v", res, err)
	}

	// Resuming from that cursor: a.png is read, then the write is cancelled.
	ctx, cancel = context.WithCancel(t.Context())
	l.onRead.Store(&stop)
	resumed, err := dimensions.Backfill(ctx, l.after, dimensions.BackfillOptions{Scope: "t", Cursor: res.NextCursor})
	if err == nil || resumed.Measured != 0 {
		t.Fatalf("cancelled at the write = %+v, %v", resumed, err)
	}
	cancel()
}
