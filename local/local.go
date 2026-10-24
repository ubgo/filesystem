// Package local is a filesystem.Driver over a directory on local disk.
//
// Every operation goes through an os.Root, so no key, symlink or ".." can
// reach outside the directory even if the Disk's own key checks were
// bypassed. Writes are atomic: bytes go to a temporary file in a reserved
// staging directory inside the root and are renamed into place, so readers
// never see a partial file and a failed write leaves nothing behind.
//
// Visibility is stored as the file mode (DefaultFilePublic or
// DefaultFilePrivate, or the modes given to WithPermissions): a file with
// the world-read bit is public. The driver stores neither metadata nor
// content types; Stat derives the content type from the key's extension.
//
// Limitation shared with every real filesystem: a key cannot be both an
// object and a directory, so "a" and "a/b" cannot both exist. Object stores
// allow it; portable applications should not rely on it.
package local

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ubgo/filesystem"
)

// StagingDir is the reserved directory, at the root of every local disk,
// holding temporary files and multipart parts. Keys inside it are rejected
// and listings skip it.
const StagingDir = ".filesystem"

// Directories inside StagingDir.
const (
	tmpDir       = StagingDir + "/tmp"
	multipartDir = StagingDir + "/multipart"
)

// Default permissions. Public files are world-readable so a web server
// serving the directory can read them; private files are owner-only.
const (
	DefaultFilePublic  fs.FileMode = 0o644
	DefaultFilePrivate fs.FileMode = 0o600
	DefaultDirMode     fs.FileMode = 0o755
)

// worldRead is the permission bit that distinguishes a public file.
const worldRead fs.FileMode = 0o004

// Driver is the local-disk backend. Create it with New; it is safe for
// concurrent use and must be closed with Close (or the Disk's Close).
type Driver struct {
	root        fileRoot
	native      *os.Root
	filePublic  fs.FileMode
	filePrivate fs.FileMode
	dirMode     fs.FileMode
	// allowReserved lifts the refusal of Windows reserved names.
	allowReserved bool
	// caseInsensitive is what New's probe found about the filesystem.
	caseInsensitive bool
	// stamps: this system reports inodes and change times, which the
	// ETag needs to tell versions apart (see etag); without them the
	// driver does not declare compare-and-swap.
	stamps bool
	// locks serialize the changes to each key (see change.go).
	locks [lockStripes]stripe
	// stamp reads a file's inode and change time: statStamp, which the
	// driver's tests replace with a coarser clock, as they replace root.
	stamp func(fs.FileInfo) (ino uint64, ctime int64, ok bool)
	// stampWait bounds pass's wait; stampTimeout unless a test sets it.
	stampWait time.Duration
	// durability is how writes are synced (WithDurability); init sets
	// DurabilityFull when it is unset.
	durability Durability
	// goos is runtime.GOOS, which init sets unless a test chose another
	// system to cover what differs there (syncsDirs).
	goos string
	// contentType derives an entry's Content-Type from its key
	// (WithContentType); nil means hostContentType.
	contentType func(key string) string
}

// Option configures a Driver.
type Option func(*Driver)

// WithPermissions overrides the modes used for public files, private files
// and directories. filePublic must have the world-read bit and filePrivate
// must not, because Stat reads visibility back from that bit; New rejects
// modes that break this.
func WithPermissions(filePublic, filePrivate, dir fs.FileMode) Option {
	return func(d *Driver) {
		d.filePublic, d.filePrivate, d.dirMode = filePublic, filePrivate, dir
	}
}

// WithContentType sets how Stat and List derive a file's Content-Type
// from its key, since a local file stores no type of its own. The default
// reads the host's MIME table (mime.TypeByExtension, which loads
// /etc/mime.types and similar files), so the same key can report a
// different type on a developer's Mac, in CI and in production. Pass
// sniff.TypeByExtension for a fixed table that answers the same on every
// host, or an application's own map. fn receives the cleaned key and
// returns "" when it does not know; a nil fn keeps the default.
func WithContentType(fn func(key string) string) Option {
	return func(d *Driver) { d.contentType = fn }
}

