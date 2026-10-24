package records_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// scopeHolder refuses to purge the file whose key is hold (a legal hold),
// and records the scopes PurgeScope hands it, failing when fail is set.
type scopeHolder struct {
	hold   string
	fail   error
	scopes []string
	purged int
	// onPurge runs after each purge it allows.
	onPurge func(ctx context.Context, s *records.Store)
}

func (h *scopeHolder) Name() string { return "holder" }
func (h *scopeHolder) BeforeCreate(context.Context, *records.Store, *records.File) error {
	return nil
}
func (h *scopeHolder) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (h *scopeHolder) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	if f.Key == h.hold {
		return errors.New("legal hold")
	}
	h.purged++
	if h.onPurge != nil {
		h.onPurge(ctx, s)
	}
	return nil
}
func (h *scopeHolder) PurgeScope(_ context.Context, _ *records.Store, scope string) error {
	if h.fail != nil {
		return h.fail
	}
	h.scopes = append(h.scopes, scope)
	return nil
}

func TestPurgeScope(t *testing.T) {
	ctx := t.Context()
	h := &scopeHolder{}
	e := newEnv(t, nil, records.WithPlugins(h))
	// More than two batches, some attached, one trashed.
	total := 2*records.PurgeScopeBatch + 5
	var files []records.File
	for i := range total {
		files = append(files, e.put(t, "t", fmt.Sprintf("t/%03d.png", i)))
	}
	post := records.Owner{Type: "post", ID: "p1"}
	_, _ = e.store.Attach(ctx, "t", files[0].ID, post, "gallery")
	_ = e.store.Trash(ctx, "t", files[1].ID)
	keep := e.put(t, "u", "u/keep.png")

	// A file that cannot be purged stops the run part-way; the scope's
	// plugin rows are not touched while files remain.
	hold := files[0]
	for _, f := range files {
		if f.ID > hold.ID {
			hold = f
		}
	}
	h.hold = hold.Key
	n, err := e.store.PurgeScope(ctx, "t")
	if err == nil || n != total-1 || len(h.scopes) != 0 {
		t.Fatalf("held PurgeScope = %d, %v, scope purges %v", n, err, h.scopes)
	}
	if _, err := e.store.File(ctx, "t", hold.ID); err != nil {
		t.Fatalf("the held file went: %v", err)
	}

	// Run again once the hold is lifted: it continues where it stopped.
	h.hold = ""
	n, err = e.store.PurgeScope(ctx, "t")
	if err != nil || n != 1 || len(h.scopes) != 1 || h.scopes[0] != "t" {
		t.Fatalf("resumed PurgeScope = %d, %v, scope purges %v", n, err, h.scopes)
	}
	for _, f := range []records.File{files[0], files[1], hold} {
		if ok, _ := e.disk.Exists(ctx, f.Key); ok {
			t.Errorf("object %s survived", f.Key)
		}
	}
	for _, trashed := range []bool{false, true} {
		if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Trashed: trashed}); len(page.Files) != 0 {
			t.Fatalf("%d files left (trashed=%v)", len(page.Files), trashed)
		}
	}
	if list, _ := e.store.Attachments(ctx, "t", post, "gallery"); len(list) != 0 {
		t.Fatalf("attachments left: %v", list)
	}
	if _, err := e.store.File(ctx, "u", keep.ID); err != nil {
		t.Fatalf("another scope's file went: %v", err)
	}
	if h.purged != total {
		t.Fatalf("BeforePurge ran %d times, want %d", h.purged, total)
	}

	// A plugin failing its scope purge is returned; a rerun retries it.
	h.fail = errors.New("refused")
	if _, err := e.store.PurgeScope(ctx, "t"); !errors.Is(err, h.fail) {
		t.Fatalf("PurgeScope with a failing plugin = %v", err)
	}
	h.fail = nil
	if n, err := e.store.PurgeScope(ctx, "t"); err != nil || n != 0 || len(h.scopes) != 2 {
		t.Fatalf("empty scope again = %d, %v, %v", n, err, h.scopes)
	}

	_, err = e.store.PurgeScope(ctx, "")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// A cancelled ctx stops the run between files.
	cancelled, cancel := context.WithCancel(ctx)
	// Cancelled once the first file's purge has committed.
	h.onPurge = func(ctx context.Context, s *records.Store) { s.AfterCommit(ctx, func(context.Context) { cancel() }) }
	e.put(t, "v", "v/a.png")
	e.put(t, "v", "v/b.png")
	if n, err := e.store.PurgeScope(cancelled, "v"); n != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PurgeScope = %d, %v", n, err)
	}
	h.onPurge = nil
	exec(t, e.db, "DROP TABLE "+records.TableNameFiles)
	if _, err := e.store.PurgeScope(ctx, "v"); err == nil {
		t.Fatal("a failed listing was ignored")
	}
}

// racer is a ScopePurger that stages an upload landing in the scope while
// its rows are purged: the file is recorded inside the final transaction,
// so the Store's count sees it, and, should that transaction roll back,
// recorded again once it has, as an upload committed on its own would be.
// It does so the first times calls, then stays quiet.
type racer struct {
	disk  *filesystem.Disk
	times int
	calls int
}

func (r *racer) Name() string { return "racer" }

func (r *racer) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	r.calls++
	if r.calls > r.times {
		return nil
	}
	key := fmt.Sprintf("late-%d.png", r.calls)
	record := func(ctx context.Context) error {
		if _, err := r.disk.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			return err
		}
		_, err := s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: key})
		return err
	}
	s.OnRollback(ctx, func(ctx context.Context) { _ = record(context.WithoutCancel(context.Background())) })
	return record(ctx)
}

func TestPurgeScopeUploadRace(t *testing.T) {
	ctx := t.Context()
	h, r := &scopeHolder{}, &racer{times: 1}
	e := newEnv(t, nil, records.WithPlugins(h, r))
	r.disk = e.disk
	e.put(t, "t", "a.png")
	n, err := e.store.PurgeScope(ctx, "t")
	if err != nil || n != 2 {
		t.Fatalf("PurgeScope = %d, %v; want the late file purged too", n, err)
	}
	for _, trashed := range []bool{false, true} {
		if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t", Trashed: trashed}); len(page.Files) != 0 {
			t.Fatalf("files left after the purge: %d", len(page.Files))
		}
	}
	if len(h.scopes) != 2 || r.calls != 2 {
		t.Fatalf("scope purgers ran %d and %d times, want 2 (the first rolled back)", len(h.scopes), r.calls)
	}

	// A scope that keeps receiving files is given up on.
	r.calls, r.times = 0, 100
	e.put(t, "u", "b.png")
	_, err = e.store.PurgeScope(ctx, "u")
	wantCode(t, err, filesystem.CodeUnavailable)
	if r.calls != 3 {
		t.Fatalf("rounds = %d, want 3", r.calls)
	}
}

// dropper is a ScopePurger that drops fs_files inside the final
// transaction, so the count after it fails (and the rollback restores
// the table on SQLite, where DDL is transactional).
type dropper struct{}

func (dropper) Name() string { return "dropper" }
func (dropper) PurgeScope(ctx context.Context, s *records.Store, _ string) error {
	_, err := s.Conn(ctx).ExecContext(ctx, "DROP TABLE "+records.TableNameFiles)
	return err
}

func TestPurgeScopeCountFailure(t *testing.T) {
	onlySQLite(t)
	e := newEnv(t, nil, records.WithPlugins(dropper{}))
	if _, err := e.store.PurgeScope(t.Context(), "t"); err == nil {
		t.Fatal("a failed count was ignored")
	}
}
