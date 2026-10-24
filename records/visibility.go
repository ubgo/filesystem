package records

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// WithVisibilityDisks keeps public and private files on different disks:
// typically a public bucket behind a CDN and a private one reached only
// through signed URLs, the usual layout now that S3 disables object ACLs
// by default. Upload with an empty disk name chooses by the policy's
// visibility (private when it has none), and SetVisibility moves a file
// between them. Both disks must be in the Store's manager.
func WithVisibilityDisks(public, private string) Option {
	return func(s *Store) error {
		if public == "" || private == "" || public == private {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: WithVisibilityDisks needs two different disk names")
		}
		s.visibilityDisks = map[filesystem.Visibility]string{filesystem.VisibilityPublic: public, filesystem.VisibilityPrivate: private}
		return nil
	}
}

// RoutesVisibility reports whether the Store was built WithVisibilityDisks,
// so an Upload with an empty disk name chooses the disk itself: handlers
// that take an optional disk check it once at construction instead of
// failing every request.
func (s *Store) RoutesVisibility() bool { return s.visibilityDisks != nil }

// diskFor returns the disk name a file of visibility v belongs on, or
// CodeInvalidArgument when no routing is configured.
func (s *Store) diskFor(v filesystem.Visibility) (string, error) {
	if s.visibilityDisks == nil {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "records: no disk named and no WithVisibilityDisks to choose one")
	}
	if v == "" {
		v = filesystem.VisibilityPrivate
	}
	return s.visibilityDisks[v], nil
}

// SetVisibility makes a file public or private. With WithVisibilityDisks
// the object moves to the other disk (copied first, the old one deleted
// after the commit); otherwise its own disk changes its access, which
// needs the SetVisibility capability. The object changes before the row,
// and a failed transaction puts it back, so the row never claims a
// visibility the object does not have. A trashed file changes only its
// row: its object stays hidden (see Trash) until Restore applies the new
// visibility. Emits EventFileVisibilityChanged; setting the current
// visibility does nothing. IfUnchanged changes it only while the file's
// content is the one the caller read.
func (s *Store) SetVisibility(ctx context.Context, scope, id string, v filesystem.Visibility, opts ...ChangeOption) error {
	if !v.Valid() {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown visibility %q", v)
	}
	o := changeOptionsOf(opts)
	if err := errors.Join(o.notForPurgeOnly("SetVisibility"), o.notForReplaceOnly("SetVisibility")); err != nil {
		return err
	}
	f, err := s.File(ctx, scope, id)
	if err != nil {
		return err
	}
	if err := o.check(f); err != nil {
		return err
	}
	if f.Visibility == v {
		return nil
	}
	// A public object is reachable without the library, so an unscanned
	// one never becomes public (see quarantine.go).
	if v == filesystem.VisibilityPublic && f.Quarantined() {
		return ErrQuarantined
	}
	moved := f
	undo := func(context.Context) {}
	switch {
	case f.DeletedAt != nil:
		// Restore applies v: with routing it moves the object, otherwise
		// it needs the disk to set visibility, checked here so a refused
		// change never reaches the row.
		if s.visibilityDisks == nil {
			disk, err := s.disks.Disk(f.Disk)
			if err != nil {
				return err
			}
			if !disk.Capabilities().SetVisibility {
				return filesystem.Errorf(filesystem.CodeUnsupported, "records: disk %q cannot set the visibility of one object", f.Disk)
			}
		}
	case s.visibilityDisks != nil && s.visibilityDisks[v] != f.Disk:
		if moved, undo, err = s.moveObject(ctx, f, s.visibilityDisks[v], v); err != nil {
			return err
		}
	default:
		if shared, err := s.sharedWith(ctx, f, false); err != nil {
			return err
		} else if shared {
			return refuseShared(f)
		}
		if undo, err = s.setObjectVisibility(ctx, f, v, f.Visibility); err != nil {
			return err
		}
	}
	return s.recordObjectChange(ctx, f, moved, undo, func(ctx context.Context) error {
		if err := s.recheck(ctx, f, o); err != nil {
			return err
		}
		if err := s.notQuarantinedNow(ctx, f, v); err != nil {
			return err
		}
		upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileDisk, moved.Disk), sqlb.Set(FileVisibility, v)).
			Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		after := f
		after.Disk, after.Visibility = moved.Disk, v
		return s.emit(ctx, Event{Kind: EventFileVisibilityChanged, Scope: scope, FileID: id, File: fileRef(after)})
	})
}

