// Package webdav is a filesystem.Driver over WebDAV (RFC 4918), the
// protocol of Nextcloud, ownCloud, Box, many NAS boxes (Synology, QNAP) and
// Apache's and nginx's dav modules. It speaks PROPFIND, GET, PUT, DELETE,
// MKCOL, MOVE and COPY over net/http and has no dependency beyond the
// library.
//
// Writes are staged: the body is PUT to a temporary file under a reserved
// staging collection and MOVEd into place with Overwrite: T, so a reader
// never sees a half-written object. They are not atomic replacements: RFC
// 4918 defines that MOVE as a delete of the destination followed by the
// move, and servers implement it that way (golang.org/x/net/webdav runs
// RemoveAll, then Rename), so while an existing key is replaced a reader
// can briefly find it missing (CodeNotFound). A body that fails part-way
// leaves only the staged file, which Write removes (and SweepAbandoned
// removes when the process died first). The staging PUT carries Expect:
// 100-continue, so no body byte is sent before the server has started
// handling it (and created the staged file): otherwise a body failing
// early could have its cleanup DELETE overtake the aborted PUT, which would
// then create the file after it. That costs one extra round trip per write.
// A write-only drop folder that refuses MKCOL inside the endpoint's
// collection can stage elsewhere (Config.StagingPath) or skip staging
// (Config.WriteInPlace, with its weaker guarantee).
//
// Parent collections are created on demand (MKCOL) and never removed, since
// a WebDAV DELETE of a collection is recursive and a check-then-delete would
// race a concurrent write into it. Listings are built from files only, so
// an empty collection never appears in one.
//
// Credentials: Basic authentication from Config.Username and
// Config.Password. Config prints and logs with the password redacted, and
// the driver keeps it inside a closure, so neither a logged Config nor a
// printed Driver leaks it. Use https: Basic authentication over plain http
// sends the password readable to anyone on the path, so New refuses an
// http endpoint unless it is on the loopback interface or
// Config.AllowInsecureHTTP says so. Redirects are followed only within the
// endpoint's scheme and host, so neither the credentials nor a request
// body is ever sent elsewhere.
package webdav

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
)

// StagingDir is the default reserved collection, under the endpoint, for
// temporary files (Config.StagingDir names another). Keys inside the
// reserved collection are refused and listings skip it.
const StagingDir = ".filesystem"

// tmpSub is the collection, inside the staging collection, that holds
// staged files; SweepAbandoned looks only there.
const tmpSub = "tmp"

// stagedName is the shape of the temporary file names Write gives staged
// files (lower-cased crypto/rand.Text). SweepAbandoned removes only files
// of this shape.
var stagedName = regexp.MustCompile(`^[a-z2-7]{26}$`)

// redacted is what Config prints in place of the password.
const redacted = "[redacted]"

