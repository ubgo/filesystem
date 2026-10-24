package records

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// ScopePurger is a plugin with rows that belong to a scope rather than to
// one file: a folder tree, a usage counter, a scope's leftover links. The
// rows tied to a file go through FileHooks.BeforePurge as each file is
// purged; PurgeScope then calls PurgeScope once, in one transaction, to
// delete the rest. It must delete only rows of scope, through
// Store.Conn(ctx), and succeed when there are none, because PurgeScope
// may run again after a failure.
type ScopePurger interface {
	PurgeScope(ctx context.Context, s *Store, scope string) error
}

// PurgeScopeBatch is how many file IDs PurgeScope reads at a time: few
// enough that a scope of millions never loads into memory at once.
const PurgeScopeBatch = 100

// purgeScopeRounds bounds how often PurgeScope purges files again after
// finding new ones at the end: enough for an upload that was in flight,
// never forever against a scope that keeps receiving them.
const purgeScopeRounds = 3

// PurgeScope deletes everything the Store and its plugins keep for one
// scope, to offboard a tenant: every file (trashed or not) with its
// attachments, through Purge, so every plugin's BeforePurge runs and the
// objects are deleted after each file's commit; then, in one transaction,
// every ScopePurger plugin's rows of the scope. Each file is purged in its
// own transaction, a batch of IDs at a time, so it can run on a scope of
// any size; call it outside a Tx. It stops at the first file that cannot
// be purged (a database failure, a plugin refusing, a cancelled ctx) and
// returns how many it purged before; nothing purged comes back, so run it
// again once the cause is fixed and it continues where it stopped. The
// scope's own rows go only when every file is gone: the last transaction
// counts the scope's files after the ScopePurgers ran and, if an upload
// landed meanwhile, rolls their work back and purges the new files first,
// up to a few rounds, then gives up with CodeUnavailable, so stop uploads
// to the scope before offboarding it. An object whose
// deletion fails goes to WithObjectErrorHandler and is left for
// SweepOrphans. Every purged file emits file.purged. A log that outlives
// files (the audit plugin) is not erased: prune it yourself if the
// tenant's history must go too. An empty scope is CodeInvalidArgument: it
// is the scope of an application without tenants, which is every file.
func (s *Store) PurgeScope(ctx context.Context, scope string) (int, error) {
	if scope == "" {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: PurgeScope refuses the empty scope, which holds every file of an application without tenants")
	}
	ctx = context.WithValue(ctx, purgingScopeKey{}, scope)
	purged := 0
	for round := 1; ; round++ {
		n, err := s.purgeScopeFiles(ctx, scope)
		purged += n
		if err != nil {
			return purged, err
		}
		err = s.purgeScopeRows(ctx, scope)
		if !errors.Is(err, errScopeNotEmpty) {
			return purged, err
		}
		if round == purgeScopeRounds {
			return purged, filesystem.Errorf(filesystem.CodeUnavailable, "records: files keep arriving in scope %q while it is purged; stop uploads to it and run PurgeScope again", scope)
		}
	}
}

// purgingScopeKey is the context key PurgeScope marks its work with.
type purgingScopeKey struct{}

// PurgingScope reports whether ctx is PurgeScope's work, and for which
// scope. It exists so hooks that refuse ordinary purges (a check-out lock,
// a hold) can let tenant offboarding through: deleting a tenant beats any
// one file's guard. A hook should compare the scope with the file's.
func PurgingScope(ctx context.Context) (scope string, ok bool) {
	scope, ok = ctx.Value(purgingScopeKey{}).(string)
	return scope, ok
}

// errScopeNotEmpty rolls back the ScopePurgers' work when files remain.
var errScopeNotEmpty = errors.New("records: scope not empty")

// purgeScopeFiles purges every file of scope, a batch of IDs at a time,
// each in its own transaction, and returns how many it purged.
func (s *Store) purgeScopeFiles(ctx context.Context, scope string) (int, error) {
	purged := 0
	for {
		var id string
		var ids []string
		q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileID, &id)).From(TableFiles).
			Where(sqlb.Eq(FileScope, scope)).Order(sqlb.OrderBy(FileID, sqlb.SortAsc)).Limit(PurgeScopeBatch)
		if err := s.Each(ctx, q, func() error { ids = append(ids, id); return nil }); err != nil {
			return purged, err
		}
		if len(ids) == 0 {
			return purged, nil
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return purged, err
			}
			// Inside a Tx, Purge hands a failed object deletion to the
			// handler instead of returning it: the row is gone either way.
			err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return s.Purge(ctx, scope, id) })
			if err != nil {
				return purged, fmt.Errorf("records: purge file %q of scope %q: %w", id, scope, err)
			}
			purged++
		}
	}
}

// purgeScopeRows runs every ScopePurger in one transaction, then counts the
// scope's files: if one arrived, the transaction rolls back with
// errScopeNotEmpty, so no plugin row of a scope that still has files is
// deleted (a quota counter under a live upload, the folder it was put in).
func (s *Store) purgeScopeRows(ctx context.Context, scope string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		for _, p := range s.plugins {
			if sp, ok := p.(ScopePurger); ok {
				if err := sp.PurgeScope(ctx, s, scope); err != nil {
					return err
				}
			}
		}
		var n int64
		q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(sqlb.Count(), &n)).From(TableFiles).Where(sqlb.Eq(FileScope, scope))
		if err := s.One(ctx, q); err != nil {
			return err
		}
		if n > 0 {
			return errScopeNotEmpty
		}
		return nil
	})
}
