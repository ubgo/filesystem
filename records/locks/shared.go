package locks

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// errNoShared is LockShared on a Plugin without WithSharedLocks.
func errNoShared() error {
	return filesystem.Errorf(filesystem.CodeUnsupported, "locks: shared locks need WithSharedLocks")
}

// LockShared gives owner a shared lock on a live file of scope for ttl
// (WithSharedLocks): one of several holders, each with its own token
// (Lock.Token, Mode Shared). Locking again extends owner's shared lock
// and keeps its token while it is live. A file under a live exclusive
// lock, owner's own included, is ErrLocked; an unknown or trashed file
// is CodeNotFound; a ttl past WithMaxTTL is CodeInvalidArgument. Two
// first shared locks of one owner racing make one CodeUnavailable, to
// retry.
func (p *Plugin) LockShared(ctx context.Context, s *records.Store, scope, fileID, owner string, ttl time.Duration) (Lock, error) {
	if !p.shared {
		return Lock{}, errNoShared()
	}
	if owner == "" || len(owner) > MaxOwnerBytes || ttl <= 0 || ttl > p.maxTTL {
		return Lock{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "locks: a lock needs an owner of 1 to %d bytes and a ttl in (0, %s]", MaxOwnerBytes, p.maxTTL)
	}
	now := p.clock(s)
	l := Lock{Scope: scope, FileID: fileID, Owner: owner, ExpiresAt: now.Add(ttl), LockedAt: now, Mode: Shared}
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// The file's row first, as Lock and the guards take it, so a
		// shared lock and an exclusive one never both pass their checks.
		trashed, found, err := lockFileRow(ctx, s, scope, fileID)
		if err != nil {
			return err
		}
		if !found || trashed {
			return filesystem.Errorf(filesystem.CodeNotFound, "locks: file %q not found or in the trash", fileID)
		}
		var ex Lock
		err = lockedOne(ctx, s, lockQuery(s, scope, fileID, &ex))
		if _, held, err := heldAt(ex, now, err); err != nil || held {
			if err != nil {
				return err
			}
			return ErrLocked
		}
		var cur Lock
		err = s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sharedBindings(&cur)...).From(SharedTable).
			Where(sqlb.Eq(SharedScope, scope), sqlb.Eq(SharedFileID, fileID), sqlb.Eq(SharedOwner, owner)))
		switch {
		case errors.Is(err, sqlb.ErrNoRows):
			if l.Token, err = p.newToken(); err != nil {
				return err
			}
			ins := sqlb.NewInsert(s.Dialect()).Into(SharedTable).Row(sqlb.Set(SharedScope, scope), sqlb.Set(SharedFileID, fileID),
				sqlb.Set(SharedOwner, owner), sqlb.Set(SharedExpiresAt, l.ExpiresAt), sqlb.Set(SharedLockedAt, now), sqlb.Set(SharedToken, l.Token))
			if _, err := s.Exec(ctx, ins); err != nil {
				if records.IsUniqueViolation(err) {
					return filesystem.NewError(filesystem.CodeUnavailable, err)
				}
				return err
			}
		case err != nil:
			return err
		default:
			// Extending a live shared lock keeps its token; one that
			// expired is taken afresh, with a new one.
			l.Token = cur.Token
			if !cur.ExpiresAt.UTC().After(now) {
				if l.Token, err = p.newToken(); err != nil {
					return err
				}
			}
			upd := sqlb.NewUpdate(s.Dialect()).Table(SharedTable).
				Set(sqlb.Set(SharedExpiresAt, l.ExpiresAt), sqlb.Set(SharedLockedAt, now), sqlb.Set(SharedToken, l.Token)).
				Where(sqlb.Eq(SharedScope, scope), sqlb.Eq(SharedFileID, fileID), sqlb.Eq(SharedOwner, owner))
			if _, err := s.Exec(ctx, upd); err != nil {
				return err
			}
		}
		return s.Emit(ctx, p, records.Event{Kind: EventLocked, Scope: scope, FileID: fileID, Subject: owner,
			Detail: map[string]string{DetailExpiresAt: l.ExpiresAt.Format(time.RFC3339Nano), DetailMode: string(Shared)}})
	})
	if err != nil {
		return Lock{}, err
	}
	return l, nil
}

// UnlockShared gives up owner's shared lock on a file; doing so when owner
// holds none does nothing. The other holders keep theirs.
func (p *Plugin) UnlockShared(ctx context.Context, s *records.Store, scope, fileID, owner string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SharedTable).
			Where(sqlb.Eq(SharedScope, scope), sqlb.Eq(SharedFileID, fileID), sqlb.Eq(SharedOwner, owner)))
		if err != nil || n == 0 {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventUnlocked, Scope: scope, FileID: fileID, Subject: owner,
			Detail: map[string]string{DetailMode: string(Shared)}})
	})
}

// SharedLocksOf returns a file's live shared locks, by owner: who has it
// open together.
func (p *Plugin) SharedLocksOf(ctx context.Context, s *records.Store, scope, fileID string) ([]Lock, error) {
	return p.sharedRead(ctx, s, scope, fileID, p.clock(s), s.Each)
}

// sharedHolders is SharedLocksOf as a locking read, inside the
// transaction of a guard or a Lock that holds the file's row: it reads
// the latest committed shared locks, never an older snapshot.
func (p *Plugin) sharedHolders(ctx context.Context, s *records.Store, scope, fileID string, now time.Time) ([]Lock, error) {
	return p.sharedRead(ctx, s, scope, fileID, now, func(ctx context.Context, st records.Statement, fn func() error) error {
		q, err := forUpdate(s, st)
		if err != nil {
			return err
		}
		return sqlb.Each(ctx, s.Conn(ctx), q, fn)
	})
}

// sharedRead reads a file's shared locks live at now with each.
func (p *Plugin) sharedRead(ctx context.Context, s *records.Store, scope, fileID string, now time.Time,
	each func(context.Context, records.Statement, func() error) error) ([]Lock, error) {
	var l Lock
	var out []Lock
	q := sqlb.NewSelect(s.Dialect()).Columns(sharedBindings(&l)...).From(SharedTable).
		Where(sqlb.Eq(SharedScope, scope), sqlb.Eq(SharedFileID, fileID), sqlb.Greater(SharedExpiresAt, now)).
		Order(sqlb.OrderBy(SharedOwner, sqlb.SortAsc))
	err := each(ctx, q, func() error {
		l.ExpiresAt, l.LockedAt, l.Mode = l.ExpiresAt.UTC(), l.LockedAt.UTC(), Shared
		out = append(out, l)
		return nil
	})
	return out, err
}

// removeShared deletes every shared lock of a file (Break), with an
// unlock event for each holder.
func (p *Plugin) removeShared(ctx context.Context, s *records.Store, scope, fileID string) error {
	var owner string
	var owners []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(SharedOwner, &owner)).From(SharedTable).
		Where(sqlb.Eq(SharedScope, scope), sqlb.Eq(SharedFileID, fileID)).Order(sqlb.OrderBy(SharedOwner, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { owners = append(owners, owner); return nil }); err != nil {
		return err
	}
	for _, o := range owners {
		if err := p.UnlockShared(ctx, s, scope, fileID, o); err != nil {
			return err
		}
	}
	return nil
}
