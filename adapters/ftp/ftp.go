// Package ftp is a filesystem.Driver over FTP and FTPS, for the hosting
// accounts, printers, cameras, mainframe gateways and partner drop boxes
// that still speak nothing else. It is a separate module on
// github.com/jlaffaye/ftp.
//
// Writes are atomic on servers whose rename replaces a file (every POSIX
// server: vsftpd, ProFTPD, Pure-FTPd): bytes are stored under a temporary
// name in a reserved staging directory and renamed into place (RNFR/RNTO),
// so a reader never sees a half-written object. A server that refuses to
// rename over an existing file (IIS) gets the old file moved aside into the
// staging directory, the new one renamed in, and the old one deleted, with
// a moment where the key is missing; a failure rolls the old file back, and
// if the rollback fails too, both contents are kept and named by a
// *StrandedError. Write says so in its doc. A write-only drop folder that
// refuses MKD inside Root can stage elsewhere (Config.StagingPath) or skip
// staging (Config.WriteInPlace, with its weaker guarantee).
//
// FTP runs one command at a time per control connection, so the driver
// keeps a pool of logged-in connections (Config.MaxConns). A connection
// that fails below the protocol (a reset, a timeout, a cancelled context)
// is closed rather than reused, and the failure is CodeUnavailable, so
// layer.Retry treats it as transient; a reply from the server keeps its
// connection. The driver never repeats an operation itself.
//
// Credentials: Config prints and logs with the password redacted, and the
// driver keeps it inside its dial closure, so neither a logged Config nor
// a printed Driver leaks it. Plain FTP sends the password readable to
// anyone on the path: set Config.TLS (explicit FTPS) unless the network is
// trusted.
package ftp

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/textproto"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

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
// of this shape.
var stagedName = regexp.MustCompile(`^[a-z2-7]{26}$`)

// Defaults for the zero Config.
const (
	// DefaultMaxConns is the pool size: enough for a web server's
	// concurrent uploads, below the per-user session caps FTP servers
	// commonly set (vsftpd's max_per_ip, Pure-FTPd's -c).
	DefaultMaxConns = 4
	// DefaultDialTimeout bounds connecting and logging in when the
	// caller's context has no deadline.
	DefaultDialTimeout = 30 * time.Second
	// DefaultDataTimeout is Config.DataTimeout when zero.
	DefaultDataTimeout = 2 * time.Minute
	// DefaultIdleCheck is Config.IdleCheck when zero: how long a pooled
	// connection may sit idle before it is checked with NOOP on its next
	// use.
	DefaultIdleCheck = 15 * time.Second
	defaultPort      = "21"
)

// redacted is what Config prints in place of the password.
const redacted = "[redacted]"

