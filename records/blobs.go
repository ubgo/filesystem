package records

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// Shared objects. Several file rows may name one object (the same disk
// and key): Upload with Dedup records a new file on an existing file's
// object, and an application may do the same with CreateFile. The object
// is reference-counted by its rows, not by a counter that could drift:
// before an object is scheduled for deletion (a purge, a replacement, a
// move, a plugin dropping its own with DeleteObjectAfterCommit), the
// transaction that removed the reference asks whether another file row
// or an ObjectOwner plugin still names it, and keeps the object if so; the
// last reference to go deletes it after its commit. The fs_files_object
// index (migration 0004) serves the question.
//
// Consistency is unchanged: a row is never left without its object. The
// lookup locks every row naming the object, in ID order, so a reference
// committed meanwhile is seen and no row is skipped. Purge takes those
// locks before it touches its own file, in the same order, so purges of
// files sharing an object run one after the other and the last one
// deletes the object. A replacement, move or visibility change of one
// sharing file racing a purge of another can still meet the database's
// deadlock detection, which fails one of them with an error to retry; it
// never deletes an object a row still names.

// References returns how many file rows, in every scope and trashed ones
// included, name the object key on disk: 0 for an orphan, more than 1
// for a shared object. Objects that plugins hold (kept versions) are not
// counted; see ObjectOwner.
func (s *Store) References(ctx context.Context, disk, key string) (int64, error) {
	var n int64
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(sqlb.Count(), &n)).From(TableFiles).
		Where(sqlb.Eq(FileDisk, disk), sqlb.Eq(FileKey, key))
	return n, s.One(ctx, q)
}

// sharedWith reports whether a file other than f names f's object; with
// liveOnly, only files not in the trash count.
func (s *Store) sharedWith(ctx context.Context, f File, liveOnly bool) (bool, error) {
	var n int64
	where := []sqlb.Predicate{sqlb.Eq(FileDisk, f.Disk), sqlb.Eq(FileKey, f.Key), sqlb.NotEq(FileID, f.ID)}
	if liveOnly {
		where = append(where, sqlb.IsNull(FileDeletedAt))
	}
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(sqlb.Count(), &n)).From(TableFiles).Where(where...)
	if err := s.One(ctx, q); err != nil {
		return false, err
	}
	return n > 0, nil
}

// referenced reports whether a file other than f, or an ObjectOwner
// plugin, still names f's object, with a locking read so a reference
// committed by a concurrent transaction is seen.
func (s *Store) referenced(ctx context.Context, f File) (bool, error) {
	ids, err := s.lockObjectRows(ctx, f.Disk, f.Key, lockShare)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id != f.ID {
			return true, nil
		}
	}
	return s.pluginHolds(ctx, f.Disk, f.Key)
}

// lockObjectRows locks, as mode says and in ID order, every file row
// naming the object key on disk, and returns their IDs. Without a LIMIT:
// on Postgres a locking read that waits on a row deleted meanwhile drops
// it and goes on to the next, so every row still naming the object is
// returned, which a LIMIT 1 could miss.
func (s *Store) lockObjectRows(ctx context.Context, disk, key string, mode rowLock) ([]string, error) {
	var id string
	var ids []string
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileID, &id)).From(TableFiles).
		Where(sqlb.Eq(FileDisk, disk), sqlb.Eq(FileKey, key)).Order(sqlb.OrderBy(FileID, sqlb.SortAsc))
	err := s.eachLocked(ctx, q, mode, func() error { ids = append(ids, id); return nil })
	return ids, err
}

// lockForPurge locks a file about to be purged: first every row naming
// its object, in ID order, then reads the file under that lock. Purges of
// files sharing an object thus take their locks in one order and run one
// after the other, and the last one sees no other row and deletes the
// object. A file whose object changed between the two reads (a
// replacement or a move landed) is CodeUnavailable, to retry.
func (s *Store) lockForPurge(ctx context.Context, scope, id string) (File, error) {
	pre, err := s.File(ctx, scope, id)
	if err != nil {
		return File{}, err
	}
	if _, err := s.lockObjectRows(ctx, pre.Disk, pre.Key, lockUpdate); err != nil {
		return File{}, err
	}
	f, err := s.lockedFile(ctx, scope, id, lockUpdate)
	if err != nil {
		return File{}, err
	}
	if f.Disk != pre.Disk || f.Key != pre.Key {
		return File{}, filesystem.Errorf(filesystem.CodeUnavailable, "records: file %q changed while it was locked; retry", id)
	}
	return f, nil
}

