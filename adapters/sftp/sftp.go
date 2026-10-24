// Package sftp is a filesystem.Driver over SFTP, for the servers, NAS
// boxes and partner drop folders many businesses still exchange files on.
//
// It is a separate module (it depends on github.com/pkg/sftp and
// golang.org/x/crypto/ssh). Writes are atomic: bytes go to a temporary file
// under a reserved staging directory and are moved into place with the
// posix-rename@openssh.com extension, which every OpenSSH server offers.
// A server without it is refused unless Config.AllowNonAtomicWrites opts
// into a weaker replace (see there). A write-only drop folder that refuses
// mkdir inside Root can stage elsewhere (Config.StagingPath) or skip
// staging (Config.WriteInPlace, with its weaker guarantee).
// Conditional writes use hard links (hardlink@openssh.com) and are claimed
// only when the server offers that extension. Visibility is the file mode,
// as on local disk.
//
// Confinement: SFTP has no equivalent of os.Root, so a symlink created on
// the server can point outside Root. Keys are cleaned and traversal is
// refused, but containment against server-side symlinks is the server's
// job: use an account chrooted to Root (sshd's ChrootDirectory).
//
// A lost connection is CodeUnavailable (see classify), so layer.Retry and
// fshttp treat it as the transient failure it is. With Config.Dial (the
// Opener always sets it) the driver reconnects: the call that finds the
// connection gone dials a new one, re-checks the server's extensions and
// swaps it in under a lock, so concurrent calls share one reconnect. A
// call in flight when the connection drops still fails; the driver never
// repeats an operation on the caller's behalf, since a write's body is
// already consumed and a rename may have landed. Without Dial a dropped
// connection fails every call until the disk is opened again.
package sftp

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"

	"github.com/ubgo/filesystem"
)

// StagingDir is the default reserved directory, under Root, for temporary
// files (Config.StagingDir names another). Keys inside the reserved
// directory are refused and listings skip it.
const StagingDir = ".filesystem"

// tmpSub is the directory, inside the staging directory, that holds staged
// files; SweepAbandoned looks only there.
const tmpSub = "tmp"

// stagedName is the shape of the temporary file names Write gives staged
// files (lower-cased crypto/rand.Text). SweepAbandoned removes only files
// of this shape, so nothing else placed in tmpDir is ever touched.
var stagedName = regexp.MustCompile(`^[a-z2-7]{26}$`)

// SFTP protocol extensions the driver uses.
const (
	extPosixRename = "posix-rename@openssh.com"
	extHardlink    = "hardlink@openssh.com"
)

// Default file modes; public files are world-readable. With these
// defaults a file reads as public when its mode has the world-read bit,
// whoever wrote it (see Config.FilePublic for custom modes).
const (
	DefaultFilePublic  fs.FileMode = 0o644
	DefaultFilePrivate fs.FileMode = 0o600
	worldRead          fs.FileMode = 0o004
)

// Dialer opens a new connection to the server: a connected SFTP client and
// the closer that ends the whole connection, client included (the opener
// returns one closing the client, then the SSH connection). With a nil
// closer the driver closes the client alone. The driver calls it for the first connection when
// Config.Client is nil and again whenever the current connection has
// dropped. ctx is the context of the operation that found the connection
// gone, so a dial is bounded by that operation's deadline. Errors it
// returns keep their filesystem.Code; an error without one is reported as
// CodeUnavailable.
type Dialer func(ctx context.Context) (*sftp.Client, io.Closer, error)

