package local

import (
	"context"
	"errors"
	"io/fs"
	"syscall"

	"github.com/ubgo/filesystem"
)

// MakeDir implements filesystem.DirMaker: it creates dir and any missing
// parents with the driver's directory mode. A file at dir is CodeExists;
// a file at one of its parents means dir cannot exist (see the package
// limitation), which is CodeInvalidArgument. Under full durability the
// directories it created are synced, like a write's.
//
// A directory lives as long as the filesystem keeps it: deleting the last
// key under it (Delete, Move) removes it again, because the driver keeps
// the tree mirroring the key space, and DeletePrefix removes it with
// everything below.
func (d *Driver) MakeDir(ctx context.Context, dir string) error {
	if err := firstErr(ctx.Err(), d.checkNewKey(dir)); err != nil {
		return err
	}
	info, err := d.root.Lstat(dir)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return filesystem.Errorf(filesystem.CodeExists, "%q is a file, not a directory", dir)
	case !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
		return err
	}
	stop, err := d.syncStop(dir)
	if err != nil {
		return err
	}
	if err := d.mkDirs(dir, dir); err != nil {
		return err
	}
	return d.syncDirs(dir, stop)
}

// DirExists implements filesystem.DirMaker: whether dir is a directory.
// A file at dir, or at one of its parents, is not one.
func (d *Driver) DirExists(ctx context.Context, dir string) (bool, error) {
	if err := firstErr(ctx.Err(), d.checkKey(dir)); err != nil {
		return false, err
	}
	info, err := d.root.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}
