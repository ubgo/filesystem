package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// eicarMarker stands in for a virus signature: the demo scanner flags any
// file holding it. A real app passes scan.Clamd or a cloud scanner.
const eicarMarker = "EICAR-STANDARD-ANTIVIRUS-TEST-FILE"

// linkTTL is how long a released file's link works.
const linkTTL = 10 * time.Minute

// inbox accepts plain-text uploads for later scanning.
var inbox = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("inbox")}

// newQuarantineStore is a Store whose disk signs URLs, so a released file
// gets a link. failure is what FailScan does with an infected file:
// records.ScanFailureKeep (the default) or records.ScanFailurePurge.
// ds:def id=newquarantinestore-t4k8w2cn
func newQuarantineStore(ctx context.Context, db *sql.DB, failure records.ScanFailure) (*records.Store, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithScanFailure(failure))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// acceptNow stores and records an upload at once, quarantined: its row
// and status exist, but no link, view, share, stream or public
// visibility is handed out until a scan passes.
// ds:def id=acceptnow-h7c3m9wq
func acceptNow(ctx context.Context, s *records.Store, name, body string) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader(body), name, inbox, records.Quarantine())
}

// demoScanner flags files that hold eicarMarker. An error from a scanner
// means the scan did not finish; Store.Scan then leaves the file
// quarantined.
var demoScanner = scan.ScannerFunc(func(_ context.Context, r io.Reader) (scan.Verdict, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return scan.Verdict{}, err
	}
	if bytes.Contains(b, []byte(eicarMarker)) {
		return scan.Verdict{Signature: "Eicar-Test-Signature"}, nil
	}
	return scan.Verdict{Clean: true}, nil
})

// scanWaiting is the worker: it lists the files still quarantined and
// scans each. Clean is PassScan (the file is released); infected is
// FailScan with "infected: <signature>".
// ds:def id=scanwaiting-p2v6n8kt
func scanWaiting(ctx context.Context, s *records.Store, sc scan.Scanner) ([]records.File, error) {
	page, err := s.Files(ctx, records.FileQuery{Scope: tenant, Filters: []sqlb.Predicate{sqlb.Eq(records.FileStatus, records.StatusQuarantined)}})
	if err != nil {
		return nil, err
	}
	var out []records.File
	for _, f := range page.Files {
		scanned, err := s.Scan(ctx, f.Scope, f.ID, sc)
		if err != nil {
			return out, err // the file stays quarantined; retry on the next run
		}
		out = append(out, scanned)
	}
	return out, nil
}

// quarantineDemo accepts two uploads quarantined, shows a link refused,
// scans them, and shows what each became.
func quarantineDemo(ctx context.Context, w io.Writer, db *sql.DB) error {
	s, err := newQuarantineStore(ctx, db, records.ScanFailureKeep)
	if err != nil {
		return err
	}
	clean, err := acceptNow(ctx, s, "notes.txt", "meeting notes")
	if err != nil {
		return err
	}
	if _, err := acceptNow(ctx, s, "invoice.txt", "pay now "+eicarMarker); err != nil {
		return err
	}
	fmt.Fprintf(w, "quarantine: %s is %s\n", clean.Filename, clean.Status)
	_, err = s.URL(ctx, clean, linkTTL)
	fmt.Fprintf(w, "quarantine: link before the scan: %s (%t)\n", filesystem.CodeOf(err), errors.Is(err, records.ErrQuarantined))
	err = s.SetVisibility(ctx, tenant, clean.ID, filesystem.VisibilityPublic)
	fmt.Fprintf(w, "quarantine: make public before the scan: %s\n", filesystem.CodeOf(err))
	scanned, err := scanWaiting(ctx, s, demoScanner)
	if err != nil {
		return err
	}
	for _, f := range scanned {
		fmt.Fprintf(w, "quarantine: scanned %s: %s %s\n", f.Filename, f.Status, f.StatusDetail)
	}
	released, err := s.File(ctx, tenant, clean.ID)
	if err != nil {
		return err
	}
	if _, err := s.URL(ctx, released, linkTTL); err != nil {
		return err
	}
	fmt.Fprintln(w, "quarantine: link after the scan: ok")
	return nil
}
