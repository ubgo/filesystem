package local

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"syscall"

	"github.com/ubgo/filesystem"
)

// Durability chooses how much a write waits for the disk before it
// returns: how many fsync calls stand between a successful Write and a
// crash or power cut that would lose it.
type Durability string

// The closed set of durability levels.
const (
	// DurabilityFull, the default, syncs the staged file before it is
	// renamed into place, then syncs the directories whose entries the
	// rename and any new parent directories added, as renameio and
	// databases do. Once Write, Append (of a new file), Move,
	// CompleteMultipart or MakeDir returns, a crash cannot lose the
	// result. It costs one more fsync per write than DurabilityFile (more
	// when the write creates directories). Directories cannot be synced
	// on Windows, where it behaves as DurabilityFile.
	DurabilityFull Durability = "full"
	// DurabilityFile syncs the file's bytes before the rename, so a crash
	// never leaves a truncated or empty object, but not the directory: a
	// crash shortly after a write may bring back the previous version,
	// or no object at all for a new key.
	DurabilityFile Durability = "file"
	// DurabilityNone syncs nothing and leaves it to the operating system
	// to write back: the fastest, and after a crash an object written
	// shortly before may be missing, empty or truncated. For caches,
	// scratch space and tests, whose content can be rebuilt.
	DurabilityNone Durability = "none"
)

// DurabilityValues lists every Durability.
var DurabilityValues = []Durability{DurabilityFull, DurabilityFile, DurabilityNone}

// WithDurability chooses how writes are synced to disk (see Durability).
// New refuses a value outside DurabilityValues. Without it the driver
// uses DurabilityFull: a write that returned is never lost, which is
// what a caller assumes unless told otherwise.
func WithDurability(v Durability) Option {
	return func(d *Driver) { d.durability = v }
}

// checkDurability defaults an unset level to DurabilityFull and refuses
// an unknown one.
func (d *Driver) checkDurability() error {
	if d.durability == "" {
		d.durability = DurabilityFull
	}
	if !slices.Contains(DurabilityValues, d.durability) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "unknown durability %q; use one of %v", d.durability, DurabilityValues)
	}
	return nil
}

// syncsFiles reports whether a staged file is synced before it is
// renamed into place.
func (d *Driver) syncsFiles() bool { return d.durability != DurabilityNone }

// syncsDirs reports whether directories are synced after an entry in
// them changes; never on Windows, which cannot open a directory to sync.
func (d *Driver) syncsDirs() bool {
	return d.durability == DurabilityFull && d.goos != goosWindows
}

// syncStop returns, before name (a key or a directory) is placed, the
// highest directory syncDirs must sync once it is: name's parent when
// that exists, else the parent of the highest directory the placement
// will create, whose new entry must reach the disk too. It is "" when
// directories are not synced.
func (d *Driver) syncStop(name string) (string, error) {
	if !d.syncsDirs() {
		return "", nil
	}
	stop := path.Dir(name)
	for stop != "." {
		_, err := d.root.Lstat(stop)
		if err == nil {
			break
		}
		// A file on the path (ENOTDIR) is reported by the placement.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", err
		}
		stop = path.Dir(stop)
	}
	return stop, nil
}

// syncDirs syncs every directory from name's parent up to stop (from
// syncStop), so name's entry and those of any directories created for it
// survive a crash. With stop "" it does nothing.
func (d *Driver) syncDirs(name, stop string) error {
	if stop == "" {
		return nil
	}
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		if err := d.syncDir(dir); err != nil {
			return fmt.Errorf("%q is in place, but directory %q could not be synced, so a crash may still lose it: %w", name, dir, err)
		}
		if dir == stop || dir == "." {
			return nil
		}
	}
}

// syncDir fsyncs one directory.
func (d *Driver) syncDir(dir string) error {
	f, err := d.root.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
