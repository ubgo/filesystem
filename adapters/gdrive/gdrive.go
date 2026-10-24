// Package gdrive is a filesystem.Driver over the Google Drive API v3,
// spoken as plain JSON over net/http (google.golang.org/api/drive/v3
// brings the whole Google API client stack for the nine requests the
// driver makes).
//
// Drive has no paths: every file and folder is an ID with a name and a
// parent, and one folder may hold several items of the same name. The
// driver maps a key to a path of names under Config.FolderID and resolves
// it one segment at a time (O(depth) requests; nothing is cached, so the
// driver keeps no state that could go stale). Where names repeat:
//
//   - folders: the oldest folder of the name is the one keys resolve
//     through; listings walk every folder below the listed one, so no
//     file there is hidden;
//   - files: a key naming two or more files is CodeConflict for Stat,
//     Read and Write, until the application removes the extras; Delete
//     removes all of them.
//
// The driver creates a missing folder at most once per process (creation
// is serialised), but two processes creating the same new folder at once
// can make two; the rules above keep that safe. Google Docs, Sheets and
// other native files have no bytes, so they are skipped by listings and
// are CodeNotFound.
//
// Writes go through a resumable upload in Config.ChunkSize pieces, so a
// body of any size streams with two chunks in memory (the one being sent
// and the next, read ahead to learn whether the current one is the last),
// and the file appears only when the last piece arrives; replacing a key uploads a new revision
// of the same file ID. Drive v3 has no conditional writes.
//
// The application supplies OAuth through Config.Token; the driver's
// transport adds it only to requests for the configured API origins.
package gdrive

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
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// API endpoints, the defaults of Config.APIURL and Config.UploadURL.
const (
	DefaultAPIURL    = "https://www.googleapis.com/drive/v3"
	DefaultUploadURL = "https://www.googleapis.com/upload/drive/v3"
)

// FolderMIME is the MIME type Drive gives folders; NativePrefix starts the
// type of every Google-native file (Docs, Sheets), which has no bytes.
const (
	FolderMIME   = "application/vnd.google-apps.folder"
	NativePrefix = "application/vnd.google-apps."
)

// Resumable-upload chunks must be multiples of ChunkUnit (256 KiB).
const (
	ChunkUnit        = 256 << 10
	DefaultChunkSize = 32 * ChunkUnit // 8 MiB
)

// DefaultTimeout bounds each request of the client New makes when
// Config.Client is nil; a request carries at most one chunk.
const DefaultTimeout = 5 * time.Minute

// TokenSource supplies the OAuth 2.0 access token. It is called once per
// request, so a refreshing source keeps a long-lived driver working; it
// must be safe for concurrent use.
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
	// FolderID is the ID of the folder holding the disk: a folder of My
	// Drive, a shared drive's ID, or "root" (the default, My Drive's root).
	// An application with the drive.file scope should create its own
	// folder and pass its ID.
	FolderID string
	// ChunkSize is the size of each upload request: a multiple of
	// ChunkUnit; 0 means DefaultChunkSize. A write holds two chunks in
	// memory: the one being sent and the next, read ahead so the final
	// chunk is known as final.
	ChunkSize int
	// Client sends the requests; nil means a client with DefaultTimeout.
	// The driver uses a copy whose transport adds the token for the API
	// origins only.
	Client *http.Client
	// APIURL and UploadURL override the endpoints, for a proxy or a test
	// server; https, or http on loopback only.
	APIURL, UploadURL string
	// Trash makes every removal move the item to Drive's trash instead of
	// deleting it permanently: Delete, DeletePrefix, and the older files a
	// Write, Copy or Move replaces. Off by default (permanent deletion,
	// as before). Turn it on for shared drives where the account is a
	// Content manager, which may trash but not delete (every permanent
	// Delete fails with CodePermission there), or to keep a recovery
	// window: Drive empties the trash after 30 days, and trashed files
	// still count against the owner's storage until then.
	Trash bool
}

