// Package onedrive is a filesystem.Driver over Microsoft Graph drives:
// OneDrive (personal and for Business) and SharePoint document libraries.
// It speaks the Graph REST API as plain JSON over net/http (the Graph Go
// SDK is a generated client of several hundred packages; the driver needs
// eight requests).
//
// Keys map to paths under Config.Root in the drive Config.Drive names.
// OneDrive paths are case-insensitive, so the driver declares
// CaseInsensitive. Characters OneDrive refuses in a name (" * : < > ? \ |
// #) and a segment ending in a dot or a space are stored percent-escaped
// (filesystem.KeyEscaper) and decoded back, so applications see the keys
// they wrote. Graph creates missing parent folders on upload.
//
// A body shorter than Config.ChunkSize is one PUT; a longer one goes
// through an upload session, committed by its last fragment, so a reader
// never sees a partial file. IfNotExists is conflictBehavior=fail, which
// Graph checks again when a session commits. IfMatch is an If-Match header
// on the item's eTag; Graph checks it only when a request (or a session)
// starts, so it is honoured for bodies shorter than one chunk and refused
// with CodeUnsupported for longer ones.
//
// The application supplies OAuth through Config.Token. The token is added
// by the driver's transport only to requests for the Graph origin: the
// pre-authenticated download and upload-session URLs Graph hands out
// (other hosts) never receive it, as Graph requires. Config prints and
// logs without the token source's contents.
package onedrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// DefaultBaseURL is the Graph v1.0 endpoint, the default of
// Config.BaseURL.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// DefaultDrive is the signed-in user's OneDrive, the default of
// Config.Drive.
const DefaultDrive = "/me/drive"

// Upload fragments must be multiples of FragmentUnit (320 KiB) and at most
// MaxChunkSize (60 MiB), per the Graph documentation.
const (
	FragmentUnit     = 320 << 10
	DefaultChunkSize = 32 * FragmentUnit // 10 MiB
	MaxChunkSize     = 192 * FragmentUnit
)

// DefaultTimeout bounds each request of the client New makes when
// Config.Client is nil; a request carries at most one chunk.
const DefaultTimeout = 5 * time.Minute

// TokenSource supplies the OAuth 2.0 access token for Graph. It is called
// once per request, so a refreshing source keeps a long-lived driver
// working; it must be safe for concurrent use.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// TokenFunc adapts a function to TokenSource (an oauth2.TokenSource ts
// becomes a func returning ts.Token()'s AccessToken).
type TokenFunc func(ctx context.Context) (string, error)

// AccessToken implements TokenSource.
func (f TokenFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

// Config configures a Driver.
type Config struct {
	// Token supplies the access token. Required.
	Token TokenSource
	// Drive is the Graph path of the drive: "/me/drive" (the default),
	// "/drives/{drive-id}", "/users/{id}/drive" or "/sites/{site-id}/drive".
	Drive string
	// Root is the folder holding the disk, such as "Apps/Invoices"; empty
	// means the drive's root.
	Root string
	// ChunkSize is the size of each upload request: a multiple of
	// FragmentUnit, at most MaxChunkSize; 0 means DefaultChunkSize. A
	// write holds up to two chunks in memory: one for a body shorter than a
	// chunk, two for an upload session (the fragment being sent and the
	// next, read ahead so the final fragment is known as final).
	ChunkSize int
	// Client sends the requests; nil means a client with DefaultTimeout.
	// The driver uses a copy whose transport adds the token for the Graph
	// origin only.
	Client *http.Client
	// BaseURL overrides DefaultBaseURL, for a national cloud or a test
	// server; https, or http on loopback only.
	BaseURL string
}

// String implements fmt.Stringer without the token source's contents.
func (c Config) String() string {
	return fmt.Sprintf("onedrive.Config{Drive: %q, Root: %q, Token: %s}", c.Drive, c.Root, tokenState(c.Token))
}

// GoString implements fmt.GoStringer like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb prints the token source.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer without the token source's contents.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("drive", c.Drive), slog.String("root", c.Root), slog.String("token", tokenState(c.Token)))
}

func tokenState(ts TokenSource) string {
	if ts == nil {
		return "none"
	}
	return "[redacted]"
}

// escaper encodes what OneDrive refuses in a name.
var escaper = filesystem.KeyEscaper{Chars: `"*:<>?\|#`, SegmentEnd: ". "}

// Driver is the Graph drive backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	base   *url.URL
	drive  string // base + drive path, without a trailing slash
	root   string // folder path inside the drive, no leading or trailing slash
	chunk  int
	client *http.Client
}