// typeOf is the Content-Type an entry reports for key (WithContentType).
func (d *Driver) typeOf(key string) string {
	if d.contentType == nil {
		return hostContentType(key)
	}
	return d.contentType(key)
}

// hostContentType is the default of WithContentType: the host's MIME
// table by the key's extension.
func hostContentType(key string) string { return mime.TypeByExtension(path.Ext(key)) }

// New opens (creating it if needed) the directory dir as a local disk.
func New(dir string, opts ...Option) (*Driver, error) {
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode}
	for _, o := range opts {
		o(d)
	}
	if d.filePublic&worldRead == 0 || d.filePrivate&worldRead != 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "public file mode %v must be world-readable and private mode %v must not", d.filePublic, d.filePrivate)
	}
	if err := d.checkDurability(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, d.dirMode); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	d.native = root
	return d.init(osRoot{root})
}

// AllowReservedNames lets keys use names Windows reserves (CON, PRN, AUX,
// NUL, COM1-COM9, LPT1-LPT9, and names ending in a dot or a space), which
// New refuses by default on every system so that a directory written on
// Linux can be copied to Windows. It has no effect on Windows, where such
// files cannot be created.
func AllowReservedNames() Option {
	return func(d *Driver) { d.allowReserved = runtime.GOOS != goosWindows }
}

// init finishes construction over a fileRoot; tests call it with a root
// that injects failures. It probes whether the filesystem folds letter
// case, which Capabilities then reports.
func (d *Driver) init(root fileRoot) (*Driver, error) {
	if d.stamp == nil {
		d.stamp = statStamp
	}
	if d.stampWait == 0 {
		d.stampWait = stampTimeout
	}
	if d.goos == "" {
		d.goos = runtime.GOOS
	}
	if err := d.checkDurability(); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := root.MkdirAll(tmpDir, d.dirMode); err != nil {
		_ = root.Close()
		return nil, err
	}
	insensitive, err := probeCase(root)
	if err == nil {
		d.root = root
		err = d.probeStamps()
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	d.caseInsensitive = insensitive
	return d, nil
}

// probeStamps reads the filesystem's clock through a staging file:
// whether this system has inodes and change times (stamps), and the
// change time every new file must pass, which is at least that of every
// file already in the directory.
func (d *Driver) probeStamps() error {
	now, ok, err := d.clock()
	if err != nil {
		return err
	}
	d.stamps = ok
	for i := range d.locks {
		d.locks[i].removed = now
	}
	return nil
}

// probeCase creates an upper-case staging file and looks for it under its
// lower-case name: found means the filesystem folds case (macOS and
// Windows by default).
func probeCase(root fileRoot) (bool, error) {
	name := tmpDir + "/" + randomName()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, DefaultFilePrivate)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Remove(name) }()
	if err := f.Close(); err != nil {
		return false, err
	}
	_, err = root.Stat(strings.ToLower(name))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// Close releases the directory handle.
func (d *Driver) Close() error { return d.root.Close() }

// Native returns the os.Root, for the filesystem.As escape hatch.
func (d *Driver) Native() *os.Root { return d.native }

// Capabilities reports what local disk does natively. Copy is not claimed:
// a local copy streams bytes exactly as the Disk's portable copy does.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Move:             true,
		SetVisibility:    true,
		DeletePrefix:     true,
		Multipart:        true,
		SweepAbandoned:   true,
		FindAbandoned:    true,
		Dirs:             true,
		ConditionalWrite: true,
		Append:           true,
		SetModTime:       true,
		CompareAndSwap:   d.stamps,
		CaseInsensitive:  d.caseInsensitive,
	}
}

