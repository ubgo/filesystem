package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/versions"
)

// testStore returns a migrated Store with versions and quota over a
// fresh SQLite file.
func testStore(t *testing.T) (*records.Store, *versions.Plugin) {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	s, v, _, err := newStore(t.Context(), db, disks)
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"uploaded on private, versions: 0",
		"edit 3 -> 4B, history: v3:3B@private v2:2B@private",
		"v1 dropped past the cap, object still stored: false",
		"quota counts 4 bytes (current content only)",
		"restored v2 -> 2B, history: v4:4B@private v3:3B@private",
		"deleted one, then published: history: v4:4B@public",
		`purged: history "", newest version object still stored: false`,
		"free-co after 3 edits keeps 1 version(s)", "pro-co after 3 edits keeps 3 version(s)",
		"Trim 40 days later drops 3 pro-co version(s) past 30 days",
		"restored copy stays in the tenant's prefix: true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPublishMovesVersionObjects(t *testing.T) {
	s, v := testStore(t)
	f, err := s.Upload(t.Context(), tenant, "", strings.NewReader("a"), "n.txt", textPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if f, err = edit(t.Context(), s, f, "bb"); err != nil {
		t.Fatal(err)
	}
	_, before, err := history(t.Context(), s, v, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := publish(t.Context(), s, f); err != nil {
		t.Fatal(err)
	}
	_, after, err := history(t.Context(), s, v, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old, err := exists(t.Context(), s, before[0]); err != nil || old {
		t.Fatalf("version left on %s: %t, %v", before[0].Disk, old, err)
	}
	if moved, err := exists(t.Context(), s, after[0]); err != nil || !moved || after[0].Disk != publicDisk {
		t.Fatalf("version on %s: %t, %v", after[0].Disk, moved, err)
	}
}

func TestKeepMustBePositive(t *testing.T) {
	if _, err := versions.New(versions.WithKeep(0)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("WithKeep(0) = %v", err)
	}
}

func TestUnknownVersionIsNotFound(t *testing.T) {
	s, v := testStore(t)
	if _, err := restore(t.Context(), s, v, "ver_missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("restore of an unknown version = %v", err)
	}
}

// TestKeepFuncFailsClosed pins that a replacement in a scope without a
// plan is refused with the KeepFunc's error and rolls back, leaving the
// content and the history as they were.
func TestKeepFuncFailsClosed(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	clock := &planClock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	s, v, err := newPlanStore(t.Context(), db, disks, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Upload(t.Context(), "no-plan", "", strings.NewReader("a"), "a.txt", tenantPolicy("no-plan"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(t.Context(), "no-plan", f.ID, strings.NewReader("bb"), "", tenantPolicy("no-plan")); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("replace without a plan = %v, want permission", err)
	}
	got, err := s.File(t.Context(), "no-plan", f.ID)
	if err != nil || got.Size != 1 {
		t.Fatalf("file after the refused replace = %+v, %v", got, err)
	}
	if list, err := v.Versions(t.Context(), s, "no-plan", f.ID); err != nil || len(list) != 0 {
		t.Fatalf("versions after the refused replace = %v, %v", list, err)
	}
}