// Config configures a Driver.
type Config struct {
	// Client is a connected SFTP client. Required unless Dial is set. The
	// driver does not close it unless Closer is set.
	Client *sftp.Client
	// Root is the absolute directory on the server holding the disk.
	// Required; created if missing.
	Root string
	// Closer, when set, is closed by Close, or when Client's connection has
	// dropped and Dial replaced it.
	Closer io.Closer
	// Dial, when set, makes the driver reconnect after a dropped
	// connection (see Dialer). With Client nil, New dials the first
	// connection too. Connections it returns are closed by the driver.
	Dial Dialer

	// AllowNonAtomicWrites lets the driver work with a server that lacks
	// posix-rename@openssh.com (some appliances and non-OpenSSH servers),
	// which New otherwise refuses with CodeUnsupported. Off by default,
	// because it gives up atomic replacement: without the extension a
	// plain SFTP rename refuses an existing target, so Write and Move then
	// remove the old file and rename the new one in two steps. Between the
	// two, a reader finds no file at the key, and a connection lost there
	// leaves the key empty (the new bytes stay staged until
	// SweepAbandoned removes them). A new key is still written atomically.
	// Servers that offer the extension use it whatever this says.
	AllowNonAtomicWrites bool
	// StagingDir names the reserved directory, directly under Root, that
	// holds staged files; empty means the package's StagingDir
	// (".filesystem"). Set it when that name collides with something on
	// the server, or a partner's poller must ignore it under a name of its
	// own. It must be one path segment (no "/", not "." or ".."): keys
	// under it are refused and listings skip it, and changing it on a
	// disk with staged files leaves those for the old name.
	StagingDir string
	// StagingPath puts staged files in a directory outside Root, an
	// absolute path on the server, instead of the reserved StagingDir
	// under it: for a write-only drop folder (a bank or EDI inbox, a
	// partner's upload box) whose server refuses mkdir inside Root, or a
	// poller that must never see anything but finished files there.
	// Writes stay atomic: bytes are staged there and renamed into Root,
	// so it must be on the same filesystem as Root (a rename cannot cross
	// devices), and it must not be Root, inside it, or contain it. Created
	// if missing; New probes letter case there, taking Root to share its
	// filesystem. No key is reserved and listings skip nothing. Exclusive
	// with StagingDir and WriteInPlace.
	StagingPath string
	// WriteInPlace writes each file straight to its key, with no staging
	// directory at all, for a drop folder where nothing outside the keys
	// may be created either. It needs AllowNonAtomicWrites, because it
	// gives up what staging buys: a reader or the server's poller can see
	// a file while its bytes are still arriving, an overwrite truncates
	// the old content first, and a failed write removes the file, so a
	// failure while replacing a key leaves the key empty. IfNotExists
	// stays atomic (an exclusive create, so ConditionalWrite is declared
	// without hardlink@openssh.com). New creates nothing, so Root must
	// exist, and there is nothing to sweep: SweepAbandoned and
	// FindAbandoned are not declared. The server's letter case cannot be
	// probed without writing into Root, so the disk reports it
	// case-sensitive. Exclusive with StagingDir and StagingPath.
	WriteInPlace bool
	// FilePublic and FilePrivate are the modes Write and SetVisibility
	// give files; zero means DefaultFilePublic or DefaultFilePrivate.
	// With either set, a file reads as public when its mode has any bit
	// FilePublic has and FilePrivate lacks (0o660 and 0o600 make group
	// read or write mean public), so New refuses a pair without such a bit.
	FilePublic, FilePrivate fs.FileMode
	// SkipChmod makes writes keep the mode the server gives new files
	// (its umask) instead of setting one, for servers that refuse SETSTAT
	// and so fail every write. The driver then cannot set visibility: it
	// declares no SetVisibility capability (so the Disk refuses a
	// WriteOptions.Visibility other than its default), and reports each
	// file's visibility from the mode the server chose.
	SkipChmod bool
}

// session is one connection to the server and what was learned from it.
// The driver replaces a session whole, never mutates one in place.
type session struct {
	c      *sftp.Client
	closer io.Closer
	// hardlink is whether this server offered hardlink@openssh.com.
	hardlink bool
	// posixRename is whether this server offered posix-rename@openssh.com;
	// false only under Config.AllowNonAtomicWrites.
	posixRename bool
	// dead is closed when the connection is known to be gone: by a watcher
	// on the client's Wait, or by an operation that saw the loss first.
	dead     chan struct{}
	deadOnce sync.Once
}

// newSession checks the extensions the driver depends on and watches the
// connection. A server without posix-rename is refused unless nonAtomic
// (Config.AllowNonAtomicWrites). The caller closes c and closer when it
// fails.
func newSession(c *sftp.Client, closer io.Closer, nonAtomic bool) (*session, error) {
	_, posixRename := c.HasExtension(extPosixRename)
	if !posixRename && !nonAtomic {
		return nil, filesystem.Errorf(filesystem.CodeUnsupported, "sftp: server lacks %s, needed for atomic writes (see Config.AllowNonAtomicWrites)", extPosixRename)
	}
	_, hardlink := c.HasExtension(extHardlink)
	s := &session{c: c, closer: closer, hardlink: hardlink, posixRename: posixRename, dead: make(chan struct{})}
	go func() {
		_ = c.Wait()
		s.kill()
	}()
	return s, nil
}

