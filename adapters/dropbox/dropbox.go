// Package dropbox is a filesystem.Driver over the Dropbox HTTP API v2,
// spoken as plain JSON over net/http (the official Go SDK is generated,
// large and unmaintained, and this driver needs eleven endpoints).
//
// Keys map to paths under Config.Root. Dropbox paths are case-insensitive,
// so the driver declares CaseInsensitive: "a.txt" and "A.txt" are one file.
// Listings return the cased path Dropbox reports (path_display), whose
// parent folders may be cased as they were first created rather than as a
// later key spelled them. A segment ending in a dot or a space, which
// Dropbox refuses, is stored percent-escaped (filesystem.KeyEscaper) and
// decoded back, so applications see the keys they wrote.
//
// Writes upload the body in Config.ChunkSize pieces through an upload
// session and commit it in one finish call, so a reader never sees a
// partial file and a body that fails part-way stores nothing. Bodies
// shorter than one chunk go up in a single request. IfNotExists maps to
// write mode "add" without autorename and IfMatch to mode "update" with
// strict_conflict, which Dropbox checks atomically at commit; the ETag is
// the file's rev.
//
// The application supplies OAuth through Config.Token; the driver never
// stores a token, sends it only to the two Dropbox API hosts (or the
// configured ones), and follows no redirect. Config prints and logs
// without the token source's contents.
package dropbox

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"unicode/utf16"

	"github.com/ubgo/filesystem"
)

// Official API hosts, the defaults of Config.APIURL and Config.ContentURL.
const (
	DefaultAPIURL     = "https://api.dropboxapi.com/2"
	DefaultContentURL = "https://content.dropboxapi.com/2"
)

// Chunk sizes. Dropbox accepts at most 150 MiB per upload request.
const (
	DefaultChunkSize = 8 << 20
	MaxChunkSize     = 150 << 20
)

// DefaultTimeout bounds each request of the client New makes when
// Config.Client is nil; a request carries at most one chunk.
const DefaultTimeout = 5 * time.Minute

// TokenSource supplies the OAuth 2.0 access token sent with each request.
// It is called once per request, so a source that refreshes (an
// oauth2.TokenSource adapted with TokenFunc) keeps a long-lived driver
// working. It must be safe for concurrent use.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// TokenFunc adapts a function to TokenSource. An oauth2.TokenSource ts
// becomes:
//
//	dropbox.TokenFunc(func(context.Context) (string, error) {
//		t, err := ts.Token()
//		if err != nil {
//			return "", err
//		}
//		return t.AccessToken, nil
//	})
type TokenFunc func(ctx context.Context) (string, error)