// Config configures a Driver.
type Config struct {
	// Addr is the server's host:port; a missing port means 21 (990 is
	// usual for ImplicitTLS and must be given).
	Addr string
	// Username and Password log in; an empty Username logs in as
	// "anonymous".
	Username string
	Password string
	// Root is the absolute directory on the server holding the disk.
	// Required; created if missing.
	Root string
	// TLS, when set, secures the control and data connections: explicit
	// FTPS (AUTH TLS on the plain port) unless ImplicitTLS is set. Without
	// ServerName the certificate is checked against Addr's host, as
	// tls.Dialer does.
	TLS *tls.Config
	// ImplicitTLS speaks TLS from the first byte (port 990). Needs TLS.
	ImplicitTLS bool
	// MaxConns caps the pooled connections; 0 means DefaultMaxConns.
	MaxConns int
	// DialTimeout bounds one dial and login without a context deadline;
	// 0 means DefaultDialTimeout.
	DialTimeout time.Duration
	// DataTimeout bounds each wait for a download's next bytes, so a
	// server that stalls mid-transfer fails the read (CodeUnavailable)
	// instead of hanging it; 0 means DefaultDataTimeout.
	DataTimeout time.Duration
	// IdleCheck is how long a pooled connection may sit idle before its
	// next use first checks it with NOOP; 0 means DefaultIdleCheck. Lower
	// it for servers or NAT gateways that drop idle sessions quickly;
	// raise it to save a round trip on servers that keep them.
	IdleCheck time.Duration
	// StagingDir names the reserved directory, directly under Root, that
	// holds staged files and IIS's moved-aside originals; empty means the
	// package's StagingDir (".filesystem"). Set it when that name collides
	// with something on the server, or a partner's poller must ignore it
	// under a name of its own. It must be one path segment (no "/", not
	// "." or ".."): keys under it are refused and listings skip it.
	StagingDir string
	// StagingPath puts staged files and moved-aside originals in a
	// directory outside Root, an absolute path on the server, instead of
	// the reserved StagingDir under it: for a write-only drop folder (a
	// bank or EDI inbox, a partner's upload box) whose server refuses MKD
	// inside Root, or a poller that must never see anything but finished
	// files there. Writes are still staged there and renamed into Root, so
	// it must be on the same filesystem as Root (a rename cannot cross
	// devices), and it must not be Root, inside it, or contain it. Created
	// if missing; New probes letter case there, taking Root to share its
	// filesystem. No key is reserved and listings skip nothing. Exclusive
	// with StagingDir and WriteInPlace.
	StagingPath string
	// WriteInPlace stores each file straight to its key (STOR), with no
	// staging directory at all, for a drop folder where nothing outside
	// the keys may be created either. It needs AllowNonAtomicWrites,
	// because it gives up what staging buys: a reader or the server's
	// poller can see a file while its bytes are still arriving, an
	// overwrite truncates the old content first, and a failed write
	// deletes the file, so a failure while replacing a key leaves it
	// empty (a connection lost mid-STOR leaves the partial file). A Move
	// onto an existing key on a server that will not rename over a file
	// deletes the target first, with no rollback. New creates nothing, so
	// Root must exist, and there is nothing to sweep: SweepAbandoned and
	// FindAbandoned are not declared. The server's letter case cannot be
	// probed without writing into Root, so the disk reports it
	// case-sensitive. Exclusive with StagingDir and StagingPath.
	WriteInPlace bool
	// AllowNonAtomicWrites acknowledges WriteInPlace's weaker guarantee
	// and is required with it; it changes nothing else, since staged writes
	// keep their own documented replace on servers that refuse to rename
	// over a file (see Driver.Write).
	AllowNonAtomicWrites bool
	// Dial, when set, opens every TCP connection the driver makes, the
	// control connection and each data connection: route through a SOCKS
	// proxy or a bastion, or pin a source address. The driver adds TLS on
	// top as Config.TLS and ImplicitTLS say. nil means a net.Dialer with
	// DialTimeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialOptions are passed to jlaffaye/ftp after the driver's own, for
	// its protocol switches: ftp.DialWithDisabledEPSV behind a NAT that
	// breaks extended passive mode, ftp.DialWithDisabledUTF8,
	// ftp.DialWithDisabledMLSD, ftp.DialWithForceListHidden. Do not pass
	// the connection or TLS options (DialWithDialFunc, DialWithNetConn,
	// DialWithTLS, DialWithExplicitTLS): the driver owns the sockets so it
	// can cancel a command, and they would replace that; use Dial and TLS.
	DialOptions []ftp.DialOption
}

// password returns the placeholder Config prints for the password.
func (c Config) password() string {
	if c.Password == "" {
		return ""
	}
	return redacted
}

// String implements fmt.Stringer with the password redacted.
func (c Config) String() string {
	return fmt.Sprintf("ftp.Config{Addr: %q, Username: %q, Password: %q, Root: %q, TLS: %t, ImplicitTLS: %t, MaxConns: %d}",
		c.Addr, c.Username, c.password(), c.Root, c.TLS != nil, c.ImplicitTLS, c.MaxConns)
}

// GoString implements fmt.GoStringer with the password redacted.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb or flag prints the password.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer with the password redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("addr", c.Addr), slog.String("username", c.Username),
		slog.String("password", c.password()), slog.String("root", c.Root), slog.Bool("tls", c.TLS != nil))
}

// Driver is the FTP backend. Create it with New; it is safe for concurrent
// use (each call takes its own pooled connection).
type Driver struct {
	dataTimeout     time.Duration
	root            string
	pool            *pool
	caseInsensitive bool
	// staging is the reserved directory's name under Root (Config.StagingDir
	// or StagingDir), empty when nothing under Root is reserved
	// (Config.StagingPath or WriteInPlace). tmpDir is the absolute server
	// path staged files and moved-aside originals go to, empty under
	// WriteInPlace.
	staging, tmpDir string
	// inPlace is Config.WriteInPlace.
	inPlace bool
}

// New checks cfg, logs in within ctx, creates Root and the staging
// directory, and probes whether the server's filesystem folds letter case.
// Wrong credentials fail here with CodePermission.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.Addr == "" || !path.IsAbs(cfg.Root) || (cfg.ImplicitTLS && cfg.TLS == nil) || cfg.MaxConns < 0 || cfg.DataTimeout < 0 || cfg.IdleCheck < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: Addr and an absolute Root are required, ImplicitTLS needs TLS, MaxConns, DataTimeout and IdleCheck may not be negative")
	}
	root := path.Clean(cfg.Root)
	staging, tmpDir, err := stagingPlace(cfg, root)
	if err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		cfg.Addr = net.JoinHostPort(cfg.Addr, defaultPort)
	}
	size := cfg.MaxConns
	if size == 0 {
		size = DefaultMaxConns
	}
	d := &Driver{
		root: root, pool: newPool(dialFunc(cfg), size, cmp.Or(cfg.IdleCheck, DefaultIdleCheck)),
		dataTimeout: cmp.Or(cfg.DataTimeout, DefaultDataTimeout), staging: staging, tmpDir: tmpDir, inPlace: cfg.WriteInPlace,
	}
	err = d.pool.run(ctx, func(c *conn) error {
		if d.inPlace {
			return nil // logged in; nothing to create or probe (see Config.WriteInPlace)
		}
		if err := mkdirAll(c.sc, d.tmpDir); err != nil {
			return err
		}
		var err error
		d.caseInsensitive, err = probeCase(c.sc, d.tmpDir)
		return err
	})
	if err != nil {
		d.pool.close()
		return nil, err
	}
	return d, nil
}