// Config configures a Driver.
type Config struct {
	// Endpoint is the URL of the collection holding the disk, such as
	// https://cloud.example.com/remote.php/dav/files/alice/app/. Required;
	// http or https. A missing trailing slash is added.
	Endpoint string
	// Client sends the requests; nil means a client with DefaultTimeout.
	// Pass one to set a transport, proxy or TLS configuration. The driver
	// uses a copy whose redirect policy is confined to the endpoint's
	// scheme and host.
	Client *http.Client
	// Username and Password are sent with Basic authentication when
	// Username is set.
	Username string
	Password string
	// AllowInsecureHTTP permits an http (not https) Endpoint on a host
	// other than the loopback interface, sending the password and every
	// byte readable on the network. Off by default; for a private network
	// the deployment trusts.
	AllowInsecureHTTP bool
	// StagingDir names the reserved collection, directly under the
	// endpoint, that holds staged files; empty means the package's
	// StagingDir (".filesystem"). Set it when that name collides with
	// something on the server, or a sync client or partner poller must
	// ignore it under a name of its own. It must be one path segment (no
	// "/", not "." or ".."): keys under it are refused and listings skip
	// it.
	StagingDir string
	// StagingPath puts staged files in a collection outside the endpoint's,
	// an absolute path on the endpoint's server (such as
	// /remote.php/dav/files/alice/.staging), instead of the reserved
	// StagingDir under it: for a write-only drop folder whose server
	// refuses MKCOL inside the endpoint's collection, or a sync client or
	// poller that must never see anything but finished files there. Writes
	// are still PUT there and MOVEd in, so the server must allow a MOVE
	// between the two. It must not be the endpoint's path, inside it, or
	// contain it; its parent collection must exist, and it is created if
	// missing. No key is reserved and listings skip nothing. Exclusive
	// with StagingDir and WriteInPlace.
	StagingPath string
	// WriteInPlace PUTs each file straight to its key, with no staging
	// collection at all, for a drop folder where nothing outside the keys
	// may be created either. It needs AllowNonAtomicWrites, because it
	// gives up what staging buys: a reader, sync client or poller can see
	// a file while its bytes are still arriving, an overwrite replaces the
	// old content as the PUT lands, and a failed PUT deletes the key, so a
	// failure while replacing a key leaves it empty. A nested key's
	// parents are created (MKCOL) before its PUT. New sends no request, so
	// wrong credentials surface on the first call; the server's letter
	// case cannot be probed without writing into the collection, so the
	// disk reports it case-sensitive; and there is nothing to sweep:
	// SweepAbandoned and FindAbandoned are not declared. Exclusive with
	// StagingDir and StagingPath.
	WriteInPlace bool
	// AllowNonAtomicWrites acknowledges WriteInPlace's weaker guarantee and
	// is required with it; it changes nothing else (a staged write's MOVE
	// keeps the replace the package comment describes).
	AllowNonAtomicWrites bool
}

// String implements fmt.Stringer with the password redacted.
func (c Config) String() string {
	pw := ""
	if c.Password != "" {
		pw = redacted
	}
	return fmt.Sprintf("webdav.Config{Endpoint: %q, Username: %q, Password: %q}", c.Endpoint, c.Username, pw)
}

// GoString implements fmt.GoStringer with the password redacted.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb or flag prints the password.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer with the password redacted.
func (c Config) LogValue() slog.Value {
	pw := ""
	if c.Password != "" {
		pw = redacted
	}
	return slog.GroupValue(slog.String("endpoint", c.Endpoint), slog.String("username", c.Username), slog.String("password", pw))
}

// DefaultTimeout bounds each request of the client New makes when
// Config.Client is nil. It is generous because a request carries a whole
// object; pass a Client to choose another.
const DefaultTimeout = 10 * time.Minute

// Driver is the WebDAV backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	base   *url.URL
	client *http.Client
	// auth adds the credentials to a request. A func, because fmt prints
	// a func as an address under every verb, so printing a Driver never
	// shows the password (a struct or pointer field would, under %s).
	auth func(*http.Request)
	// staged is whether the staging collection is known to exist.
	staged atomic.Bool
	// caseInsensitive is what New's probe found.
	caseInsensitive bool
	// mkcol serialises collection creation (see mkcolAll).
	mkcol sync.Mutex
	// staging is the reserved collection's name under the endpoint
	// (Config.StagingDir or StagingDir), empty when nothing there is
	// reserved (Config.StagingPath or WriteInPlace).
	staging string
	// st is the driver staged files are addressed through: d itself, or
	// for Config.StagingPath one based at that path's parent collection,
	// sharing d's client and credentials. tmpDir is the collection, under
	// st's base, staged files go to; empty under WriteInPlace.
	st     *Driver
	tmpDir string
	// inPlace is Config.WriteInPlace.
	inPlace bool
}