// AccessToken implements TokenSource.
func (f TokenFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

// Config configures a Driver.
type Config struct {
	// Token supplies the access token. Required.
	Token TokenSource
	// Root is the folder holding the disk, such as "/apps/photos"; empty
	// means the root of the account (or of the app folder, for an app
	// with app-folder access).
	Root string
	// ChunkSize is the size of each upload request; 0 means
	// DefaultChunkSize, at most MaxChunkSize. A write holds one chunk in
	// memory.
	ChunkSize int
	// Client sends the requests; nil means a client with DefaultTimeout.
	// The driver uses a copy that follows no redirect.
	Client *http.Client
	// APIURL and ContentURL override the API hosts, for a proxy or a test
	// server. They must be https unless on the loopback interface.
	APIURL, ContentURL string
	// RootNamespaceID makes paths (Root and every key) resolve against
	// this namespace, the team space's root for a Dropbox Business
	// member, instead of the member's own folder: it is sent as
	// Dropbox-API-Path-Root {".tag": "root", "root": ID} on every request,
	// and Dropbox refuses the call when the ID is not the user's root
	// namespace. Take it from users/get_current_account
	// (root_info.root_namespace_id). Exclusive with NamespaceID.
	RootNamespaceID string
	// NamespaceID resolves paths against any namespace the user can reach
	// (a team folder, a shared folder), sent as Dropbox-API-Path-Root
	// {".tag": "namespace_id", "namespace_id": ID}. Exclusive with
	// RootNamespaceID.
	NamespaceID string
	// SelectUser is the team member ID a team-scoped token acts as
	// (Dropbox-API-Select-User), for an admin application working in a
	// member's files. Empty acts as the token's own user.
	SelectUser string
}

// The headers Config.RootNamespaceID, NamespaceID and SelectUser send.
const (
	headerPathRoot   = "Dropbox-API-Path-Root"
	headerSelectUser = "Dropbox-API-Select-User"
)

// pathRootHeader is the Dropbox-API-Path-Root value for cfg, or "".
func pathRootHeader(cfg Config) (string, error) {
	var v map[string]string
	switch {
	case cfg.RootNamespaceID != "" && cfg.NamespaceID != "":
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "dropbox: RootNamespaceID and NamespaceID are exclusive")
	case cfg.RootNamespaceID != "":
		v = map[string]string{".tag": "root", "root": cfg.RootNamespaceID}
	case cfg.NamespaceID != "":
		v = map[string]string{".tag": "namespace_id", "namespace_id": cfg.NamespaceID}
	default:
		return "", nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return string(raw), nil
}

// String implements fmt.Stringer without the token source's contents.
func (c Config) String() string {
	return fmt.Sprintf("dropbox.Config{Root: %q, Token: %s}", c.Root, tokenState(c.Token))
}

// GoString implements fmt.GoStringer like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb or flag prints the token.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer without the token source's contents.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("root", c.Root), slog.String("token", tokenState(c.Token)))
}

func tokenState(ts TokenSource) string {
	if ts == nil {
		return "none"
	}
	return "[redacted]"
}

// escaper encodes what Dropbox refuses: a name ending in a dot or a space.
var escaper = filesystem.KeyEscaper{SegmentEnd: ". "}

// Driver is the Dropbox backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	api, content string
	root         string
	chunk        int
	client       *http.Client
	// token is the source's method value: fmt prints a func as an address,
	// so printing a Driver never shows a token a source holds.
	token func(context.Context) (string, error)
	// extra are the headers every request carries: Dropbox-API-Path-Root
	// and Dropbox-API-Select-User when configured, else empty.
	extra http.Header
}

// New checks cfg and returns a driver. It makes no request.
func New(cfg Config) (*Driver, error) {
	if cfg.Token == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "dropbox: Config.Token is required")
	}
	api, err := endpoint(cfg.APIURL, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	content, err := endpoint(cfg.ContentURL, DefaultContentURL)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSuffix(cfg.Root, "/")
	if root != "" && !strings.HasPrefix(root, "/") {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "dropbox: Root must start with /")
	}
	chunk := cfg.ChunkSize
	switch {
	case chunk == 0:
		chunk = DefaultChunkSize
	case chunk < 0 || chunk > MaxChunkSize:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "dropbox: ChunkSize must be between 1 and %d", MaxChunkSize)
	}
	client := &http.Client{Timeout: DefaultTimeout}
	if cfg.Client != nil {
		c := *cfg.Client
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	extra := http.Header{}
	pathRoot, err := pathRootHeader(cfg)
	if err != nil {
		return nil, err
	}
	if pathRoot != "" {
		extra.Set(headerPathRoot, pathRoot)
	}
	if cfg.SelectUser != "" {
		extra.Set(headerSelectUser, cfg.SelectUser)
	}
	return &Driver{api: api, content: content, root: root, chunk: chunk, client: client, token: cfg.Token.AccessToken, extra: extra}, nil
}