// New checks cfg and returns a driver. It makes no request.
func New(cfg Config) (*Driver, error) {
	if cfg.Token == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "onedrive: Config.Token is required")
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || base.Host == "" || base.User != nil || !secure(base) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "onedrive: BaseURL %q must be an https URL (http only on loopback)", raw)
	}
	drive := cfg.Drive
	if drive == "" {
		drive = DefaultDrive
	}
	if !strings.HasPrefix(drive, "/") || strings.Contains(drive, "..") || strings.ContainsAny(drive, "?#") {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "onedrive: Drive must be a Graph path such as /me/drive")
	}
	chunk := cfg.ChunkSize
	switch {
	case chunk == 0:
		chunk = DefaultChunkSize
	case chunk < 0 || chunk > MaxChunkSize || chunk%FragmentUnit != 0:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "onedrive: ChunkSize must be a multiple of %d up to %d", FragmentUnit, MaxChunkSize)
	}
	client := &http.Client{Timeout: DefaultTimeout}
	if cfg.Client != nil {
		c := *cfg.Client
		client = &c
	}
	client.Transport = &authTransport{origin: base.Scheme + "://" + base.Host, next: client.Transport, token: cfg.Token.AccessToken}
	return &Driver{
		base:   base,
		drive:  base.String() + strings.TrimSuffix(drive, "/"),
		root:   strings.Trim(cfg.Root, "/"),
		chunk:  chunk,
		client: client,
	}, nil
}

func secure(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// authTransport adds the bearer token to requests for origin and to no
// other: Graph's download redirects and upload-session URLs are
// pre-authenticated and must not receive it.
type authTransport struct {
	origin string
	next   http.RoundTripper
	// token is a method value; fmt prints a func as an address.
	token func(context.Context) (string, error)
}

// errToken marks a failing token source inside a transport error.
var errToken = errors.New("onedrive: token source failed")

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	if req.URL.Scheme+"://"+req.URL.Host != t.origin {
		req.Header.Del("Authorization")
		return next.RoundTrip(req)
	}
	tok, err := t.token(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %w", errToken, err)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return next.RoundTrip(r)
}

// Native returns the *http.Client (its transport adds the token for the
// Graph origin), for the filesystem.As escape hatch: call Graph endpoints
// the driver does not, such as sharing links or permissions.
func (d *Driver) Native() *http.Client { return d.client }

// Capabilities reports what Graph offers. No Copy: Graph copies
// asynchronously behind a monitor URL, so the Disk streams instead. No
// presigning: Graph's pre-authenticated URLs cannot be given an expiry.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Move:             true,
		DeletePrefix:     true,
		ConditionalWrite: true,
		CompareAndSwap:   true,
		CaseInsensitive:  true,
	}
}

// itemURL is the Graph URL of key's item (the root folder for ""), with
// suffix appended (":/content", ":/children", or ":" for the item).
func (d *Driver) itemURL(key, suffix string) string {
	p := d.root
	if key != "" {
		p = strings.Trim(p+"/"+escaper.EncodeKey(key), "/")
	}
	if p == "" {
		return d.drive + "/root" + strings.TrimPrefix(suffix, ":")
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return d.drive + "/root:/" + strings.Join(segs, "/") + suffix
}

// graphError is the body of a Graph error answer.
type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const maxErrorBody = 8 << 10

// maxJSONBody caps what is read of a JSON answer, far above the largest
// page the API sends, so a hostile or broken server cannot make the
// driver buffer without bound.
const maxJSONBody = 16 << 20

// statusError classifies an error answer.
func statusError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e graphError
	_ = json.Unmarshal(raw, &e)
	cause := fmt.Errorf("onedrive: %s: %s %s", what, resp.Status, e.Error.Code)
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return filesystem.NewError(filesystem.CodeNotFound, cause)
	case http.StatusConflict:
		return filesystem.NewError(filesystem.CodeExists, cause)
	case http.StatusPreconditionFailed:
		return filesystem.NewError(filesystem.CodeConflict, cause)
	case http.StatusUnauthorized, http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case http.StatusRequestEntityTooLarge:
		return filesystem.NewError(filesystem.CodeTooLarge, cause)
	case http.StatusInsufficientStorage:
		return filesystem.NewError(filesystem.CodeQuotaExceeded, cause)
	case http.StatusBadRequest:
		return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
	case http.StatusTooManyRequests:
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	}
	return filesystem.NewError(filesystem.CodeUnknown, cause)
}

