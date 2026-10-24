package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// FS returns a read-only io/fs view of the disk, so code written against
// the standard interfaces (html/template.ParseFS, http.FileServerFS,
// fs.WalkDir, testing/fstest) works on any backend. It implements fs.FS,
// fs.StatFS, fs.ReadDirFS and fs.ReadFileFS; fs.Sub gives a view of a
// directory.
//
// io/fs has no context, so ctx governs every call made through the view:
// cancel it to stop them. Every call goes through the Disk, so keys are
// cleaned and interceptors run as for any other read.
//
// Names are io/fs paths (fs.ValidPath: slash-separated, no "." or ".."
// elements, "." for the root). A name the Disk would clean into a different
// key (a backslash, a control character) does not exist in the view, so
// two names never reach one object. Directories are implied by keys, as in
// Disk.List: a directory exists while some key is below it, and the root
// always exists. Files report mode 0444 and directories fs.ModeDir|0555;
// FileInfo.Sys returns the Entry.
func (d *Disk) FS(ctx context.Context) fs.FS {
	return &diskFS{ctx: ctx, d: d}
}

// diskFS is the view FS returns.
type diskFS struct {
	ctx context.Context
	d   *Disk
}

// Interfaces the view promises; a missing method fails the build.
var (
	_ fs.StatFS      = (*diskFS)(nil)
	_ fs.ReadDirFS   = (*diskFS)(nil)
	_ fs.ReadFileFS  = (*diskFS)(nil)
	_ fs.ReadDirFile = (*fsDir)(nil)
)

// File modes the view reports: read-only, as the view is.
const (
	fsFileMode fs.FileMode = 0o444
	fsDirMode              = fs.ModeDir | 0o555
)

// fsRoot is the io/fs name of the root directory.
const fsRoot = "."

