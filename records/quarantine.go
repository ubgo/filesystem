package records

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/sqlb"
)

// Quarantine until scanned (SE-06): a file recorded with
// StatusQuarantined (NewFile.Status, or Upload's Quarantine option) is
// stored but unusable until a scan passes, for uploads scanned after the
// request returns (a queue, a slow scanner, a scan in another service).
// Every way the library hands a file out refuses it, each by one check:
//
//   - Store.Served, which Store.URL, Store.DownloadURL and every handler
//     built on them report through (recordshttp's file route and
//     listings, the share plugin, the imaging variants handler), reads
//     the file's status from the database and refuses a quarantined file
//     with ErrQuarantined, so a stale File value or one built by a plugin
//     (a variant link) cannot slip a link out. An application streaming
//     bytes itself calls Served (ServeStream) first, as it already must.
//   - Store.Viewed, which every serving route asks first, refuses it with
//     ErrQuarantined.
//   - A quarantined file is never public: CreateFile refuses one with
//     public visibility and SetVisibility refuses to make one public
//     (ErrPublicQuarantine, ErrQuarantined), because a public object is
//     reachable without the library.
//   - Dedup never reuses a quarantined file's object for a new upload.
//   - SetStatus refuses to move a quarantined file to any other status, so
//     a processing plugin finishing its own work cannot release it.
//
// The file's row, metadata and attachments stay readable (a client polls
// the status route and shows "scanning"); its bytes do not.

// ErrQuarantined is the error of an operation refused because the file is
// quarantined (CodeConflict): it cannot be served, linked, shared or made
// public until PassScan releases it. Match it with errors.Is; recordshttp
// answers it 409 so a client keeps polling the status route.
var ErrQuarantined = filesystem.Errorf(filesystem.CodeConflict, "records: the file is quarantined until its scan passes")

// ErrPublicQuarantine refuses quarantining a public file, or recording a
// quarantined file as public (CodeInvalidArgument): its object would be
// reachable without the library while unscanned. Make it private first,
// and public after PassScan.
var ErrPublicQuarantine = filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a public file cannot be quarantined; make it private first")

// ErrNotQuarantined is the error of FailScan on a file that is not
// quarantined (CodeConflict): a verdict arriving for a file another worker
// already released, or one never quarantined. Nothing is changed.
var ErrNotQuarantined = filesystem.Errorf(filesystem.CodeConflict, "records: the file is not quarantined")

// Quarantined reports whether f is quarantined (StatusQuarantined). It
// reads the value f holds; the serving paths re-read the database.
func (f File) Quarantined() bool { return f.Status == StatusQuarantined }

// ScanFailure is what FailScan does with a file whose scan failed.
type ScanFailure string

// The closed set of scan failure actions.
const (
	// ScanFailureKeep keeps the file quarantined, with the verdict in
	// StatusDetail, for an administrator to inspect or purge. The default:
	// nothing is lost, and the file stays unusable.
	ScanFailureKeep ScanFailure = "keep"
	// ScanFailurePurge purges the file (Store.Purge: its rows, then its
	// object once no other file names it).
	ScanFailurePurge ScanFailure = "purge"
)

// ScanFailureValues lists every ScanFailure.
var ScanFailureValues = []ScanFailure{ScanFailureKeep, ScanFailurePurge}

// WithScanFailure chooses what FailScan does with a file whose scan
// failed; without it, ScanFailureKeep. An unknown action is
// CodeInvalidArgument.
func WithScanFailure(a ScanFailure) Option {
	return func(s *Store) error {
		if !slices.Contains(ScanFailureValues, a) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown scan failure action %q", a)
		}
		s.scanFailure = a
		return nil
	}
}

// Quarantine makes Upload record the file quarantined (StatusQuarantined)
// until a scan passes; see PassScan, FailScan and Scan. The policy's
// visibility must not be public (ErrPublicQuarantine).
func Quarantine() UploadOption { return func(o *uploadOptions) { o.quarantine = true } }

// checkQuarantineChange refuses a status change that would release a
// quarantined file, or quarantine a public one.
func checkQuarantineChange(f File, st Status) error {
	if f.Quarantined() && st != StatusQuarantined {
		return ErrQuarantined
	}
	if st == StatusQuarantined && f.Visibility == filesystem.VisibilityPublic {
		return ErrPublicQuarantine
	}
	return nil
}

// checkServable refuses a read of the file fileID of scope while it is
// quarantined, from the row as it is now. A file not found is left to the
// caller: Served reports links to objects whose row may already be gone
// (a purge racing a listing), and has never refused those.
func (s *Store) checkServable(ctx context.Context, scope, fileID string) error {
	if fileID == "" {
		return nil
	}
	var st Status
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileStatus, &st)).From(TableFiles).
		Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, fileID))
	switch err := s.One(ctx, q); {
	case errors.Is(err, sqlb.ErrNoRows):
		return nil
	case err != nil:
		return err
	case st == StatusQuarantined:
		return ErrQuarantined
	}
	return nil
}

// ReleaseHooks lets a plugin do, when a quarantined file is released,
// what its FileHooks.BeforeCreate skipped for the file while it was
// unscanned: read the bytes and describe them (dimensions, duration,
// document information). BeforeRelease runs inside PassScan's
// transaction, before the row changes, with the file as it will be
// released (StatusReady), and may change its Metadata only; an error aborts the release
// and the file stays quarantined. Plugins that work after the commit
// (thumbnails, text extraction) react to EventFileReleased instead.
type ReleaseHooks interface {
	BeforeRelease(ctx context.Context, s *Store, f *File) error
}

