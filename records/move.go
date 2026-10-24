package records

import (
	"context"
	"errors"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// FileDiskMover is a plugin that keeps objects of its own beside a file
// (image variants, kept versions) and moves them when the file moves to
// another disk (MoveFile). MoveFileDisk runs inside MoveFile's
// transaction after the file's row changed: f is the file as it was
// (f.Disk the old disk) and to the new disk. It must copy its objects to
// to, update its rows with Store.Conn(ctx), delete its copies again on
// rollback (Store.OnRollback) and the old objects only after the commit
// (Store.DeleteObjectAfterCommit), so a failed move leaves everything as
// it was. An error rolls the whole move back. A plugin that keeps objects
// on the file's disk and does not implement it is left pointing at the
// old disk, whose objects SweepOrphans then keeps only while the plugin
// reports them (ObjectOwner).
type FileDiskMover interface {
	MoveFileDisk(ctx context.Context, s *Store, f File, to string) error
}

// MoveFile moves one live file's object to the disk named to, under the
// same key, and records it there, for storage tiering and for "accept
// fast, store later" (an upload landed on a local disk, promoted to the
// bucket by a job). It is object-first like every change of the Store:
//
//  1. the object is copied to to and the copy checked against the row
//     (size, and SHA-256 when both are known);
//  2. in one transaction, with the file's row locked, the row's disk
//     changes and every FileDiskMover plugin moves its own objects;
//  3. the old object is deleted after the commit (a failure goes to
//     WithObjectErrorHandler and leaves an orphan for SweepOrphans).
//
// A failure before the commit deletes the copy again, so the file stays
// where it was, whole. A crash leaves at most an extra copy on one disk,
// never a row without its object, and running MoveFile again finishes the
// move (it overwrites a copy left behind); a file already on to is
// returned as it is, with no event. It returns the file as moved and
// emits EventFileMoved inside the transaction, with the file on its new
// disk and Detail naming the old and new disk.
//
// Refused with nothing changed: a trashed file (CodeNotFound, as Replace
// does), another file recorded under the same key on to (CodeExists), a disk the Manager does not have (CodeNotFound), a disk that
// cannot give the file its visibility (CodeUnsupported, as CreateFile
// refuses it), and, with WithVisibilityDisks, a move that would leave the
// file off the disk its visibility routes to (CodeInvalidArgument): out
// of one of those disks to any other, or into one that is not its
// visibility's. A row that changed during the copy (replaced, moved) is
// CodeUnavailable, to retry.
func (s *Store) MoveFile(ctx context.Context, scope, id, to string) (File, error) {
	dst, err := s.disks.Disk(to)
	if err != nil {
		return File{}, err
	}
	f, err := s.File(ctx, scope, id)
	if err != nil {
		return File{}, err
	}
	if f.DeletedAt != nil {
		return File{}, filesystem.Errorf(filesystem.CodeNotFound, "records: file %q is in the trash", id)
	}
	if f.Disk == to {
		return f, nil
	}
	if s.visibilityDisks != nil && (s.isVisibilityDisk(f.Disk) || s.isVisibilityDisk(to)) && s.visibilityDisks[f.Visibility] != to {
		return File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a %s file belongs on disk %q (WithVisibilityDisks), not %q", f.Visibility, s.visibilityDisks[f.Visibility], to)
	}
	if !dst.Capabilities().SetVisibility && dst.DefaultVisibility() != f.Visibility {
		return File{}, filesystem.Errorf(filesystem.CodeUnsupported, "records: disk %q has no per-object visibility; its objects are %s, the file is %s", to, dst.DefaultVisibility(), f.Visibility)
	}
	// Another file recorded under the same key on to would be overwritten
	// by the copy: refused. (Keys from upload policies are random; this
	// guards keys an application chose.)
	var other string
	taken := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileID, &other)).From(TableFiles).
		Where(sqlb.Eq(FileDisk, to), sqlb.Eq(FileKey, f.Key)).Limit(1)
	switch err := s.One(ctx, taken); {
	case err == nil:
		return File{}, filesystem.Errorf(filesystem.CodeExists, "records: file %q already uses key %q on disk %q", other, f.Key, to)
	case !errors.Is(err, sqlb.ErrNoRows):
		return File{}, err
	}
	src, err := s.disks.Disk(f.Disk)
	if err != nil {
		return File{}, err
	}
	copied, err := filesystem.CopyAcross(ctx, src, f.Key, dst, f.Key)
	if err != nil {
		return File{}, err
	}
	undo := func(ctx context.Context) { _ = dst.Delete(context.WithoutCancel(ctx), f.Key) }
	if err := checkCopy(f, copied); err != nil {
		undo(ctx)
		return File{}, err
	}
	if dst.Capabilities().SetVisibility && copied.Visibility != f.Visibility {
		if err := dst.SetVisibility(ctx, f.Key, f.Visibility); err != nil {
			return File{}, errors.Join(err, dst.Delete(context.WithoutCancel(ctx), f.Key))
		}
	}
	moved := f
	moved.Disk = to
	err = s.recordObjectChange(ctx, f, moved, undo, func(ctx context.Context) error {
		cur, err := s.lockedFile(ctx, scope, id, lockUpdate)
		if err != nil {
			return err
		}
		if cur.Disk != f.Disk || cur.Key != f.Key || cur.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeUnavailable, "records: file %q changed while it was moved; retry", id)
		}
		upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileDisk, to)).Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		for _, p := range s.plugins {
			if m, ok := p.(FileDiskMover); ok {
				if err := m.MoveFileDisk(ctx, s, cur, to); err != nil {
					return err
				}
			}
		}
		after := cur
		after.Disk = to
		return s.emit(ctx, Event{Kind: EventFileMoved, Scope: scope, FileID: id, File: fileRef(after), Detail: map[string]string{DetailFrom: cur.Disk, DetailTo: to}})
	})
	if err != nil {
		return File{}, err
	}
	return moved, nil
}

// checkCopy compares a copy with the row it is for: the size always, the
// SHA-256 when the row and the copy both have one.
func checkCopy(f File, copied filesystem.Entry) error {
	if copied.Size != f.Size || (f.SHA256 != "" && copied.SHA256 != "" && copied.SHA256 != f.SHA256) {
		return filesystem.Errorf(filesystem.CodeUnknown, "records: the copy of file %q differs from its record (size %d, want %d)", f.ID, copied.Size, f.Size)
	}
	return nil
}