func retryAfter(resp *http.Response) time.Duration {
	s, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// do sends one request; the caller closes the response body.
func (d *Driver) do(ctx context.Context, method, target string, body io.Reader, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, redactURLError(err, d.graphOrigin))
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := d.client.Do(req)
	err = redactURLError(err, d.graphOrigin)
	switch {
	case err == nil:
		return resp, nil
	case errors.Is(err, errToken):
		return nil, filesystem.NewError(filesystem.CodePermission, err)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
}

// graphOrigin reports the Graph origin, whose paths name items rather
// than carry credentials.
func (d *Driver) graphOrigin(origin string) bool { return origin == d.base.Scheme+"://"+d.base.Host }

// json sends a request with an optional JSON body and decodes a 2xx
// answer into out (nil discards it).
func (d *Driver) json(ctx context.Context, method, target string, in, out any, header http.Header) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		body = bytes.NewReader(raw)
		if header == nil {
			header = http.Header{}
		}
		header.Set("Content-Type", "application/json")
	}
	resp, err := d.do(ctx, method, target, body, header)
	if err != nil {
		return err
	}
	return decode(resp, method, out)
}

func decode(resp *http.Response, what string, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return statusError(resp, what)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out); err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("onedrive: %s: decoding the answer: %w", what, err))
	}
	return nil
}

// item is a driveItem as Graph describes it.
type item struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"lastModifiedDateTime"`
	ETag     string    `json:"eTag"`
	Folder   *struct{} `json:"folder"`
	File     *struct{} `json:"file"`
}

func entry(key string, it item) filesystem.Entry {
	return filesystem.Entry{Key: key, Size: it.Size, ModTime: it.Modified, ETag: strings.Trim(it.ETag, `"`)}
}

// Stat implements filesystem.Driver. A folder is not an object.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	var it item
	if err := d.json(ctx, http.MethodGet, d.itemURL(key, ":"), nil, &it, nil); err != nil {
		return filesystem.Entry{}, err
	}
	if it.File == nil {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeNotFound, "onedrive: %q is not a file", key)
	}
	return entry(key, it), nil
}

// Read implements filesystem.Driver with GET .../content, which Graph
// answers with a redirect to a pre-authenticated URL; the Range header
// travels with the redirect, the token does not.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	h := http.Header{}
	if rng.Offset > 0 || rng.Length > 0 {
		v := "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-"
		if rng.Length > 0 {
			v += strconv.FormatInt(rng.Offset+rng.Length-1, 10)
		}
		h.Set("Range", v)
	}
	resp, err := d.do(ctx, http.MethodGet, d.itemURL(key, ":/content"), nil, h)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return io.NopCloser(strings.NewReader("")), nil
	case http.StatusOK:
		if _, err := io.CopyN(io.Discard, resp.Body, rng.Offset); err != nil && err != io.EOF {
			_ = resp.Body.Close()
			return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		if rng.Length > 0 {
			return readCloser{io.LimitReader(resp.Body, rng.Length), resp.Body}, nil
		}
		return resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil, statusError(resp, "GET content")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Graph's @microsoft.graph.conflictBehavior values: what a create, upload
// or move does when the target name exists. conflictFail turns that into a
// 409 (mapped to CodeExists); conflictReplace overwrites it.
const (
	conflictFail    = "fail"
	conflictReplace = "replace"
)

// conflictBehavior is Graph's answer to a name that exists.
func conflictBehavior(opts filesystem.WriteOptions) string {
	if opts.IfNotExists {
		return conflictFail
	}
	return conflictReplace
}

// itemRef is Graph's parentReference: the folder an item moves into, as a
// drive path ("/drive/root" or "/drive/root:/a/b").
type itemRef struct {
	Path string `json:"path"`
}

// moveRequest is the PATCH body that renames and reparents an item.
type moveRequest struct {
	ParentReference itemRef `json:"parentReference"`
	Name            string  `json:"name"`
}

// folderFacet marks a created item as a folder; Graph wants an empty object.
type folderFacet struct{}

// createFolderRequest is the POST body that creates one folder. Its
// conflict behavior is always conflictFail so an existing folder answers
// 409, which mkdirAll treats as success.
type createFolderRequest struct {
	Name             string      `json:"name"`
	Folder           folderFacet `json:"folder"`
	ConflictBehavior string      `json:"@microsoft.graph.conflictBehavior"`
}

// uploadSessionRequest is the createUploadSession body; uploadItem carries
// the conflict behavior the finished upload applies.
type uploadSessionRequest struct {
	Item uploadItem `json:"item"`
}

type uploadItem struct {
	ConflictBehavior string `json:"@microsoft.graph.conflictBehavior"`
}

// condition is the If-Match header of a compare-and-swap write.
func condition(opts filesystem.WriteOptions) http.Header {
	h := http.Header{}
	if opts.IfMatch != "" {
		h.Set("If-Match", `"`+opts.IfMatch+`"`)
	}
	return h
}

// writeError maps a refused conditional write: with IfMatch, a missing
// item (404) or a changed one (412, 409) is CodeConflict.
func writeError(err error, opts filesystem.WriteOptions) error {
	if opts.IfMatch == "" {
		return err
	}
	switch filesystem.CodeOf(err) {
	case filesystem.CodeNotFound, filesystem.CodeExists:
		return filesystem.NewError(filesystem.CodeConflict, err)
	}
	return err
}

// Write implements filesystem.Driver (see the package comment).
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	buf := make([]byte, d.chunk)
	n, last, err := readChunk(body, buf)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if last {
		return d.put(ctx, key, buf[:n], opts)
	}
	if opts.IfMatch != "" {
		// Graph checks If-Match when an upload session is created, not
		// when its last fragment commits, so a swap through a session
		// would not be atomic.
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnsupported, "onedrive: IfMatch applies only to bodies shorter than ChunkSize (%d bytes); Graph does not re-check it when an upload session commits", d.chunk)
	}
	return d.session(ctx, key, body, buf[:n], opts)
}