// recordObjectChange records, by running write in a transaction, an
// object change already made: f is the file before it, moved the file
// after it (another disk when the object was copied), and undo reverses
// it. undo runs when the transaction rolls back (this one, or the
// caller's that it joined) and directly when it never started, so the row
// never claims a state the object does not have. When the object moved,
// the old copy is deleted after the commit. Every change that touches an
// object before its row (SetVisibility, Trash, Restore) goes through it.
func (s *Store) recordObjectChange(ctx context.Context, f, moved File, undo func(context.Context), write func(ctx context.Context) error) error {
	started := false
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		started = true
		s.OnRollback(ctx, undo)
		if err := write(ctx); err != nil {
			return err
		}
		if moved.Disk != f.Disk {
			return s.releaseObject(ctx, f)
		}
		return nil
	})
	if err != nil && !started {
		undo(ctx)
	}
	return err
}

// setObjectVisibility sets the visibility of f's object in place, on its
// own disk (CodeUnsupported without the SetVisibility capability), and
// returns how to put previous back.
func (s *Store) setObjectVisibility(ctx context.Context, f File, v, previous filesystem.Visibility) (func(context.Context), error) {
	disk, err := s.disks.Disk(f.Disk)
	if err != nil {
		return nil, err
	}
	if err := disk.SetVisibility(ctx, f.Key, v); err != nil {
		return nil, err
	}
	return func(ctx context.Context) { _ = disk.SetVisibility(context.WithoutCancel(ctx), f.Key, previous) }, nil
}

// trashExposure changes the object of a public file f as it enters
// (hide) or leaves the trash, so a trashed file never answers at its
// public URL: with WithVisibilityDisks the object moves from the public
// disk to the private one, and back on Restore, while the row keeps the
// file's visibility; otherwise its own disk makes it private in place,
// and public again on Restore. A disk without the SetVisibility
// capability cannot hide one object, so its object is left as it is (a
// whole-disk-public bucket needs WithVisibilityDisks). A private file
// needs nothing. It returns the file as it will be and how to undo the
// change.
func (s *Store) trashExposure(ctx context.Context, f File, hide bool) (File, func(context.Context), error) {
	none := func(context.Context) {}
	if f.Visibility != filesystem.VisibilityPublic {
		return f, none, nil
	}
	from, to, want := s.visibilityDisks[filesystem.VisibilityPrivate], s.visibilityDisks[filesystem.VisibilityPublic], filesystem.VisibilityPublic
	if hide {
		from, to, want = to, from, filesystem.VisibilityPrivate
	}
	if s.visibilityDisks != nil && f.Disk == from {
		return s.moveObject(ctx, f, to, want)
	}
	disk, err := s.disks.Disk(f.Disk)
	if err != nil {
		return File{}, nil, err
	}
	if !disk.Capabilities().SetVisibility {
		return f, none, nil
	}
	// A shared object stays public while another live file shares it: the
	// same bytes answer at that file's URL anyway (blobs.go).
	if hide {
		if shared, err := s.sharedWith(ctx, f, true); err != nil || shared {
			return f, none, err
		}
	}
	previous := filesystem.VisibilityPrivate
	if hide {
		previous = filesystem.VisibilityPublic
	}
	undo, err := s.setObjectVisibility(ctx, f, want, previous)
	if err != nil {
		return File{}, nil, err
	}
	return f, undo, nil
}

// moveObject copies f's object to disk name with visibility v and returns
// the file as it will be, and how to delete the copy again.
func (s *Store) moveObject(ctx context.Context, f File, name string, v filesystem.Visibility) (File, func(context.Context), error) {
	src, err := s.disks.Disk(f.Disk)
	if err != nil {
		return File{}, nil, err
	}
	dst, _ := s.disks.Disk(name) // checked by New
	if _, err := filesystem.CopyAcross(ctx, src, f.Key, dst, f.Key); err != nil {
		return File{}, nil, err
	}
	if dst.Capabilities().SetVisibility {
		if err := dst.SetVisibility(ctx, f.Key, v); err != nil {
			return File{}, nil, errors.Join(err, dst.Delete(context.WithoutCancel(ctx), f.Key))
		}
	}
	moved := f
	moved.Disk = name
	return moved, func(ctx context.Context) { _ = dst.Delete(context.WithoutCancel(ctx), f.Key) }, nil
}