// String implements fmt.Stringer without the token source's contents.
func (c Config) String() string {
	return fmt.Sprintf("gdrive.Config{FolderID: %q, Token: %s}", c.FolderID, tokenState(c.Token))
}

// GoString implements fmt.GoStringer like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb prints the token source.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer without the token source's contents.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("folder_id", c.FolderID), slog.String("token", tokenState(c.Token)))
}

func tokenState(ts TokenSource) string {
	if ts == nil {
		return "none"
	}
	return "[redacted]"
}

// Driver is the Google Drive backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	api, upload string
	root        string
	chunk       int
	client      *http.Client
	// trash is Config.Trash.
	trash bool
	// mkdir serialises folder creation, so one process never makes two
	// folders of one name.
	mkdir sync.Mutex
}

// New checks cfg and returns a driver. It makes no request.
func New(cfg Config) (*Driver, error) {
	if cfg.Token == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gdrive: Config.Token is required")
	}
	api, err := endpoint(cfg.APIURL, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	upload, err := endpoint(cfg.UploadURL, DefaultUploadURL)
	if err != nil {
		return nil, err
	}
	root := cfg.FolderID
	if root == "" {
		root = "root"
	}
	if strings.ContainsAny(root, `/'\?#`) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gdrive: FolderID %q is not a Drive ID", root)
	}
	chunk := cfg.ChunkSize
	switch {
	case chunk == 0:
		chunk = DefaultChunkSize
	case chunk < 0 || chunk%ChunkUnit != 0:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "gdrive: ChunkSize must be a positive multiple of %d", ChunkUnit)
	}
	client := &http.Client{Timeout: DefaultTimeout}
	if cfg.Client != nil {
		c := *cfg.Client
		client = &c
	}
	origins := map[string]bool{origin(api): true, origin(upload): true}
	client.Transport = &authTransport{origins: origins, next: client.Transport, token: cfg.Token.AccessToken}
	return &Driver{api: api, upload: upload, root: root, chunk: chunk, client: client, trash: cfg.Trash}, nil
}

func endpoint(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || !secure(u) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "gdrive: endpoint %q must be an https URL (http only on loopback)", raw)
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

// authTransport adds the bearer token to requests for the API origins and
// to no other host.
type authTransport struct {
	origins map[string]bool
	next    http.RoundTripper
	// token is a method value; fmt prints a func as an address.
	token func(context.Context) (string, error)
}

// errToken marks a failing token source inside a transport error.
var errToken = errors.New("gdrive: token source failed")

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	if !t.origins[req.URL.Scheme+"://"+req.URL.Host] {
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

// Native returns the *http.Client (its transport adds the token), for the
// filesystem.As escape hatch: permissions, revisions, comments.
func (d *Driver) Native() *http.Client { return d.client }

// Capabilities reports what Drive offers. No conditional writes (v3
// dropped If-Match) and no presigning (Drive has no signed URLs). Names
// are case-sensitive.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Copy:         true,
		Move:         true,
		DeletePrefix: true,
		ContentType:  true,
	}
}

// file is a Drive file resource with the fields the driver asks for.
type file struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MimeType    string    `json:"mimeType"`
	Size        string    `json:"size"` // int64 as a JSON string
	Modified    time.Time `json:"modifiedTime"`
	MD5Checksum string    `json:"md5Checksum"`
}

// fileMeta is the JSON body of a create, copy, rename or upload-session
// request. Every field is omitempty because each request sends only what
// it changes: a rename sends Name alone, an upload to an existing file sends
// MimeType alone (Drive refuses parents on an update).
type fileMeta struct {
	Name     string   `json:"name,omitempty"`
	MimeType string   `json:"mimeType,omitempty"`
	Parents  []string `json:"parents,omitempty"`
}

// fileFields is the fields parameter for a file; listFields for a page.
const (
	fileFields = "id,name,mimeType,size,modifiedTime,md5Checksum"
	listFields = "nextPageToken,files(" + fileFields + ")"
)

