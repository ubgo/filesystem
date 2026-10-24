package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
)

// testStore returns a migrated Store with the quota plugin over a fresh
// SQLite file.
func testStore(t *testing.T) (*records.Store, *quota.Plugin) {
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
	s, q, err := newStore(t.Context(), db, disks)
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"two uploads:           acme: 18 of 25 bytes, 2 of 3 files",
		"over the byte limit:   quota_exceeded",
		"after a rollback:      acme: 18 of 25 bytes, 2 of 3 files",
		"replaced 10 -> 4:      acme: 12 of 25 bytes, 2 of 3 files",
		"growth past the limit: quota_exceeded",
		"trashed:               acme: 12 of 25 bytes, 2 of 3 files",
		"purged:                acme: 8 of 25 bytes, 1 of 3 files",
		"recount:               8 bytes, 1 files",
		"second ticket while the first holds the room: quota_exceeded",
		"reserved 15 of 25 bytes", "confirmed: 5 bytes used, 0 reserved",
		"abandoned ticket released: true", "usage events: 5 bytes (5)",
		"org over its plan:     quota_exceeded",
		"  org:initech: 18 bytes, 2 files", "  type:initech-north:text: 12 bytes, 1 files",
		"rows deleted, then RecountDimensions:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestFileLimit(t *testing.T) {
	s, _ := testStore(t)
	for range freePlan.Files {
		if _, err := uploadText(t.Context(), s, freeTenant, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := uploadText(t.Context(), s, freeTenant, "x"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("upload past the file limit = %v, want quota_exceeded", err)
	}
}

func TestUnknownScopeIsRefusedByTheLimitFunc(t *testing.T) {
	s, _ := testStore(t)
	if _, err := uploadText(t.Context(), s, "nobody", "x"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("upload of an unknown scope = %v, want permission", err)
	}
}

func TestUsageOfANewScopeIsZero(t *testing.T) {
	s, q := testStore(t)
	u, err := q.Usage(t.Context(), s, proTenant)
	if err != nil || u != (quota.Usage{}) {
		t.Fatalf("Usage = %+v, %v", u, err)
	}
}

func TestRecountRebuildsADeletedRow(t *testing.T) {
	s, q := testStore(t)
	if _, err := uploadWithRetry(t.Context(), s, proTenant, "hello"); err != nil {
		t.Fatal(err)
	}
	del := "DELETE FROM " + quota.TableNameUsage
	if _, err := s.Conn(t.Context()).ExecContext(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	u, err := repair(t.Context(), s, q, proTenant)
	if err != nil || u != (quota.Usage{Bytes: 5, Files: 1}) {
		t.Fatalf("Recount = %+v, %v", u, err)
	}
}

// TestDimensionsFailClosed pins that a workspace the DimensionFunc does
// not know is refused, not stored uncounted, and that the organisation's
// limit spans both workspaces.
func TestDimensionsFailClosed(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "orgs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	s, q, err := newOrgStore(t.Context(), db, disks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uploadText(t.Context(), s, "stray-workspace", "x"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("upload without an organisation = %v, want permission", err)
	}
	if _, err := uploadText(t.Context(), s, northWS, strings.Repeat("x", int(orgPlan.Bytes))); err != nil {
		t.Fatal(err)
	}
	if _, err := uploadText(t.Context(), s, southWS, "y"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("upload past the organisation's plan from the other workspace = %v, want quota_exceeded", err)
	}
	if u, err := q.DimensionUsage(t.Context(), s, "org:"+orgID); err != nil || u.Bytes != orgPlan.Bytes {
		t.Fatalf("org usage = %+v, %v", u, err)
	}
}