// stagingPlace checks the staging fields of cfg and returns the name
// reserved under root (empty when none is) and the absolute directory
// staged files go to (empty under WriteInPlace).
func stagingPlace(cfg Config, root string) (staging, tmpDir string, err error) {
	switch {
	case cfg.WriteInPlace:
		if !cfg.AllowNonAtomicWrites || cfg.StagingDir != "" || cfg.StagingPath != "" {
			return "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: WriteInPlace needs AllowNonAtomicWrites and excludes StagingDir and StagingPath")
		}
		return "", "", nil
	case cfg.StagingPath != "":
		p := path.Clean(cfg.StagingPath)
		if cfg.StagingDir != "" || !path.IsAbs(p) || within(p, root) || within(root, p) {
			return "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: StagingPath %q must be absolute, outside Root and not containing it, and excludes StagingDir", cfg.StagingPath)
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
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "ftp: StagingDir %q must be one path segment", name)
	}
	return name, nil
}

// Do runs fn on one of the driver's pooled connections, for what the
// Driver does not cover: setting a file's modification time (MFMT or
// MDTM), recursive removal of a directory, raw listings (NLST, MLSD or
// LIST entries with their owners and modes), paths outside Root. The
// client library (github.com/jlaffaye/ftp) sends no arbitrary command,
// so a SITE command is out of reach here too. It is the escape hatch other adapters offer
// through Native; FTP has no single client to hand out, since each call
// takes its own connection. fn runs within ctx (its end closes the
// socket), paths it uses are the server's, not keys (Root is not applied
// and the staging directory is not protected), and the connection must be
// left in its working directory and without a transfer open. Errors are
// read as for the driver's own calls: a server reply is mapped to its
// filesystem.Code and keeps the connection; a *filesystem.Error from fn is
// returned as is and keeps it too, so return one for your own verdicts;
// any other error is taken for a lost connection (CodeUnavailable), and
// the connection is discarded.
func (d *Driver) Do(ctx context.Context, fn func(*ftp.ServerConn) error) error {
	return d.pool.run(ctx, func(c *conn) error { return fn(c.sc) })
}

// dialFunc returns the dialer for cfg. The credentials live only in this
// closure. The control socket is dialled here and handed to jlaffaye/ftp
// (see connFunc), so the pool can close it to cancel a command.
func dialFunc(cfg Config) dialer {
	user, pw := cfg.Username, cfg.Password
	if user == "" {
		user, pw = "anonymous", "anonymous"
	}
	timeout := cfg.DialTimeout
	if timeout == 0 {
		timeout = DefaultDialTimeout
	}
	return func(ctx context.Context) (*conn, error) {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		dial := cfg.Dial
		if dial == nil {
			dial = (&net.Dialer{Timeout: timeout}).DialContext
		}
		// Every TLS session (the control connection, explicit or implicit,
		// and each data connection) verifies the server by Addr's host
		// when TLS names none.
		var tlsConfig *tls.Config
		if cfg.TLS != nil {
			tlsConfig = tlsFor(cfg.TLS, cfg.Addr)
		}
		raw, err := dial(ctx, "tcp", cfg.Addr)
		if err == nil && cfg.ImplicitTLS {
			tc := tls.Client(raw, tlsConfig)
			if err = tc.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
			}
			raw = tc
		}
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		opts := []ftp.DialOption{ftp.DialWithDialFunc(connFunc(raw, dial, timeout, tlsConfig))}
		if tlsConfig != nil {
			if cfg.ImplicitTLS {
				opts = append(opts, ftp.DialWithTLS(tlsConfig))
			} else {
				opts = append(opts, ftp.DialWithExplicitTLS(tlsConfig))
			}
		}
		opts = append(opts, cfg.DialOptions...)
		sc, err := ftp.Dial(cfg.Addr, opts...)
		if err != nil {
			_ = raw.Close()
			return nil, classifyDial(ctx, err)
		}
		if err := sc.Login(user, pw); err != nil {
			_ = sc.Quit()
			_ = raw.Close()
			return nil, classifyDial(ctx, err)
		}
		return &conn{sc: sc, raw: raw}, nil
	}
}

// tlsFor is cfg with ServerName taken from addr when cfg leaves it empty,
// as tls.Dialer does, so certificate verification has a name to check.
func tlsFor(cfg *tls.Config, addr string) *tls.Config {
	if cfg.ServerName != "" {
		return cfg
	}
	c := cfg.Clone()
	c.ServerName, _, _ = net.SplitHostPort(addr)
	return c
}