func (f file) folder() bool { return f.MimeType == FolderMIME }

// object reports a file with bytes: not a folder, not a native document.
func (f file) object() bool { return !strings.HasPrefix(f.MimeType, NativePrefix) }

func entry(key string, f file) filesystem.Entry {
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	return filesystem.Entry{Key: key, Size: size, ModTime: f.Modified, ETag: f.MD5Checksum, ContentType: f.MimeType}
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

const maxErrorBody = 8 << 10

// maxJSONBody caps what is read of a JSON answer, far above the largest
// page the API sends, so a hostile or broken server cannot make the
// driver buffer without bound.
const maxJSONBody = 16 << 20

func statusError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e apiError
	_ = json.Unmarshal(raw, &e)
	reason := ""
	if len(e.Error.Errors) > 0 {
		reason = e.Error.Errors[0].Reason
	}
	cause := fmt.Errorf("gdrive: %s: %s %s", what, resp.Status, reason)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return filesystem.NewError(filesystem.CodeNotFound, cause)
	case resp.StatusCode == http.StatusForbidden && (reason == "storageQuotaExceeded" || reason == "quotaExceeded"):
		return filesystem.NewError(filesystem.CodeQuotaExceeded, cause)
	case resp.StatusCode == http.StatusForbidden && (reason == "userRateLimitExceeded" || reason == "rateLimitExceeded"):
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return filesystem.NewError(filesystem.CodeTooLarge, cause)
	case resp.StatusCode == http.StatusBadRequest:
		return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
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

// apiOrigin reports the two API origins, whose paths name files rather
// than carry credentials (an upload session's ID is in its query).
func (d *Driver) apiOrigin(o string) bool { return o == origin(d.api) || o == origin(d.upload) }

func (d *Driver) do(ctx context.Context, method, target string, body io.Reader, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, redactURLError(err, d.apiOrigin))
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := d.client.Do(req)
	err = redactURLError(err, d.apiOrigin)
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

// call sends a request with an optional JSON body and decodes a 2xx answer
// into out (nil discards it).
func (d *Driver) call(ctx context.Context, method, target string, in, out any) error {
	var body io.Reader
	h := http.Header{}
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		body = bytes.NewReader(raw)
		h.Set("Content-Type", "application/json; charset=UTF-8")
	}
	resp, err := d.do(ctx, method, target, body, h)
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
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("gdrive: %s: decoding the answer: %w", what, err))
	}
	return nil
}