// checkKey rejects keys inside the reserved staging directory.
func (d *Driver) checkKey(key string) error {
	if key == StagingDir || strings.HasPrefix(key, StagingDir+"/") {
		return filesystem.Errorf(filesystem.CodeInvalidKey, "keys under %q are reserved by the local driver", StagingDir)
	}
	return nil
}

// checkNewKey is checkKey for a key an operation may create (Write,
// Append, a Move's destination, a multipart upload): it also refuses,
// unless AllowReservedNames, a segment Windows reserves. Files that already
// have such names (copied in from elsewhere) stay readable, movable and
// deletable, so they can be renamed away.
func (d *Driver) checkNewKey(key string) error {
	if err := d.checkKey(key); err != nil {
		return err
	}
	if !d.allowReserved && hasReservedSegment(key) {
		return filesystem.Errorf(filesystem.CodeInvalidKey, "the key has a segment Windows reserves (a device name such as CON or COM1, or a trailing dot or space)")
	}
	return nil
}

// windowsDevices are the names Windows reserves for devices, whatever the
// extension ("con.txt" too).
var windowsDevices = []string{"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

// hasReservedSegment reports whether a segment of key is a Windows device
// name (before its first dot, any case) or ends in a dot or a space.
func hasReservedSegment(key string) bool {
	for seg := range strings.SplitSeq(key, "/") {
		base, _, _ := strings.Cut(seg, ".")
		if slices.Contains(windowsDevices, strings.ToUpper(strings.TrimRight(base, " "))) || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return true
		}
	}
	return false
}

func (d *Driver) entry(key string, info fs.FileInfo) filesystem.Entry {
	v := filesystem.VisibilityPrivate
	if info.Mode().Perm()&worldRead != 0 {
		v = filesystem.VisibilityPublic
	}
	return filesystem.Entry{
		Key:         key,
		Size:        info.Size(),
		ModTime:     info.ModTime(),
		ContentType: d.typeOf(key),
		ETag:        d.etag(info),
		Visibility:  v,
	}
}

// etag identifies a file's version by its modification time, change time,
// size and inode. Every change holds its key's lock and passes the change
// time of every earlier version of the key (see change.go) before it is
// made, whatever the timestamps' precision (a FAT second, an ext4 kernel
// tick), so a version's ETag never repeats and a stale IfMatch can never
// win. This holds for changes made through this driver in this process.
func (d *Driver) etag(info fs.FileInfo) string {
	ino, ctime, _ := d.stamp(info)
	return fmt.Sprintf("%x-%x-%x-%x", info.ModTime().UnixNano(), ctime, info.Size(), ino)
}

