package versions_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/sqlb"
)

// refuser is a FileDiskMover installed after versions that fails every
// move while err is set, so the move rolls back after versions moved its
// objects; clearing err lets a rerun through.
type refuser struct{ err error }

func (*refuser) Name() string { return "refuser" }

func (r *refuser) MoveFileDisk(context.Context, *records.Store, records.File, string) error {
	return r.err
}

// moveEnv is a Store with three disks, "local", "bucket" and "archive", versions
// keeping 5 per file, and more plugins installed after it.
func moveEnv(t *testing.T, more ...records.Plugin) (*records.Store, *versions.Plugin, *filesystem.Disk, *filesystem.Disk) {
	t.Helper()
	db, _ := testdb.OpenFaulty(t)
	local, _ := filesystem.NewDisk("local", memory.New())
	bucket, _ := filesystem.NewDisk("bucket", memory.New())
	archive, _ := filesystem.NewDisk("archive", memory.New())
	mgr, _ := filesystem.NewManager(local, bucket, archive)
	p, err := versions.New(versions.WithKeep(5))
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(append([]records.Plugin{p}, more...)...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s, p, local, bucket
}

func exists(t *testing.T, d *filesystem.Disk, key string) bool {
	t.Helper()
	ok, err := d.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestMoveFileTakesVersions: MoveFile moves a file's kept versions to the
// new disk with it, so they still read and restore there, and their old
// objects are deleted after the commit.
func TestMoveFileTakesVersions(t *testing.T) {
	ctx := t.Context()
	s, p, local, bucket := moveEnv(t)
	f, err := s.Upload(ctx, scope, "local", strings.NewReader("v1"), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"v2", "v3"} {
		if _, err := s.Replace(ctx, scope, f.ID, strings.NewReader(body), "", anything); err != nil {
			t.Fatal(err)
		}
	}
	before := mustVersions(t, p, s, f.ID)
	if _, err := s.MoveFile(ctx, scope, f.ID, "bucket"); err != nil {
		t.Fatal(err)
	}
	after := mustVersions(t, p, s, f.ID)
	if len(after) != 2 {
		t.Fatalf("versions after the move = %+v", after)
	}
	for i, v := range after {
		if v.Disk != "bucket" || !exists(t, bucket, v.Key) || exists(t, local, before[i].Key) {
			t.Fatalf("version %d after the move: %+v (on bucket %v, still on local %v)", v.Number, v, exists(t, bucket, v.Key), exists(t, local, before[i].Key))
		}
	}
	if _, err := p.Restore(ctx, s, scope, after[1].ID); err != nil {
		t.Fatalf("restore after the move: %v", err)
	}
	got, err := s.File(ctx, scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := bucket.ReadAll(ctx, got.Key); err != nil || string(data) != "v1" {
		t.Fatalf("restored content = %q, %v", data, err)
	}
}

// TestMoveFileRollbackKeepsVersions: when the move fails after versions
// moved its objects, the rows still name the old disk, the old objects
// are there and the copies are gone; rerunning the move then finishes it,
// and a third run (the file already on the target) changes nothing.
func TestMoveFileRollbackKeepsVersions(t *testing.T) {
	ctx := t.Context()
	refused := errors.New("refused")
	r := &refuser{err: refused}
	s, p, local, bucket := moveEnv(t, r)
	f, err := s.Upload(ctx, scope, "local", strings.NewReader("v1"), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(ctx, scope, f.ID, strings.NewReader("v2"), "", anything); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveFile(ctx, scope, f.ID, "bucket"); !errors.Is(err, refused) {
		t.Fatalf("MoveFile = %v", err)
	}
	kept := mustVersions(t, p, s, f.ID)
	for _, v := range kept {
		if v.Disk != "local" || !exists(t, local, v.Key) || exists(t, bucket, v.Key) {
			t.Fatalf("a rolled-back move changed version %+v", v)
		}
	}

	r.err = nil
	for run := range 2 {
		if _, err := s.MoveFile(ctx, scope, f.ID, "bucket"); err != nil {
			t.Fatalf("run %d: %v", run+2, err)
		}
		for _, v := range mustVersions(t, p, s, f.ID) {
			if v.Disk != "bucket" || !exists(t, bucket, v.Key) || exists(t, local, v.Key) {
				t.Fatalf("run %d: version %+v (on bucket %v, still on local %v)", run+2, v, exists(t, bucket, v.Key), exists(t, local, v.Key))
			}
		}
	}
	if got := mustVersions(t, p, s, f.ID); len(got) != len(kept) {
		t.Fatalf("the reruns changed the history: %+v", got)
	}
}

// TestMoveFileLeavesOtherDisks: a version already on another disk than
// the file's is not touched by the file's move.
func TestMoveFileLeavesOtherDisks(t *testing.T) {
	ctx := t.Context()
	s, p, local, _ := moveEnv(t)
	archive, err := s.Disks().Disk("archive")
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Upload(ctx, scope, "local", strings.NewReader("v1"), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(ctx, scope, f.ID, strings.NewReader("v2"), "", anything); err != nil {
		t.Fatal(err)
	}
	// The kept version sits on archive (moved there before the file moved).
	v := mustVersions(t, p, s, f.ID)[0]
	if _, err := filesystem.CopyAcross(ctx, local, v.Key, archive, v.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(versions.Table).Set(sqlb.Set(versions.Disk, "archive")).Where(sqlb.Eq(versions.ID, v.ID))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveFile(ctx, scope, f.ID, "bucket"); err != nil {
		t.Fatal(err)
	}
	if got := mustVersions(t, p, s, f.ID)[0]; got.Disk != "archive" || !exists(t, archive, got.Key) {
		t.Fatalf("a version on another disk moved: %+v", got)
	}
}

// TestMoveFileDiskErrors: called directly, MoveFileDisk refuses a disk
// the Manager does not have (as either end) and returns a failed read,
// changing nothing.
func TestMoveFileDiskErrors(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	local, _ := filesystem.NewDisk("local", memory.New())
	mgr, _ := filesystem.NewManager(local)
	p, err := versions.New()
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := records.File{ID: "fil_x", Scope: scope, Disk: "local"}
	if err := p.MoveFileDisk(ctx, s, f, "nowhere"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("an unknown target disk = %v", err)
	}
	f.Disk = "gone"
	if err := p.MoveFileDisk(ctx, s, f, "local"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("an unknown source disk = %v", err)
	}
	faults.Arm(1)
	err = p.MoveFileDisk(ctx, s, f, "local")
	faults.Arm(0)
	if !errors.Is(err, testdb.ErrInjected) {
		t.Fatalf("a failed read = %v", err)
	}

	// A version whose row cannot be updated fails the move.
	kept, err := s.Upload(ctx, scope, "local", strings.NewReader("v1"), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(ctx, scope, kept.ID, strings.NewReader("v2"), "", anything); err != nil {
		t.Fatal(err)
	}
	archive, _ := filesystem.NewDisk("archive", memory.New())
	if err := mgr.Add(archive); err != nil {
		t.Fatal(err)
	}
	faults.Arm(2) // the versions read, then the row update
	err = p.MoveFileDisk(ctx, s, kept, "archive")
	faults.Arm(0)
	if !errors.Is(err, testdb.ErrInjected) {
		t.Fatalf("a failed row update = %v", err)
	}
}
