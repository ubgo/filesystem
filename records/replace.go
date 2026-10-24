package records

import (
	"context"
	"database/sql"
	"io"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// NewContent is the object that becomes a file's content in
// ReplaceObject.
type NewContent struct {
	// Key is the object, on the file's own disk; it must exist and differ
	// from the current one.
	Key string
	// Filename replaces the display name; "" keeps the current one.
	Filename string
	// ContentType, Size and SHA256 default to what the disk reports;
	// SHA256 is 64 hex digits in either case, stored lowercase.
	ContentType string
	Size        int64
	SHA256      string
	// Checksums are the new content's extra digests, as NewFile.Checksums
	// takes them; they replace the file's, so none leaves it none.
	Checksums map[string]string
}

// ReplaceHooks lets a plugin take part in a content replacement: quota
// charges the size difference. BeforeReplace runs inside the transaction
// before the row changes and may change next's metadata, not its ID,
// Scope, Disk or Key; an error aborts the replacement.
type ReplaceHooks interface {
	BeforeReplace(ctx context.Context, s *Store, current File, next *File) error
}

// Versioner keeps replaced content (the versions plugin). KeepVersion runs
// inside the replacement's transaction with the file as it was. Returning
// true takes ownership of that content's object: the Store does not
// delete it, and the plugin deletes it when it drops the version and
// before the file is purged.
type Versioner interface {
	KeepVersion(ctx context.Context, s *Store, previous File) (bool, error)
}

// ReplaceObject makes an existing object the content of a live file. The
// file keeps its ID, so its attachments, and whatever plugins tie to it,
// stay. Like CreateFile it is object-first: the new object must exist,
// and is given the file's visibility where its disk can set it (put back
// if the transaction fails), and the previous one is deleted only after the transaction commits
// (failures go to WithObjectErrorHandler), unless a Versioner keeps it.
// Emits EventFileReplaced. With IfUnchanged it replaces only the content
// the caller read (optimistic concurrency); with QuarantineContent the
// file is quarantined with its new content. A quarantined file stays
// quarantined whatever replaces it.
func (s *Store) ReplaceObject(ctx context.Context, scope, id string, in NewContent, opts ...ChangeOption) (File, error) {
	o := changeOptionsOf(opts)
	if err := o.notForPurgeOnly("ReplaceObject"); err != nil {
		return File{}, err
	}
	key, err := filesystem.CleanKey(in.Key)
	if err != nil {
		return File{}, err
	}
	sum, err := digest(in.SHA256, "")
	if err != nil {
		return File{}, err
	}
	sums, err := checksumsOf(in.Checksums)
	if err != nil {
		return File{}, err
	}
	var next File
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		cur, err := s.File(ctx, scope, id)
		if o.guarded() {
			// Locked, so no other replacement lands between the check and
			// this one's write.
			cur, err = s.lockedFile(ctx, scope, id, lockUpdate)
		}
		if err != nil {
			return err
		}
		if cur.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeNotFound, "records: file %q is in the trash", id)
		}
		if err := o.check(cur); err != nil {
			return err
		}
		if key == cur.Key {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: %q is already the content of file %q", key, id)
		}
		disk, err := s.disks.Disk(cur.Disk)
		if err != nil {
			return err
		}
		e, err := disk.Stat(ctx, key)
		if err != nil {
			return err
		}
		// The new object takes the file's visibility, as Replace's does, so
		// the row never says private over a public object; a rollback puts
		// the object's own visibility back.
		if disk.Capabilities().SetVisibility && e.Visibility != cur.Visibility {
			undo, err := s.setObjectVisibility(ctx, File{Disk: cur.Disk, Key: key}, cur.Visibility, e.Visibility)
			if err != nil {
				return err
			}
			s.OnRollback(ctx, undo)
		}
		next = cur
		next.Key = key // before the hooks: they inspect the new content
		// The status the replacement will have, so a hook can tell new
		// content that is about to be quarantined (and must not be read)
		// from content that is not; it is set again after the hooks.
		if o.quarantine {
			next.Status, next.StatusDetail = StatusQuarantined, ""
		}
		next.Filename = upload.SafeFilename(orDefault(in.Filename, cur.Filename))
		next.ContentType = orDefault(in.ContentType, e.ContentType)
		next.SHA256 = orDefault(sum, reported(e.SHA256))
		next.Checksums = sums
		next.Size = in.Size
		if next.Size == 0 {
			next.Size = e.Size
		}
		for _, p := range s.plugins {
			if h, ok := p.(ReplaceHooks); ok {
				if err := h.BeforeReplace(ctx, s, cur, &next); err != nil {
					return err
				}
			}
		}
		next.ID, next.Scope, next.Disk, next.Key = cur.ID, cur.Scope, cur.Disk, key
		// The status is the Store's, set after the hooks, so no hook can
		// release a quarantined file by replacing it.
		next.Status, next.StatusDetail = cur.Status, cur.StatusDetail
		if o.quarantine {
			if cur.Visibility == filesystem.VisibilityPublic {
				return ErrPublicQuarantine
			}
			next.Status, next.StatusDetail = StatusQuarantined, ""
		}
		// After the hooks, as in CreateFile: text a hook adds is held to
		// the same limits.
		if err := checkText(next.Metadata); err != nil {
			return err
		}
		upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(
			sqlb.Set(FileKey, next.Key), sqlb.Set(FileFilename, next.Filename), sqlb.Set(FileContentType, next.ContentType),
			sqlb.Set(FileSize, next.Size), sqlb.Set(FileSHA256, next.SHA256), sqlb.Set(FileMetadata, encodeMetadata(next.Metadata)),
			sqlb.Set(FileStatus, next.Status), sqlb.Set(FileStatusDetail, next.StatusDetail),
			sqlb.Set(FileChecksums, encodeChecksums(next.Checksums)),
		).Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		kept := false
		for _, p := range s.plugins {
			if v, ok := p.(Versioner); ok {
				keep, err := v.KeepVersion(ctx, s, cur)
				if err != nil {
					return err
				}
				kept = kept || keep
			}
		}
		if err := s.emit(ctx, Event{Kind: EventFileReplaced, Scope: scope, FileID: id, File: fileRef(next)}); err != nil {
			return err
		}
		if !kept {
			if err := s.releaseObject(ctx, cur); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return File{}, err
	}
	return next, nil
}

