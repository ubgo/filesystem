package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2026-10-03 2400 bytes in 3 requests", "2026-10-04 1000 bytes in 1 requests", "total 3400 bytes in 4 requests",
		"report on day 2: 1 event(s)", "report on day 3 (first run): 1 event(s)", "report on day 3 (second run): 0 event(s)",
		"billed: 2026-10-03 +2400 bytes +3 requests; 2026-10-04 +2000 bytes +2 requests", "pruned 2 reported day(s)",
		// WithBucket(tenantDay): the default UTC day would put Tokyo's read on 2026-10-03 too.
		"acme-tokyo: read at 2026-10-03 20:00 UTC lands on 2026-10-04",
		"globex-newyork: read at 2026-10-03 20:00 UTC lands on 2026-10-03",
		"report at 2026-10-04 10:00 UTC: 1 event(s): 2026-10-03 +500 bytes +1 requests",
		"per link: press 2000 bytes in 2 requests", "per link: partner 1000 bytes in 1 requests",
		"per link: scope 3000 bytes in 3 requests",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestNegativeBytes pins that a read with negative bytes is refused
// rather than subtracted from the bill.
func TestNegativeBytes(t *testing.T) {
	s, _, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Served(t.Context(), records.Served{Kind: records.ServeStream, Scope: tenant, FileID: "fil_x", Bytes: -1})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Served with -1 bytes = %v, want invalid_argument", err)
	}
}

// TestPerSubjectNeedsTheKind pins that the breakdown is opt-in per kind:
// a plain link read (records.ServeLink) on the per-link Store counts in
// the scope only, and leaves no subject bucket behind.
func TestPerSubjectNeedsTheKind(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	s, eg, _, err := newPerLinkStore(ctx, db, func() time.Time { return day })
	if err != nil {
		t.Fatal(err)
	}
	f, err := record(ctx, s, "a.pdf", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.URL(ctx, f, linkTTL, records.ServedAs(records.ServeLink, "not-a-link")); err != nil {
		t.Fatal(err)
	}
	days, err := eg.SubjectDays(ctx, s, tenant, records.ServeLink, "not-a-link", day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	total, err := eg.Total(ctx, s, tenant, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 0 || total.Bytes != 3 {
		t.Errorf("subject buckets %v, scope bytes %d; want none and 3", days, total.Bytes)
	}
}
