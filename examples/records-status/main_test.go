package main

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/scan"
)

// TestRun runs the demo and checks every step's outcome.
func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"poll before: processing (queued)",
		"event: photos/good.png is processing (thumbnails 1/2)",
		"event: photos/good.png is ready",
		"event: photos/bad.png is processing (thumbnails 1/2)",
		"event: photos/bad.png is failed (thumbnail: unsupported format)",
		"poll after: ready",
		"failed: photos/bad.png: thumbnail: unsupported format",
		"quarantine: notes.txt is quarantined",
		"quarantine: link before the scan: conflict (true)",
		"quarantine: make public before the scan: conflict",
		"quarantine: scanned invoice.txt: quarantined infected: Eicar-Test-Signature",
		"quarantine: scanned notes.txt: ready ",
		"quarantine: link after the scan: ok",
	}, "\n") + "\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestScanFailurePurge pins records.ScanFailurePurge: an infected file is
// purged, row and object, instead of kept for an administrator.
func TestScanFailurePurge(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, err := newQuarantineStore(t.Context(), db, records.ScanFailurePurge)
	if err != nil {
		t.Fatal(err)
	}
	f, err := acceptNow(t.Context(), s, "bad.txt", eicarMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanWaiting(t.Context(), s, demoScanner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.File(t.Context(), tenant, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("infected file after a purging scan = %v, want not_found", err)
	}
}

// TestScanFailsClosed pins that a scanner error leaves the file
// quarantined.
func TestScanFailsClosed(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, err := newQuarantineStore(t.Context(), db, records.ScanFailureKeep)
	if err != nil {
		t.Fatal(err)
	}
	f, err := acceptNow(t.Context(), s, "a.txt", "hello")
	if err != nil {
		t.Fatal(err)
	}
	down := scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) {
		return scan.Verdict{}, filesystem.Errorf(filesystem.CodeUnavailable, "scanner down")
	})
	if _, err := scanWaiting(t.Context(), s, down); err == nil {
		t.Fatal("a scan with the scanner down succeeded")
	}
	got, err := s.File(t.Context(), tenant, f.ID)
	if err != nil || !got.Quarantined() {
		t.Fatalf("after a failed scan: %+v, %v", got.Status, err)
	}
}
