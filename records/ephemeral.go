package records

import (
	"context"
	"time"

	"github.com/ubgo/sqlb"
)

// Ephemeral content: a file recorded with NewFile.ExpiresAt (or Upload's
// Expires) is temporary, and one recorded with ViewOnce expires at its
// first view. Nothing is purged in the background: SweepExpired does it,
// on the application's schedule (the jobs module runs it like the other
// sweeps), and a reader asks Viewed before serving, so an expired file is
// never served even before the sweep has run.

// View-once states of the view_once column.
const (
	viewOnceUnviewed int64 = 1
	viewOnceViewed   int64 = 2
)

// Expired reports whether f has expired at now: its ExpiresAt is set and
// not after now.
func (f File) Expired(now time.Time) bool {
	return f.ExpiresAt != nil && !f.ExpiresAt.After(now)
}

// Viewed is what a route serving a file asks before it serves it, and
// counts the view: it returns false for an expired file, true for any
// other file, and for a view-once file true exactly once, setting its
// ExpiresAt to now at that first view so SweepExpired purges it and later
// views are refused. Two concurrent first views of a view-once file get
// one true between them: the expiry is set by one conditional UPDATE. A
// missing file is CodeNotFound; a quarantined one is false with
// ErrQuarantined, and a view-once one spends no view.
func (s *Store) Viewed(ctx context.Context, scope, id string) (bool, error) {
	f, err := s.File(ctx, scope, id)
	if err != nil {
		return false, err
	}
	now := s.clock()
	if f.DeletedAt != nil {
		return false, nil
	}
	if f.Quarantined() {
		return false, ErrQuarantined
	}
	if !f.ViewOnce {
		return !f.Expired(now), nil
	}
	if f.Expired(now) {
		return false, nil
	}
	// The conditional UPDATE alone decides for a view-once file: only the
	// view that flips it from unviewed to viewed is served, whatever the
	// clocks of the servers say.
	upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileViewOnce, viewOnceViewed), sqlb.Set(FileExpiresAt, &now)).
		Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id), sqlb.Eq(FileViewOnce, viewOnceUnviewed))
	n, err := s.Exec(ctx, upd)
	return n == 1, err
}

// SweepExpired purges every file, in any scope, whose ExpiresAt is not
// after now, live or trashed, and returns how many it purged. Each file
// is purged in its own transaction (Purge, so plugins' hooks run and a
// shared object stays for its other files); a file that cannot be purged
// does not stop the sweep, and the returned error names each one.
// Schedule it; the library starts no background work.
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	at := now.UTC()
	return s.purgeWhere(ctx, sqlb.IsNotNull(FileExpiresAt), sqlb.LessEq(FileExpiresAt, &at))
}

// viewOnceState is the view_once value a new file starts with.
func viewOnceState(viewOnce bool) int64 {
	if viewOnce {
		return viewOnceUnviewed
	}
	return 0
}