// connFunc is the dial function jlaffaye/ftp calls for every connection
// of one session: the first call takes the control socket dialled already
// (so the pool holds it and can close it), every later one dials a data
// connection through dial (bounded by timeout), under TLS when the session
// uses it. jlaffaye/ftp applies no TLS itself to what a dial function
// returns.
func connFunc(control net.Conn, dial func(context.Context, string, string) (net.Conn, error), timeout time.Duration, tlsConfig *tls.Config) func(network, addr string) (net.Conn, error) {
	first := true
	return func(network, addr string) (net.Conn, error) {
		if first {
			first = false
			return control, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := dial(ctx, network, addr)
		if err != nil || tlsConfig == nil {
			return c, err
		}
		return tls.Client(c, tlsConfig), nil
	}
}

// classifyDial reports a failed greeting or login: a reply is the server's
// verdict (530 is CodePermission), anything else a connection that did not
// come up.
func classifyDial(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, errors.Join(ctx.Err(), err))
	}
	return classify(err)
}

// probeCase stores an empty staged-shape (lower-case) file in dir and asks
// for its size under its upper-case name: found means the server's
// filesystem folds case, which FTP cannot report any other way. A server
// refusing the upload (a read-only account) is reported case-sensitive,
// since such an account cannot write two keys that would collide.
func probeCase(sc *ftp.ServerConn, dir string) (bool, error) {
	name := strings.ToLower(rand.Text())
	if err := sc.Stor(path.Join(dir, name), strings.NewReader("")); err != nil {
		if isReply(err) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = sc.Delete(path.Join(dir, name)) }()
	_, err := sc.FileSize(path.Join(dir, strings.ToUpper(name)))
	if err == nil {
		return true, nil
	}
	if isReply(err) {
		return false, nil
	}
	return false, err
}

// replyCode returns the reply code err carries, or 0 when it is not a reply.
func replyCode(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

// isReply reports whether err is the server's answer to a command, which
// leaves the connection usable.
func isReply(err error) bool {
	var fe *filesystem.Error
	return err != nil && !errors.As(err, &fe) && !brokenBy(err)
}

// Close closes the pooled connections and refuses further calls.
// Connections in use close when their call ends.
func (d *Driver) Close() error {
	d.pool.close()
	return nil
}

// Capabilities reports what FTP offers portably. No visibility (SITE
// CHMOD is not standard), no metadata, no server-side copy, and no
// conditional write (no FTP command creates a file only if absent).
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Move:            true,
		DeletePrefix:    true,
		SweepAbandoned:  !d.inPlace,
		FindAbandoned:   !d.inPlace,
		Dirs:            true,
		CaseInsensitive: d.caseInsensitive,
	}
}

func (d *Driver) full(key string) string { return path.Join(d.root, key) }

func (d *Driver) checkKey(keys ...string) error {
	for _, key := range keys {
		if d.staging != "" && (key == d.staging || strings.HasPrefix(key, d.staging+"/")) {
			return filesystem.Errorf(filesystem.CodeInvalidKey, "ftp: keys under %q are reserved", d.staging)
		}
	}
	return nil
}

// stat returns key's entry: MLST when the server has it (RFC 3659, exact
// type and time), otherwise SIZE and MDTM. A directory is not an object
// (CodeNotFound).
func (d *Driver) stat(sc *ftp.ServerConn, key string) (filesystem.Entry, error) {
	full := d.full(key)
	e, err := sc.GetEntry(full)
	if err == nil {
		if e.Type != ftp.EntryTypeFile {
			return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeNotFound, "ftp: %q is not a file", key)
		}
		return entry(key, int64(e.Size), e.Time), nil
	}
	if code := replyCode(err); code != ftp.StatusNotImplemented && code != ftp.StatusCommandNotImplemented && code != ftp.StatusBadCommand {
		return filesystem.Entry{}, err // missing (550), or the connection broke
	}
	size, err := sc.FileSize(full)
	if err != nil {
		return filesystem.Entry{}, err
	}
	mod, err := sc.GetTime(full)
	if err != nil && !isReply(err) {
		return filesystem.Entry{}, err
	}
	return entry(key, size, mod), nil
}

// entryType reports whether full is a file or a directory: MLST where
// the server has it, otherwise SIZE (files only) then CWD (directories
// only). A path that is neither is the server's 550.
func entryType(sc *ftp.ServerConn, full string) (ftp.EntryType, error) {
	e, err := sc.GetEntry(full)
	if err == nil {
		return e.Type, nil
	}
	if replyCode(err) != ftp.StatusNotImplemented && replyCode(err) != ftp.StatusBadCommand {
		return 0, err
	}
	if _, err := sc.FileSize(full); err == nil || !isReply(err) {
		return ftp.EntryTypeFile, err
	}
	if err := sc.ChangeDir(full); err != nil {
		return 0, err
	}
	return ftp.EntryTypeFolder, sc.ChangeDir("/")
}

