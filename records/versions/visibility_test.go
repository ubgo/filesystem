package versions_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/upload"
)

func count(t *testing.T, d *filesystem.Disk) int {
	t.Helper()
	page, err := d.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Entries)
}

// TestVersionsFollowVisibility makes a public file private on a store
// that keeps public and private files on different disks: its old
// versions move with it, and restoring one still works.
func TestVersionsFollowVisibility(t *testing.T) {
	ctx := t.Context()
	pub, _ := filesystem.NewDisk("cdn", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	priv, _ := filesystem.NewDisk("vault", memory.New())
	mgr, _ := filesystem.NewManager(priv, pub)
	p, _ := versions.New()
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p), records.WithVisibilityDisks("cdn", "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	public := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := s.Upload(ctx, scope, "", strings.NewReader("one"), "doc.txt", public)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"two", "three"} {
		if _, err := s.Replace(ctx, scope, f.ID, strings.NewReader(body), "", public); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, pub) != 3 {
		t.Fatalf("public disk has %d objects before", count(t, pub))
	}
	if err := s.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	if count(t, pub) != 0 || count(t, priv) != 3 {
		t.Fatalf("after making it private: public %d, private %d", count(t, pub), count(t, priv))
	}
	list, _ := p.Versions(ctx, s, scope, f.ID)
	for _, v := range list {
		if v.Disk != "vault" {
			t.Fatalf("version %d stayed on %s", v.Number, v.Disk)
		}
	}
	restored, err := p.Restore(ctx, s, scope, list[len(list)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := priv.ReadAll(ctx, restored.Key); string(got) != "one" {
		t.Fatalf("restored %q", got)
	}
}

// TestVersionsVisibilityInPlace changes a file's visibility on one disk:
// its versions' objects change with it.
func TestVersionsVisibilityInPlace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	if err := e.store.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	list, _ := e.p.Versions(ctx, e.store, scope, f.ID)
	if st, _ := e.disk.Stat(ctx, list[0].Key); st.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("version visibility = %s", st.Visibility)
	}
}

// faultyDisks fails the nth storage operation across the disks it wraps.
type faultyDisks struct {
	n, seen int
}

func (f *faultyDisks) interceptor() filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		f.seen++
		if f.seen == f.n {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected disk fault")
		}
		return call(ctx)
	}
}

// splitStore is a store with a public and a private disk, both failing
// through faults, on a database that can fail through db.
type splitStore struct {
	s         *records.Store
	p         *versions.Plugin
	pub, priv *filesystem.Disk
	db        *testdb.Faults
	disks     *faultyDisks
	sql       *sql.DB
}

func newSplitStore(t *testing.T) *splitStore {
	t.Helper()
	e := &splitStore{disks: &faultyDisks{}}
	e.sql, e.db = testdb.OpenFaulty(t)
	in := filesystem.WithInterceptors(e.disks.interceptor())
	e.pub, _ = filesystem.NewDisk("cdn", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), in)
	e.priv, _ = filesystem.NewDisk("vault", memory.New(), in)
	mgr, _ := filesystem.NewManager(e.priv, e.pub)
	e.p, _ = versions.New()
	var err error
	if e.s, err = records.New(e.sql, testdb.Dialect(), mgr, records.WithPlugins(e.p), records.WithVisibilityDisks("cdn", "vault")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

// state renders version rows, file rows and every object with its
// visibility, on both disks.
func (e *splitStore) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT " + versions.ColID + ", " + versions.ColDisk + " FROM " + versions.TableNameVersions + " ORDER BY 1",
		"SELECT " + records.ColFileID + ", " + records.ColFileDisk + " FROM " + records.TableNameFiles + " ORDER BY 1",
	} {
		rows, err := e.sql.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var x, y string
			_ = rows.Scan(&x, &y)
			fmt.Fprintf(&b, "%s=%s;", x, y)
		}
		_ = rows.Close()
	}
	for _, d := range []*filesystem.Disk{e.pub, e.priv} {
		page, _ := d.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
		for _, en := range page.Entries {
			st, _ := d.Stat(t.Context(), en.Key)
			fmt.Fprintf(&b, "|%s:%s=%s", d.Name(), en.Key, string(st.Visibility))
		}
	}
	return b.String()
}

// TestVisibilityFaultSweep fails every database step, then every storage
// operation, of a visibility change that moves a file and its versions:
// each failure must leave rows and objects as they were.
func TestVisibilityFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newSplitStore(t)
	public := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := e.s.Upload(ctx, scope, "", strings.NewReader("one"), "doc.txt", public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Replace(ctx, scope, f.ID, strings.NewReader("two"), "", public); err != nil {
		t.Fatal(err)
	}
	change := func() error { return e.s.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPrivate) }
	sweep := func(name string, arm func(int), disarm func()) {
		t.Run(name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := e.state(t)
				arm(n)
				err := change()
				disarm()
				if err == nil {
					// It went through: put it back for the next sweep.
					if err := e.s.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPublic); err != nil {
						t.Fatal(err)
					}
					return
				}
				if after := e.state(t); after != before {
					t.Fatalf("failure %d changed the store:\nbefore %s\nafter  %s", n, before, after)
				}
			}
		})
	}
	sweep("database", e.db.Arm, func() { e.db.Arm(0) })
	sweep("storage", func(n int) { e.disks.n, e.disks.seen = n, 0 }, func() { e.disks.n = 0 })
}

