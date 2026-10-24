package records

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// ObjectOwner is a plugin that stores objects of its own beside the files'
// (kept versions, image variants). SweepOrphans asks every installed
// ObjectOwner before deleting an object no fs_files row names, so it never
// deletes an object a plugin still holds. A plugin that stores objects on
// a disk SweepOrphans may sweep must implement it.
type ObjectOwner interface {
	// OwnedObjects reports which of keys, all on the disk named disk, the
	// plugin holds. Keys missing from the map, or mapped to false, are not
	// the plugin's.
	OwnedObjects(ctx context.Context, s *Store, disk string, keys []string) (map[string]bool, error)
}

// orphanBatch is how many listed objects SweepOrphans checks against the
// database in one query (and the listing page size): few round trips on
// a large disk, and far under every engine's limit on bound parameters
// (SQLite's default is 32766).
const orphanBatch = 500

// SweepOrphans deletes the objects under prefix on the named disk that
// no file row names and no ObjectOwner plugin holds, last modified before
// olderThan, and returns how many it deleted. Orphans are what the
// object-first rule can leave behind: an object whose row insert failed
// and whose cleanup failed too, or one whose deletion after a purge
// failed. Unlike upload.Sweep, which deletes by age alone, it never
// deletes a recorded file, so prefix may be the root ("") of a disk the
// Store owns.
//
// The contract of prefix: every object under it is either a file's, a
// plugin's that implements ObjectOwner, or garbage. Do not point it at
// objects the application keeps outside records (exports, backups), or at
// a direct-upload area whose objects wait for a later CreateFile; for
// those, keep olderThan well before the oldest upload that may still be
// recorded. An object whose ModTime the disk does not report is never
// deleted, since its age is unknown.
//
// A failed deletion does not stop the sweep; the returned error joins one
// error per object left. A listing or database failure stops it with the
// count so far, and a rerun continues.
func (s *Store) SweepOrphans(ctx context.Context, diskName, prefix string, olderThan time.Time) (int, error) {
	disk, err := s.disks.Disk(diskName)
	if err != nil {
		return 0, err
	}
	var errs []error
	deleted, err := s.orphans(ctx, disk, prefix, olderThan, func(k string) {
		if err := disk.Delete(ctx, k); err != nil {
			errs = append(errs, fmt.Errorf("records: delete orphan %q on disk %q: %w", k, disk.Name(), err))
			return
		}
	})
	return deleted - len(errs), errors.Join(append(errs, err)...)
}

// FindOrphans is SweepOrphans without the deletions: it calls found with
// the key of every object SweepOrphans would delete, in listing order, and
// returns how many it found. Run it first on a disk you have not swept
// before, to see what a sweep would take. A listing or database failure
// stops it with the count so far.
func (s *Store) FindOrphans(ctx context.Context, diskName, prefix string, olderThan time.Time, found func(key string)) (int, error) {
	disk, err := s.disks.Disk(diskName)
	if err != nil {
		return 0, err
	}
	return s.orphans(ctx, disk, prefix, olderThan, found)
}

// orphans calls act for every object under prefix on disk older than
// olderThan that no row names and no ObjectOwner holds, a batch at a
// time, and returns how many it found. The listing's cursor is the last
// key returned, so acting on (deleting) a batch before the next page is
// fetched skips nothing.
func (s *Store) orphans(ctx context.Context, disk *filesystem.Disk, prefix string, olderThan time.Time, act func(key string)) (int, error) {
	found := 0
	var batch []string
	flush := func() error {
		known, err := s.knownObjects(ctx, disk.Name(), batch)
		if err != nil {
			return err
		}
		for _, k := range batch {
			if !known[k] {
				found++
				act(k)
			}
		}
		batch = batch[:0]
		return nil
	}
	for e, err := range disk.List(ctx, prefix, filesystem.ListOptions{Recursive: true, Limit: orphanBatch}) {
		if err != nil {
			return found, err
		}
		if e.IsDir || e.ModTime.IsZero() || !e.ModTime.Before(olderThan) {
			continue
		}
		batch = append(batch, e.Key)
		if len(batch) == orphanBatch {
			if err := flush(); err != nil {
				return found, err
			}
		}
	}
	if len(batch) > 0 {
		return found, flush()
	}
	return found, nil
}

// MissingObjects calls missing with every file row, in any scope, live
// or trashed, on the named disk whose object the disk does not have, and
// returns how many it found: the inverse of FindOrphans, for an audit
// after a restore from backup or a hand-edited bucket. It only reports;
// it never changes a row, because whether to purge the row, restore the
// object from a backup or re-upload it is the application's decision. It
// checks each object with one Exists call, reading rows a batch at a
// time in ID order. Objects plugins keep (versions, variants) are not
// checked. A database or storage failure stops it with the count so far.
func (s *Store) MissingObjects(ctx context.Context, diskName string, missing func(File)) (int, error) {
	disk, err := s.disks.Disk(diskName)
	if err != nil {
		return 0, err
	}
	found := 0
	after := ""
	for {
		var r fileRow
		var rows []File
		q := sqlb.NewSelect(s.dialect).Columns(fileBindings(&r)...).From(TableFiles).
			Where(sqlb.Eq(FileDisk, disk.Name()), sqlb.Greater(FileID, after)).
			Order(sqlb.OrderBy(FileID, sqlb.SortAsc)).Limit(orphanBatch)
		if err := s.Each(ctx, q, func() error { rows = append(rows, r.decode()); return nil }); err != nil {
			return found, err
		}
		for _, f := range rows {
			ok, err := disk.Exists(ctx, f.Key)
			if err != nil {
				return found, fmt.Errorf("records: check object of file %q: %w", f.ID, err)
			}
			if !ok {
				found++
				missing(f)
			}
		}
		if len(rows) < orphanBatch {
			return found, nil
		}
		after = rows[len(rows)-1].ID
	}
}

// knownObjects reports which of keys on disk a file row (in any scope,
// live or trashed) or an ObjectOwner plugin holds.
func (s *Store) knownObjects(ctx context.Context, disk string, keys []string) (map[string]bool, error) {
	known := map[string]bool{}
	var key string
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileKey, &key)).From(TableFiles).
		Where(sqlb.Eq(FileDisk, disk), sqlb.In(FileKey, keys...))
	if err := s.Each(ctx, q, func() error { known[key] = true; return nil }); err != nil {
		return nil, err
	}
	for _, p := range s.plugins {
		o, ok := p.(ObjectOwner)
		if !ok {
			continue
		}
		owned, err := o.OwnedObjects(ctx, s, disk, keys)
		if err != nil {
			return nil, fmt.Errorf("records: plugin %q owned objects: %w", p.Name(), err)
		}
		for k, held := range owned {
			known[k] = known[k] || held
		}
	}
	return known, nil
}