func entry(key string, size int64, mod time.Time) filesystem.Entry {
	return filesystem.Entry{Key: key, Size: size, ModTime: mod.UTC(), ContentType: mime.TypeByExtension(path.Ext(key))}
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (e filesystem.Entry, err error) {
	if err := d.checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	err = d.pool.run(ctx, func(c *conn) error {
		e, err = d.stat(c.sc, key)
		return err
	})
	return e, err
}

// Read implements filesystem.Driver with RETR, from an offset with REST.
// The connection stays with the reader until it is closed. A range that
// starts at or past the end reads nothing.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if err := d.checkKey(key); err != nil {
		return nil, err
	}
	c, err := d.pool.get(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.raw.Close() })
	fail := func(err error) (io.ReadCloser, error) {
		cancelled := !stop()
		d.pool.put(c, cancelled || brokenBy(err))
		if cancelled {
			return nil, ctx.Err()
		}
		return nil, classify(err)
	}
	e, err := d.stat(c.sc, key)
	if err != nil {
		return fail(err)
	}
	if rng.Offset >= e.Size && e.Size > 0 || rng.Offset > 0 && e.Size == 0 {
		stop()
		d.pool.put(c, false)
		return io.NopCloser(strings.NewReader("")), nil
	}
	resp, err := c.sc.RetrFrom(d.full(key), uint64(rng.Offset))
	if err != nil {
		return fail(err)
	}
	if !stop() {
		_ = resp.Close()
		d.pool.put(c, true)
		return nil, ctx.Err()
	}
	t := &transfer{resp: resp, c: c, pool: d.pool, ctx: ctx, timeout: d.dataTimeout}
	// The reader outlives Read, so ctx stays bound to it: when ctx ends,
	// the data socket's deadline passes (failing a Read in progress) and
	// the control socket closes.
	t.stop = context.AfterFunc(ctx, func() {
		_ = resp.SetDeadline(time.Unix(1, 0))
		_ = c.raw.Close()
	})
	t.Reader = t.body()
	if rng.Length > 0 {
		t.Reader = io.LimitReader(t.Reader, rng.Length)
	}
	return t, nil
}

// transfer is a download in progress. Closing it before the server sent
// the whole file (a range, or a caller giving up) aborts the transfer; the
// server's answer to that is not an error, but the connection is not
// reused, since servers differ in what they send and when.
type transfer struct {
	io.Reader
	resp *ftp.Response
	c    *conn
	pool *pool
	ctx  context.Context
	// timeout bounds each wait for data (Config.DataTimeout).
	timeout time.Duration
	// stop unbinds ctx; false means ctx already cut the connection.
	stop func() bool
	done bool
	// eof is whether the data connection itself ended, which a range's
	// LimitReader stopping early does not.
	eof bool
}

// body reads the data connection, noting when it ends.
func (t *transfer) body() io.Reader { return eofReader{t} }

type eofReader struct{ t *transfer }