func (s *session) kill() { s.deadOnce.Do(func() { close(s.dead) }) }

func (s *session) alive() bool {
	select {
	case <-s.dead:
		return false
	default:
		return true
	}
}

func (s *session) close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// Driver is the SFTP backend. Create it with New; it is safe for concurrent
// use (pkg/sftp clients are, and the connection is swapped under mu).
type Driver struct {
	root string
	dial Dialer
	// hardlink is ConditionalWrite as declared at New. Capabilities are
	// fixed for the Disk's lifetime, so a reconnect to a server without
	// the extension makes conditional writes CodeUnsupported instead.
	hardlink bool
	// caseInsensitive is what New's probe found about Root's filesystem.
	caseInsensitive bool
	// nonAtomic is Config.AllowNonAtomicWrites, applied to every session.
	nonAtomic bool
	// staging is the reserved directory's name under Root (Config.StagingDir
	// or StagingDir), empty when nothing under Root is reserved
	// (Config.StagingPath or WriteInPlace). tmpDir is the absolute server
	// path staged files go to, empty under WriteInPlace.
	staging, tmpDir string
	// inPlace is Config.WriteInPlace.
	inPlace bool
	// filePublic and filePrivate are the modes visibility maps to, and
	// publicBits the bits that make a file read as public.
	filePublic, filePrivate, publicBits fs.FileMode
	// skipChmod is Config.SkipChmod.
	skipChmod bool

	// mu guards cur and closed, and is only ever held briefly, so Close,
	// Native and calls on a live connection never wait behind a dial.
	mu     sync.Mutex
	cur    *session
	closed bool
	// redial is a one-slot semaphore held by the call that is
	// reconnecting. Other calls wait for it in a select on their own
	// context, so a stuck dial never holds them past their deadline.
	redial chan struct{}
}

// New connects (through Dial when Client is nil), checks the server's
// extensions and prepares Root.
func New(cfg Config) (*Driver, error) {
	if (cfg.Client == nil && cfg.Dial == nil) || !path.IsAbs(cfg.Root) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp: Client or Dial, and an absolute Root, are required")
	}
	root := path.Clean(cfg.Root)
	staging, tmpDir, err := stagingPlace(cfg, root)
	if err != nil {
		return nil, err
	}
	pub, priv, bits, err := fileModes(cfg.FilePublic, cfg.FilePrivate)
	if err != nil {
		return nil, err
	}
	d := &Driver{
		root: root, dial: cfg.Dial, redial: make(chan struct{}, 1),
		nonAtomic: cfg.AllowNonAtomicWrites, staging: staging, tmpDir: tmpDir, inPlace: cfg.WriteInPlace,
		filePublic: pub, filePrivate: priv, publicBits: bits, skipChmod: cfg.SkipChmod,
	}
	var s *session
	if cfg.Client != nil {
		s, err = newSession(cfg.Client, cfg.Closer, d.nonAtomic)
	} else {
		s, err = d.connect(context.Background())
	}
	if err != nil {
		return nil, err
	}
	d.cur, d.hardlink = s, s.hardlink
	if d.inPlace {
		return d, nil // nothing to create or probe (see Config.WriteInPlace)
	}
	if err := s.c.MkdirAll(d.tmpDir); err != nil {
		if cfg.Client == nil {
			_ = s.close() // dialled here, so ours to close
		}
		return nil, err
	}
	insensitive, err := probeCase(s.c, d.tmpDir)
	if err != nil {
		if cfg.Client == nil {
			_ = s.close()
		}
		return nil, err
	}
	d.caseInsensitive = insensitive
	return d, nil
}

// stagingPlace checks the staging fields of cfg and returns the name
// reserved under root (empty when none is) and the absolute directory
// staged files go to (empty under WriteInPlace).
func stagingPlace(cfg Config, root string) (staging, tmpDir string, err error) {
	switch {
	case cfg.WriteInPlace:
		if !cfg.AllowNonAtomicWrites || cfg.StagingDir != "" || cfg.StagingPath != "" {
			return "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp: WriteInPlace needs AllowNonAtomicWrites and excludes StagingDir and StagingPath")
		}
		return "", "", nil
	case cfg.StagingPath != "":
		p := path.Clean(cfg.StagingPath)
		if cfg.StagingDir != "" || !path.IsAbs(p) || within(p, root) || within(root, p) {
			return "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp: StagingPath %q must be absolute, outside Root and not containing it, and excludes StagingDir", cfg.StagingPath)
		}
		return "", p, nil
	}
	staging, err = stagingName(cfg.StagingDir)
	if err != nil {
		return "", "", err
	}
	return staging, path.Join(root, staging, tmpSub), nil
}