// TestVisibilityHookContract calls OnEvent as any Store might, with a
// version or a file on a disk the store does not have.
func TestVisibilityHookContract(t *testing.T) {
	ctx := t.Context()
	e := newSplitStore(t)
	public := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}, Visibility: filesystem.VisibilityPublic}
	f, _ := e.s.Upload(ctx, scope, "", strings.NewReader("one"), "doc.txt", public)
	_, _ = e.s.Replace(ctx, scope, f.ID, strings.NewReader("two"), "", public)
	only, _ := filesystem.NewManager(e.priv)
	partial, _ := records.New(e.sql, testdb.Dialect(), only, records.WithPlugins(e.p))
	ev := records.Event{Kind: records.EventFileVisibilityChanged, Scope: scope, FileID: f.ID}
	if err := e.p.OnEvent(ctx, partial, ev); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("the file's disk missing = %v", err)
	}
	list, _ := e.p.Versions(ctx, e.s, scope, f.ID)
	if _, err := e.p.Restore(ctx, partial, scope, list[0].ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("restore with the version's disk missing = %v", err)
	}
	testdb.Exec(t, e.sql, "UPDATE "+versions.TableNameVersions+" SET "+versions.ColDisk+" = 'vault'")
	if _, err := e.p.Restore(ctx, partial, scope, list[0].ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("restore with the file's disk missing = %v", err)
	}
	testdb.Exec(t, e.sql, "UPDATE "+versions.TableNameVersions+" SET "+versions.ColDisk+" = 'cdn'")
	testdb.Exec(t, e.sql, "UPDATE "+records.TableNameFiles+" SET "+records.ColFileDisk+" = 'vault'")
	if err := e.p.OnEvent(ctx, partial, ev); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a version's disk missing = %v", err)
	}
	if err := e.p.OnEvent(ctx, partial, records.Event{Kind: records.EventFileVisibilityChanged, Scope: scope, FileID: "fil_missing"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing file = %v", err)
	}
	if err := e.p.OnEvent(ctx, partial, records.Event{Kind: records.EventFileReplaced}); err != nil {
		t.Fatalf("other events = %v", err)
	}
}

// TestInPlaceVisibilityStorageSweep fails every storage operation of a
// visibility change on one disk: the file and its versions keep their
// visibility each time.
func TestInPlaceVisibilityStorageSweep(t *testing.T) {
	ctx := t.Context()
	faults := &faultyDisks{}
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(faults.interceptor()))
	mgr, _ := filesystem.NewManager(disk)
	p, _ := versions.New()
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	anything := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	f, _ := s.Upload(ctx, scope, "media", strings.NewReader("one"), "doc.txt", anything)
	_, _ = s.Replace(ctx, scope, f.ID, strings.NewReader("two"), "", anything)
	visibilities := func() string {
		page, _ := disk.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
		var out []string
		for _, en := range page.Entries {
			st, _ := disk.Stat(ctx, en.Key)
			out = append(out, en.Key+"="+string(st.Visibility))
		}
		row, _ := s.File(ctx, scope, f.ID)
		return strings.Join(out, ",") + "|" + string(row.Visibility)
	}
	for n := 1; ; n++ {
		before := visibilities()
		faults.n, faults.seen = n, 0
		err := s.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPublic)
		faults.n = 0
		if err == nil {
			if !strings.Contains(visibilities(), "|public") {
				t.Fatal("the change did not happen")
			}
			return
		}
		if after := visibilities(); after != before {
			t.Fatalf("storage failure %d changed visibility:\nbefore %s\nafter  %s", n, before, after)
		}
	}
}