// Append implements filesystem.Appender. The body is staged first, so a
// failing body appends nothing, then copied onto the end of the file under
// the key's lock, so two appends in this process never interleave while
// changes to other keys go on. A missing file is created with the
// visibility in opts, committed like a write.
func (d *Driver) Append(ctx context.Context, key string, body io.Reader, opts filesystem.AppendOptions) (filesystem.Entry, error) {
	if err := firstErr(ctx.Err(), d.checkNewKey(key)); err != nil {
		return filesystem.Entry{}, err
	}
	tmp, err := d.stage(ctx, body, d.mode(opts.Visibility))
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer func() { _ = d.root.Remove(tmp) }()
	i := d.stripeOf(key)
	defer d.lock(i)()
	cur, err := d.current(key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if err := d.checkAppendSize(tmp, cur, opts.MaxSize); err != nil {
		return filesystem.Entry{}, err
	}
	if cur == nil {
		return d.replace(ctx, i, tmp, key, nil, false)
	}
	// The live file passes its own change time before a byte is added.
	if _, err := d.pass(ctx, key, d.floor(i, cur)); err != nil {
		return filesystem.Entry{}, err
	}
	if err := d.appendFile(tmp, key, d.mode(opts.Visibility)); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return d.entryOf(key)
}

// checkAppendSize refuses an append to cur (nil for a new file) that
// would take it past max (when positive). Callers hold the key's lock, so
// no other append grows it meanwhile.
func (d *Driver) checkAppendSize(tmp string, cur fs.FileInfo, max int64) error {
	if max <= 0 {
		return nil
	}
	staged, err := d.root.Stat(tmp)
	if err != nil {
		return err
	}
	var current int64
	if cur != nil {
		current = cur.Size()
	}
	if current+staged.Size() > max {
		return filesystem.Errorf(filesystem.CodeTooLarge, "the object would exceed %d bytes", max)
	}
	return nil
}

// appendFile copies the staged file onto the end of key, creating key with
// mode when it does not exist, and syncs it unless DurabilityNone.
func (d *Driver) appendFile(tmp, key string, mode fs.FileMode) error {
	src, err := d.root.Open(tmp)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := d.root.OpenFile(key, os.O_WRONLY|os.O_CREATE|os.O_APPEND, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if err == nil && d.syncsFiles() {
		err = dst.Sync()
	}
	return errors.Join(err, dst.Close())
}

// SetModTime implements filesystem.ModTimeSetter.
func (d *Driver) SetModTime(ctx context.Context, key string, t time.Time) error {
	if err := firstErr(ctx.Err(), d.checkKey(key)); err != nil {
		return err
	}
	return d.changeInPlace(ctx, key, func() error { return d.root.Chtimes(key, t, t) })
}

// changeInPlace makes a change to key's file that keeps its inode (its
// mode or times) under the key's lock, after the file has passed its own
// change time, so the new version's change time is later.
func (d *Driver) changeInPlace(ctx context.Context, key string, change func() error) error {
	i := d.stripeOf(key)
	defer d.lock(i)()
	cur, err := d.statFile(key)
	if err != nil {
		return err
	}
	if _, err := d.pass(ctx, key, d.floor(i, cur)); err != nil {
		return err
	}
	return change()
}

// statFile stats key and refuses anything that is not a regular file, so a
// directory never reads as an object.
func (d *Driver) statFile(key string) (fs.FileInfo, error) {
	info, err := d.root.Stat(key)
	if err != nil {
		return nil, mapErr(err)
	}
	if !info.Mode().IsRegular() {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "%q is not a file", key)
	}
	return info, nil
}

// mapErr classifies OS errors. "Not a directory" (a path component is a
// file) means the key cannot exist, so it is CodeNotFound.
func mapErr(err error) error {
	if errors.Is(err, syscall.ENOTDIR) {
		return filesystem.NewError(filesystem.CodeNotFound, err)
	}
	return err
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := firstErr(ctx.Err(), d.checkKey(key)); err != nil {
		return filesystem.Entry{}, err
	}
	info, err := d.statFile(key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return d.entry(key, info), nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// rangeFile is the reader Read returns: the open file limited to the range.
type rangeFile struct {
	io.Reader
	f file
}

func (r *rangeFile) Close() error { return r.f.Close() }

// Read implements filesystem.Driver.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if err := firstErr(ctx.Err(), d.checkKey(key)); err != nil {
		return nil, err
	}
	if _, err := d.statFile(key); err != nil {
		return nil, err
	}
	f, err := d.root.Open(key)
	if err != nil {
		return nil, mapErr(err)
	}
	if _, err := f.Seek(rng.Offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	var r io.Reader = f
	if rng.Length > 0 {
		r = io.LimitReader(f, rng.Length)
	}
	return &rangeFile{Reader: r, f: f}, nil
}

// Write implements filesystem.Driver: the body goes to a staging file that
// is renamed (or, for IfNotExists, hard-linked) into place.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	if err := firstErr(ctx.Err(), d.checkNewKey(key)); err != nil {
		return filesystem.Entry{}, err
	}
	tmp, err := d.stage(ctx, body, d.mode(opts.Visibility))
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer func() { _ = d.root.Remove(tmp) }()
	return d.commit(ctx, tmp, key, opts.IfNotExists, opts.IfMatch)
}

