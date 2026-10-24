package quota_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/upload"
)

const heldScope = "t1"

// heldEnv is a Store with quota (limit bytes) and versions (keep per
// file) installed, quota counting held objects when held is true.
type heldEnv struct {
	store *records.Store
	quota *quota.Plugin
	vers  *versions.Plugin
}

func newHeldEnv(t *testing.T, limit int64, keep int, held bool, more ...records.Plugin) *heldEnv {
	t.Helper()
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	var opts []quota.Option
	if held {
		opts = append(opts, quota.WithHeldObjects())
	}
	q, err := quota.New(fixed(quota.Limit{Bytes: limit}), opts...)
	if err != nil {
		t.Fatal(err)
	}
	v, err := versions.New(versions.WithKeep(keep))
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(append([]records.Plugin{q, v}, more...)...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &heldEnv{store: s, quota: q, vers: v}
}

var heldPolicy = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

func (e *heldEnv) upload(t *testing.T, size int) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), heldScope, "media", strings.NewReader(strings.Repeat("a", size)), "doc.txt", heldPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// replace stores size bytes of fill as the file's new content.
func (e *heldEnv) replace(t *testing.T, id string, size int, fill string) error {
	t.Helper()
	_, err := e.store.Replace(t.Context(), heldScope, id, strings.NewReader(strings.Repeat(fill, size)), "", heldPolicy)
	return err
}

func (e *heldEnv) bytes(t *testing.T) int64 {
	t.Helper()
	u, err := e.quota.Usage(t.Context(), e.store, heldScope)
	if err != nil {
		t.Fatal(err)
	}
	return u.Bytes
}

// TestHeldObjects: with WithHeldObjects a kept version counts toward the
// byte limit from the moment it is kept until it is dropped, deleted or
// purged with its file, and Recount includes it.
func TestHeldObjects(t *testing.T) {
	ctx := t.Context()
	e := newHeldEnv(t, 100, 5, true)
	f := e.upload(t, 40)
	if err := e.replace(t, f.ID, 30, "b"); err != nil {
		t.Fatal(err)
	}
	if got := e.bytes(t); got != 70 { // 30 current + 40 kept
		t.Fatalf("after keeping a version: %d bytes", got)
	}
	// 50 current + 40 + 30 kept is 120: refused, and nothing changes.
	if err := e.replace(t, f.ID, 50, "c"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("a replacement whose kept version passes the limit = %v", err)
	}
	list, err := e.vers.Versions(ctx, e.store, heldScope, f.ID)
	if err != nil || len(list) != 1 || e.bytes(t) != 70 {
		t.Fatalf("the refused replacement left %d versions, %d bytes (%v)", len(list), e.bytes(t), err)
	}
	if err := e.vers.Delete(ctx, e.store, heldScope, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := e.bytes(t); got != 30 {
		t.Fatalf("after deleting the version: %d bytes", got)
	}
	if err := e.replace(t, f.ID, 50, "c"); err != nil {
		t.Fatal(err)
	}
	if got := e.bytes(t); got != 80 { // 50 current + 30 kept
		t.Fatalf("after the second replacement: %d bytes", got)
	}
	held, err := e.vers.HeldBytes(ctx, e.store, heldScope)
	if err != nil || held != 30 {
		t.Fatalf("HeldBytes = %d, %v", held, err)
	}
	if u, err := e.quota.Recount(ctx, e.store, heldScope); err != nil || u.Bytes != 80 || u.Files != 1 {
		t.Fatalf("Recount = %+v, %v", u, err)
	}
	if err := e.store.Purge(ctx, heldScope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.bytes(t); got != 0 {
		t.Fatalf("after purging the file and its versions: %d bytes", got)
	}
}

// TestHeldObjectsAtTheCap: a file at its version cap drops its oldest
// version before the new one is held, so it needs room only for the
// difference.
func TestHeldObjectsAtTheCap(t *testing.T) {
	e := newHeldEnv(t, 100, 1, true)
	f := e.upload(t, 40)
	if err := e.replace(t, f.ID, 40, "b"); err != nil {
		t.Fatal(err)
	}
	if got := e.bytes(t); got != 80 {
		t.Fatalf("after the first replacement: %d bytes", got)
	}
	if err := e.replace(t, f.ID, 40, "c"); err != nil {
		t.Fatalf("a replacement at the cap that frees what it holds: %v", err)
	}
	if got := e.bytes(t); got != 80 {
		t.Fatalf("after replacing at the cap: %d bytes", got)
	}
}

// TestHeldObjectsOffByDefault: without WithHeldObjects kept versions are
// not charged, as before the option existed.
func TestHeldObjectsOffByDefault(t *testing.T) {
	ctx := t.Context()
	e := newHeldEnv(t, 100, 5, false)
	f := e.upload(t, 40)
	for _, fill := range []string{"b", "c"} {
		if err := e.replace(t, f.ID, 60, fill); err != nil {
			t.Fatalf("an uncounted version was refused: %v", err)
		}
	}
	if got := e.bytes(t); got != 60 {
		t.Fatalf("usage counted versions: %d bytes", got)
	}
	if u, err := e.quota.Recount(ctx, e.store, heldScope); err != nil || u.Bytes != 60 {
		t.Fatalf("Recount = %+v, %v", u, err)
	}
	if err := e.store.Purge(ctx, heldScope, f.ID); err != nil || e.bytes(t) != 0 {
		t.Fatalf("after purge: %d bytes, %v", e.bytes(t), err)
	}
}

// brokenCounter is a records.HeldCounter whose total cannot be read.
type brokenCounter struct{ err error }

func (brokenCounter) Name() string { return "broken" }

func (b brokenCounter) HeldBytes(context.Context, *records.Store, string) (int64, error) {
	return 0, b.err
}

// TestRecountHeldFailure: with WithHeldObjects a holder that cannot report
// its bytes fails Recount, which leaves the usage row as it was rather
// than writing a total that misses those bytes.
func TestRecountHeldFailure(t *testing.T) {
	broken := errors.New("cannot count")
	e := newHeldEnv(t, 100, 5, true, brokenCounter{err: broken})
	f := e.upload(t, 40)
	if err := e.replace(t, f.ID, 30, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.quota.Recount(t.Context(), e.store, heldScope); !errors.Is(err, broken) {
		t.Fatalf("Recount = %v", err)
	}
	if got := e.bytes(t); got != 70 {
		t.Fatalf("a failed Recount changed usage to %d", got)
	}
}
