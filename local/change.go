package local

import (
	"context"
	"errors"
	"hash/fnv"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// How changes to objects are serialized and told apart.
//
// Every change to a key holds that key's lock: one of lockStripes mutexes,
// picked by hashing the key, so a write can never replace a file an append
// is writing to, a compare-and-swap's check and rename are one step, and
// changes to unrelated keys run in parallel. A move locks both keys' stripes
// and a prefix delete every stripe, always in index order, so they cannot
// deadlock. The locks coordinate this process only: two processes sharing
// the directory are not coordinated.
//
// A new version must carry a change time later than every version that
// key had before (see pass), because the ETag includes it and a freed inode
// can be reused, in the same timestamp tick, by a file of the same size and
// modification time. The wait happens before the change is made, on the
// staging file for a write or on the live file for an in-place change, so a
// change time that will not move fails the change before anything is
// replaced.
const (
	// lockStripes is how many key locks a driver has: enough that
	// unrelated keys rarely share one.
	lockStripes = 64
	// stampPoll is the pause between two looks at the change time; a
	// kernel tick is 1 to 10ms.
	stampPoll = time.Millisecond
	// stampTimeout gives up on a change time that does not move, which
	// even the coarsest filesystem (FAT, 2s) outlasts.
	stampTimeout = 5 * time.Second
	// placeAttempts bounds the retries of a rename or link whose parent
	// directory a concurrent delete of another key removed.
	placeAttempts = 3
)

// stripe is one key lock.
type stripe struct {
	mu sync.Mutex
	// removed is the latest change time of a version removed from one of
	// this stripe's keys (deleted, or moved away), initially that of a
	// probe at New, which is later than every file already in the
	// directory: a new file at any of these keys must pass it, so it never
	// repeats a removed version's ETag. Guarded by mu.
	removed int64
}

// stripeOf is the index of key's lock. On a filesystem that folds case,
// keys differing only in case name one file, so they share a lock.
func (d *Driver) stripeOf(key string) int {
	if d.caseInsensitive {
		key = strings.ToLower(key)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // a hash.Hash never fails
	return int(h.Sum32() % lockStripes)
}

// lock takes the locks of the given stripes, each once, in index order,
// and returns the function that releases them.
func (d *Driver) lock(stripes ...int) func() {
	slices.Sort(stripes)
	stripes = slices.Compact(stripes)
	for _, i := range stripes {
		d.locks[i].mu.Lock()
	}
	return func() {
		for _, i := range slices.Backward(stripes) {
			d.locks[i].mu.Unlock()
		}
	}
}

// lockAll takes every key lock, for a change to many keys at once.
func (d *Driver) lockAll() func() {
	all := make([]int, lockStripes)
	for i := range all {
		all[i] = i
	}
	return d.lock(all...)
}

// floor is the change time a new version of a key in stripe i must pass:
// the later of the stripe's removed versions and the key's current
// version cur (nil when the key has none). Callers hold the stripe's lock.
func (d *Driver) floor(i int, cur fs.FileInfo) int64 {
	floor := d.locks[i].removed
	if cur != nil {
		_, ctime, _ := d.stamp(cur)
		floor = max(floor, ctime)
	}
	return floor
}

// removed records that a version with change time ctime left a key of
// stripe i. Callers hold the stripe's lock.
func (d *Driver) removed(i int, ctime int64) {
	d.locks[i].removed = max(d.locks[i].removed, ctime)
}

// pass makes the file at name carry a change time later than floor before
// it becomes (or changes) an object: while the filesystem still shows an
// earlier or equal time (two changes inside one timestamp tick), it sets
// the file's mode to what it already is, which changes nothing but the
// change time, until the clock moves on. A change time that does not move
// within stampWait is CodeUnavailable, and a ctx cancelled meanwhile is
// ctx's error, both returned before the change is made. It returns the
// change time reached.
func (d *Driver) pass(ctx context.Context, name string, floor int64) (int64, error) {
	if !d.stamps {
		return 0, nil
	}
	deadline := time.Now().Add(d.stampWait)
	for {
		info, err := d.root.Stat(name)
		if err != nil {
			return 0, err
		}
		if _, ctime, _ := d.stamp(info); ctime > floor {
			return ctime, nil
		}
		if time.Now().After(deadline) {
			return 0, filesystem.Errorf(filesystem.CodeUnavailable, "the filesystem's change time did not move past the last version's within %s; nothing was changed", d.stampWait)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(stampPoll):
		}
		if err := d.root.Chmod(name, info.Mode().Perm()); err != nil {
			return 0, err
		}
	}
}

// clock reads the filesystem's current change time from a new staging
// file, and whether this system reports change times and inodes at all.
// Every file changed before the call has a change time no later.
func (d *Driver) clock() (int64, bool, error) {
	name := tmpDir + "/" + randomName()
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, DefaultFilePrivate)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = d.root.Remove(name) }()
	if err := f.Close(); err != nil {
		return 0, false, err
	}
	info, err := d.root.Stat(name)
	if err != nil {
		return 0, false, err
	}
	_, ctime, ok := d.stamp(info)
	return ctime, ok, nil
}

// place runs op, a rename or link onto key, after creating key's parent
// directories, and again when it failed because a concurrent delete of
// another key removed a parent that had just become empty. Failing that
// placeAttempts times is CodeUnavailable, a retryable condition, not the
// CodeNotFound the underlying error would read as.
func (d *Driver) place(key string, op func() error) error {
	var err error
	for range placeAttempts {
		if err = d.mkParents(key); err != nil {
			return err
		}
		if err = op(); !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return filesystem.Errorf(filesystem.CodeUnavailable, "%q could not be placed in %d attempts: a directory on its path, or the staged file, kept disappearing under concurrent deletes; nothing was changed, retry: %w", key, placeAttempts, err)
}

// current is key's file, or nil when the key has no object.
func (d *Driver) current(key string) (fs.FileInfo, error) {
	info, err := d.statFile(key)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil, nil
	}
	return info, err
}