// New checks cfg, then probes the server within ctx: it creates the
// staging collection and finds whether the server's storage folds letter
// case (see probeCase). An account that may not write is reported
// case-sensitive and gets its staging collection on the first write. Wrong
// credentials fail here, with CodePermission, rather than at first use.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: Endpoint must be an http or https URL")
	}
	if u.User != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: Endpoint must not carry credentials; use Username and Password")
	}
	if u.Scheme == "http" && !cfg.AllowInsecureHTTP && !loopback(u.Hostname()) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: Endpoint %s is plain http; use https, or set AllowInsecureHTTP for a trusted network", u.Redacted())
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	staging, stBase, tmpDir, err := stagingPlace(cfg, u.Path)
	if err != nil {
		return nil, err
	}
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	client := &http.Client{Timeout: DefaultTimeout}
	if cfg.Client != nil {
		c := *cfg.Client // a copy: the caller's client is left as it was
		client = &c
	}
	client.Transport = waitForContinue(client.Transport)
	client.CheckRedirect = sameOrigin(u, client.CheckRedirect)
	d := &Driver{base: u, client: client, auth: func(*http.Request) {}, staging: staging, tmpDir: tmpDir, inPlace: cfg.WriteInPlace}
	if user, pw := cfg.Username, cfg.Password; user != "" {
		d.auth = func(r *http.Request) { r.SetBasicAuth(user, pw) }
	}
	d.st = d
	if stBase != "" {
		b := *u
		b.Path = stBase
		d.st = &Driver{base: &b, client: client, auth: d.auth}
	}
	if d.inPlace {
		return d, nil // nothing to create or probe (see Config.WriteInPlace)
	}
	insensitive, err := d.probeCase(ctx)
	if err != nil {
		return nil, err
	}
	d.caseInsensitive = insensitive
	return d, nil
}

// stagingPlace checks the staging fields of cfg against the endpoint's
// path base (with its trailing slash) and returns the name reserved under
// the endpoint (empty when none is), the base path staged files are
// addressed from when it is not the endpoint's (empty when it is), and
// the collection under that base they go to (empty under WriteInPlace).
func stagingPlace(cfg Config, base string) (staging, stBase, tmpDir string, err error) {
	switch {
	case cfg.WriteInPlace:
		if !cfg.AllowNonAtomicWrites || cfg.StagingDir != "" || cfg.StagingPath != "" {
			return "", "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: WriteInPlace needs AllowNonAtomicWrites and excludes StagingDir and StagingPath")
		}
		return "", "", "", nil
	case cfg.StagingPath != "":
		p := path.Clean(cfg.StagingPath)
		ep := path.Clean(base)
		if cfg.StagingDir != "" || !path.IsAbs(cfg.StagingPath) || p == "/" || within(p, ep) || within(ep, p) {
			return "", "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: StagingPath %q must be an absolute path outside the endpoint's collection and not containing it, and excludes StagingDir", cfg.StagingPath)
		}
		parent := path.Dir(p)
		if parent != "/" {
			parent += "/"
		}
		return "", parent, path.Base(p), nil
	}
	staging, err = stagingName(cfg.StagingDir)
	if err != nil {
		return "", "", "", err
	}
	return staging, "", staging + "/" + tmpSub, nil
}

// within reports whether the clean absolute path p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// stagingName validates Config.StagingDir: empty means StagingDir, and a
// name must be one path segment, because keys under it are refused and
// listings skip it by its name directly under the endpoint.
func stagingName(name string) (string, error) {
	if name == "" {
		return StagingDir, nil
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "webdav: StagingDir %q must be one path segment", name)
	}
	return name, nil
}

// ExpectContinueTimeout is how long a staging PUT waits for the server's
// 100 Continue before sending the body anyway (a server that ignores
// Expect costs each write this long). It matches http.DefaultTransport's.
const ExpectContinueTimeout = time.Second

// expectContinue is the header the staging PUT sends (see the package
// comment for why).
var expectContinue = http.Header{"Expect": {"100-continue"}}

// waitForContinue returns rt (nil meaning http.DefaultTransport) such that
// a request carrying Expect: 100-continue really waits for the server: an
// *http.Transport whose ExpectContinueTimeout is zero sends the body at
// once, so a copy of it with ExpectContinueTimeout set is returned instead
// (the caller's transport is never modified). Any other RoundTripper is
// returned unchanged; it is the caller's to configure.
func waitForContinue(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	t, ok := rt.(*http.Transport)
	if !ok || t.ExpectContinueTimeout > 0 {
		return rt
	}
	c := t.Clone()
	c.ExpectContinueTimeout = ExpectContinueTimeout
	return c
}