// within reports whether the clean absolute path p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// stagingName validates Config.StagingDir: empty means StagingDir, and a
// name must be one path segment, because keys under it are refused and
// listings skip it by its name directly under Root.
func stagingName(name string) (string, error) {
	if name == "" {
		return StagingDir, nil
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp: StagingDir %q must be one path segment", name)
	}
	return name, nil
}

// fileModes applies the defaults to Config.FilePublic and FilePrivate and
// returns the bits that make a file read as public: the world-read bit
// with the defaults (unchanged from before the modes were configurable),
// otherwise what public has and private lacks, which must not be empty or
// visibility could not be read back.
func fileModes(pub, priv fs.FileMode) (fs.FileMode, fs.FileMode, fs.FileMode, error) {
	if pub == 0 && priv == 0 {
		return DefaultFilePublic, DefaultFilePrivate, worldRead, nil
	}
	pub, priv = cmp.Or(pub, DefaultFilePublic).Perm(), cmp.Or(priv, DefaultFilePrivate).Perm()
	bits := pub &^ priv
	if bits == 0 {
		return 0, 0, 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp: FilePublic %o has no permission bit FilePrivate %o lacks, so visibility could not be read back", pub, priv)
	}
	return pub, priv, bits, nil
}

// probeCase creates a staged-shape (lower-case) file in dir and looks for
// it under its upper-case name: found means the server's filesystem folds
// case (macOS APFS, Windows, many NAS shares), which SFTP cannot report
// any other way. The name matches stagedName, so a probe file left behind
// by a lost connection is removed by SweepAbandoned like any staged file.
// A read-only account cannot create the file; it is reported
// case-sensitive, since it cannot write two keys that would collide.
func probeCase(c *sftp.Client, dir string) (bool, error) {
	name := strings.ToLower(rand.Text())
	f, err := c.OpenFile(path.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if errors.Is(err, fs.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Remove(path.Join(dir, name)) }()
	if err := f.Close(); err != nil {
		return false, err
	}
	_, err = c.Lstat(path.Join(dir, strings.ToUpper(name)))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// connect dials a new session, closing the connection when the server
// fails the extension check.
func (d *Driver) connect(ctx context.Context) (*session, error) {
	c, closer, err := d.dial(ctx)
	if err != nil {
		if filesystem.CodeOf(err) == filesystem.CodeUnknown {
			err = filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		return nil, err
	}
	if closer == nil {
		closer = c
	}
	s, err := newSession(c, closer, d.nonAtomic)
	if err != nil {
		_ = closer.Close()
		return nil, err
	}
	return s, nil
}

// current returns the session and whether the driver is closed.
func (d *Driver) current() (*session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur, d.closed
}

// session returns the connection an operation uses, reconnecting first
// when it has dropped and Dial is set. One call dials at a time (redial);
// the others wait for it, but only as long as their own ctx allows, then
// return ctx's error. Each re-checks after its wait, so concurrent callers
// share one reconnect. A failed reconnect leaves the dead session in
// place, so the next call tries again. After Close the driver never dials,
// and a dial that finishes after Close is closed at once.
func (d *Driver) session(ctx context.Context) (*session, error) {
	s, closed := d.current()
	if s.alive() || d.dial == nil || closed {
		return s, nil
	}
	select {
	case d.redial <- struct{}{}:
	case <-ctx.Done():
		return nil, filesystem.NewError(filesystem.CodeUnavailable, ctx.Err())
	}
	defer func() { <-d.redial }()
	if s, closed = d.current(); s.alive() || closed {
		return s, nil
	}
	ns, err := d.connect(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		_ = ns.close()
		return s, nil
	}
	old := d.cur
	d.cur = ns
	d.mu.Unlock()
	_ = old.close()
	return ns, nil
}

// Native returns the current SFTP client, for the filesystem.As escape
// hatch. With Dial set, a reconnect replaces it: do not hold it across
// operations.
func (d *Driver) Native() *sftp.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur.c
}

// Close closes the current connection's closer (Config.Closer, or what
// Dial returned) and stops reconnecting. It never waits for a dial in
// progress (that dial's connection is closed when it finishes), and a
// second Close does nothing.
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	s := d.cur
	d.mu.Unlock()
	return s.close()
}

// Capabilities reports what the server supports.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Move:             true,
		SetVisibility:    !d.skipChmod,
		DeletePrefix:     true,
		SweepAbandoned:   !d.inPlace,
		FindAbandoned:    !d.inPlace,
		Dirs:             true,
		ConditionalWrite: d.hardlink || d.inPlace,
		CaseInsensitive:  d.caseInsensitive,
	}
}