// Replace stores body as the new content of a live file, under policy p
// (see upload.Receive), and records it with ReplaceObject. The new object
// gets the file's visibility, whatever p says, so a private file cannot
// become public by being replaced. filename "" keeps the display name. If
// the replacement fails, or an enclosing Tx rolls back, the new object is
// deleted again.
func (s *Store) Replace(ctx context.Context, scope, id string, body io.Reader, filename string, p upload.Policy, opts ...ChangeOption) (File, error) {
	cur, err := s.File(ctx, scope, id)
	if err != nil {
		return File{}, err
	}
	// Checked before the body is received too, so a stale edit costs no
	// upload; ReplaceObject checks again under the row lock.
	if err := changeOptionsOf(opts).check(cur); err != nil {
		return File{}, err
	}
	disk, err := s.disks.Disk(cur.Disk)
	if err != nil {
		return File{}, err
	}
	p.Visibility = cur.Visibility
	res, err := upload.Receive(ctx, disk, body, orDefault(filename, cur.Filename), p)
	if err != nil {
		return File{}, err
	}
	discard := s.discarder(disk, res.Key, p.Settle != nil)
	f, err := s.ReplaceObject(ctx, scope, id, NewContent{
		Key: res.Key, Filename: res.Filename, ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256,
		Checksums: res.Checksums,
	}, opts...)
	if err != nil {
		discard(ctx)
		return File{}, err
	}
	s.OnRollback(ctx, discard)
	return f, nil
}
