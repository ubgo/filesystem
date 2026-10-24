package records

import (
	"context"
	"database/sql"

	"github.com/ubgo/filesystem"
)

// MaxBulk caps the IDs of one bulk call: a selection a user can make in a
// file manager, never a whole scope (PurgeScope is for that).
const MaxBulk = 1000

// BulkResult is what a bulk call did to one file: Err is nil when the
// file was changed (or was already in the wanted state), else why not.
type BulkResult struct {
	ID  string
	Err error
}

// TrashMany trashes each file of ids, as Trash, each in its own
// transaction. See bulk for the rules every *Many method shares.
func (s *Store) TrashMany(ctx context.Context, scope string, ids []string) ([]BulkResult, error) {
	return s.bulk(ctx, ids, func(ctx context.Context, id string) error { return s.Trash(ctx, scope, id) })
}

// RestoreMany restores each file of ids, as Restore, each in its own
// transaction.
func (s *Store) RestoreMany(ctx context.Context, scope string, ids []string) ([]BulkResult, error) {
	return s.bulk(ctx, ids, func(ctx context.Context, id string) error { return s.Restore(ctx, scope, id) })
}

// PurgeMany purges each file of ids, as Purge, each in its own
// transaction; an object whose deletion fails after its rows went is
// reported to WithObjectErrorHandler, not in the results, as with
// SweepTrash.
func (s *Store) PurgeMany(ctx context.Context, scope string, ids []string) ([]BulkResult, error) {
	return s.bulk(ctx, ids, func(ctx context.Context, id string) error {
		return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return s.Purge(ctx, scope, id) })
	})
}

// SetVisibilityMany sets the visibility of each file of ids, as
// SetVisibility, each in its own transaction.
func (s *Store) SetVisibilityMany(ctx context.Context, scope string, ids []string, v filesystem.Visibility) ([]BulkResult, error) {
	if !v.Valid() {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown visibility %q", v)
	}
	return s.bulk(ctx, ids, func(ctx context.Context, id string) error { return s.SetVisibility(ctx, scope, id, v) })
}

// bulk runs fn for each distinct ID, in order, and returns one result per
// distinct ID. Each file changes in its own transaction, so one file that
// fails (not found, refused by a plugin, a backend error) never undoes
// or blocks the others, and the results say which went through. For the
// same reason a bulk call refuses to run inside a Tx (CodeInvalidArgument):
// there every file would share one transaction, and on Postgres the first
// failed statement would abort it for the rest. More than MaxBulk IDs is
// CodeInvalidArgument and changes nothing. A cancelled ctx stops the run:
// the files not reached get its error.
func (s *Store) bulk(ctx context.Context, ids []string, fn func(ctx context.Context, id string) error) ([]BulkResult, error) {
	if txFrom(ctx) != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: bulk calls run each file in its own transaction; call them outside Store.Tx")
	}
	if len(ids) > MaxBulk {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: at most %d files per bulk call, got %d", MaxBulk, len(ids))
	}
	seen := make(map[string]bool, len(ids))
	out := make([]BulkResult, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := ctx.Err(); err != nil {
			out = append(out, BulkResult{ID: id, Err: err})
			continue
		}
		out = append(out, BulkResult{ID: id, Err: fn(ctx, id)})
	}
	return out, nil
}