func (r eofReader) Read(p []byte) (int, error) {
	if err := r.t.ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.t.resp.SetDeadline(time.Now().Add(r.t.timeout)); err != nil {
		return 0, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	n, err := r.t.resp.Read(p)
	if err != nil && err != io.EOF {
		if cerr := r.t.ctx.Err(); cerr != nil {
			return n, cerr
		}
		return n, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	if err == io.EOF {
		r.t.eof = true
	}
	return n, err
}

func (t *transfer) Close() error {
	if t.done {
		return nil
	}
	t.done = true
	cut := !t.stop()
	if !t.eof {
		// The connection will not be reused, and a stalled server would
		// keep Close waiting for its end-of-transfer reply: cut it now.
		_ = t.c.raw.Close()
	}
	err := t.resp.Close()
	t.pool.put(t.c, cut || err != nil || !t.eof)
	if err != nil && t.eof {
		return classify(err)
	}
	return nil
}

// Write implements filesystem.Driver: STOR to a staged name, then rename
// into place. A server that refuses to rename over an existing file (IIS
// answers "550 … already exists") gets the replace rename describes: the old
// object is moved aside, so on such a server the key is briefly missing
// during a replace, and a failure puts it back; POSIX servers replace
// atomically. Only if putting it back fails too does Write return a
// *StrandedError, naming where the new and the old content are kept.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, _ filesystem.WriteOptions) (e filesystem.Entry, err error) {
	if err := d.checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	if d.inPlace {
		return d.writeInPlace(ctx, key, body)
	}
	tmp := path.Join(d.tmpDir, strings.ToLower(rand.Text())) // see stagedName
	err = d.pool.run(ctx, func(c *conn) error {
		if err := c.sc.Stor(tmp, bodyReader{&ctxReader{ctx: ctx, r: body}}); err != nil {
			return err
		}
		if err := d.rename(c.sc, tmp, d.full(key)); err != nil {
			if !errors.As(err, new(*StrandedError)) {
				_ = c.sc.Delete(tmp)
			}
			return err
		}
		e, err = d.stat(c.sc, key)
		return err
	})
	// A connection that broke mid-write leaves its staged file for
	// SweepAbandoned.
	return e, err
}

// writeInPlace is Write under Config.WriteInPlace: STOR straight to the
// key, after creating its parents. A failed STOR deletes what it stored.
func (d *Driver) writeInPlace(ctx context.Context, key string, body io.Reader) (e filesystem.Entry, err error) {
	full := d.full(key)
	err = d.pool.run(ctx, func(c *conn) error {
		if dir := path.Dir(key); dir != "." {
			if err := mkdirAll(c.sc, d.full(dir)); err != nil {
				return err
			}
		}
		if err := c.sc.Stor(full, bodyReader{&ctxReader{ctx: ctx, r: body}}); err != nil {
			_ = c.sc.Delete(full) // fails fast when the connection is gone
			return err
		}
		e, err = d.stat(c.sc, key)
		return err
	})
	return e, err
}

// renameAttempts is how many times each rename of a replace (moving the old
// object aside, the new one in, or the old one back) is tried.
const renameAttempts = 3

// StrandedError is a replace that left its key empty: the server refused
// to rename over the key, the driver moved the old object aside, the rename
// of the new content failed, and so did moving the old object back. Nothing
// is lost: the new content is at Staged and the old at Previous (absolute
// paths on the server, both in the staging directory), and the driver keeps
// both; rename one of them to Target by hand, or write again.
// SweepAbandoned removes staged files by age, so recover them before the
// next sweep.
type StrandedError struct {
	// Staged is the server path (in the staging directory) holding the new
	// content.
	Staged string
	// Previous is the server path (in the staging directory) holding the
	// content Target had before the replace.
	Previous string
	// Target is the server path (under Config.Root) of the replaced key.
	Target string
	// Err is the last rename failure, which Unwrap returns.
	Err error
}

// Error names every path, so a log line alone is enough to recover by hand.
func (e *StrandedError) Error() string {
	return fmt.Sprintf("ftp: %s could not be replaced or restored; its previous content is kept at %s and the new content at %s: %v", e.Target, e.Previous, e.Staged, e.Err)
}

// Unwrap returns the rename failure, so filesystem.CodeOf and errors.Is see
// its cause.
func (e *StrandedError) Unwrap() error { return e.Err }

// rename moves from onto to: once; then, unless the server said to exists,
// again after creating to's parents; and, only when the server refused
// because to exists and to is a file, by replacing it (see replace). A
// refusal is CodePermission (550 on RNTO means the server would not do it,
// not that a key is missing).
func (d *Driver) rename(sc *ftp.ServerConn, from, to string) error {
	err := sc.Rename(from, to)
	if err == nil || !isReply(err) {
		return err
	}
	if !refusedAsExisting(err) {
		if err := mkdirAll(sc, path.Dir(to)); err != nil {
			return err
		}
		if err = sc.Rename(from, to); err == nil || !isReply(err) {
			return err
		}
		if !refusedAsExisting(err) {
			return renameRefused(from, to, err)
		}
	}
	if kind, serr := entryType(sc, to); serr != nil || kind != ftp.EntryTypeFile {
		return renameRefused(from, to, err)
	}
	// Moving to aside is pointless if there is nothing to put in its place.
	if _, serr := sc.FileSize(from); serr != nil {
		if !isReply(serr) {
			return serr
		}
		return renameRefused(from, to, fmt.Errorf("%w; the staged file is gone: %w", err, serr))
	}
	return d.replace(sc, from, to)
}

// replace puts from in place of the existing file to on a server that will
// not rename over a file.
//
// Why not "delete to, then rename": if the rename then fails, the old
// object is gone. Instead to is moved aside to a staged name in the
// staging directory (a backup), from is renamed onto to, and the backup is
// deleted. If renaming from fails, the backup is moved back, so to keeps
// its old content and from stays where it was. Each rename is tried
// renameAttempts times, since a busy server may refuse one transiently.
//
// What stays non-atomic: to is missing between the two renames; a backup
// whose delete fails is left in the staging directory for SweepAbandoned;
// and if moving the backup back fails too, both contents are kept and the
// error is a *StrandedError naming them.
//
// Under Config.WriteInPlace there is nowhere to move to aside, so to is
// deleted and from renamed onto it, with no rollback (the mode's
// documented trade).
func (d *Driver) replace(sc *ftp.ServerConn, from, to string) error {
	if d.inPlace {
		if err := sc.Delete(to); err != nil {
			return err
		}
		return renameRetried(sc, from, to)
	}
	bak := path.Join(d.tmpDir, strings.ToLower(rand.Text())) // see stagedName
	if err := renameRetried(sc, to, bak); err != nil {
		if isReply(err) {
			return renameRefused(to, bak, err)
		}
		return err
	}
	err := renameRetried(sc, from, to)
	if err == nil {
		_ = sc.Delete(bak) // cleanup only: to already holds the new content
		return nil
	}
	if rerr := renameRetried(sc, bak, to); rerr != nil {
		return filesystem.NewError(filesystem.CodeUnknown, &StrandedError{Staged: from, Previous: bak, Target: to, Err: rerr})
	}
	if isReply(err) {
		return renameRefused(from, to, err)
	}
	return err
}

// renameRetried renames from to to, trying again after a reply (a refusal)
// up to renameAttempts times in all; a failure below the protocol is
// returned at once, since the connection is gone.
func renameRetried(sc *ftp.ServerConn, from, to string) error {
	var err error
	for range renameAttempts {
		if err = sc.Rename(from, to); err == nil || !isReply(err) {
			return err
		}
	}
	return err
}

// refusedAsExisting reports a rename refused because the target exists.
// FTP has no reply code for it; servers that refuse (IIS: "550 Cannot
// create a file when that file already exists.") say so in the text.
func refusedAsExisting(err error) bool {
	var te *textproto.Error
	if !errors.As(err, &te) || (te.Code != ftp.StatusFileUnavailable && te.Code != ftp.StatusBadFileName) {
		return false
	}
	msg := strings.ToLower(te.Msg)
	for _, phrase := range existsPhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// existsPhrases are the reply texts that say a rename's target exists
// (IIS, and the strerror of EEXIST). "does not exist" matches none of
// them, so a missing source is never mistaken for an existing target.
var existsPhrases = []string{"already exists", "file exists"}

// renameRefused classifies a rename the server answered with a refusal.
func renameRefused(from, to string, err error) error {
	return filesystem.NewError(filesystem.CodePermission, fmt.Errorf("ftp: server refused to rename %s to %s: %w", from, to, err))
}

// mkdirAll creates dir and its parents. MKD of an existing directory
// fails, so each level's failure is ignored and the result is checked
// once, by changing into dir (and back to /, since paths are absolute).
func mkdirAll(sc *ftp.ServerConn, dir string) error {
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	for i := range parts {
		if err := sc.MakeDir("/" + strings.Join(parts[:i+1], "/")); err != nil && !isReply(err) {
			return err
		}
	}
	if err := sc.ChangeDir(dir); err != nil {
		if isReply(err) {
			return filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		return err
	}
	return sc.ChangeDir("/")
}

// pruneParents removes directories left empty, up to Root. RMD of a
// directory that is not empty fails, which is where pruning stops.
func (d *Driver) pruneParents(sc *ftp.ServerConn, key string) {
	for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
		if err := sc.RemoveDir(d.full(dir)); err != nil {
			return
		}
	}
}

// Delete implements filesystem.Driver. A directory at key is not an
// object and is left alone.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if err := d.checkKey(key); err != nil {
		return err
	}
	return d.pool.run(ctx, func(c *conn) error {
		if _, err := d.stat(c.sc, key); err != nil {
			if filesystem.CodeOf(classify(err)) == filesystem.CodeNotFound {
				return nil
			}
			return err
		}
		if err := c.sc.Delete(d.full(key)); err != nil {
			return err
		}
		d.pruneParents(c.sc, key)
		return nil
	})
}