// endpoint validates an API base URL: https, or http on loopback only, so
// the token never crosses a network in the clear.
func endpoint(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "dropbox: endpoint %q must be an https URL (http only on loopback)", raw)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Native returns the *http.Client, for the filesystem.As escape hatch:
// call endpoints the driver does not (sharing, team spaces) with it. It
// adds no token; AuthorizedClient returns one that does.
func (d *Driver) Native() *http.Client { return d.client }

// Capabilities reports what the Dropbox API offers. No presigning: a
// temporary link (files/get_temporary_link) lasts four hours and cannot be
// given a shorter expiry, and Dropbox has no signed upload a server can
// constrain.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Copy:             true,
		Move:             true,
		DeletePrefix:     true,
		ConditionalWrite: true,
		CompareAndSwap:   true,
		CaseInsensitive:  true,
	}
}

// pathOf is the Dropbox path of key ("" is the root folder, which the API
// names with an empty string rather than "/").
func (d *Driver) pathOf(key string) string {
	if key == "" {
		return d.root
	}
	return d.root + "/" + escaper.EncodeKey(key)
}

// keyOf turns a listed path back into a key, or reports false for a path
// outside the root or one this driver did not write.
func (d *Driver) keyOf(p string) (string, bool) {
	if len(p) <= len(d.root)+1 || !strings.EqualFold(p[:len(d.root)], d.root) || p[len(d.root)] != '/' {
		return "", false
	}
	k, err := escaper.DecodeKey(p[len(d.root)+1:])
	return k, err == nil
}

// apiError is the body of a Dropbox error answer.
type apiError struct {
	Summary string `json:"error_summary"`
}

// maxErrorBody caps what is read of an error response's body.
const maxErrorBody = 8 << 10

// maxJSONBody caps what is read of a JSON answer, far above the largest
// page the API sends, so a hostile or broken server cannot make the
// driver buffer without bound.
const maxJSONBody = 16 << 20

// statusError classifies an error answer. Endpoint errors arrive as 409
// with an error_summary such as "path/not_found/..".
func statusError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e apiError
	_ = json.Unmarshal(raw, &e)
	cause := fmt.Errorf("dropbox: %s: %s %s", what, resp.Status, e.Summary)
	switch {
	case resp.StatusCode == http.StatusConflict:
		switch s := e.Summary; {
		case strings.Contains(s, "not_found"):
			return filesystem.NewError(filesystem.CodeNotFound, cause)
		case strings.Contains(s, "conflict"):
			return filesystem.NewError(filesystem.CodeExists, cause)
		case strings.Contains(s, "insufficient_space"):
			return filesystem.NewError(filesystem.CodeQuotaExceeded, cause)
		case strings.Contains(s, "malformed_path"), strings.Contains(s, "disallowed_name"):
			return filesystem.NewError(filesystem.CodeInvalidKey, cause)
		case strings.Contains(s, "too_many"):
			return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
		}
		return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
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

// headerJSON encodes v for the Dropbox-API-Arg header, which must be ASCII:
// every rune past U+007F is written as a \u escape (a surrogate pair past
// U+FFFF), as the API documents.
func headerJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range string(raw) {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			continue
		}
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String(), nil
}

// call sends one request (see send) and classifies a non-2xx answer; the
// caller closes a 2xx response's body.
func (d *Driver) call(ctx context.Context, content bool, endpoint string, arg any, body io.Reader, header http.Header) (*http.Response, error) {
	resp, err := d.send(ctx, content, endpoint, arg, body, header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp, endpoint)
	}
	return resp, nil
}