func (d *Driver) full(key string) string { return path.Join(d.root, key) }

func (d *Driver) checkKey(key string) error {
	if d.staging != "" && (key == d.staging || strings.HasPrefix(key, d.staging+"/")) {
		return filesystem.Errorf(filesystem.CodeInvalidKey, "sftp: keys under %q are reserved", d.staging)
	}
	return nil
}

// begin starts an operation: the context and keys are checked first, then
// the session is fetched (reconnecting if needed). The caller defers
// s.classify on its result.
func (d *Driver) begin(ctx context.Context, keys ...string) (*session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, k := range keys {
		if err := d.checkKey(k); err != nil {
			return nil, err
		}
	}
	return d.session(ctx)
}

func (d *Driver) entry(key string, info fs.FileInfo) filesystem.Entry {
	v := filesystem.VisibilityPrivate
	if info.Mode().Perm()&d.publicBits != 0 {
		v = filesystem.VisibilityPublic
	}
	return filesystem.Entry{
		Key:         key,
		Size:        info.Size(),
		ModTime:     info.ModTime(),
		ContentType: mime.TypeByExtension(path.Ext(key)),
		Visibility:  v,
	}
}

// statFile Lstats key and refuses anything but a regular file, so neither
// a directory nor a symlink ever reads as an object.
func (d *Driver) statFile(s *session, key string) (fs.FileInfo, error) {
	info, err := s.c.Lstat(d.full(key))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "sftp: %q is not a file", key)
	}
	return info, nil
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (_ filesystem.Entry, err error) {
	s, err := d.begin(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer s.classify(&err)
	return d.stat(s, key)
}

func (d *Driver) stat(s *session, key string) (filesystem.Entry, error) {
	info, err := d.statFile(s, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return d.entry(key, info), nil
}

type rangeFile struct {
	io.Reader
	f *sftp.File
}

func (r *rangeFile) Close() error { return r.f.Close() }

// Read implements filesystem.Driver.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (_ io.ReadCloser, err error) {
	s, err := d.begin(ctx, key)
	if err != nil {
		return nil, err
	}
	defer s.classify(&err)
	if _, err := d.statFile(s, key); err != nil {
		return nil, err
	}
	f, err := s.c.Open(d.full(key))
	if err != nil {
		return nil, err
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

// mode is the file mode for v; zero under SkipChmod, which stage reads as
// "leave the server's mode".
func (d *Driver) mode(v filesystem.Visibility) fs.FileMode {
	switch {
	case d.skipChmod:
		return 0
	case v == filesystem.VisibilityPublic:
		return d.filePublic
	}
	return d.filePrivate
}

// replace moves the file at src onto dst, replacing any file there: one
// posix-rename when the server offers it (atomic), otherwise a plain
// rename, which SFTP servers refuse onto an existing file, retried after
// removing dst (Config.AllowNonAtomicWrites documents the gap between the
// two steps).
func (d *Driver) replace(s *session, src, dst string) error {
	if s.posixRename {
		return s.c.PosixRename(src, dst)
	}
	err := s.c.Rename(src, dst)
	if err == nil {
		return nil
	}
	if _, serr := s.c.Lstat(dst); serr != nil {
		return err // nothing in the way: the rename failed for its own reason
	}
	if err := s.c.Remove(dst); err != nil {
		return err
	}
	return s.c.Rename(src, dst)
}

// Write implements filesystem.Driver: stage, then rename (or hard-link for
// IfNotExists) into place.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (_ filesystem.Entry, err error) {
	s, err := d.begin(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer s.classify(&err)
	if d.inPlace {
		return d.writeInPlace(ctx, s, key, body, opts)
	}
	if opts.IfNotExists && !s.hardlink {
		// The server reconnected to no longer offers hard links, though
		// ConditionalWrite was declared when the disk opened.
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnsupported, "sftp: server no longer offers %s, needed for conditional writes", extHardlink)
	}
	tmp := path.Join(d.tmpDir, strings.ToLower(rand.Text())) // see stagedName
	if err := stage(ctx, s.c, tmp, os.O_EXCL, body, d.mode(opts.Visibility)); err != nil {
		return filesystem.Entry{}, err
	}
	defer func() { _ = s.c.Remove(tmp) }()
	if err := d.mkParents(s, key); err != nil {
		return filesystem.Entry{}, err
	}
	target := d.full(key)
	if opts.IfNotExists {
		if err := s.c.Link(tmp, target); err != nil {
			if _, serr := s.c.Lstat(target); serr == nil {
				return filesystem.Entry{}, filesystem.NewError(filesystem.CodeExists, err)
			}
			return filesystem.Entry{}, err
		}
	} else if err := d.replace(s, tmp, target); err != nil {
		return filesystem.Entry{}, err
	}
	return d.stat(s, key)
}

// writeInPlace is Write under Config.WriteInPlace: the bytes go straight
// to the key, created exclusively for IfNotExists (CodeExists when a file
// is there) and truncated otherwise. A failure removes the file.
func (d *Driver) writeInPlace(ctx context.Context, s *session, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	if err := d.mkParents(s, key); err != nil {
		return filesystem.Entry{}, err
	}
	target, flag := d.full(key), os.O_TRUNC
	if opts.IfNotExists {
		flag = os.O_EXCL
	}
	if err := stage(ctx, s.c, target, flag, body, d.mode(opts.Visibility)); err != nil {
		// stage removes a file it created, so one still there after an
		// exclusive create failed is another writer's.
		if _, serr := s.c.Lstat(target); opts.IfNotExists && serr == nil {
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeExists, err)
		}
		return filesystem.Entry{}, err
	}
	return d.stat(s, key)
}

// stage writes body to the file name, opened with O_WRONLY|O_CREATE|flag
// (O_EXCL for a staged file), with mode (zero leaves the server's mode),
// removing it on any failure after it was opened.
func stage(ctx context.Context, c *sftp.Client, name string, flag int, body io.Reader, mode fs.FileMode) error {
	f, err := c.OpenFile(name, os.O_WRONLY|os.O_CREATE|flag)
	if err != nil {
		return err
	}
	src := &ctxReader{ctx: ctx, r: body}
	_, err = io.Copy(f, src)
	// pkg/sftp's ReadFrom ends the copy without an error when the body
	// fails with io.ErrUnexpectedEOF (it reads with io.ReadFull), which
	// would store a truncated upload as complete; the body's own failure
	// is therefore taken from src, not from the copy.
	if src.err != nil {
		err = src.err
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && mode != 0 {
		err = c.Chmod(name, mode)
	}
	if err != nil {
		_ = c.Remove(name)
	}
	return err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
	// err is the first failure Read returned other than io.EOF, kept
	// because the copy may swallow it (see stage).
	err error
}

// Read stops at a cancelled context and marks the body's own failures as
// bodyError, so classify never reads a client's broken upload stream as
// a lost server connection.
func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		c.err = cmp.Or(c.err, err)
		return 0, err
	}
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		err = bodyError{err}
		c.err = cmp.Or(c.err, err)
	}
	return n, err
}

