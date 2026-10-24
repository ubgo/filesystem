package video

import (
	"context"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// derivedRef names where the plugin records one derived object in a
// file's metadata: its key under keyMeta and, when it is not on the
// file's own disk, that disk under diskMeta.
type derivedRef struct{ keyMeta, diskMeta string }

// posterRef and hlsRef are the derived objects the plugin records. A
// poster is one object; a ladder is every object in its master
// playlist's directory (hlsDir), named by the master's key.
var (
	posterRef = derivedRef{MetaPosterKey, MetaPosterDisk}
	hlsRef    = derivedRef{MetaHLSKey, MetaHLSDisk}
)

// derivedRefs are every derived object the plugin records on a file.
var derivedRefs = []derivedRef{posterRef, hlsRef}

// hlsDir is the directory of a ladder whose master playlist is at key,
// with its trailing slash: every file of the ladder, and nothing else,
// is under it, because its last part is a random token.
func hlsDir(key string) string {
	return path.Dir(key) + "/"
}

// hlsMasterFor is the master playlist's key of the ladder key would
// belong to, or "" for a key in no directory.
func hlsMasterFor(key string) string {
	if !strings.Contains(key, "/") {
		return ""
	}
	return hlsDir(key) + HLSMaster
}

// ownedBatch is how many keys one OwnedObjects query matches, each a LIKE
// on the metadata column: SweepOrphans asks up to 500 at a time, and a
// smaller OR list keeps each statement short on every engine.
const ownedBatch = 100

// The plugin stores objects no fs_files row names, so it must answer
// SweepOrphans, and move them with their file.
var (
	_ records.ObjectOwner   = (*Plugin)(nil)
	_ records.FileDiskMover = (*Plugin)(nil)
)

// OwnedObjects implements records.ObjectOwner, so Store.SweepOrphans never
// deletes a derived object whose file row, live or trashed, still names
// it; one whose file was purged (its deletion after the purge failed) is
// not claimed and is swept. It reports which of keys, all on the disk
// named disk, are derived objects of a file: a poster by its own key,
// a ladder's file by the master playlist its directory holds. The
// metadata column is
// matched with LIKE on each key as it is stored (a JSON pair), and every
// candidate row is decoded and checked exactly, disk included, so a near
// match (a case-insensitive collation, another key containing this one)
// never claims an object.
func (p *Plugin) OwnedObjects(ctx context.Context, s *records.Store, disk string, keys []string) (map[string]bool, error) {
	owned := map[string]bool{}
	for start := 0; start < len(keys); start += ownedBatch {
		chunk := keys[start:min(start+ownedBatch, len(keys))]
		var preds []sqlb.Predicate
		wanted := map[string]bool{}
		// byDir groups the keys that may be a ladder's files under the
		// directory whose master would own them.
		byDir := map[string][]string{}
		for _, k := range chunk {
			wanted[k] = true
			preds = append(preds, sqlb.Contains(records.FileMetadata, jsonPair(posterRef.keyMeta, k)))
			if m := hlsMasterFor(k); m != "" {
				if _, asked := byDir[hlsDir(m)]; !asked {
					preds = append(preds, sqlb.Contains(records.FileMetadata, jsonPair(hlsRef.keyMeta, m)))
				}
				byDir[hlsDir(m)] = append(byDir[hlsDir(m)], k)
			}
		}
		var fileDisk, metadata string
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(records.FileDisk, &fileDisk), sqlb.Bind(records.FileMetadata, &metadata)).
			From(records.TableFiles).Where(sqlb.Or(preds...))
		err := s.Each(ctx, q, func() error {
			var meta map[string]string
			if json.Unmarshal([]byte(metadata), &meta) != nil {
				return nil // not a map of strings: names no object of ours
			}
			f := records.File{Disk: fileDisk, Metadata: meta}
			if d, k, ok := refLocation(f, posterRef); ok && d == disk && wanted[k] {
				owned[k] = true
			}
			if d, m, ok := refLocation(f, hlsRef); ok && d == disk && path.Base(m) == HLSMaster {
				for _, k := range byDir[hlsDir(m)] {
					owned[k] = true
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return owned, nil
}

// jsonPair is "name":"value" as the Store encodes metadata (json.Marshal,
// so with its escaping), the text a LIKE looks for.
func jsonPair(name, value string) string {
	n, _ := json.Marshal(name)  // a string cannot fail to marshal
	v, _ := json.Marshal(value) // a string cannot fail to marshal
	return string(n) + ":" + string(v)
}

// refLocation reads one derived object's disk and key from f's metadata;
// ok is false when f has none.
func refLocation(f records.File, ref derivedRef) (disk, key string, ok bool) {
	key = f.Metadata[ref.keyMeta]
	if key == "" {
		return "", "", false
	}
	disk = f.Metadata[ref.diskMeta]
	if disk == "" {
		disk = f.Disk
	}
	return disk, key, true
}

// MoveFileDisk implements records.FileDiskMover, inside Store.MoveFile's
// transaction: each derived object of f that follows its file (no disk
// recorded, so on f's own disk) is copied to the disk named to under the
// same key (its size checked against the source), and the old object is
// deleted after the commit; the metadata needs no change, since such an
// object is wherever its file is. A copy is deleted again if the move
// rolls back, so a failed move leaves everything where it was. An object
// WithLocation put on a disk of its own (its disk recorded) stays there.
// A ladder moves file by file, every object of its directory. A derived
// object missing from the disk fails the move, as a variant's does,
// rather than leaving the file pointing at nothing.
func (p *Plugin) MoveFileDisk(ctx context.Context, s *records.Store, f records.File, to string) error {
	src, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	dst, err := s.Disks().Disk(to)
	if err != nil {
		return err
	}
	var copied, moved []string
	s.OnRollback(ctx, func(ctx context.Context) {
		for _, key := range copied {
			// Best effort, run by the Store without the request's
			// cancellation: an orphan is swept by SweepOrphans.
			_ = dst.Delete(ctx, key)
		}
	})
	for _, ref := range derivedRefs {
		key := f.Metadata[ref.keyMeta]
		if key == "" || f.Metadata[ref.diskMeta] != "" {
			continue
		}
		keys := []string{key}
		if ref == hlsRef {
			if keys, err = dirKeys(ctx, src, hlsDir(key)); err != nil {
				return err
			}
			if !slices.Contains(keys, key) {
				return filesystem.Errorf(filesystem.CodeNotFound, "video: HLS master %q is missing", key)
			}
		}
		for _, key := range keys {
			st, err := src.Stat(ctx, key)
			if err != nil {
				return err
			}
			e, err := filesystem.CopyAcross(ctx, src, key, dst, key)
			if err != nil {
				return err
			}
			copied = append(copied, key)
			if e.Size != st.Size {
				return filesystem.Errorf(filesystem.CodeUnavailable, "video: %q copied as %d bytes, stored as %d", key, e.Size, st.Size)
			}
			moved = append(moved, key)
		}
	}
	for _, key := range moved {
		releaseAfterCommit(ctx, s, records.File{ID: f.ID, Scope: f.Scope, Disk: f.Disk, Key: key})
	}
	return nil
}

// releaseAfterCommit deletes a derived object o once the transaction in
// ctx commits. Store.DeleteObjectAfterCommit keeps an object some file
// row or ObjectOwner still names, and it checks when it is called; called
// inside the transaction, from a hook that runs before the row changes
// (a purge, a replacement), the row would still name o through
// OwnedObjects and o would be kept forever. Called after the commit, the
// row no longer names it, so it is deleted, with a failure reported to
// the Store's object error handler. A rollback deletes nothing.
func releaseAfterCommit(ctx context.Context, s *records.Store, o records.File) {
	s.AfterCommit(ctx, func(ctx context.Context) { s.DeleteObjectAfterCommit(ctx, o) })
}

// dirKeys lists every key under dir on d.
func dirKeys(ctx context.Context, d *filesystem.Disk, dir string) ([]string, error) {
	var keys []string
	for e, err := range d.List(ctx, dir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return nil, err
		}
		if !e.IsDir {
			keys = append(keys, e.Key)
		}
	}
	return keys, nil
}

// releaseDirAfterCommit deletes every object under dir on the disk named
// disk once the transaction in ctx commits, each through
// Store.DeleteObjectAfterCommit (see releaseAfterCommit), so an object
// some row still names is kept. The directory is listed after the commit;
// a listing that fails leaves the files for SweepOrphans, which no longer
// finds a row naming them.
func releaseDirAfterCommit(ctx context.Context, s *records.Store, disk, dir string) {
	s.AfterCommit(ctx, func(ctx context.Context) {
		d, err := s.Disks().Disk(disk)
		if err != nil {
			return
		}
		keys, err := dirKeys(ctx, d, dir)
		if err != nil {
			return
		}
		for _, key := range keys {
			s.DeleteObjectAfterCommit(ctx, records.File{Disk: disk, Key: key})
		}
	})
}

var _ records.DiskRenamer = (*Plugin)(nil)

// RenameDisk implements records.DiskRenamer, inside Store.RenameDisk's
// transaction (s.Exec joins it): every file whose metadata records a
// derived object on the disk named from (a disk WithLocation chose, kept
// under a <x>_disk key) is pointed at to instead, so the Location readers
// and the purge keep finding the object. Objects with no disk recorded
// follow their file and need nothing. Rows are read first and rewritten
// after, so no statement runs while a result set is open. The metadata is
// matched with LIKE and checked exactly after decoding, as in
// OwnedObjects; an error rolls the whole rename back.
func (p *Plugin) RenameDisk(ctx context.Context, s *records.Store, from, to string) error {
	var preds []sqlb.Predicate
	for _, ref := range derivedRefs {
		preds = append(preds, sqlb.Contains(records.FileMetadata, jsonPair(ref.diskMeta, from)))
	}
	type row struct {
		scope, id string
		meta      map[string]string
	}
	var rows []row
	var scope, id, metadata string
	q := sqlb.NewSelect(s.Dialect()).
		Columns(sqlb.Bind(records.FileScope, &scope), sqlb.Bind(records.FileID, &id), sqlb.Bind(records.FileMetadata, &metadata)).
		From(records.TableFiles).Where(sqlb.Or(preds...))
	err := s.Each(ctx, q, func() error {
		var meta map[string]string
		if json.Unmarshal([]byte(metadata), &meta) != nil {
			return nil // not a map of strings: records no disk of ours
		}
		changed := false
		for _, ref := range derivedRefs {
			if meta[ref.diskMeta] == from {
				meta[ref.diskMeta] = to
				changed = true
			}
		}
		if changed {
			rows = append(rows, row{scope: scope, id: id, meta: meta})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		text, _ := json.Marshal(r.meta) // a map of strings cannot fail to marshal
		upd := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, string(text))).
			Where(sqlb.Eq(records.FileScope, r.scope), sqlb.Eq(records.FileID, r.id))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
	}
	return nil
}