// send sends one request with the token. arg goes in the JSON body (API
// endpoints) or, when content is true, in the Dropbox-API-Arg header of a
// content endpoint, whose body is the payload. The caller closes the response body.
func (d *Driver) send(ctx context.Context, content bool, endpoint string, arg any, body io.Reader, header http.Header) (*http.Response, error) {
	tok, err := d.token(ctx)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodePermission, fmt.Errorf("dropbox: token: %w", err))
	}
	base, reqBody := d.api, body
	h := http.Header{}
	if !content {
		raw, err := json.Marshal(arg)
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		reqBody = bytes.NewReader(raw)
		h.Set("Content-Type", "application/json")
	} else {
		base = d.content
		a, err := headerJSON(arg)
		if err != nil {
			return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		h.Set("Dropbox-API-Arg", a)
		if body != nil {
			h.Set("Content-Type", "application/octet-stream")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+endpoint, reqBody)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	for k, v := range d.extra {
		h[k] = v
	}
	for k, v := range header {
		h[k] = v
	}
	req.Header = h
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return resp, nil
}

// rpc calls an API endpoint and decodes its JSON answer into out (nil
// discards it).
func (d *Driver) rpc(ctx context.Context, endpoint string, arg, out any) error {
	resp, err := d.call(ctx, false, endpoint, arg, nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out); err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("dropbox: %s: decoding the answer: %w", endpoint, err))
	}
	return nil
}

// metadata is a file or folder as the API describes it.
type metadata struct {
	Tag            string `json:".tag"`
	PathDisplay    string `json:"path_display"`
	ServerModified string `json:"server_modified"`
	Rev            string `json:"rev"`
	Size           int64  `json:"size"`
}

func (d *Driver) entry(key string, m metadata) filesystem.Entry {
	t, _ := time.Parse(time.RFC3339, m.ServerModified)
	return filesystem.Entry{Key: key, Size: m.Size, ModTime: t, ETag: m.Rev}
}

type pathArg struct {
	Path string `json:"path"`
}

// Stat implements filesystem.Driver. A folder is not an object.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	var m metadata
	if err := d.rpc(ctx, "files/get_metadata", pathArg{d.pathOf(key)}, &m); err != nil {
		return filesystem.Entry{}, err
	}
	if m.Tag != "file" {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeNotFound, "dropbox: %q is not a file", key)
	}
	return d.entry(key, m), nil
}