// loopback reports a host on the loopback interface, where plain http
// never leaves the machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// maxRedirects caps the redirects one request follows, as net/http's
// default policy does (which a custom CheckRedirect replaces), so a server
// redirecting to itself cannot loop a request forever.
const maxRedirects = 10

// sameOrigin is the client's redirect policy: a redirect to another scheme
// or host is not followed (the 3xx answer is returned, and classified as
// an unexpected status), more than maxRedirects fail the request, then the
// caller's own policy, if any, applies.
func sameOrigin(base *url.URL, next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
			return http.ErrUseLastResponse
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("webdav: stopped after %d redirects", maxRedirects)
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
}

// probeCase PUTs an empty staged-shape (lower-case) file and asks for it
// under its upper-case name: found means the server's storage folds case
// (a NAS on a case-insensitive volume, a server on macOS or Windows), which
// WebDAV cannot report any other way. The probe has a staged name, so one
// left behind by a crash is removed by SweepAbandoned. A refused MKCOL or
// PUT (a read-only account) reports case-sensitive, since such an account
// cannot write two keys that would collide.
func (d *Driver) probeCase(ctx context.Context) (bool, error) {
	if err := d.st.mkcolAll(ctx, d.tmpDir); err != nil {
		if filesystem.CodeOf(err) == filesystem.CodePermission {
			if _, perr := d.propfind(ctx, "", true, depthZero); perr != nil {
				return false, perr // not read-only: the credentials or the endpoint are wrong
			}
			return false, nil
		}
		return false, err
	}
	d.staged.Store(true)
	name := strings.ToLower(rand.Text())
	resp, err := d.do(ctx, http.MethodPut, d.st.urlOf(d.tmpDir+"/"+name, false), http.NoBody, 0, nil)
	if err != nil {
		return false, err
	}
	drain(resp)
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusMethodNotAllowed:
		return false, nil
	case resp.StatusCode/100 != 2:
		return false, statusError(resp, "PUT probe")
	}
	defer func() { _ = d.st.remove(context.WithoutCancel(ctx), d.tmpDir+"/"+name) }()
	_, err = d.st.propfind(ctx, d.tmpDir+"/"+strings.ToUpper(name), false, depthZero)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return false, nil
	}
	return err == nil, err
}

// Native returns the *http.Client, for the filesystem.As escape hatch.
func (d *Driver) Native() *http.Client { return d.client }

// Capabilities reports what WebDAV offers portably. Visibility, metadata
// and conditional writes are not: servers differ, and a MOVE with
// Overwrite: F is a check-then-rename on common servers, not atomic.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Copy:            true,
		Move:            true,
		DeletePrefix:    true,
		SweepAbandoned:  !d.inPlace,
		FindAbandoned:   !d.inPlace,
		Dirs:            true,
		CaseInsensitive: d.caseInsensitive,
	}
}

func (d *Driver) checkKey(key string) error {
	if d.staging != "" && (key == d.staging || strings.HasPrefix(key, d.staging+"/")) {
		return filesystem.Errorf(filesystem.CodeInvalidKey, "webdav: keys under %q are reserved", d.staging)
	}
	return nil
}

// urlOf returns the URL of key (a collection when dir is true). url.URL
// escapes the path when it is rendered, so a key's "?", "#" or "%" stays
// part of its name.
func (d *Driver) urlOf(key string, dir bool) string {
	u := *d.base
	u.Path += key
	if dir && key != "" {
		u.Path += "/"
	}
	return u.String()
}