// query quotes s as a string literal of the Drive query language.
func query(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// children lists the items in folder parent, all of them or only those
// named name, oldest first, following nextPageToken.
func (d *Driver) children(ctx context.Context, parent, name string) ([]file, error) {
	q := query(parent) + " in parents and trashed = false"
	if name != "" {
		q += " and name = " + query(name)
	}
	v := url.Values{
		"q":                         {q},
		"fields":                    {listFields},
		"pageSize":                  {strconv.Itoa(listPageSize)},
		"orderBy":                   {"createdTime"},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	var all []file
	for {
		var page struct {
			Files []file `json:"files"`
			Next  string `json:"nextPageToken"`
		}
		if err := d.call(ctx, http.MethodGet, d.api+"/files?"+v.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Files...)
		if page.Next == "" {
			return all, nil
		}
		v.Set("pageToken", page.Next)
	}
}

// listPageSize is the pageSize asked of files.list (its maximum).
const listPageSize = 1000

// folderOf resolves the folder dir (a key prefix; "" is the root) to its
// ID through the oldest folder of each name. found is false when a segment
// is missing; a segment naming only files is CodeInvalidArgument.
func (d *Driver) folderOf(ctx context.Context, dir string) (id string, found bool, err error) {
	id = d.root
	if dir == "" {
		return id, true, nil
	}
	for _, seg := range strings.Split(dir, "/") {
		items, err := d.children(ctx, id, seg)
		if err != nil {
			return "", false, err
		}
		next := ""
		for _, it := range items {
			if it.folder() {
				next = it.ID
				break
			}
		}
		if next == "" {
			if len(items) > 0 {
				return "", false, filesystem.Errorf(filesystem.CodeInvalidArgument, "gdrive: %q is a file, not a folder", seg)
			}
			return "", false, nil
		}
		id = next
	}
	return id, true, nil
}

// split returns key's folder ("" for the root) and name.
func split(key string) (dir, name string) {
	dir, name = path.Split(key)
	return strings.TrimSuffix(dir, "/"), name
}

// files returns the objects named key (normally zero or one), with the ID
// of their folder ("" when the folder is missing).
func (d *Driver) files(ctx context.Context, key string) (parent string, found []file, err error) {
	dir, name := split(key)
	parent, ok, err := d.folderOf(ctx, dir)
	if err != nil || !ok {
		if filesystem.CodeOf(err) == filesystem.CodeInvalidArgument {
			err = nil // a file in the way: key cannot exist
		}
		return "", nil, err
	}
	items, err := d.children(ctx, parent, name)
	if err != nil {
		return "", nil, err
	}
	for _, it := range items {
		if !it.folder() && it.object() {
			found = append(found, it)
		}
	}
	return parent, found, nil
}

// one returns the single object named key: CodeNotFound when there is
// none, CodeConflict when the name repeats.
func (d *Driver) one(ctx context.Context, key string) (file, error) {
	_, found, err := d.files(ctx, key)
	switch {
	case err != nil:
		return file{}, err
	case len(found) == 0:
		return file{}, filesystem.Errorf(filesystem.CodeNotFound, "gdrive: %q not found", key)
	case len(found) > 1:
		return file{}, filesystem.Errorf(filesystem.CodeConflict, "gdrive: %d files are named %q; remove the extras", len(found), key)
	}
	return found[0], nil
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	f, err := d.one(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return entry(key, f), nil
}

// Read implements filesystem.Driver with alt=media and a Range header.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	f, err := d.one(ctx, key)
	if err != nil {
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
	resp, err := d.do(ctx, http.MethodGet, d.api+"/files/"+url.PathEscape(f.ID)+"?alt=media&supportsAllDrives=true", nil, h)
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
	return nil, statusError(resp, "GET media")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// mkdirAll returns the ID of folder dir, creating missing folders from the
// top. Creation is serialised so this process never duplicates a folder.
func (d *Driver) mkdirAll(ctx context.Context, dir string) (string, error) {
	if id, ok, err := d.folderOf(ctx, dir); err != nil || ok {
		return id, err
	}
	d.mkdir.Lock()
	defer d.mkdir.Unlock()
	id := d.root
	for _, seg := range strings.Split(dir, "/") {
		items, err := d.children(ctx, id, seg)
		if err != nil {
			return "", err
		}
		next := ""
		for _, it := range items {
			if it.folder() {
				next = it.ID
				break
			}
		}
		if next == "" {
			if len(items) > 0 {
				return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "gdrive: %q is a file, not a folder", seg)
			}
			var made file
			in := fileMeta{Name: seg, MimeType: FolderMIME, Parents: []string{id}}
			if err := d.call(ctx, http.MethodPost, d.api+"/files?supportsAllDrives=true&fields=id", in, &made); err != nil {
				return "", err
			}
			next = made.ID
		}
		id = next
	}
	return id, nil
}

// Write implements filesystem.Driver with a resumable upload: a new file
// when the key has none, a new revision of the file when it has one.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	dir, name := split(key)
	parent := d.root
	if dir != "" {
		var err error
		if parent, err = d.mkdirAll(ctx, dir); err != nil {
			return filesystem.Entry{}, err
		}
	}
	_, found, err := d.files(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	meta := fileMeta{MimeType: opts.ContentType}
	method, target := http.MethodPost, d.upload+"/files"
	switch len(found) {
	case 0:
		meta.Name, meta.Parents = name, []string{parent}
	case 1:
		method, target = http.MethodPatch, d.upload+"/files/"+url.PathEscape(found[0].ID)
	default:
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeConflict, "gdrive: %d files are named %q; remove the extras", len(found), key)
	}
	f, err := d.resumable(ctx, method, target+"?uploadType=resumable&supportsAllDrives=true&fields="+fileFields, meta, body, opts.SizeHint)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return entry(key, f), nil
}

// statusResumeIncomplete is the 308 a resumable upload answers each
// non-final chunk with.
const statusResumeIncomplete = 308

// resumable starts a resumable upload session with meta and sends body in
// chunks. Content-Range states the total from sizeHint when known, else
// "*" until the final chunk, which states it. A failure cancels the
// session (DELETE), so no file appears.
func (d *Driver) resumable(ctx context.Context, method, target string, meta fileMeta, body io.Reader, sizeHint int64) (file, error) {
	raw, err := json.Marshal(meta)
	if err != nil {
		return file{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := d.do(ctx, method, target, bytes.NewReader(raw), h)
	if err != nil {
		return file{}, err
	}
	if err := decode(resp, "start upload", nil); err != nil {
		return file{}, err
	}
	session := resp.Header.Get("Location")
	if u, err := url.Parse(session); err != nil || u.User != nil || origin(session) != origin(d.upload) {
		return file{}, filesystem.Errorf(filesystem.CodeUnavailable, "gdrive: the upload session URL is not on the upload host")
	}
	cancel := func() {
		if resp, err := d.do(context.WithoutCancel(ctx), http.MethodDelete, session, nil, nil); err == nil {
			_ = resp.Body.Close()
		}
	}
	total := "*"
	if sizeHint > 0 {
		total = strconv.FormatInt(sizeHint, 10)
	}
	cur, spare := make([]byte, d.chunk), make([]byte, d.chunk)
	n, last, err := readChunk(body, cur)
	if err != nil {
		cancel()
		return file{}, err
	}
	var offset int64
	for {
		m := 0
		if !last {
			// Read ahead, so the chunk ending the body is known as final.
			if m, last, err = readChunk(body, spare); err != nil {
				cancel()
				return file{}, err
			}
		}
		final := last && m == 0
		if final {
			total = strconv.FormatInt(offset+int64(n), 10)
		}
		h := http.Header{}
		switch {
		case n > 0:
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%s", offset, offset+int64(n)-1, total))
		default:
			h.Set("Content-Range", "bytes */"+total) // an empty file
		}
		resp, err := d.do(ctx, http.MethodPut, session, bytes.NewReader(cur[:n]), h)
		if err != nil {
			cancel()
			return file{}, err
		}
		if final {
			var f file
			if err := decode(resp, "upload", &f); err != nil {
				return file{}, err
			}
			return f, nil
		}
		if resp.StatusCode != statusResumeIncomplete {
			err := statusError(resp, "upload chunk")
			_ = resp.Body.Close()
			cancel()
			return file{}, err
		}
		_ = resp.Body.Close()
		offset += int64(n)
		cur, spare, n = spare, cur, m
	}
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

// trashBody is the files.update body that moves an item to the trash.
var trashBody = map[string]bool{"trashed": true}

// remove deletes the item id, permanently or into the trash under
// Config.Trash (a trashed folder takes its contents with it, as a deleted
// one does); a missing one succeeds.
func (d *Driver) remove(ctx context.Context, id string) error {
	target := d.api + "/files/" + url.PathEscape(id) + "?supportsAllDrives=true"
	var err error
	if d.trash {
		err = d.call(ctx, http.MethodPatch, target, trashBody, nil)
	} else {
		err = d.call(ctx, http.MethodDelete, target, nil, nil)
	}
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

// Delete implements filesystem.Driver: every file named key is deleted
// permanently, or trashed under Config.Trash; folders are left alone.
func (d *Driver) Delete(ctx context.Context, key string) error {
	_, found, err := d.files(ctx, key)
	if err != nil {
		return err
	}
	for _, f := range found {
		if err := d.remove(ctx, f.ID); err != nil {
			return err
		}
	}
	return nil
}

// Copy implements filesystem.Copier with files.copy. Files already named
// dst are deleted after the copy lands, so dst is never missing; until
// then the name briefly repeats.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	return d.relocate(ctx, src, dst, true)
}

// Move implements filesystem.Mover by changing the file's parent and name
// in one update, then deleting the files dst named before.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	return d.relocate(ctx, src, dst, false)
}

func (d *Driver) relocate(ctx context.Context, src, dst string, keep bool) (filesystem.Entry, error) {
	f, err := d.one(ctx, src)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if src == dst {
		return entry(dst, f), nil
	}
	dir, name := split(dst)
	parent, err := d.mkdirAll(ctx, dir)
	if err != nil {
		return filesystem.Entry{}, err
	}
	_, old, err := d.files(ctx, dst)
	if err != nil {
		return filesystem.Entry{}, err
	}
	var moved file
	if keep {
		in := fileMeta{Name: name, Parents: []string{parent}}
		err = d.call(ctx, http.MethodPost, d.api+"/files/"+url.PathEscape(f.ID)+"/copy?supportsAllDrives=true&fields="+fileFields, in, &moved)
	} else {
		srcDir, _ := split(src)
		from, _, ferr := d.folderOf(ctx, srcDir)
		if ferr != nil {
			return filesystem.Entry{}, ferr
		}
		v := url.Values{"supportsAllDrives": {"true"}, "fields": {fileFields}}
		if from != parent {
			v.Set("addParents", parent)
			v.Set("removeParents", from)
		}
		err = d.call(ctx, http.MethodPatch, d.api+"/files/"+url.PathEscape(f.ID)+"?"+v.Encode(), fileMeta{Name: name}, &moved)
	}
	if err != nil {
		return filesystem.Entry{}, err
	}
	for _, o := range old {
		if o.ID != moved.ID {
			if err := d.remove(ctx, o.ID); err != nil {
				return filesystem.Entry{}, err
			}
		}
	}
	return entry(dst, moved), nil
}

// List implements filesystem.Driver by walking folders breadth first,
// through every folder of a repeated name, then pages with
// filesystem.PageOf.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	id, ok, err := d.folderOf(ctx, dir)
	if filesystem.CodeOf(err) == filesystem.CodeInvalidArgument {
		return filesystem.Page{}, nil // a file named dir: nothing inside
	}
	if err != nil || !ok {
		return filesystem.Page{}, err
	}
	type folder struct{ id, key string }
	var all []filesystem.Entry
	queue := []folder{{id, dir}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		items, err := d.children(ctx, cur.id, "")
		if err != nil {
			return filesystem.Page{}, err
		}
		for _, it := range items {
			if it.Name == "" || it.Name == "." || it.Name == ".." || strings.Contains(it.Name, "/") {
				continue // no clean key holds this name
			}
			k := it.Name
			if cur.key != "" {
				k = cur.key + "/" + it.Name
			}
			switch {
			case it.folder():
				queue = append(queue, folder{it.ID, k})
			case it.object():
				all = append(all, entry(k, it))
			}
		}
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// DeletePrefix implements filesystem.PrefixDeleter by deleting every
// folder named dir (Drive deletes their contents with them). A file named
// dir is not inside dir and is left alone.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	parentDir, name := split(dir)
	parent, ok, err := d.folderOf(ctx, parentDir)
	if filesystem.CodeOf(err) == filesystem.CodeInvalidArgument || (err == nil && !ok) {
		return nil
	}
	if err != nil {
		return err
	}
	items, err := d.children(ctx, parent, name)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.folder() {
			if err := d.remove(ctx, it.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