// put uploads a whole small body in one request.
func (d *Driver) put(ctx context.Context, key string, data []byte, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	target := d.itemURL(key, ":/content") + "?@microsoft.graph.conflictBehavior=" + conflictBehavior(opts)
	h := condition(opts)
	h.Set("Content-Type", "application/octet-stream")
	resp, err := d.do(ctx, http.MethodPut, target, bytes.NewReader(data), h)
	if err != nil {
		return filesystem.Entry{}, err
	}
	var it item
	if err := decode(resp, "PUT content", &it); err != nil {
		return filesystem.Entry{}, writeError(err, opts)
	}
	return entry(key, it), nil
}

// session uploads a body longer than one chunk through an upload session.
// first holds the first chunk, already read and full. The total size goes
// in each fragment's Content-Range: opts.SizeHint when known, else "*"
// until the last fragment, which states it. A failure cancels the session,
// so nothing is committed.
func (d *Driver) session(ctx context.Context, key string, body io.Reader, first []byte, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	var created struct {
		UploadURL string `json:"uploadUrl"`
	}
	in := uploadSessionRequest{Item: uploadItem{ConflictBehavior: conflictBehavior(opts)}}
	if err := d.json(ctx, http.MethodPost, d.itemURL(key, ":/createUploadSession"), in, &created, nil); err != nil {
		return filesystem.Entry{}, writeError(err, opts)
	}
	u, err := url.Parse(created.UploadURL)
	if err != nil || !secure(u) || u.User != nil {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "onedrive: the upload session URL is not https")
	}
	cancel := func() {
		if resp, err := d.do(context.WithoutCancel(ctx), http.MethodDelete, created.UploadURL, nil, nil); err == nil {
			_ = resp.Body.Close()
		}
	}
	total := "*"
	if opts.SizeHint > 0 {
		total = strconv.FormatInt(opts.SizeHint, 10)
	}
	cur, spare := first, make([]byte, len(first))
	var offset int64
	for {
		// Read ahead, so the fragment that ends the body is known as the
		// final one and carries the total.
		m, last, err := readChunk(body, spare)
		if err != nil {
			cancel()
			return filesystem.Entry{}, err
		}
		final := last && m == 0
		if final {
			total = strconv.FormatInt(offset+int64(len(cur)), 10)
		}
		resp, err := d.fragment(ctx, created.UploadURL, cur, offset, total)
		if err != nil {
			cancel()
			return filesystem.Entry{}, err
		}
		if final {
			var it item
			if err := decode(resp, "upload fragment", &it); err != nil {
				return filesystem.Entry{}, writeError(err, opts)
			}
			return entry(key, it), nil
		}
		if err := decode(resp, "upload fragment", nil); err != nil {
			cancel()
			return filesystem.Entry{}, err
		}
		offset += int64(len(cur))
		cur, spare = spare[:m], cur[:cap(cur)]
		if last {
			total = strconv.FormatInt(offset+int64(m), 10)
			resp, err := d.fragment(ctx, created.UploadURL, cur, offset, total)
			if err != nil {
				cancel()
				return filesystem.Entry{}, err
			}
			var it item
			if err := decode(resp, "upload fragment", &it); err != nil {
				return filesystem.Entry{}, writeError(err, opts)
			}
			return entry(key, it), nil
		}
	}
}