// Move implements filesystem.Mover with RNFR/RNTO.
func (d *Driver) Move(ctx context.Context, src, dst string) (e filesystem.Entry, err error) {
	if err := d.checkKey(src, dst); err != nil {
		return filesystem.Entry{}, err
	}
	err = d.pool.run(ctx, func(c *conn) error {
		if _, err := d.stat(c.sc, src); err != nil {
			return err
		}
		if src != dst {
			if err := d.rename(c.sc, d.full(src), d.full(dst)); err != nil {
				return err
			}
			d.pruneParents(c.sc, src)
		}
		e, err = d.stat(c.sc, dst)
		return err
	})
	return e, err
}

// List implements filesystem.Driver by walking directories (MLSD where
// the server has it, LIST otherwise), O(n) in the files under dir per
// page like the local driver. Names that are not clean keys are skipped
// (see serverKey).
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (p filesystem.Page, err error) {
	var all []filesystem.Entry
	err = d.pool.run(ctx, func(c *conn) error {
		queue := []string{dir}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			entries, err := c.sc.List(d.full(cur))
			if err != nil {
				if cur == dir && isReply(err) {
					return nil // a missing directory lists as empty
				}
				return err
			}
			for _, e := range entries {
				k := filesystem.JoinKey(cur, e.Name)
				if !serverKey(k, e.Name) {
					continue
				}
				switch e.Type {
				case ftp.EntryTypeFolder:
					if d.staging == "" || k != d.staging {
						queue = append(queue, k)
					}
				case ftp.EntryTypeFile:
					all = append(all, entry(k, int64(e.Size), e.Time))
				}
			}
		}
		return nil
	})
	if err != nil {
		return filesystem.Page{}, err
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// serverKey reports whether a name the server listed is one the Disk
// would accept as written: one path segment, and a key CleanKey leaves
// unchanged. Others ("..", a backslash, a control character) are skipped
// rather than trusted, so a server cannot make a listing name a key the
// Disk would refuse or read as another.
func serverKey(key, name string) bool {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return false
	}
	clean, err := filesystem.CleanKey(key)
	return err == nil && clean == key
}

