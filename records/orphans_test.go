package records_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// holder is a plugin holding some objects of its own (as kept versions
// and image variants are), or failing to say which.
type holder struct {
	held []string
	err  error
}

func (*holder) Name() string { return "holder" }
func (k *holder) OwnedObjects(_ context.Context, _ *records.Store, disk string, keys []string) (map[string]bool, error) {
	if k.err != nil {
		return nil, k.err
	}
	out := map[string]bool{}
	for _, key := range keys {
		out[key] = disk == "media" && slices.Contains(k.held, key)
	}
	return out, nil
}

// diskKeys lists every key on d.
func diskKeys(t *testing.T, d *filesystem.Disk) string {
	t.Helper()
	var out []string
	for en, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, en.Key)
	}
	return strings.Join(out, ",")
}

// TestSweepOrphans deletes only objects no row names and no plugin holds,
// older than the cutoff, under the prefix; recorded files, live or
// trashed, are never touched, even with the root as the prefix.
func TestSweepOrphans(t *testing.T) {
	ctx := t.Context()
	k := &holder{held: []string{"variants/a-thumb.png"}}
	e := newEnv(t, nil, records.WithPlugins(k, &refuser{})) // refuser owns no objects
	live := e.put(t, "t", "live.png")
	trashed := e.put(t, "u", "trashed.png")
	_ = e.store.Trash(ctx, "u", trashed.ID)
	for _, key := range []string{"orphan.png", "sub/orphan.png", "variants/a-thumb.png"} {
		if _, err := e.disk.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(-time.Hour)); n != 0 || err != nil {
		t.Fatalf("young objects swept: %d, %v", n, err)
	}
	if n, err := e.store.SweepOrphans(ctx, "media", "sub", time.Now().Add(time.Hour)); n != 1 || err != nil {
		t.Fatalf("SweepOrphans(sub) = %d, %v", n, err)
	}
	n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour))
	if n != 1 || err != nil {
		t.Fatalf("SweepOrphans = %d, %v", n, err)
	}
	if got, want := diskKeys(t, e.disk), "live.png,trashed.png,variants/a-thumb.png"; got != want {
		t.Fatalf("left %s, want %s", got, want)
	}
	for _, f := range []records.File{live, trashed} {
		if _, err := e.store.File(ctx, f.Scope, f.ID); err != nil {
			t.Fatal(err)
		}
	}

	_, err = e.store.SweepOrphans(ctx, "nowhere", "", time.Now())
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.store.SweepOrphans(ctx, "media", "../up", time.Now())
	wantCode(t, err, filesystem.CodeInvalidKey)
	k.err = errors.New("plugin down")
	if _, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour)); !errors.Is(err, k.err) || !strings.Contains(err.Error(), `plugin "holder"`) {
		t.Fatalf("plugin failure = %v", err)
	}
}

// TestSweepOrphansManyPages sweeps more objects than one batch.
func TestSweepOrphansManyPages(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "keep.png")
	const orphans = 1203 // more than two batches, and not a multiple of one
	for i := range orphans {
		if _, err := e.disk.WriteBytes(ctx, fmt.Sprintf("o/%05d", i), []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour)); n != orphans || err != nil {
		t.Fatalf("SweepOrphans = %d, %v", n, err)
	}
	if got := diskKeys(t, e.disk); got != f.Key {
		t.Fatalf("left %s", got)
	}
}

// undatedDriver reports no ModTime, as some backends do not.
type undatedDriver struct{ *memory.Driver }

func (u undatedDriver) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	p, err := u.Driver.List(ctx, dir, o)
	for i := range p.Entries {
		p.Entries[i].ModTime = time.Time{}
	}
	return p, err
}