// fragment PUTs one fragment to the session URL (which takes no token).
func (d *Driver) fragment(ctx context.Context, target string, data []byte, offset int64, total string) (*http.Response, error) {
	h := http.Header{}
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%s", offset, offset+int64(len(data))-1, total))
	return d.do(ctx, http.MethodPut, target, bytes.NewReader(data), h)
}

// readChunk fills buf from body. last reports that body ended within it
// (cleanly, with io.EOF); any other error is the body's, returned as is,
// io.ErrUnexpectedEOF included, so a truncated upload is never committed.
func readChunk(body io.Reader, buf []byte) (n int, last bool, err error) {
	for n < len(buf) {
		m, err := body.Read(buf[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return n, true, nil
		}
		if err != nil {
			return n, false, err
		}
	}
	return n, false, nil
}

// Delete implements filesystem.Driver. A folder at key is left alone; a
// missing key succeeds.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if _, err := d.Stat(ctx, key); err != nil {
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil
		}
		return err
	}
	return d.remove(ctx, key)
}

func (d *Driver) remove(ctx context.Context, key string) error {
	err := d.json(ctx, http.MethodDelete, d.itemURL(key, ":"), nil, nil, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

// Move implements filesystem.Mover with a PATCH of the item's parent and
// name, replacing an existing dst; missing parent folders of dst are
// created first.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if _, err := d.Stat(ctx, src); err != nil {
		return filesystem.Entry{}, err
	}
	if src == dst {
		return d.Stat(ctx, dst)
	}
	parent := path.Dir(dst)
	if parent == "." {
		parent = ""
	}
	if err := d.mkdirAll(ctx, parent); err != nil {
		return filesystem.Entry{}, err
	}
	parentPath := "/drive/root"
	if p := strings.Trim(d.root+"/"+escaper.EncodeKey(parent), "/"); parent != "" || d.root != "" {
		parentPath += ":/" + p
	}
	in := moveRequest{ParentReference: itemRef{Path: parentPath}, Name: escaper.EncodeKey(path.Base(dst))}
	if err := d.json(ctx, http.MethodPatch, d.itemURL(src, ":")+"?@microsoft.graph.conflictBehavior="+conflictReplace, in, nil, nil); err != nil {
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, dst)
}

// mkdirAll creates dir and its parents under the root, from the top; a
// folder that exists (409) is success.
func (d *Driver) mkdirAll(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		parent := strings.Join(parts[:i], "/")
		in := createFolderRequest{Name: escaper.EncodeKey(parts[i]), ConflictBehavior: conflictFail}
		err := d.json(ctx, http.MethodPost, d.itemURL(parent, ":/children"), in, nil, nil)
		if err != nil && filesystem.CodeOf(err) != filesystem.CodeExists {
			return err
		}
	}
	return nil
}

type children struct {
	Value    []item `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// listPageSize is the $top asked of each children request.
const listPageSize = 1000

// List implements filesystem.Driver by walking folders with children
// requests, following @odata.nextLink within the Graph origin, then pages
// with filesystem.PageOf.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	var all []filesystem.Entry
	queue := []string{dir}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next := d.itemURL(cur, ":/children") + "?$top=" + strconv.Itoa(listPageSize)
		for next != "" {
			var page children
			err := d.json(ctx, http.MethodGet, next, nil, &page, nil)
			if filesystem.CodeOf(err) == filesystem.CodeNotFound {
				break
			}
			if err != nil {
				return filesystem.Page{}, err
			}
			for _, it := range page.Value {
				name, err := escaper.DecodeKey(it.Name)
				if err != nil || name == "" || strings.Contains(name, "/") {
					continue // not a name this driver wrote
				}
				k := name
				if cur != "" {
					k = cur + "/" + name
				}
				switch {
				case it.Folder != nil:
					queue = append(queue, k)
				case it.File != nil:
					all = append(all, entry(k, it))
				}
			}
			next = page.NextLink
			if next != "" && !strings.HasPrefix(next, d.base.Scheme+"://"+d.base.Host+"/") {
				return filesystem.Page{}, filesystem.Errorf(filesystem.CodeUnavailable, "onedrive: nextLink leaves the Graph origin")
			}
		}
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// DeletePrefix implements filesystem.PrefixDeleter with one DELETE of the
// folder. A file named dir is not inside dir and is left alone.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	var it item
	err := d.json(ctx, http.MethodGet, d.itemURL(dir, ":"), nil, &it, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if it.Folder == nil {
		return nil
	}
	return d.remove(ctx, dir)
}