// Read implements filesystem.Driver with files/download and a Range
// header; a range starting past the end is an empty read.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	h := http.Header{}
	if rng.Offset > 0 || rng.Length > 0 {
		v := "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-"
		if rng.Length > 0 {
			v += strconv.FormatInt(rng.Offset+rng.Length-1, 10)
		}
		h.Set("Range", v)
	}
	resp, err := d.send(ctx, true, "files/download", pathArg{d.pathOf(key)}, nil, h)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return io.NopCloser(strings.NewReader("")), nil
	case resp.StatusCode/100 != 2:
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp, "files/download")
	}
	if resp.StatusCode == http.StatusOK && (rng.Offset > 0 || rng.Length > 0) {
		// The server ignored Range: cut the body to it here.
		if _, err := io.CopyN(io.Discard, resp.Body, rng.Offset); err != nil && err != io.EOF {
			_ = resp.Body.Close()
			return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		if rng.Length > 0 {
			return readCloser{io.LimitReader(resp.Body, rng.Length), resp.Body}, nil
		}
	}
	return resp.Body, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// writeMode is the Dropbox WriteMode union.
type writeMode struct {
	Tag    string `json:".tag"`
	Update string `json:"update,omitempty"`
}

type commitInfo struct {
	Path           string    `json:"path"`
	Mode           writeMode `json:"mode"`
	Autorename     bool      `json:"autorename"`
	Mute           bool      `json:"mute"`
	StrictConflict bool      `json:"strict_conflict"`
}

type cursor struct {
	SessionID string `json:"session_id"`
	Offset    int64  `json:"offset"`
}

// Write implements filesystem.Driver (see the package comment).
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	commit := commitInfo{Path: d.pathOf(key), Mode: writeMode{Tag: "overwrite"}, Mute: true}
	switch {
	case opts.IfNotExists:
		// Without strict_conflict, Dropbox reports no conflict when the
		// upload is identical to the existing file.
		commit.Mode = writeMode{Tag: "add"}
		commit.StrictConflict = true
	case opts.IfMatch != "":
		commit.Mode = writeMode{Tag: "update", Update: opts.IfMatch}
		commit.StrictConflict = true
	}
	buf := make([]byte, d.chunk)
	n, last, err := readChunk(body, buf)
	if err != nil {
		return filesystem.Entry{}, err
	}
	var m metadata
	if last {
		resp, err := d.call(ctx, true, "files/upload", commit, bytes.NewReader(buf[:n]), nil)
		if err != nil {
			return filesystem.Entry{}, conflictCode(err, opts)
		}
		err = decode(resp, &m)
		if err != nil {
			return filesystem.Entry{}, err
		}
		return d.entry(key, m), nil
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	resp, err := d.call(ctx, true, "files/upload_session/start", struct {
		Close bool `json:"close"`
	}{}, bytes.NewReader(buf[:n]), nil)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if err := decode(resp, &started); err != nil {
		return filesystem.Entry{}, err
	}
	at := cursor{SessionID: started.SessionID, Offset: int64(n)}
	for {
		// An abandoned session expires on Dropbox's side after a week and
		// never becomes a file, so a failing body stores nothing.
		n, last, err = readChunk(body, buf)
		if err != nil {
			return filesystem.Entry{}, err
		}
		if last {
			break
		}
		resp, err := d.call(ctx, true, "files/upload_session/append_v2", struct {
			Cursor cursor `json:"cursor"`
			Close  bool   `json:"close"`
		}{Cursor: at}, bytes.NewReader(buf[:n]), nil)
		if err != nil {
			return filesystem.Entry{}, err
		}
		_ = resp.Body.Close()
		at.Offset += int64(n)
	}
	resp, err = d.call(ctx, true, "files/upload_session/finish", struct {
		Cursor cursor     `json:"cursor"`
		Commit commitInfo `json:"commit"`
	}{Cursor: at, Commit: commit}, bytes.NewReader(buf[:n]), nil)
	if err != nil {
		return filesystem.Entry{}, conflictCode(err, opts)
	}
	if err := decode(resp, &m); err != nil {
		return filesystem.Entry{}, err
	}
	return d.entry(key, m), nil
}

// conflictCode turns a commit conflict into what the write asked for:
// CodeConflict for a failed compare-and-swap (whose rev no longer matches,
// or whose file is gone), CodeExists otherwise.
func conflictCode(err error, opts filesystem.WriteOptions) error {
	if opts.IfMatch != "" && (filesystem.CodeOf(err) == filesystem.CodeExists || filesystem.CodeOf(err) == filesystem.CodeNotFound) {
		return filesystem.NewError(filesystem.CodeConflict, err)
	}
	return err
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

func decode(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out); err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("dropbox: decoding the answer: %w", err))
	}
	return nil
}

// Delete implements filesystem.Driver. A folder at key is not an object,
// so it is left alone; a missing key succeeds.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if _, err := d.Stat(ctx, key); err != nil {
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil
		}
		return err
	}
	err := d.rpc(ctx, "files/delete_v2", pathArg{d.pathOf(key)}, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

type relocation struct {
	From       string `json:"from_path"`
	To         string `json:"to_path"`
	Autorename bool   `json:"autorename"`
}

// SwapPrefix starts the name of the transient files Copy and Move create
// beside an existing destination while they replace it (see relocate). A
// file with this prefix survives only when a replacement fails and so does
// its rollback; the error then names it, and an application can sweep any
// it finds.
const SwapPrefix = ".fs-swap-"

// Copy implements filesystem.Copier with files/copy_v2. Dropbox refuses to
// copy onto an existing file, so an existing dst is replaced in steps that
// never lose it: src is copied to a transient sibling first, then swapped
// in (see relocate). A reader can briefly find dst missing during the swap.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	return d.relocate(ctx, endpointCopy, src, dst)
}