// mode maps a visibility to a file mode; an empty one is private.
func (d *Driver) mode(v filesystem.Visibility) fs.FileMode {
	if v == filesystem.VisibilityPublic {
		return d.filePublic
	}
	return d.filePrivate
}

// stage copies body into a new temporary file under tmpDir and returns its
// path. The file is synced (unless DurabilityNone) so a crash after the
// rename cannot leave an empty object. On failure the temporary file is
// removed.
func (d *Driver) stage(ctx context.Context, body io.Reader, mode fs.FileMode) (string, error) {
	tmp := tmpDir + "/" + randomName()
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, &ctxReader{ctx: ctx, r: body})
	if err == nil && d.syncsFiles() {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// OpenFile's mode is filtered by the umask; set it exactly.
		err = d.root.Chmod(tmp, mode)
	}
	if err != nil {
		_ = d.root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// commit moves a staged file to key under the key's lock. With ifMatch
// it first checks that key's ETag is still ifMatch, so the check and the
// rename are one step.
func (d *Driver) commit(ctx context.Context, tmp, key string, ifNotExists bool, ifMatch string) (filesystem.Entry, error) {
	i := d.stripeOf(key)
	defer d.lock(i)()
	cur, err := d.current(key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if ifMatch != "" && (cur == nil || d.etag(cur) != ifMatch) {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeConflict, "object changed since ETag %s", ifMatch)
	}
	return d.replace(ctx, i, tmp, key, cur, ifNotExists)
}

// replace makes the staged file tmp the object at key, whose current
// version is cur (nil for none), in stripe i: the staging file passes the
// change time of every earlier version first, so once it is in place
// nothing is left that can fail but reading its entry. With ifNotExists it
// is hard-linked, which fails atomically when key exists, and the staging
// name is removed before the entry is read: removing a link changes the
// object's change time, so done later it would leave the returned ETag
// stale. Callers hold the stripe's lock.
func (d *Driver) replace(ctx context.Context, i int, tmp, key string, cur fs.FileInfo, ifNotExists bool) (filesystem.Entry, error) {
	if _, err := d.pass(ctx, tmp, d.floor(i, cur)); err != nil {
		return filesystem.Entry{}, err
	}
	stop, err := d.syncStop(key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	err = d.place(key, func() error {
		if ifNotExists {
			return d.root.Link(tmp, key)
		}
		return d.root.Rename(tmp, key)
	})
	if errors.Is(err, fs.ErrExist) {
		return filesystem.Entry{}, filesystem.NewError(filesystem.CodeExists, err)
	}
	if err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	if ifNotExists {
		if err := d.root.Remove(tmp); err != nil {
			return filesystem.Entry{}, fmt.Errorf("the object was written, but its staging name could not be removed: %w", err)
		}
	}
	if err := d.syncDirs(key, stop); err != nil {
		return filesystem.Entry{}, err
	}
	return d.entryOf(key)
}

// entryOf reads key's entry after a change: the last step of every change
// made under a key's lock, since any later change to the file's inode
// (even unlinking another name of it) would make the entry's ETag stale.
func (d *Driver) entryOf(key string) (filesystem.Entry, error) {
	info, err := d.statFile(key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return d.entry(key, info), nil
}

// mkParents creates key's parent directories. When a parent path is
// already a file, the key cannot exist on a filesystem (see the package
// limitation), which is CodeInvalidArgument rather than a misleading
// CodeExists about the key itself.
func (d *Driver) mkParents(key string) error {
	dir := path.Dir(key)
	if dir == "." {
		return nil
	}
	return d.mkDirs(dir, key)
}

// mkDirs creates dir and its missing parents for name (a key, or dir
// itself for MakeDir), naming name when a parent is a file.
func (d *Driver) mkDirs(dir, name string) error {
	err := d.root.MkdirAll(dir, d.dirMode)
	if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "a parent of %q is a file, so it cannot be created", name)
	}
	return err
}

// randomName returns 128 random bits as 26 base32 characters (rand.Text),
// so staging names and upload IDs never collide. It cannot fail: crypto/rand
// aborts the process rather than return an error.
func randomName() string { return rand.Text() }

// ctxReader stops a long copy when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Delete implements filesystem.Driver and removes directories the delete
// left empty, so the tree mirrors the key space.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if err := firstErr(ctx.Err(), d.checkKey(key)); err != nil {
		return err
	}
	// Under the key's lock, so a delete cannot interleave with an append.
	i := d.stripeOf(key)
	defer d.lock(i)()
	info, err := d.root.Lstat(key)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	if err := d.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, ctime, _ := d.stamp(info)
	d.removed(i, ctime)
	d.pruneParents(key)
	return nil
}