// do sends one request with the driver's credentials and turns a transport
// failure into CodeUnavailable. The caller closes the response body.
func (d *Driver) do(ctx context.Context, method, target string, body io.Reader, size int64, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	if size > 0 {
		req.ContentLength = size
	}
	for k, v := range header {
		req.Header[k] = v
	}
	d.auth(req)
	resp, err := d.client.Do(req)
	if err != nil {
		var be bodyError
		if errors.As(err, &be) {
			return nil, be.err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return resp, nil
}

// drain discards and closes a response body so the connection is reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

// statusLocked is WebDAV's 423 Locked: another client holds the resource,
// a transient condition.
const statusLocked = 423

// maxErrorBody caps what is read of an error response's body.
const maxErrorBody = 4 << 10

// statusError classifies an unexpected status. what names the request for
// the message.
func statusError(resp *http.Response, what string) error {
	cause := fmt.Errorf("webdav: %s: %s", what, resp.Status)
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return filesystem.NewError(filesystem.CodeNotFound, cause)
	case http.StatusUnauthorized, http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case http.StatusPreconditionFailed:
		return filesystem.NewError(filesystem.CodeExists, cause)
	case http.StatusConflict, http.StatusMethodNotAllowed:
		return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
	case http.StatusRequestEntityTooLarge:
		return filesystem.NewError(filesystem.CodeTooLarge, cause)
	case http.StatusInsufficientStorage:
		return filesystem.NewError(filesystem.CodeQuotaExceeded, cause)
	case http.StatusTooManyRequests, http.StatusRequestTimeout, statusLocked:
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	}
	return filesystem.NewError(filesystem.CodeUnknown, cause)
}

// retryAfter reads a Retry-After header in seconds; 0 when absent or a
// date (which a client may not trust its clock to compare).
func retryAfter(resp *http.Response) time.Duration {
	s, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// Stat implements filesystem.Driver with a PROPFIND of depth 0. A
// collection is not an object (CodeNotFound).
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := d.checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	rs, err := d.propfind(ctx, key, false, depthZero)
	if err != nil {
		return filesystem.Entry{}, err
	}
	for _, r := range rs {
		if !r.dir {
			r.entry.Key = key
			return r.entry, nil
		}
	}
	return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeNotFound, "webdav: %q is not a file", key)
}

// Read implements filesystem.Driver with a GET, ranged with a Range header.
// A server that ignores Range (200) is cut to the range here; 416 (the
// range starts past the end) is an empty read, as the contract asks.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if err := d.checkKey(key); err != nil {
		return nil, err
	}
	h := http.Header{}
	if rng.Offset > 0 || rng.Length > 0 {
		v := "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-"
		if rng.Length > 0 {
			v += strconv.FormatInt(rng.Offset+rng.Length-1, 10)
		}
		h.Set("Range", v)
	}
	resp, err := d.do(ctx, http.MethodGet, d.urlOf(key, false), nil, 0, h)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusRequestedRangeNotSatisfiable:
		drain(resp)
		return io.NopCloser(strings.NewReader("")), nil
	case http.StatusOK:
		if _, err := io.CopyN(io.Discard, resp.Body, rng.Offset); err != nil && err != io.EOF {
			_ = resp.Body.Close()
			return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		var r io.Reader = resp.Body
		if rng.Length > 0 {
			r = io.LimitReader(resp.Body, rng.Length)
		}
		return readCloser{Reader: r, Closer: resp.Body}, nil
	}
	defer drain(resp)
	return nil, statusError(resp, "GET "+key)
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Write implements filesystem.Driver: PUT to a staged name, then MOVE into
// place, creating the staging collection and the key's parents when the
// server reports them missing (409).
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	if err := d.checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	if d.inPlace {
		return d.writeInPlace(ctx, key, body, opts.SizeHint)
	}
	tmp := d.tmpDir + "/" + strings.ToLower(rand.Text()) // see stagedName
	if err := d.stage(ctx, tmp, body, opts.SizeHint); err != nil {
		return filesystem.Entry{}, err
	}
	if err := d.relocate(ctx, "MOVE", d.st, tmp, key); err != nil {
		_ = d.st.remove(context.WithoutCancel(ctx), tmp)
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, key)
}

