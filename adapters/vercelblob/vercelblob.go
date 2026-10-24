// Package vercelblob is a filesystem.Driver over Vercel Blob, spoken over
// its REST API on net/http (Vercel ships only a JavaScript SDK).
//
// A key is a blob's pathname. Writes PUT the body to the API with no
// random suffix and overwriting allowed, streamed in one request; reads GET
// the blob's own URL on the store's host (https://<store>.<access>.blob.
// vercel-storage.com/<key>). Blobs are served through Vercel's CDN, so
// after an overwrite or a delete a read of a public blob can return the
// old bytes until the CDN refreshes (up to a minute, per Vercel); reads of
// private blobs ask for an uncached copy. Stat and List go to the API and
// are current.
//
// The read-write token is the store's only credential: Config prints and
// logs with it redacted, the driver keeps it inside a func, and its
// transport sends it only to the API origin and, for a private store, the
// store's blob host.
package vercelblob

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
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// DefaultAPIURL is the Vercel Blob API, the default of Config.APIURL.
const DefaultAPIURL = "https://vercel.com/api/blob"

// apiVersion is the x-api-version the driver speaks (that of @vercel/blob
// 2.x, whose requests it mirrors).
const apiVersion = "12"

// Access is a store's access mode, fixed when the store is created.
type Access string

// The two access modes.
const (
	// AccessPublic: every blob is readable by anyone with its URL, which
	// the driver offers as its PublicURL.
	AccessPublic Access = "public"
	// AccessPrivate: blobs are read with the token.
	AccessPrivate Access = "private"
)

// tokenPrefix starts every read-write token; the store ID is its fourth
// "_"-separated field (vercel_blob_rw_<store>_<secret>).
const tokenPrefix = "vercel_blob_rw_"

// DefaultTimeout bounds each request of the client New makes when
// Config.Client is nil.
const DefaultTimeout = 10 * time.Minute

// Config configures a Driver.
type Config struct {
	// Token is the store's read-write token (BLOB_READ_WRITE_TOKEN).
	// Required.
	Token string
	// Access must match the store's mode. Required: a mismatch makes
	// every write fail.
	Access Access
	// Client sends the requests; nil means a client with DefaultTimeout.
	// The driver uses a copy whose transport adds the token for the API
	// and blob hosts only, and which follows no redirect.
	Client *http.Client
	// APIURL and BlobURL override the API and the store's blob host
	// (derived from the token), for a proxy or a test server; https, or
	// http on loopback only.
	APIURL, BlobURL string
	// CacheMaxAge is how long Vercel's CDN and browsers may cache a blob
	// this disk writes (x-cache-control-max-age), in whole seconds and at
	// least MinCacheMaxAge; 0 sends nothing, leaving Vercel's default of
	// one month. Shorten it for blobs that are overwritten, so readers of
	// a public blob stop seeing old bytes sooner after a write.
	CacheMaxAge time.Duration
}

// MinCacheMaxAge is the shortest Config.CacheMaxAge Vercel accepts.
const MinCacheMaxAge = time.Minute

const redacted = "[redacted]"

// String implements fmt.Stringer with the token redacted.
func (c Config) String() string {
	return fmt.Sprintf("vercelblob.Config{Access: %q, Token: %s}", c.Access, redact(c.Token))
}

// GoString implements fmt.GoStringer like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb prints the token.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer with the token redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("access", string(c.Access)), slog.String("token", redact(c.Token)))
}

func redact(s string) string {
	if s == "" {
		return `""`
	}
	return redacted
}

// Driver is the Vercel Blob backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	api, blob string
	access    Access
	client    *http.Client
	// cacheMaxAge is Config.CacheMaxAge; 0 sends no header.
	cacheMaxAge time.Duration
}