// DeletePrefix implements filesystem.PrefixDeleter by removing the
// directory tree (the root's children, keeping the staging directory).
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	if err := d.checkKey(dir); err != nil && dir != "" {
		return err
	}
	return d.pool.run(ctx, func(c *conn) error {
		if dir != "" {
			kind, err := entryType(c.sc, d.full(dir))
			if err != nil {
				if isReply(err) {
					return nil
				}
				return err
			}
			if kind != ftp.EntryTypeFolder {
				return nil // a file named dir is not inside dir
			}
			if err := c.sc.RemoveDirRecur(d.full(dir)); err != nil {
				return err
			}
			d.pruneParents(c.sc, dir)
			return nil
		}
		entries, err := c.sc.List(d.root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." || (d.staging != "" && e.Name == d.staging) {
				continue
			}
			full := d.full(e.Name)
			if e.Type == ftp.EntryTypeFolder {
				err = c.sc.RemoveDirRecur(full)
			} else {
				err = c.sc.Delete(full)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// SweepAbandoned implements filesystem.AbandonedSweeper: it removes staged
// files a Write left in the staging directory's tmp when the connection or the process
// died between the STOR and the rename, and whose modification time is
// before before. Only files named the way Write names them are removed.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (n int, err error) {
	err = d.pool.run(ctx, func(c *conn) error {
		return d.scanAbandoned(ctx, c.sc, before, func(e *ftp.Entry) error {
			if err := c.sc.Delete(path.Join(d.tmpDir, e.Name)); err != nil {
				if isReply(err) {
					return nil // renamed or removed by its write meanwhile
				}
				return err
			}
			n++
			return nil
		})
	})
	return n, err
}

// FindAbandoned implements filesystem.AbandonedFinder: the staged files
// SweepAbandoned would remove, found by the same scan, removing nothing.
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) (found []filesystem.Abandoned, err error) {
	err = d.pool.run(ctx, func(c *conn) error {
		return d.scanAbandoned(ctx, c.sc, before, func(e *ftp.Entry) error {
			found = append(found, filesystem.Abandoned{Kind: filesystem.AbandonedStaging, ID: e.Name, LastActivity: e.Time})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// scanAbandoned calls visit for every staged file in the staging directory's tmp
// modified before before and named the way Write names them; a staging
// directory the server does not have (a reply error) holds none. An error
// from visit ends the scan. It is the one definition of an abandoned
// staged file both SweepAbandoned and FindAbandoned use.
func (d *Driver) scanAbandoned(ctx context.Context, sc *ftp.ServerConn, before time.Time, visit func(*ftp.Entry) error) error {
	if d.inPlace {
		return filesystem.Errorf(filesystem.CodeUnsupported, "ftp: WriteInPlace stages nothing, so there is nothing to sweep")
	}
	entries, err := sc.List(d.tmpDir)
	if err != nil {
		if isReply(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.Type != ftp.EntryTypeFile || !stagedName.MatchString(e.Name) || !e.Time.Before(before) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(e); err != nil {
			return err
		}
	}
	return nil
}

// MakeDir implements filesystem.DirMaker with MKD, level by level. A file
// at dir is CodeExists; a file at one of its parents makes dir impossible
// (CodeInvalidArgument). Like the local driver, deleting the last file
// under a directory removes it again, and DeletePrefix removes it with
// everything below.
func (d *Driver) MakeDir(ctx context.Context, dir string) error {
	if err := d.checkKey(dir); err != nil {
		return err
	}
	return d.pool.run(ctx, func(c *conn) error {
		t, err := entryType(c.sc, d.full(dir))
		switch {
		case err == nil && t == ftp.EntryTypeFolder:
			return nil
		case err == nil:
			return filesystem.Errorf(filesystem.CodeExists, "ftp: %q is a file, not a directory", dir)
		case !isReply(err):
			return err
		}
		return mkdirAll(c.sc, d.full(dir))
	})
}

// DirExists implements filesystem.DirMaker: whether dir is a directory on
// the server. A file at dir, or a path the server answers with a reply
// error (missing, or through a file), is not one; a broken connection is
// an error.
func (d *Driver) DirExists(ctx context.Context, dir string) (exists bool, err error) {
	if err := d.checkKey(dir); err != nil {
		return false, err
	}
	err = d.pool.run(ctx, func(c *conn) error {
		t, err := entryType(c.sc, d.full(dir))
		switch {
		case err == nil:
			exists = t == ftp.EntryTypeFolder
		case isReply(err):
			exists = false
		default:
			return err
		}
		return nil
	})
	return exists, err
}

// ctxReader stops a body at a cancelled context, so a long upload ends
// when the caller gives up.
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