// writeInPlace is Write under Config.WriteInPlace: the key's parents are
// made first (the body can be sent only once), then the body is PUT
// straight to the key. A failed PUT deletes the key.
func (d *Driver) writeInPlace(ctx context.Context, key string, body io.Reader, size int64) (filesystem.Entry, error) {
	if dir := path.Dir(key); dir != "." {
		if err := d.mkcolAll(ctx, dir); err != nil {
			return filesystem.Entry{}, err
		}
	}
	resp, err := d.do(ctx, http.MethodPut, d.urlOf(key, false), bodyReader{body}, size, expectContinue)
	if err != nil {
		d.removeFailedPut(context.WithoutCancel(ctx), key)
		return filesystem.Entry{}, err
	}
	drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return filesystem.Entry{}, statusError(resp, "PUT "+key)
	}
	return d.Stat(ctx, key)
}

// Bounds of removeFailedPut's retries: long enough for a server to notice
// the aborted body and end its PUT, short enough not to stall the caller.
const (
	failedPutRemoveAttempts = 20
	failedPutRemoveWait     = 50 * time.Millisecond
)

// removeFailedPut deletes what a failed in-place PUT left at key, best
// effort. The server's handler for the aborted PUT can still hold the
// resource's lock when the DELETE arrives (it has not yet noticed the
// client went away), and answers 423, so a CodeUnavailable answer is
// retried a bounded number of times; without that, the partial file the
// WriteInPlace contract promises to remove would stay.
func (d *Driver) removeFailedPut(ctx context.Context, key string) {
	for range failedPutRemoveAttempts {
		if err := d.remove(ctx, key); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
			return
		}
		time.Sleep(failedPutRemoveWait)
	}
}

// stage PUTs body to tmp. The body can be sent only once, so the staging
// collection is created before the first PUT rather than after a 409; a
// PUT that still finds it missing (someone removed it) forgets that it was
// made, fails with CodeUnavailable, and the caller's retry recreates it.
func (d *Driver) stage(ctx context.Context, tmp string, body io.Reader, size int64) error {
	if !d.staged.Load() {
		if err := d.st.mkcolAll(ctx, d.tmpDir); err != nil {
			return err
		}
		d.staged.Store(true)
	}
	resp, err := d.do(ctx, http.MethodPut, d.st.urlOf(tmp, false), bodyReader{body}, size, expectContinue)
	if err != nil {
		_ = d.st.remove(context.WithoutCancel(ctx), tmp)
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusConflict {
		d.staged.Store(false)
		return filesystem.Errorf(filesystem.CodeUnavailable, "webdav: staging collection %s is missing; retry", d.tmpDir)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		_ = d.st.remove(context.WithoutCancel(ctx), tmp)
		return statusError(resp, "PUT")
	}
	return nil
}

// bodyReader marks the caller's body failing, so do returns that error
// rather than the transport's description of it, and never mistakes a
// broken upload stream for an unavailable server.
type bodyReader struct{ r io.Reader }

func (b bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		err = bodyError{err}
	}
	return n, err
}

// bodyError marks a failure of the caller's body inside a transport error.
type bodyError struct{ err error }

func (b bodyError) Error() string { return b.err.Error() }
func (b bodyError) Unwrap() error { return b.err }

// mkcolAll creates dir and its parents, from the top. 405 (it exists) is
// success; 409 under a file is CodeInvalidArgument.
func (d *Driver) mkcolAll(ctx context.Context, dir string) error {
	// Servers lock a resource for the length of a request and answer a
	// second MKCOL of it meanwhile with 423, so this process's MKCOLs go
	// one at a time; another process's are CodeUnavailable to retry.
	d.mkcol.Lock()
	defer d.mkcol.Unlock()
	parts := strings.Split(dir, "/")
	for i := range parts {
		sub := strings.Join(parts[:i+1], "/")
		resp, err := d.do(ctx, "MKCOL", d.urlOf(sub, true), nil, 0, nil)
		if err != nil {
			return err
		}
		drain(resp)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusOK {
			return statusError(resp, "MKCOL "+sub)
		}
	}
	return nil
}