// New checks cfg and returns a driver. It makes no request.
func New(cfg Config) (*Driver, error) {
	if cfg.Token == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob: Config.Token is required")
	}
	if cfg.Access != AccessPublic && cfg.Access != AccessPrivate {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob: Config.Access must be %q or %q, matching the store", AccessPublic, AccessPrivate)
	}
	if c := cfg.CacheMaxAge; c != 0 && (c < MinCacheMaxAge || c%time.Second != 0) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob: CacheMaxAge %s must be whole seconds and at least %s", c, MinCacheMaxAge)
	}
	api, err := endpoint(cfg.APIURL, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	blobDefault := ""
	if rest, ok := strings.CutPrefix(cfg.Token, tokenPrefix); ok {
		if store, _, ok := strings.Cut(rest, "_"); ok && store != "" {
			blobDefault = "https://" + strings.ToLower(store) + "." + string(cfg.Access) + ".blob.vercel-storage.com"
		}
	}
	blob, err := endpoint(cfg.BlobURL, blobDefault)
	if err != nil {
		return nil, err
	}
	if blob == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob: the token does not name a store; pass a read-write token or BlobURL")
	}
	client := &http.Client{Timeout: DefaultTimeout}
	if cfg.Client != nil {
		c := *cfg.Client
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	token := cfg.Token
	origins := map[string]bool{origin(api): true}
	if cfg.Access == AccessPrivate {
		origins[origin(blob)] = true // public blobs are read without it
	}
	client.Transport = &authTransport{
		origins: origins,
		next:    client.Transport,
		header:  func(h http.Header) { h.Set("Authorization", "Bearer "+token) },
	}
	return &Driver{api: api, blob: blob, access: cfg.Access, client: client, cacheMaxAge: cfg.CacheMaxAge}, nil
}

func endpoint(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || !secure(u) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "vercelblob: endpoint %q must be an https URL (http only on loopback)", raw)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func secure(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}

func origin(raw string) string {
	u, _ := url.Parse(raw)
	return u.Scheme + "://" + u.Host
}

// authTransport adds the token to requests for its origins and to no
// other host.
type authTransport struct {
	origins map[string]bool
	next    http.RoundTripper
	// header is a closure over the token; fmt prints a func as an address.
	header func(http.Header)
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	if !t.origins[req.URL.Scheme+"://"+req.URL.Host] {
		req.Header.Del("Authorization")
		return next.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	t.header(r.Header)
	return next.RoundTrip(r)
}

// Native returns the *http.Client (its transport adds the token), for the
// filesystem.As escape hatch: multipart uploads, folders, client tokens.
func (d *Driver) Native() *http.Client { return d.client }

// Capabilities reports what Vercel Blob offers. Copy is server-side
// (fromUrl), DeleteKeys one request per batch, CompareAndSwap the API's
// x-if-match. Not declared: IfNotExists (Vercel refuses an existing
// pathname without a documented status), presigning (Vercel's presigned
// URLs are client upload tokens, not signed reads), Move (no atomic
// rename).
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Copy:           true,
		DeleteKeys:     true,
		CompareAndSwap: true,
		ContentType:    true,
		PublicURL:      d.access == AccessPublic,
	}
}

// blobURL is key's URL on the store's host.
func (d *Driver) blobURL(key string) string { return d.blob + "/" + filesystem.EscapeKey(key) }