// bodyError is a failure reading the caller's body during Write. It
// unwraps to the original, so errors.Is and filesystem.CodeOf see through
// it.
type bodyError struct{ err error }

func (b bodyError) Error() string { return b.err.Error() }
func (b bodyError) Unwrap() error { return b.err }

// classify is classify for an operation on s, and marks s dead when the
// error means its connection is gone, so the next operation reconnects
// without waiting for the client's own watcher to notice.
func (s *session) classify(err *error) {
	if *err != nil && !errors.As(*err, new(bodyError)) && connectionLost(*err) {
		s.kill()
	}
	classify(err)
}

// classify turns a lost connection into CodeUnavailable, in place. pkg/sftp
// reports one as ErrSSHFxConnectionLost to calls in flight when it drops,
// and as the error that ended its connection (io.EOF, a closed pipe, a
// net error) to every call after; the server can also answer with the
// connection-lost or no-connection status codes. Errors already
// classified, and the caller's own body failures, are left alone.
func classify(err *error) {
	var fe *filesystem.Error
	if *err == nil || errors.As(*err, &fe) || errors.As(*err, new(bodyError)) {
		return
	}
	if connectionLost(*err) {
		*err = filesystem.NewError(filesystem.CodeUnavailable, *err)
	}
}

// connectionLost reports an error that means the connection is gone. It
// matches *net.OpError rather than the net.Error interface, which
// syscall.Errno also implements (an ENOTDIR from the server is not a
// network failure).
func connectionLost(err error) bool {
	var ne *net.OpError
	return errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, sftp.ErrSSHFxNoConnection) ||
		statusLost(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.As(err, &ne)
}