// relocate MOVEs or COPYs src (a key of from: d, or d.st for a staged
// file) onto dst with Overwrite: T, creating dst's parents and retrying
// once when the server reports them missing.
func (d *Driver) relocate(ctx context.Context, method string, from *Driver, src, dst string) error {
	for attempt := 0; ; attempt++ {
		h := http.Header{}
		h.Set("Destination", d.urlOf(dst, false))
		h.Set("Overwrite", "T")
		if method == "COPY" {
			h.Set("Depth", depthZero)
		}
		resp, err := d.do(ctx, method, from.urlOf(src, false), nil, 0, h)
		if err != nil {
			return err
		}
		drain(resp)
		switch {
		case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK:
			return nil
		case (resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusForbidden) && attempt == 0 && path.Dir(dst) != ".":
			// RFC 4918 answers a missing parent with 409; some servers
			// (golang.org/x/net/webdav among them) with 403. Either way
			// the parents are made and the request tried once more; a
			// real refusal fails the MKCOL or the retry.
			if err := d.mkcolAll(ctx, path.Dir(dst)); err != nil {
				return err
			}
			continue
		}
		return statusError(resp, method+" "+src)
	}
}

// remove DELETEs target; a missing target is success.
func (d *Driver) remove(ctx context.Context, target string) error {
	return d.delete(ctx, d.urlOf(target, false))
}

func (d *Driver) delete(ctx context.Context, target string) error {
	resp, err := d.do(ctx, http.MethodDelete, target, nil, 0, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return statusError(resp, "DELETE")
}

// Delete implements filesystem.Driver. A collection at key is not an
// object, so it is left alone.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if err := d.checkKey(key); err != nil {
		return err
	}
	if _, err := d.Stat(ctx, key); err != nil {
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil
		}
		return err
	}
	return d.remove(ctx, key)
}

// Copy implements filesystem.Copier with a server-side COPY.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	return d.copyMove(ctx, "COPY", src, dst)
}

// Move implements filesystem.Mover with a server-side MOVE.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	return d.copyMove(ctx, "MOVE", src, dst)
}

func (d *Driver) copyMove(ctx context.Context, method, src, dst string) (filesystem.Entry, error) {
	for _, k := range []string{src, dst} {
		if err := d.checkKey(k); err != nil {
			return filesystem.Entry{}, err
		}
	}
	if _, err := d.Stat(ctx, src); err != nil {
		return filesystem.Entry{}, err
	}
	if src != dst {
		if err := d.relocate(ctx, method, d, src, dst); err != nil {
			return filesystem.Entry{}, err
		}
	}
	return d.Stat(ctx, dst)
}

// List implements filesystem.Driver by walking collections with depth-1
// PROPFINDs (depth infinity is disabled on many servers), O(n) in the
// files under dir per page like the local driver.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	if err := d.checkKey(dir); err != nil && dir != "" {
		return filesystem.Page{}, err
	}
	var all []filesystem.Entry
	queue := []string{dir}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		rs, err := d.propfind(ctx, cur, true, depthOne)
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			continue
		}
		if err != nil {
			return filesystem.Page{}, err
		}
		for _, r := range rs {
			k := r.entry.Key
			if k == cur || !filesystem.KeyInDir(k, cur) || k == d.staging {
				continue
			}
			if r.dir {
				queue = append(queue, k)
			} else if filesystem.KeyInDir(k, dir) {
				all = append(all, r.entry)
			}
		}
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// DeletePrefix implements filesystem.PrefixDeleter with one recursive
// DELETE of the collection; the root is emptied child by child, keeping the
// staging collection.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	if dir != "" {
		if err := d.checkKey(dir); err != nil {
			return err
		}
		rs, err := d.propfind(ctx, dir, true, depthZero)
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if len(rs) == 0 || !rs[0].dir {
			return nil // a file named dir is not inside dir
		}
		return d.delete(ctx, d.urlOf(dir, true))
	}
	rs, err := d.propfind(ctx, "", true, depthOne)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.entry.Key == "" || r.entry.Key == d.staging {
			continue
		}
		if err := d.delete(ctx, d.urlOf(r.entry.Key, r.dir)); err != nil {
			return err
		}
	}
	return nil
}

// SweepAbandoned implements filesystem.AbandonedSweeper: it removes staged
// files a Write left in the staging collection's tmp when the process died between the
// PUT and the MOVE and whose last modification is before before. Only files
// named the way Write names them are removed.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := d.scanAbandoned(ctx, before, func(r resource) error {
		if err := d.st.remove(ctx, r.entry.Key); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// FindAbandoned implements filesystem.AbandonedFinder: the staged files
