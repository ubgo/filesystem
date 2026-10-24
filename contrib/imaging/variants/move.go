package variants

import (
	"context"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Variant objects live on their file's disk, so moving a file moves them.
var _ records.FileDiskMover = (*Plugin)(nil)

// MoveFileDisk implements records.FileDiskMover, inside Store.MoveFile's
// transaction: each of f's variants on f.Disk is copied to the disk named
// to under the same key (its size checked), its row is pointed at to, and
// the old object is deleted after the commit. A copy is deleted again if
// the move rolls back, so a failed move leaves the variants where they
// were and nothing behind on to. Usage plugins (quota) hear nothing: the
// bytes held do not change.
func (p *Plugin) MoveFileDisk(ctx context.Context, s *records.Store, f records.File, to string) error {
	list, err := p.list(ctx, s, f.Scope, f.ID)
	if err != nil {
		return err
	}
	src, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	dst, err := s.Disks().Disk(to)
	if err != nil {
		return err
	}
	var copied []string
	s.OnRollback(ctx, func(ctx context.Context) {
		for _, key := range copied {
			// Best effort, run by the Store without the request's
			// cancellation: an orphan is swept by SweepOrphans.
			_ = dst.Delete(ctx, key)
		}
	})
	var moved []Variant
	for _, v := range list {
		if v.Disk != f.Disk {
			continue
		}
		e, err := filesystem.CopyAcross(ctx, src, v.Key, dst, v.Key)
		if err != nil {
			return err
		}
		copied = append(copied, v.Key)
		if e.Size != v.Size {
			return filesystem.Errorf(filesystem.CodeUnavailable, "variants: %q copied as %d bytes, recorded as %d", v.Key, e.Size, v.Size)
		}
		moved = append(moved, v)
	}
	upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(Disk, to)).
		Where(sqlb.Eq(Scope, f.Scope), sqlb.Eq(FileID, f.ID), sqlb.Eq(Disk, f.Disk))
	if _, err := s.Exec(ctx, upd); err != nil {
		return err
	}
	deleteObjects(ctx, s, moved)
	return nil
}
