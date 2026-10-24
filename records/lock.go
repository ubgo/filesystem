package records

import (
	"context"
	"errors"
	"fmt"

	"github.com/ubgo/sqlb"
)

// rowLock is how a read locks the rows it returns until the transaction
// ends. Locking reads also read the latest committed rows, not the
// transaction's snapshot, which matters on MySQL (REPEATABLE READ).
type rowLock string

// The closed set of row locks.
const (
	// lockShare lets other shared readers in and keeps writers out: Attach
	// holds it on the file it attaches, so the file cannot be purged under
	// it.
	lockShare rowLock = "FOR SHARE"
	// lockUpdate keeps every other locking reader and writer out: Purge
	// holds it on the file it purges.
	lockUpdate rowLock = "FOR UPDATE"
)

// oneLocked is One with the rows read locked as mode says. SQLite has no
// row locks and needs none: a write transaction excludes every other
// writer there, so the clause is left out.
func (s *Store) oneLocked(ctx context.Context, st Statement, mode rowLock) error {
	q, err := s.lockedQuery(st, mode)
	if err != nil {
		return err
	}
	return sqlb.One(ctx, s.Conn(ctx), q)
}

// eachLocked is Each with the rows read locked as mode says (see
// oneLocked).
func (s *Store) eachLocked(ctx context.Context, st Statement, mode rowLock, fn func() error) error {
	q, err := s.lockedQuery(st, mode)
	if err != nil {
		return err
	}
	return sqlb.Each(ctx, s.Conn(ctx), q, fn)
}

// lockedQuery renders st with mode's locking clause, which SQLite leaves
// out (see oneLocked). Every locking read builds through it, so the
// clause is added in one place.
func (s *Store) lockedQuery(st Statement, mode rowLock) (sqlb.Query, error) {
	q, err := s.Build(st)
	if err != nil {
		return sqlb.Query{}, fmt.Errorf("records: build: %w", err)
	}
	if engineOf(s.dialect) != EngineSQLite {
		q.SQL += " " + string(mode)
	}
	return q, nil
}

// lockedFile is File read with the file's row locked as mode says.
func (s *Store) lockedFile(ctx context.Context, scope, id string, mode rowLock) (File, error) {
	var r fileRow
	q := sqlb.NewSelect(s.dialect).Columns(fileBindings(&r)...).From(TableFiles).
		Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
	if err := s.oneLocked(ctx, q, mode); err != nil {
		return File{}, notFound(err, "file", id)
	}
	return r.decode(), nil
}

// attached reports whether any attachment of scope uses the file, with a
// locking read, so an attachment committed after the transaction's
// snapshot is still seen.
func (s *Store) attached(ctx context.Context, scope, fileID string) (bool, error) {
	var id string
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(AttachmentID, &id)).From(TableAttachments).
		Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentFileID, fileID)).Limit(1)
	err := s.oneLocked(ctx, q, lockShare)
	if errors.Is(err, sqlb.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