// key maps an io/fs name to a disk key ("" for the root), or fails with a
// *fs.PathError for op. A name the Disk would rewrite is fs.ErrNotExist:
// the view only has names that are exactly keys.
func (f *diskFS) key(op, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if name == fsRoot {
		return "", nil
	}
	clean, err := f.d.CleanKey(name)
	if err != nil || clean != name {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return clean, nil
}

// pathError wraps a Disk error for op on name, so errors.Is finds the
// io/fs sentinel (fs.ErrNotExist for CodeNotFound, fs.ErrPermission for
// CodePermission) and CodeOf still finds the Disk's code.
func pathError(op, name string, err error) error {
	var sentinel error
	switch CodeOf(err) {
	case CodeNotFound:
		sentinel = fs.ErrNotExist
	case CodePermission:
		sentinel = fs.ErrPermission
	default:
		return &fs.PathError{Op: op, Path: name, Err: err}
	}
	return &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%w: %w", sentinel, err)}
}

// Open implements fs.FS. A file is read lazily, on its first Read.
func (f *diskFS) Open(name string) (fs.File, error) {
	info, err := f.stat("open", name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return &fsDir{fsys: f, name: name, info: info}, nil
	}
	return &fsFile{fsys: f, info: info}, nil
}

// Stat implements fs.StatFS: the object's Entry, or a directory when a key
// lies below name.
func (f *diskFS) Stat(name string) (fs.FileInfo, error) {
	return f.stat("stat", name)
}

func (f *diskFS) stat(op, name string) (fileInfo, error) {
	key, err := f.key(op, name)
	if err != nil {
		return fileInfo{}, err
	}
	if key == "" {
		return dirInfo(fsRoot), nil
	}
	e, err := f.d.Stat(f.ctx, key)
	if err == nil {
		return fileInfo{e: e}, nil
	}
	if CodeOf(err) != CodeNotFound {
		return fileInfo{}, pathError(op, name, err)
	}
	// Not an object: a directory if anything lies below it.
	page, lerr := f.d.ListPage(f.ctx, key, ListOptions{Limit: 1})
	if lerr != nil {
		return fileInfo{}, pathError(op, name, lerr)
	}
	if len(page.Entries) == 0 {
		return fileInfo{}, pathError(op, name, err)
	}
	return dirInfo(key), nil
}

// ReadDir implements fs.ReadDirFS: the directory's entries sorted by name.
func (f *diskFS) ReadDir(name string) ([]fs.DirEntry, error) {
	info, err := f.stat("readdir", name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	return f.list(name)
}

// list reads every entry of the directory name (already known to exist).
func (f *diskFS) list(name string) ([]fs.DirEntry, error) {
	key, _ := f.key("readdir", name) // valid: name was stat'ed
	var out []fs.DirEntry
	for e, err := range f.d.List(f.ctx, key, ListOptions{}) {
		if err != nil {
			return nil, pathError("readdir", name, err)
		}
		if e.IsDir {
			out = append(out, fs.FileInfoToDirEntry(dirInfo(e.Key)))
		} else {
			out = append(out, fs.FileInfoToDirEntry(fileInfo{e: e}))
		}
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// ReadFile implements fs.ReadFileFS.
func (f *diskFS) ReadFile(name string) ([]byte, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if fi, _ := file.Stat(); fi.IsDir() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("is a directory")}
	}
	return io.ReadAll(file)
}

// fileInfo is an fs.FileInfo for an object, or for a directory when dir is
// set.
type fileInfo struct {
	e   Entry
	dir string
}

// dirInfo is the FileInfo of the directory at key ("." for the root).
func dirInfo(key string) fileInfo {
	return fileInfo{e: Entry{Key: key, IsDir: true}, dir: key}
}

func (i fileInfo) Name() string {
	if i.dir == fsRoot {
		return fsRoot
	}
	return path.Base(i.e.Key)
}
func (i fileInfo) Size() int64 { return i.e.Size }
func (i fileInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fsDirMode
	}
	return fsFileMode
}
func (i fileInfo) ModTime() time.Time { return i.e.ModTime }
func (i fileInfo) IsDir() bool        { return i.e.IsDir }

// Sys returns the Entry the information came from.
func (i fileInfo) Sys() any { return i.e }

// fsFile is an open object. The read starts on the first Read, so Open
// followed by Stat costs no download.
type fsFile struct {
	fsys   *diskFS
	info   fileInfo
	rc     io.ReadCloser
	closed bool
}

func (f *fsFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *fsFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, &fs.PathError{Op: "read", Path: f.info.e.Key, Err: fs.ErrClosed}
	}
	if f.rc == nil {
		rc, err := f.fsys.d.Read(f.fsys.ctx, f.info.e.Key)
		if err != nil {
			return 0, pathError("read", f.info.e.Key, err)
		}
		f.rc = rc
	}
	return f.rc.Read(p)
}

func (f *fsFile) Close() error {
	if f.closed {
		return &fs.PathError{Op: "close", Path: f.info.e.Key, Err: fs.ErrClosed}
	}
	f.closed = true
	if f.rc == nil {
		return nil
	}
	return f.rc.Close()
}

// fsDir is an open directory. Its entries are listed on the first
// ReadDir and handed out from there, as fs.ReadDirFile describes.
type fsDir struct {
	fsys    *diskFS
	name    string
	info    fileInfo
	entries []fs.DirEntry
	listed  bool
	closed  bool
}

func (d *fsDir) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *fsDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

func (d *fsDir) Close() error {
	if d.closed {
		return &fs.PathError{Op: "close", Path: d.name, Err: fs.ErrClosed}
	}
	d.closed = true
	return nil
}

// ReadDir implements fs.ReadDirFile: n > 0 returns at most n entries and
// io.EOF once none are left; n <= 0 returns all that are left.
func (d *fsDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, &fs.PathError{Op: "readdir", Path: d.name, Err: fs.ErrClosed}
	}
	if !d.listed {
		entries, err := d.fsys.list(d.name)
		if err != nil {
			return nil, err
		}
		d.entries, d.listed = entries, true
	}
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}