// pruneParents removes empty directories from key's parent up to the root.
// Failures are ignored: a non-empty or concurrently re-filled directory is
// exactly the one that must stay.
func (d *Driver) pruneParents(key string) {
	for dir := path.Dir(key); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if err := d.root.Remove(dir); err != nil {
			return
		}
	}
}

// List implements filesystem.Driver by walking the directory. It is O(n) in
// the number of files under dir per page, which suits local development and
// moderate trees; large trees belong on an object store.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	if err := ctx.Err(); err != nil {
		return filesystem.Page{}, err
	}
	start := dir
	if start == "" {
		start = "."
	}
	var all []filesystem.Entry
	err := fs.WalkDir(d.root.FS(), start, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == start && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)) {
				return fs.SkipAll
			}
			return err
		}
		if de.IsDir() {
			if p == StagingDir {
				return fs.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() || !filesystem.KeyInDir(p, dir) {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		all = append(all, d.entry(p, info))
		return nil
	})
	if err != nil {
		return filesystem.Page{}, err
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// Move implements filesystem.Mover with an atomic rename, under both
// keys' locks; the file passes the change time of every earlier version of
// dst before it is renamed.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if err := firstErr(ctx.Err(), d.checkKey(src), d.checkNewKey(dst)); err != nil {
		return filesystem.Entry{}, err
	}
	si, di := d.stripeOf(src), d.stripeOf(dst)
	defer d.lock(si, di)()
	if _, err := d.statFile(src); err != nil {
		return filesystem.Entry{}, err
	}
	cur, err := d.current(dst)
	if err != nil {
		return filesystem.Entry{}, err
	}
	ctime, err := d.pass(ctx, src, d.floor(di, cur))
	if err != nil {
		return filesystem.Entry{}, err
	}
	stop, err := d.syncStop(dst)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if err := d.place(dst, func() error { return d.root.Rename(src, dst) }); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	d.removed(si, ctime)
	d.pruneParents(src)
	// Durable at dst; the source's directory is not synced (it may just
	// have been pruned), so a crash can bring the source name back too.
	if err := d.syncDirs(dst, stop); err != nil {
		return filesystem.Entry{}, err
	}
	return d.entryOf(dst)
}

// SetVisibility implements filesystem.VisibilitySetter by changing the mode.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	if err := firstErr(ctx.Err(), d.checkKey(key)); err != nil {
		return err
	}
	return d.changeInPlace(ctx, key, func() error { return d.root.Chmod(key, d.mode(v)) })
}

// DeletePrefix implements filesystem.PrefixDeleter with one RemoveAll.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	if err := firstErr(ctx.Err(), d.checkKey(dir)); err != nil {
		return err
	}
	// Under every key's lock, so a delete cannot interleave with an append.
	defer d.lockAll()()
	info, err := d.root.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	// Read before the removal, the clock is no earlier than any removed
	// file's change time: every stripe's new versions must pass it.
	now, _, err := d.clock()
	if err != nil {
		return err
	}
	if err := d.root.RemoveAll(dir); err != nil {
		return err
	}
	for i := range d.locks {
		d.removed(i, now)
	}
	d.pruneParents(dir)
	return nil
}
