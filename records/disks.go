package records

import (
	"context"
	"database/sql"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// DiskRenamer is a plugin that stores disk names of its own beside
// fs_files.disk (image variants, kept versions). RenameDisk must rewrite
// every one of them that is from to to, with Store.Conn(ctx) so the
// change joins the transaction of Store.RenameDisk; an error rolls the
// whole rename back. A plugin that stores disk names and does not
// implement it is left pointing at the old name.
type DiskRenamer interface {
	RenameDisk(ctx context.Context, s *Store, from, to string) error
}

// RenameDisk points every record on the disk named from at the disk named
// to instead, in every scope and in one transaction: fs_files.disk, then
// each DiskRenamer plugin's own columns. It is the records side of
// renaming a disk in the Manager (moving a store to S3 under a new name):
// it changes rows only and copies no bytes, so the objects must already be
// reachable on to under the same keys. to must be a disk of the Store's
// Manager; from need not be any more. Trashed files move too. It emits no
// events (it can touch every row), so pause uploads to from while it runs
// and keep both names registered until it has committed. Returns how many
// fs_files rows changed. from == to, or an empty name, is
// CodeInvalidArgument; a to the Manager does not have is CodeNotFound.
//
// With WithVisibilityDisks, renaming from one of those disks to a disk
// that is not one is CodeInvalidArgument: the Store would keep uploading
// to, and moving files between, the old names. Rebuild the Store with
// WithVisibilityDisks naming the new disk first, then rename.
func (s *Store) RenameDisk(ctx context.Context, from, to string) (int64, error) {
	if from == "" || to == "" || from == to {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: RenameDisk needs two different disk names")
	}
	if _, err := s.disks.Disk(to); err != nil {
		return 0, err
	}
	if s.isVisibilityDisk(from) && !s.isVisibilityDisk(to) {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: disk %q is a WithVisibilityDisks disk of this Store; build the Store with %q in its place, then rename", from, to)
	}
	var n int64
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		upd := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileDisk, to)).Where(sqlb.Eq(FileDisk, from))
		if n, err = s.Exec(ctx, upd); err != nil {
			return err
		}
		for _, p := range s.plugins {
			if r, ok := p.(DiskRenamer); ok {
				if err := r.RenameDisk(ctx, s, from, to); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// isVisibilityDisk reports whether name is one of the WithVisibilityDisks
// disks.
func (s *Store) isVisibilityDisk(name string) bool {
	for _, d := range s.visibilityDisks {
		if d == name {
			return true
		}
	}
	return false
}