// pluginHolds reports whether an ObjectOwner plugin holds the object.
func (s *Store) pluginHolds(ctx context.Context, disk, key string) (bool, error) {
	for _, p := range s.plugins {
		if o, ok := p.(ObjectOwner); ok {
			owned, err := o.OwnedObjects(ctx, s, disk, []string{key})
			if err != nil {
				return false, fmt.Errorf("records: plugin %q owned objects: %w", p.Name(), err)
			}
			if owned[key] {
				return true, nil
			}
		}
	}
	return false, nil
}

// releaseObject is the core's DeleteObjectAfterCommit: it schedules the
// deletion of f's object after the commit unless the object is still
// referenced, and returns a failed lookup, which fails the change rather
// than risk deleting bytes another row needs.
func (s *Store) releaseObject(ctx context.Context, f File) error {
	shared, err := s.referenced(ctx, f)
	if err != nil || shared {
		return err
	}
	s.AfterCommit(ctx, func(ctx context.Context) {
		if err := s.deleteObject(ctx, f); err != nil {
			s.objectErrors(ctx, f, err)
		}
	})
	return nil
}

// discarder returns how a failed, rolled-back or deduplicated upload drops
// the object it stored at key. An object whose key a Settler chose
// (upload.ContentAddressed) is never deleted: identical uploads in flight
// share that key, and one whose row is not committed yet cannot be seen by
// any lock, so deleting could take the object it is about to record. Such
// an object is left for SweepOrphans, whose olderThan grace covers uploads
// still in flight; it costs storage until the sweep, never data. Any
// other key is the upload's own, deleted through discardObject.
func (s *Store) discarder(disk *filesystem.Disk, key string, settled bool) func(context.Context) {
	if settled {
		return func(context.Context) {}
	}
	return func(ctx context.Context) { s.discardObject(ctx, disk, key) }
}

// renewIfReused renews key's age (renewModTime) only when the object is
// older than the upload that settled onto it, started: a dedup hit on an
// existing object. An object the upload itself stored is as young as the
// upload and is left alone, so a fresh upload costs one Stat, not the
// copies a renewal takes on object stores. An object whose age the disk
// does not report is renewed, since its age is unknown.
func (s *Store) renewIfReused(ctx context.Context, disk *filesystem.Disk, key string, started time.Time) error {
	e, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	if !e.ModTime.IsZero() && !e.ModTime.Before(started) {
		return nil
	}
	return s.renewModTime(ctx, disk, key)
}

// renewModTime makes key's modification time now, so SweepOrphans, which
// spares only objects younger than its olderThan, cannot take an old
// object an upload is reusing before the upload's row commits. With the
// SetModTime capability it sets the time; otherwise it rewrites the
// object in place (copied aside, then moved back over it), which every
// backend stamps with the current time.
func (s *Store) renewModTime(ctx context.Context, disk *filesystem.Disk, key string) error {
	if disk.Capabilities().SetModTime {
		return disk.SetModTime(ctx, key, s.clock())
	}
	aside := key + renewSuffix + strings.ToLower(rand.Text())
	if _, err := disk.Copy(ctx, key, aside); err != nil {
		return err
	}
	if _, err := disk.Move(ctx, aside, key); err != nil {
		return errors.Join(err, disk.Delete(context.WithoutCancel(ctx), aside))
	}
	return nil
}

// renewSuffix names the short-lived copy renewModTime moves back over an
// object.
const renewSuffix = ".renew-"

// discardObject deletes key on disk, an object a failed or rolled-back
// write stored, unless a file row or a plugin names it (an application
// that recorded the same key with CreateFile).
// The check locks the rows naming the object for the deletion's duration,
// so a purge of such a file cannot interleave. A failed check keeps the
// object (an orphan for SweepOrphans), never deletes it on a doubt.
func (s *Store) discardObject(ctx context.Context, disk *filesystem.Disk, key string) {
	ctx = context.WithoutCancel(ctx)
	_ = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		ids, err := s.lockObjectRows(ctx, disk.Name(), key, lockShare)
		if err != nil || len(ids) > 0 {
			return err
		}
		if held, err := s.pluginHolds(ctx, disk.Name(), key); err != nil || held {
			return err
		}
		return disk.Delete(ctx, key)
	})
}

// refuseShared is the error of changing a shared object in place (its
// visibility on its own disk): the change would reach every file that
// shares it.
func refuseShared(f File) error {
	return filesystem.Errorf(filesystem.CodeConflict, "records: file %q shares its object with other files; its visibility cannot change in place (route visibilities to their own disks with WithVisibilityDisks, which copies the object)", f.ID)
}
