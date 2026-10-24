package records

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// Status is where a file's processing stands: the work that runs after
// its bytes are stored (image variants, a virus scan, a transcode). A
// file is usable as stored whatever its status, except
// StatusQuarantined: the status tells a client whether the derived things
// it may want (a thumbnail, a scan verdict) are there yet.
type Status string

// The closed set of statuses.
const (
	// StatusReady: nothing is pending. Every file starts ready unless
	// NewFile.Status says otherwise, and files recorded before the status
	// existed are ready.
	StatusReady Status = "ready"
	// StatusProcessing: work on the file is under way.
	StatusProcessing Status = "processing"
	// StatusFailed: the work failed; StatusDetail says why.
	StatusFailed Status = "failed"
	// StatusQuarantined: the file is unusable until a scan passes (see
	// quarantine.go): no link, view, share or stream is handed out for
	// it, and it cannot be made public. Only PassScan (or Scan) leaves it;
	// SetStatus refuses to, so a plugin marking its own work done can
	// never release a file nobody scanned. A failed scan keeps it here
	// with the verdict in StatusDetail, or purges it (WithScanFailure).
	StatusQuarantined Status = "quarantined"
)

// StatusValues lists every Status.
var StatusValues = []Status{StatusReady, StatusProcessing, StatusFailed, StatusQuarantined}

// Valid reports whether st is one of StatusValues.
func (st Status) Valid() bool {
	return slices.Contains(StatusValues, st)
}

// MaxStatusDetailChars is the longest StatusDetail, the width of its
// column.
const MaxStatusDetailChars = 255

// checkStatus refuses a status outside the closed set, or a detail that
// is invalid UTF-8, too long or holds control characters.
func checkStatus(st Status, detail string) error {
	if !st.Valid() {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown status %q", st)
	}
	if !utf8.ValidString(detail) || utf8.RuneCountInString(detail) > MaxStatusDetailChars || strings.IndexFunc(detail, unicode.IsControl) >= 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a status detail is valid UTF-8 of at most %d characters without control characters", MaxStatusDetailChars)
	}
	return nil
}

// SetStatus records where a file's processing stands: a plugin or a
// worker calls it with StatusProcessing when it starts, StatusReady when
// it is done, and StatusFailed with the reason in detail when it gives up
// ("thumbnail: unsupported format"). detail is a short note for the
// application and its users ("" for none), never a stack trace. It runs in
// a transaction (joining the one in ctx) and emits EventFileStatusChanged
// with the file as updated; setting the current status and detail does
// nothing. It works on a trashed file too. An unknown status or a bad
// detail is CodeInvalidArgument; an unknown file is CodeNotFound.
//
// Quarantine is guarded here because every processing plugin calls
// SetStatus: a quarantined file cannot be moved to any other status
// (ErrQuarantined; PassScan releases it), and a public file cannot be
// quarantined (ErrPublicQuarantine), since its object is already
// reachable without the library. The row is locked for the check.
func (s *Store) SetStatus(ctx context.Context, scope, id string, st Status, detail string) (File, error) {
	if err := checkStatus(st, detail); err != nil {
		return File{}, err
	}
	var out File
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.lockedFile(ctx, scope, id, lockUpdate)
		if err != nil {
			return err
		}
		out = f
		if f.Status == st && f.StatusDetail == detail {
			return nil
		}
		if err := checkQuarantineChange(f, st); err != nil {
			return err
		}
		return s.writeStatus(ctx, &out, st, detail)
	})
	if err != nil {
		return File{}, err
	}
	return out, nil
}

// writeStatus updates f's status and detail and emits
// EventFileStatusChanged with f as updated. It runs in the caller's
// transaction, with f's row already locked by it.
func (s *Store) writeStatus(ctx context.Context, f *File, st Status, detail string) error {
	upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileStatus, st), sqlb.Set(FileStatusDetail, detail)).
		Where(sqlb.Eq(FileScope, f.Scope), sqlb.Eq(FileID, f.ID))
	if _, err := s.Exec(ctx, upd); err != nil {
		return err
	}
	f.Status, f.StatusDetail = st, detail
	return s.emit(ctx, Event{Kind: EventFileStatusChanged, Scope: f.Scope, FileID: f.ID, File: fileRef(*f)})
}