// statusLost reports a server status of connection lost or no connection.
func statusLost(err error) bool {
	var se *sftp.StatusError
	if !errors.As(err, &se) {
		return false
	}
	c := se.FxCode()
	return c == sftp.ErrSSHFxConnectionLost || c == sftp.ErrSSHFxNoConnection
}

// mkParents creates key's parent directories; a parent that is a file
// makes the key impossible (CodeInvalidArgument). A lost connection is
// left for classify, not blamed on the key.
func (d *Driver) mkParents(s *session, key string) error {
	dir := path.Dir(key)
	if dir == "." {
		return nil
	}
	return d.mkdirAll(s, dir)
}

// mkdirAll creates dir and its parents; a file on the way makes dir
// impossible (CodeInvalidArgument), and a lost connection is left for
// classify.
func (d *Driver) mkdirAll(s *session, dir string) error {
	if err := s.c.MkdirAll(d.full(dir)); err != nil {
		if connectionLost(err) {
			return err
		}
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return nil
}

// pruneParents removes directories left empty, up to Root. Failure means
// the directory is not empty, which is where pruning must stop.
func (d *Driver) pruneParents(s *session, key string) {
	for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
		if err := s.c.RemoveDirectory(d.full(dir)); err != nil {
			return
		}
	}
}