// apiError is the body of an API error answer.
type apiError struct {
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

// statusError classifies an API error by its code (as @vercel/blob does),
// falling back to the status.
func statusError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e apiError
	_ = json.Unmarshal(raw, &e)
	cause := fmt.Errorf("vercelblob: %s: %s %s", what, resp.Status, e.Error.Code)
	switch {
	case e.Error.Code == "not_found" || e.Error.Code == "store_not_found" || resp.StatusCode == http.StatusNotFound:
		return filesystem.NewError(filesystem.CodeNotFound, cause)
	case e.Error.Code == "precondition_failed" || resp.StatusCode == http.StatusPreconditionFailed:
		return filesystem.NewError(filesystem.CodeConflict, cause)
	case e.Error.Code == "file_too_large" || resp.StatusCode == http.StatusRequestEntityTooLarge:
		return filesystem.NewError(filesystem.CodeTooLarge, cause)
	case e.Error.Code == "rate_limited" || e.Error.Code == "service_unavailable" || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	case e.Error.Code == "forbidden" || e.Error.Code == "store_suspended" || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case e.Error.Code == "content_type_not_allowed":
		return filesystem.NewError(filesystem.CodeRejected, cause)
	case resp.StatusCode == http.StatusBadRequest:
		return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
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
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("x-api-version", apiVersion)
	resp, err := d.client.Do(req)
	if err != nil {
		var be bodyError
		switch {
		case errors.As(err, &be):
			return nil, be.err
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return resp, nil
}

// call sends an API request and decodes a 2xx JSON answer into out (nil
// discards it).
func (d *Driver) call(ctx context.Context, method, target string, body io.Reader, header http.Header, out any) error {
	resp, err := d.do(ctx, method, target, body, header)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return statusError(resp, method)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out); err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("vercelblob: %s: decoding the answer: %w", method, err))
	}
	return nil
}

// blob is a blob as the API describes it.
type blob struct {
	URL         string    `json:"url"`
	Pathname    string    `json:"pathname"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	UploadedAt  time.Time `json:"uploadedAt"`
	ETag        string    `json:"etag"`
}

func entry(key string, b blob) filesystem.Entry {
	return filesystem.Entry{Key: key, Size: b.Size, ModTime: b.UploadedAt, ContentType: b.ContentType, ETag: strings.Trim(b.ETag, `"`)}
}

// Stat implements filesystem.Driver with the API's head (a GET of
// ?url=<blob URL>).
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	var b blob
	if err := d.call(ctx, http.MethodGet, d.api+"/?"+url.Values{"url": {d.blobURL(key)}}.Encode(), nil, nil, &b); err != nil {
		return filesystem.Entry{}, err
	}
	return entry(key, b), nil
}

// Read implements filesystem.Driver with a GET of the blob's URL and a
// Range header. Private blobs are read uncached (cache=0).
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	target := d.blobURL(key)
	if d.access == AccessPrivate {
		target += "?cache=0"
	}
	h := http.Header{}
	if rng.Offset > 0 || rng.Length > 0 {
		v := "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-"
		if rng.Length > 0 {
			v += strconv.FormatInt(rng.Offset+rng.Length-1, 10)
		}
		h.Set("Range", v)
	}
	resp, err := d.do(ctx, http.MethodGet, target, nil, h)
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
	return nil, statusError(resp, "GET blob")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// headerCacheMaxAge carries Config.CacheMaxAge on a PUT, in seconds.
const headerCacheMaxAge = "x-cache-control-max-age"

// putHeader is the header of a PUT: no random suffix, overwrite allowed,
// the store's access mode.
func (d *Driver) putHeader(opts filesystem.WriteOptions) http.Header {
	h := http.Header{}
	h.Set("x-vercel-blob-access", string(d.access))
	h.Set("x-add-random-suffix", "0")
	h.Set("x-allow-overwrite", "1")
	if opts.ContentType != "" {
		h.Set("x-content-type", opts.ContentType)
	}
	if d.cacheMaxAge > 0 {
		h.Set(headerCacheMaxAge, strconv.FormatInt(int64(d.cacheMaxAge/time.Second), 10))
	}
	if opts.IfMatch != "" {
		h.Set("x-if-match", `"`+opts.IfMatch+`"`)
	}
	return h
}

// Write implements filesystem.Driver: one streamed PUT, then a Stat for
// the size and time the PUT's answer does not carry. A body that fails
// aborts the request, and its own error is returned.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	target := d.api + "/?" + url.Values{"pathname": {key}}.Encode()
	err := d.call(ctx, http.MethodPut, target, bodyReader{body}, d.putHeader(opts), nil)
	if err != nil {
		if opts.IfMatch != "" && filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeConflict, err)
		}
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, key)
}