// Move implements filesystem.Mover with files/move_v2, replacing an
// existing dst the way Copy does. A move to the same key in another letter
// case renames it.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	return d.relocate(ctx, endpointMove, src, dst)
}

// Relocation endpoints. Both fail with to/conflict on an existing
// destination when autorename is false, and neither has an overwrite mode.
const (
	endpointCopy = "files/copy_v2"
	endpointMove = "files/move_v2"
)

// relocate copies or moves src onto dst.
//
// Why it is not "delete dst, then copy": Dropbox has no overwriting copy
// or move, and deleting first loses the old dst whenever the copy then
// fails (quota, a vanished src, a network error). So:
//
//   - dst absent: one call, which fails rather than overwrite if a file
//     appears at dst meanwhile.
//   - dst present: a copy goes to a transient sibling first (a failure here
//     leaves dst untouched), then replace swaps it in.
//
// What stays non-atomic, because the API offers nothing stronger: between
// replace's two moves dst is missing; during the operation the transient
// files show up in listings; a writer racing on dst can make a step fail,
// which is rolled back; and if a rollback itself fails, the previous file
// is left at a SwapPrefix name that the error reports.
func (d *Driver) relocate(ctx context.Context, endpoint, src, dst string) (filesystem.Entry, error) {
	if _, err := d.Stat(ctx, src); err != nil {
		return filesystem.Entry{}, err
	}
	switch {
	case src == dst:
		return d.Stat(ctx, dst)
	case strings.EqualFold(src, dst):
		if endpoint == endpointCopy {
			return d.Stat(ctx, dst) // the same file
		}
		// A case-only rename: move_v2 renames in place.
		if err := d.rpc(ctx, endpoint, relocation{From: d.pathOf(src), To: d.pathOf(dst)}, nil); err != nil {
			return filesystem.Entry{}, err
		}
		return d.Stat(ctx, dst)
	}
	_, err := d.Stat(ctx, dst)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		err = d.rpc(ctx, endpoint, relocation{From: d.pathOf(src), To: d.pathOf(dst)}, nil)
	case err != nil:
		return filesystem.Entry{}, err
	case endpoint == endpointCopy:
		tmp := swapKey(dst)
		if err = d.rpc(ctx, endpointCopy, relocation{From: d.pathOf(src), To: d.pathOf(tmp)}, nil); err != nil {
			return filesystem.Entry{}, err
		}
		if err = d.replace(ctx, tmp, dst); err != nil {
			// The copy is only a copy: dropping it loses nothing.
			_ = d.rpc(context.WithoutCancel(ctx), "files/delete_v2", pathArg{d.pathOf(tmp)}, nil)
		}
	default:
		err = d.replace(ctx, src, dst)
	}
	if err != nil {
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, dst)
}

// replace moves from onto the existing file dst: dst is moved aside to a
// transient backup, from is moved to dst, and the backup is deleted. If
// moving from fails, the backup is moved back, so dst keeps its old
// content and from stays where it was.
//
// Invariant: the rollback and the cleanup run under a context that ignores
// cancellation, so a caller giving up mid-swap does not strand dst at the
// backup name. A failed cleanup is not an error: dst holds the new content,
// and only a SwapPrefix file is left behind.
func (d *Driver) replace(ctx context.Context, from, dst string) error {
	bak := swapKey(dst)
	if err := d.rpc(ctx, endpointMove, relocation{From: d.pathOf(dst), To: d.pathOf(bak)}, nil); err != nil {
		return err
	}
	keep := context.WithoutCancel(ctx)
	if err := d.rpc(ctx, endpointMove, relocation{From: d.pathOf(from), To: d.pathOf(dst)}, nil); err != nil {
		if rerr := d.rpc(keep, endpointMove, relocation{From: d.pathOf(bak), To: d.pathOf(dst)}, nil); rerr != nil {
			return fmt.Errorf("%w (restoring the previous %q also failed, so it is kept at %q: %v)", err, dst, bak, rerr)
		}
		return err
	}
	_ = d.rpc(keep, "files/delete_v2", pathArg{d.pathOf(bak)}, nil)
	return nil
}