// splitWithVersions is a store with versions over a public and a private disk.
func splitWithVersions(t *testing.T) (*records.Store, *versions.Plugin, *filesystem.Disk, *filesystem.Disk) {
	t.Helper()
	pub, _ := filesystem.NewDisk("cdn", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	priv, _ := filesystem.NewDisk("vault", memory.New())
	mgr, _ := filesystem.NewManager(priv, pub)
	p, _ := versions.New()
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p), records.WithVisibilityDisks("cdn", "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, p, pub, priv
}

// TestVersionsFollowTrash: trashing a public file on visibility disks
// moves its kept versions to the private disk with it, and restoring it
// moves them back, so no old version stays public while it is trashed.
func TestVersionsFollowTrash(t *testing.T) {
	ctx := t.Context()
	s, p, pub, priv := splitWithVersions(t)
	public := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}, Visibility: filesystem.VisibilityPublic}
	f, err := s.Upload(ctx, scope, "", strings.NewReader("one"), "doc.txt", public)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"two", "three"} {
		if _, err := s.Replace(ctx, scope, f.ID, strings.NewReader(body), "", public); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if count(t, pub) != 0 || count(t, priv) != 3 {
		t.Fatalf("trashed: public %d, private %d", count(t, pub), count(t, priv))
	}
	if err := s.Restore(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if count(t, pub) != 3 || count(t, priv) != 0 {
		t.Fatalf("restored: public %d, private %d", count(t, pub), count(t, priv))
	}
	list, _ := p.Versions(ctx, s, scope, f.ID)
	for _, v := range list {
		if st, err := pub.Stat(ctx, v.Key); err != nil || v.Disk != "cdn" || st.Visibility != filesystem.VisibilityPublic {
			t.Fatalf("version %d after restoring: %+v, %v", v.Number, v, err)
		}
	}
	// A version whose object was lost does not block the trash.
	_ = pub.Delete(ctx, list[0].Key)
	if err := s.Trash(ctx, scope, f.ID); err != nil {
		t.Fatalf("trash with a lost version object = %v", err)
	}
	if count(t, pub) != 0 || count(t, priv) != 2 {
		t.Fatalf("trashed again: public %d, private %d", count(t, pub), count(t, priv))
	}
}

// TestVersionsTrashInPlace: on one disk, a trashed public file's versions
// are made private, and public again on Restore; a private file's are
// left alone, as are versions on a disk that cannot hide one object.
func TestVersionsTrashInPlace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	_ = e.store.Trash(ctx, scope, f.ID) // private: nothing to hide
	_ = e.store.Restore(ctx, scope, f.ID)
	if err := e.store.SetVisibility(ctx, scope, f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	visibilities := func() string {
		page, _ := e.disk.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
		var out []string
		for _, en := range page.Entries {
			st, _ := e.disk.Stat(ctx, en.Key)
			out = append(out, string(st.Visibility))
		}
		return strings.Join(out, ",")
	}
	if err := e.store.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := visibilities(); got != "private,private" {
		t.Fatalf("trashed: %s", got)
	}
	if err := e.store.Restore(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := visibilities(); got != "public,public" {
		t.Fatalf("restored: %s", got)
	}

	plainDisk, _ := filesystem.NewDisk("media", noVisibility{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	mgr, _ := filesystem.NewManager(plainDisk)
	p, _ := versions.New()
	s, _ := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = plainDisk.WriteBytes(ctx, "a.txt", []byte("one"), filesystem.WriteOptions{})
	g, _ := s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: "a.txt", Visibility: filesystem.VisibilityPublic})
	if _, err := s.Replace(ctx, scope, g.ID, strings.NewReader("two"), "", anything); err != nil {
		t.Fatal(err)
	}
	if err := s.Trash(ctx, scope, g.ID); err != nil {
		t.Fatalf("trash on a disk without per-object visibility = %v", err)
	}
}

// noVisibility is the memory driver without per-object visibility.
type noVisibility struct{ *memory.Driver }

func (n noVisibility) Capabilities() filesystem.Capabilities {
	c := n.Driver.Capabilities()
	c.SetVisibility = false
	return c
}

// TestRestoreOfTrashedFileCopiesNothing: restoring a version of a trashed
// file is refused before its object is copied.
func TestRestoreOfTrashedFileCopiesNothing(t *testing.T) {
	ctx := t.Context()
	writes := 0
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite || op.Kind == filesystem.OpCopy {
			writes++
		}
		return call(ctx)
	}))
	mgr, _ := filesystem.NewManager(disk)
	p, _ := versions.New()
	s, _ := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ := s.Upload(ctx, scope, "media", strings.NewReader("one"), "doc.txt", anything)
	_, _ = s.Replace(ctx, scope, f.ID, strings.NewReader("two"), "", anything)
	list, _ := p.Versions(ctx, s, scope, f.ID)
	_ = s.Trash(ctx, scope, f.ID)
	writes = 0
	if _, err := p.Restore(ctx, s, scope, list[0].ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Restore of a trashed file's version = %v", err)
	}
	if writes != 0 {
		t.Fatalf("the refused restore wrote %d objects", writes)
	}
}

// TestSweepOrphansKeepsVersions: a kept version's object has no fs_files
// row, but the plugin owns it, so the orphan sweep leaves it.
func TestSweepOrphansKeepsVersions(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, 5)
	f := e.upload(t, "one")
	e.replace(t, f.ID, "two")
	_, _ = e.disk.WriteBytes(ctx, "orphan.txt", []byte("x"), filesystem.WriteOptions{})
	n, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour))
	if n != 1 || err != nil || e.history(t, f.ID) != "1:one" || e.content(t, f.ID) != "two" {
		t.Fatalf("SweepOrphans = %d, %v; history %s", n, err, e.history(t, f.ID))
	}
	testdb.OnlySQLite(t)
	testdb.Exec(t, e.db, "DROP TABLE "+versions.TableNameVersions)
	if _, err := e.store.SweepOrphans(ctx, "media", "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a failed ownership query was ignored")
	}
}