// SweepAbandoned would remove, found by the same scan, removing nothing.
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	var found []filesystem.Abandoned
	err := d.scanAbandoned(ctx, before, func(r resource) error {
		found = append(found, filesystem.Abandoned{Kind: filesystem.AbandonedStaging, ID: path.Base(r.entry.Key), LastActivity: r.entry.ModTime})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// scanAbandoned calls visit for every staged file directly in
// the staging collection's tmp, named the way Write names them and last modified before
// before; a missing staging collection holds none. An error from visit
// ends the scan. It is the one definition of an abandoned staged file
// both SweepAbandoned and FindAbandoned use.
func (d *Driver) scanAbandoned(ctx context.Context, before time.Time, visit func(resource) error) error {
	if d.inPlace {
		return filesystem.Errorf(filesystem.CodeUnsupported, "webdav: WriteInPlace stages nothing, so there is nothing to sweep")
	}
	rs, err := d.st.propfind(ctx, d.tmpDir, true, depthOne)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range rs {
		name := path.Base(r.entry.Key)
		if r.dir || path.Dir(r.entry.Key) != d.tmpDir || !stagedName.MatchString(name) || !r.entry.ModTime.Before(before) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	return nil
}

// MakeDir implements filesystem.DirMaker with MKCOL, from the top. A file
// at dir is CodeExists (checked first: MKCOL answers 405 for any existing
// resource, file or collection); a file at one of its parents makes dir
// impossible (CodeInvalidArgument). Deleting files never removes a
// collection, so a made directory lasts until DeletePrefix removes it or a
// parent.
func (d *Driver) MakeDir(ctx context.Context, dir string) error {
	if err := d.checkKey(dir); err != nil {
		return err
	}
	isDir, err := d.collection(ctx, dir)
	switch {
	case err == nil && isDir:
		return nil
	case err == nil:
		return filesystem.Errorf(filesystem.CodeExists, "webdav: %q is a file, not a directory", dir)
	case errors.Is(err, errUnderFile):
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	case filesystem.CodeOf(err) != filesystem.CodeNotFound:
		return err
	}
	return d.mkcolAll(ctx, dir)
}

// DirExists implements filesystem.DirMaker: whether dir is a collection on
// the server. A file at dir, or a path the server does not have, is not
// one.
func (d *Driver) DirExists(ctx context.Context, dir string) (bool, error) {
	if err := d.checkKey(dir); err != nil {
		return false, err
	}
	isDir, err := d.collection(ctx, dir)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return false, nil
	}
	return isDir, err
}

// errUnderFile marks a directory that cannot exist because a parent is a
// file, so MakeDir refuses it rather than MKCOL a path some servers
// answer with a misleading success.
var errUnderFile = errors.New("a parent is a file")

// collection reports whether dir exists as a collection (true) or a file
// (false); a missing one is CodeNotFound. A path through a file is missing
// too, though servers answer it differently: 404 or 409, or 405 from
// golang.org/x/net/webdav, which classifies as CodeInvalidArgument; so on
// that answer the parent is examined, and a parent that is a file or
// missing makes dir missing.
func (d *Driver) collection(ctx context.Context, dir string) (bool, error) {
	rs, err := d.propfind(ctx, dir, false, depthZero)
	if filesystem.CodeOf(err) == filesystem.CodeInvalidArgument && path.Dir(dir) != "." {
		parentDir, perr := d.collection(ctx, path.Dir(dir))
		if (perr == nil && !parentDir) || filesystem.CodeOf(perr) == filesystem.CodeNotFound {
			return false, filesystem.NewError(filesystem.CodeNotFound, fmt.Errorf("webdav: %q: %w", dir, errUnderFile))
		}
	}
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if r.entry.Key == dir {
			return r.dir, nil
		}
	}
	return false, filesystem.Errorf(filesystem.CodeNotFound, "webdav: the server did not describe %q", dir)
}