// swapKey names a transient file beside key: same folder (so the move
// stays inside whatever folder permissions apply to key), a short fixed
// length (so a key near Dropbox's name limit still fits), and a random
// part from crypto/rand so concurrent operations never collide.
func swapKey(key string) string {
	name := SwapPrefix + strings.ToLower(rand.Text())
	if dir := path.Dir(key); dir != "." {
		return dir + "/" + name
	}
	return name
}

type listAnswer struct {
	Entries []metadata `json:"entries"`
	Cursor  string     `json:"cursor"`
	HasMore bool       `json:"has_more"`
}

// listLimit is the page size asked of files/list_folder (its maximum).
const listLimit = 2000

// List implements filesystem.Driver with a recursive files/list_folder,
// following its cursor, then pages with filesystem.PageOf.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	var page listAnswer
	err := d.rpc(ctx, "files/list_folder", struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		Limit     int    `json:"limit"`
	}{d.pathOf(dir), true, listLimit}, &page)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound || (err != nil && strings.Contains(err.Error(), "not_folder")) {
		return filesystem.Page{}, nil
	}
	if err != nil {
		return filesystem.Page{}, err
	}
	var all []filesystem.Entry
	for {
		for _, m := range page.Entries {
			if m.Tag != "file" {
				continue
			}
			if k, ok := d.keyOf(m.PathDisplay); ok && filesystem.KeyInDir(k, dir) {
				all = append(all, d.entry(k, m))
			}
		}
		if !page.HasMore {
			break
		}
		next := page.Cursor
		page = listAnswer{}
		if err := d.rpc(ctx, "files/list_folder/continue", struct {
			Cursor string `json:"cursor"`
		}{next}, &page); err != nil {
			return filesystem.Page{}, err
		}
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// DeletePrefix implements filesystem.PrefixDeleter with one files/delete_v2
// of the folder (Dropbox deletes it with everything inside). A file named
// dir is not inside dir and is left alone.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	var m metadata
	err := d.rpc(ctx, "files/get_metadata", pathArg{d.pathOf(dir)}, &m)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Tag != "folder" {
		return nil
	}
	err = d.rpc(ctx, "files/delete_v2", pathArg{d.pathOf(dir)}, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

// AuthorizedClient returns a copy of the driver's client that adds the
// token, and the path-root and select-user headers the Config sets, to
// requests for the API and content hosts, so an application can call
// endpoints the driver does not (sharing, file requests, team endpoints)
// the way the driver does. Requests to any other host get nothing added,
// so the client cannot leak the token elsewhere. Headers a request
// already carries are kept.
func (d *Driver) AuthorizedClient() *http.Client {
	c := *d.client
	c.Transport = &authTransport{origins: map[string]bool{origin(d.api): true, origin(d.content): true}, next: d.client.Transport, token: d.token, extra: d.extra}
	return &c
}

// authTransport is AuthorizedClient's transport.
type authTransport struct {
	origins map[string]bool
	next    http.RoundTripper
	token   func(context.Context) (string, error)
	extra   http.Header
}

// RoundTrip implements http.RoundTripper. It clones the request before
// adding headers, as RoundTrippers must not change theirs.
func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	if !t.origins[r.URL.Scheme+"://"+r.URL.Host] {
		return next.RoundTrip(r)
	}
	tok, err := t.token(r.Context())
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, filesystem.NewError(filesystem.CodePermission, fmt.Errorf("dropbox: token: %w", err))
	}
	r = r.Clone(r.Context())
	if r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range t.extra {
		if r.Header.Get(k) == "" {
			r.Header[k] = v
		}
	}
	return next.RoundTrip(r)
}

// origin is base's scheme and host, as authTransport compares them.
func origin(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