// PassScan releases a quarantined file: every ReleaseHooks plugin runs on
// it, then its status becomes StatusReady with no detail, with the
// metadata the hooks set, and EventFileStatusChanged and then
// EventFileReleased are emitted, all in one transaction; after it the
// file can be served, shared and made public. A file that is not
// quarantined is returned unchanged, so a verdict delivered twice is
// harmless. The row is locked for the change. An unknown file is
// CodeNotFound.
func (s *Store) PassScan(ctx context.Context, scope, id string) (File, error) {
	var out File
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.lockedFile(ctx, scope, id, lockUpdate)
		if err != nil {
			return err
		}
		out = f
		if !f.Quarantined() {
			return nil
		}
		// The hooks see the file as released (ready), so a describer that
		// skips quarantined files reads this one.
		next := f
		next.Status, next.StatusDetail = StatusReady, ""
		for _, p := range s.plugins {
			if h, ok := p.(ReleaseHooks); ok {
				if err := h.BeforeRelease(ctx, s, &next); err != nil {
					return err
				}
			}
		}
		// Only the metadata is the hooks' to change.
		out.Metadata = next.Metadata
		if err := checkText(out.Metadata); err != nil {
			return err
		}
		upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileMetadata, encodeMetadata(out.Metadata))).
			Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		if err := s.writeStatus(ctx, &out, StatusReady, ""); err != nil {
			return err
		}
		return s.emit(ctx, Event{Kind: EventFileReleased, Scope: scope, FileID: id, File: fileRef(out)})
	})
	if err != nil {
		return File{}, err
	}
	return out, nil
}

// FailScan records a failed scan of a quarantined file, detail being the
// verdict ("infected: Win.Test.EICAR_HDB-1"; the StatusDetail rules
// apply). With ScanFailureKeep (the default) the file stays quarantined
// with detail as its StatusDetail; with ScanFailurePurge it is purged, and
// the File returned is the one purged. A file that is not quarantined is
// ErrNotQuarantined and nothing changes: a late verdict never purges a
// file another worker released. An unknown file is CodeNotFound.
func (s *Store) FailScan(ctx context.Context, scope, id, detail string) (File, error) {
	if err := checkStatus(StatusQuarantined, detail); err != nil {
		return File{}, err
	}
	var out File
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.lockedFile(ctx, scope, id, lockUpdate)
		if err != nil {
			return err
		}
		out = f
		if !f.Quarantined() {
			return ErrNotQuarantined
		}
		if s.scanFailure == ScanFailurePurge {
			return s.Purge(ctx, scope, id)
		}
		if f.StatusDetail == detail {
			return nil
		}
		return s.writeStatus(ctx, &out, StatusQuarantined, detail)
	})
	if err != nil {
		return File{}, err
	}
	return out, nil
}

// Scan scans a quarantined file's object with sc and records the verdict:
// clean is PassScan, infected is FailScan with "infected: <signature>".
// It is what a worker runs for each quarantined file (list them with
// Files and FileStatus). A scan that does not complete (sc's error, the
// object unreadable) is returned and changes nothing, so the file stays
// quarantined: scanning fails closed. A file that is not quarantined is
// returned unchanged without being read.
func (s *Store) Scan(ctx context.Context, scope, id string, sc scan.Scanner) (File, error) {
	f, err := s.File(ctx, scope, id)
	if err != nil || !f.Quarantined() {
		return f, err
	}
	disk, err := s.disks.Disk(f.Disk)
	if err != nil {
		return File{}, err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return File{}, err
	}
	v, err := sc.Scan(ctx, rc)
	_ = rc.Close() // a read-only stream: closing it reports nothing the scan did not
	if err != nil {
		return File{}, err
	}
	if v.Clean {
		return s.PassScan(ctx, scope, id)
	}
	return s.FailScan(ctx, scope, id, infectedDetail(v.Signature))
}

// infectedPrefix starts the StatusDetail FailScan records for an infected
// file, so an admin page can tell a verdict from another failure.
const infectedPrefix = "infected: "

// infectedDetail is the StatusDetail of a scan that found signature,
// bounded to MaxStatusDetailChars and stripped of control characters (a
// scanner's signature is not the library's to trust).
func infectedDetail(signature string) string {
	clean := []rune(infectedPrefix)
	for _, r := range signature {
		if len(clean) == MaxStatusDetailChars {
			break
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		clean = append(clean, r)
	}
	return string(clean)
}

// notQuarantinedNow re-reads f's row, locked, before SetVisibility makes
// it public, and refuses with ErrQuarantined if it was quarantined since
// it was read: SetStatus locks the same row, so the two serialize and a
// file is never both public and quarantined.
func (s *Store) notQuarantinedNow(ctx context.Context, f File, v filesystem.Visibility) error {
	if v != filesystem.VisibilityPublic {
		return nil
	}
	cur, err := s.lockedFile(ctx, f.Scope, f.ID, lockUpdate)
	if err != nil {
		return err
	}
	if cur.Quarantined() {
		return ErrQuarantined
	}
	return nil
}