// Delete implements filesystem.Driver.
func (d *Driver) Delete(ctx context.Context, key string) (err error) {
	s, err := d.begin(ctx, key)
	if err != nil {
		return err
	}
	defer s.classify(&err)
	info, err := s.c.Lstat(d.full(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	if err := s.c.Remove(d.full(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d.pruneParents(s, key)
	return nil
}

// List implements filesystem.Driver by walking the directory (O(n) in the
// files under dir per page, like the local driver).
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (_ filesystem.Page, err error) {
	s, err := d.begin(ctx)
	if err != nil {
		return filesystem.Page{}, err
	}
	defer s.classify(&err)
	var all []filesystem.Entry
	w := s.c.Walk(d.full(dir))
	for w.Step() {
		if err := w.Err(); err != nil {
			if w.Path() == d.full(dir) && errors.Is(err, fs.ErrNotExist) {
				break
			}
			return filesystem.Page{}, err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(w.Path(), d.root), "/")
		info := w.Stat()
		if info.IsDir() {
			if d.staging != "" && rel == d.staging {
				w.SkipDir()
			}
			continue
		}
		if info.Mode().IsRegular() && filesystem.KeyInDir(rel, dir) {
			all = append(all, d.entry(rel, info))
		}
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// Move implements filesystem.Mover with an atomic rename (two steps over an
// existing dst under Config.AllowNonAtomicWrites).
func (d *Driver) Move(ctx context.Context, src, dst string) (_ filesystem.Entry, err error) {
	s, err := d.begin(ctx, src, dst)
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer s.classify(&err)
	if _, err := d.statFile(s, src); err != nil {
		return filesystem.Entry{}, err
	}
	if err := d.mkParents(s, dst); err != nil {
		return filesystem.Entry{}, err
	}
	if err := d.replace(s, d.full(src), d.full(dst)); err != nil {
		return filesystem.Entry{}, err
	}
	d.pruneParents(s, src)
	return d.stat(s, dst)
}

// SetVisibility implements filesystem.VisibilitySetter with chmod.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) (err error) {
	s, err := d.begin(ctx, key)
	if err != nil {
		return err
	}
	defer s.classify(&err)
	if d.skipChmod {
		return filesystem.Errorf(filesystem.CodeUnsupported, "sftp: SkipChmod is set, so visibility cannot be set")
	}
	if _, err := d.statFile(s, key); err != nil {
		return err
	}
	return s.c.Chmod(d.full(key), d.mode(v))
}

// DeletePrefix implements filesystem.PrefixDeleter.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) (err error) {
	s, err := d.begin(ctx, dir)
	if err != nil {
		return err
	}
	defer s.classify(&err)
	info, err := s.c.Lstat(d.full(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if err := s.c.RemoveAll(d.full(dir)); err != nil {
		return err
	}
	d.pruneParents(s, dir)
	return nil
}

// SweepAbandoned implements filesystem.AbandonedSweeper: it removes staged
// files a Write left in the staging directory's tmp when the process or the connection
// died between staging and the rename. A staged file's last activity is its
// modification time on the server, which advances while bytes arrive, so a
// slow upload still in progress is kept as long as before is older than its
// last chunk. Only regular files named the way Write names them are
// removed; stored files are never touched. The driver has no multipart, so
// staged files are all there is to sweep.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (n int, err error) {
	s, err := d.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer s.classify(&err)
	err = d.scanAbandoned(ctx, s, before, func(info fs.FileInfo) error {
		err := s.c.Remove(path.Join(d.tmpDir, info.Name()))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil // renamed or removed by its write meanwhile
		case err != nil:
			return err
		}
		n++
		return nil
	})
	return n, err
}

// FindAbandoned implements filesystem.AbandonedFinder: the staged files
// SweepAbandoned would remove, found by the same scan, removing nothing.
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) (_ []filesystem.Abandoned, err error) {
	s, err := d.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer s.classify(&err)
	var found []filesystem.Abandoned
	err = d.scanAbandoned(ctx, s, before, func(info fs.FileInfo) error {
		found = append(found, filesystem.Abandoned{Kind: filesystem.AbandonedStaging, ID: info.Name(), LastActivity: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// scanAbandoned calls visit for every staged file last modified before
// before: regular files named the way Write names them, nothing else. An
// error from visit ends the scan. It is the one definition of an abandoned
// staged file both SweepAbandoned and FindAbandoned use.
func (d *Driver) scanAbandoned(ctx context.Context, s *session, before time.Time, visit func(fs.FileInfo) error) error {
	if d.inPlace {
		return filesystem.Errorf(filesystem.CodeUnsupported, "sftp: WriteInPlace stages nothing, so there is nothing to sweep")
	}
	entries, err := s.c.ReadDir(d.tmpDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, info := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !stagedName.MatchString(info.Name()) || !info.ModTime().Before(before) {
			continue
		}
		if err := visit(info); err != nil {
			return err
		}
	}
	return nil
}

// MakeDir implements filesystem.DirMaker: it creates dir and any missing
// parents on the server. A file at dir is CodeExists; a file at one of its
// parents makes dir impossible (CodeInvalidArgument). Like the local
// driver, deleting the last file under a directory removes it again, and
// DeletePrefix removes it with everything below.
func (d *Driver) MakeDir(ctx context.Context, dir string) (err error) {
	s, err := d.begin(ctx, dir)
	if err != nil {
		return err
	}
	defer s.classify(&err)
	info, err := s.c.Lstat(d.full(dir))
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return filesystem.Errorf(filesystem.CodeExists, "sftp: %q is a file, not a directory", dir)
	}
	return d.mkdirAll(s, dir)
}

// DirExists implements filesystem.DirMaker: whether dir is a directory on
// the server. A file at dir, or a path through a file, is not one.
func (d *Driver) DirExists(ctx context.Context, dir string) (_ bool, err error) {
	s, err := d.begin(ctx, dir)
	if err != nil {
		return false, err
	}
	defer s.classify(&err)
	info, err := s.c.Lstat(d.full(dir))
	if err == nil {
		return info.IsDir(), nil
	}
	// No such file, or a path through a file, which OpenSSH and pkg/sftp
	// servers answer with the generic failure status (ENOTDIR): no
	// directory. Anything else (permission denied, a lost connection) is
	// an error, never a false.
	var se *sftp.StatusError
	if errors.Is(err, fs.ErrNotExist) || (errors.As(err, &se) && se.FxCode() == sftp.ErrSSHFxFailure) {
		return false, nil
	}
	return false, err
}
