package memory

import (
	"context"

	"github.com/ubgo/filesystem"
)

// MakeDir implements filesystem.DirMaker by recording dir, so DirExists
// reports it while it is empty, as a filesystem would. An object at dir's
// key is CodeExists. Unlike the local driver, which removes a directory
// with the last key in it, a recorded directory lasts until DeletePrefix
// removes it (or one of its parents); objects under it are unaffected
// either way, and List shows keys only, never an empty directory.
func (d *Driver) MakeDir(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.objects[dir]; ok {
		return filesystem.Errorf(filesystem.CodeExists, "%q is an object, not a directory", dir)
	}
	d.dirs[dir] = struct{}{}
	return nil
}

// DirExists implements filesystem.DirMaker: dir exists when MakeDir
// recorded it or a directory under it, or when an object is under it.
func (d *Driver) DirExists(ctx context.Context, dir string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for made := range d.dirs {
		if made == dir || filesystem.KeyInDir(made, dir) {
			return true, nil
		}
	}
	for k := range d.objects {
		if filesystem.KeyInDir(k, dir) {
			return true, nil
		}
	}
	return false, nil
}

// removeDirs forgets dir and every recorded directory under it, for
// DeletePrefix. Callers hold the write lock.
func (d *Driver) removeDirs(dir string) {
	for made := range d.dirs {
		if made == dir || filesystem.KeyInDir(made, dir) {
			delete(d.dirs, made)
		}
	}
}