// TestSweepOrphansFailures: an undeletable orphan does not stop the
// sweep and is named in the error; an object of unknown age is kept; a
// database failure stops the sweep.
func TestSweepOrphansFailures(t *testing.T) {
	ctx := t.Context()
	bad := newEnv(t, undeletable{memory.New()})
	_, _ = bad.disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{})
	_, _ = bad.disk.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{})
	n, err := bad.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour))
	if n != 0 || filesystem.CodeOf(err) != filesystem.CodePermission || !strings.Contains(err.Error(), `"a.png"`) || !strings.Contains(err.Error(), `"b.png"`) {
		t.Fatalf("undeletable orphans = %d, %v", n, err)
	}

	undated := newEnv(t, undatedDriver{memory.New()})
	_, _ = undated.disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{})
	if n, err := undated.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour)); n != 0 || err != nil {
		t.Fatalf("an object of unknown age swept: %d, %v", n, err)
	}

	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, nil)
	for i := range 501 { // a full batch, then a partial one
		_, _ = e.disk.WriteBytes(ctx, fmt.Sprintf("o/%05d", i), []byte("x"), filesystem.WriteOptions{})
	}
	for _, step := range []int{1, 2} {
		faults.Arm(step)
		_, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour))
		faults.Arm(0)
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("database failure at step %d = %v", step, err)
		}
	}

	// A listing failure stops the sweep.
	failing, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpList {
			return filesystem.Errorf(filesystem.CodeUnavailable, "listing down")
		}
		return call(ctx)
	}))
	mgr, _ := filesystem.NewManager(failing)
	s, _ := records.New(openDB(t), testDialect(), mgr)
	if _, err := s.SweepOrphans(ctx, "media", "", time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("listing failure = %v", err)
	}
}

// TestFindOrphans reports exactly what SweepOrphans would delete and
// deletes nothing.
func TestFindOrphans(t *testing.T) {
	ctx := t.Context()
	k := &holder{held: []string{"variants/a-thumb.png"}}
	e := newEnv(t, nil, records.WithPlugins(k))
	e.put(t, "t", "live.png")
	for _, key := range []string{"orphan.png", "sub/orphan.png", "variants/a-thumb.png"} {
		if _, err := e.disk.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	before := diskKeys(t, e.disk)
	var found []string
	n, err := e.store.FindOrphans(ctx, "media", "", time.Now().Add(time.Hour), func(key string) { found = append(found, key) })
	if err != nil || n != 2 || strings.Join(found, ",") != "orphan.png,sub/orphan.png" {
		t.Fatalf("FindOrphans = %d %v, %v", n, found, err)
	}
	if diskKeys(t, e.disk) != before {
		t.Fatal("FindOrphans deleted something")
	}
	if n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour)); n != 2 || err != nil {
		t.Fatalf("the sweep after = %d, %v", n, err)
	}
	_, err = e.store.FindOrphans(ctx, "nowhere", "", time.Now(), func(string) {})
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestMissingObjects reports rows whose object is gone, in every scope and
// across batches, and changes no row.
func TestMissingObjects(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	var lost []string
	for i := range 503 { // a full batch, then a partial one
		f := e.put(t, fmt.Sprintf("s%d", i%3), fmt.Sprintf("o/%05d.png", i))
		if i != 7 { // every object but one is lost, so both batches report
			if err := e.disk.Delete(ctx, f.Key); err != nil {
				t.Fatal(err)
			}
			lost = append(lost, f.ID)
		}
	}
	trashed := e.put(t, "t", "trashed.png")
	_ = e.store.Trash(ctx, "t", trashed.ID)
	_ = e.disk.Delete(ctx, trashed.Key)
	lost = append(lost, trashed.ID)
	var got []string
	n, err := e.store.MissingObjects(ctx, "media", func(f records.File) { got = append(got, f.ID) })
	slices.Sort(got)
	slices.Sort(lost)
	if err != nil || n != len(lost) || !slices.Equal(got, lost) {
		t.Fatalf("MissingObjects = %d, %v; want %d", n, err, len(lost))
	}
	if _, err := e.store.File(ctx, "t", trashed.ID); err != nil {
		t.Fatalf("a reported row was changed: %v", err)
	}
	_, err = e.store.MissingObjects(ctx, "nowhere", func(records.File) {})
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestMissingObjectsFailures(t *testing.T) {
	ctx := t.Context()
	failing := filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpStat {
			return filesystem.Errorf(filesystem.CodeUnavailable, "down")
		}
		return call(ctx)
	})
	disk, _ := filesystem.NewDisk("media", memory.New(), failing)
	mgr, _ := filesystem.NewManager(disk)
	db := openDB(t)
	s, _ := records.New(db, testDialect(), mgr)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "INSERT INTO "+records.TableNameFiles+" (id, scope, disk, storage_key, filename, content_type, size, sha256, visibility, metadata, created_at, uploaded_by) "+
		"VALUES ('fil_x', 't', 'media', 'x.png', 'x.png', 'image/png', 1, '', 'private', '{}', '2026-10-06 00:00:00', '')")
	if _, err := s.MissingObjects(ctx, "media", func(records.File) {}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("storage failure = %v", err)
	}
	exec(t, db, "DROP TABLE "+records.TableNameFiles)
	if _, err := s.MissingObjects(ctx, "media", func(records.File) {}); err == nil {
		t.Fatal("a database failure was ignored")
	}
}
