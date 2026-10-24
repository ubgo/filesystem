package local

import (
	"io"
	"io/fs"
	"os"
	"time"
)

// fileRoot is the subset of *os.Root the driver uses. It exists so the
// driver's tests can inject an OS failure at any step and prove that every
// error is returned and no staging file is left behind; production always
// uses osRoot. It is not configurable from outside the package.
type fileRoot interface {
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	Open(name string) (file, error)
	OpenFile(name string, flag int, perm fs.FileMode) (file, error)
	MkdirAll(name string, perm fs.FileMode) error
	Link(oldname, newname string) error
	Rename(oldname, newname string) error
	Remove(name string) error
	RemoveAll(name string) error
	Chmod(name string, mode fs.FileMode) error
	Chtimes(name string, atime, mtime time.Time) error
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
	FS() fs.FS
	Close() error
}

// file is the subset of *os.File the driver uses.
type file interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer
	Sync() error
}

// osRoot adapts *os.Root to fileRoot; only Open and OpenFile differ, to
// return the file interface.
type osRoot struct{ *os.Root }

func (r osRoot) Open(name string) (file, error) {
	f, err := r.Root.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (r osRoot) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	f, err := r.Root.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return f, nil
}