// bodyReader marks the caller's body failing, so do returns that error
// rather than the transport's description of it.
type bodyReader struct{ r io.Reader }

func (b bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = bodyError{err}
	}
	return n, err
}

type bodyError struct{ err error }

func (b bodyError) Error() string { return b.err.Error() }
func (b bodyError) Unwrap() error { return b.err }

// Copy implements filesystem.Copier with a server-side PUT ?fromUrl=.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if _, err := d.Stat(ctx, src); err != nil {
		return filesystem.Entry{}, err
	}
	if src == dst {
		return d.Stat(ctx, dst)
	}
	target := d.api + "/?" + url.Values{"pathname": {dst}, "fromUrl": {d.blobURL(src)}}.Encode()
	if err := d.call(ctx, http.MethodPut, target, nil, d.putHeader(filesystem.WriteOptions{}), nil); err != nil {
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, dst)
}

// Delete implements filesystem.Driver; a missing key succeeds.
func (d *Driver) Delete(ctx context.Context, key string) error {
	return d.deleteURLs(ctx, []string{d.blobURL(key)})
}

func (d *Driver) deleteURLs(ctx context.Context, urls []string) error {
	raw, err := json.Marshal(map[string][]string{"urls": urls})
	if err != nil {
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	err = d.call(ctx, http.MethodPost, d.api+"/delete", bytes.NewReader(raw), h, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

// DeleteBatch is how many blobs one delete request names.
const DeleteBatch = 100

// DeleteKeys implements filesystem.KeysDeleter, DeleteBatch keys per
// request. A failed request reports its keys in a *BatchError and the
// others are still tried.
func (d *Driver) DeleteKeys(ctx context.Context, keys []string) error {
	var failed []filesystem.KeyError
	for start := 0; start < len(keys); start += DeleteBatch {
		batch := keys[start:min(start+DeleteBatch, len(keys))]
		urls := make([]string, len(batch))
		for i, k := range batch {
			urls[i] = d.blobURL(k)
		}
		if err := d.deleteURLs(ctx, urls); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for _, k := range batch {
				failed = append(failed, filesystem.KeyError{Key: k, Err: err})
			}
		}
	}
	if len(failed) > 0 {
		return &filesystem.BatchError{Failures: failed}
	}
	return nil
}

// listLimit is the page size asked of the list endpoint (its maximum).
const listLimit = 1000

// List implements filesystem.Driver with the API's prefix listing,
// following its cursor, then pages with filesystem.PageOf. Folder
// placeholders (pathnames ending in "/") are not objects and are skipped.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	v := url.Values{"limit": {strconv.Itoa(listLimit)}}
	if prefix != "" {
		v.Set("prefix", prefix)
	}
	var all []filesystem.Entry
	for {
		var page struct {
			Blobs   []blob `json:"blobs"`
			Cursor  string `json:"cursor"`
			HasMore bool   `json:"hasMore"`
		}
		if err := d.call(ctx, http.MethodGet, d.api+"/?"+v.Encode(), nil, nil, &page); err != nil {
			return filesystem.Page{}, err
		}
		for _, b := range page.Blobs {
			if strings.HasSuffix(b.Pathname, "/") || !filesystem.KeyInDir(b.Pathname, dir) {
				continue
			}
			all = append(all, entry(b.Pathname, b))
		}
		if !page.HasMore {
			break
		}
		v.Set("cursor", page.Cursor)
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// PublicURL implements filesystem.PublicURLer for a public store: the
// blob's permanent URL on the store's host.
func (d *Driver) PublicURL(_ context.Context, key string) (string, error) {
	if d.access != AccessPublic {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "vercelblob: a private store has no public URLs")
	}
	return d.blobURL(key), nil
}
