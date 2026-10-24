// Package cloudinary is a filesystem.Driver over Cloudinary, storing each
// key as a raw resource (exact bytes, no transformation) through the Upload
// and Admin REST APIs on net/http (the Go SDK pulls a large dependency
// tree for six requests). Config.ResourceType opts into image or video
// resources instead, so Cloudinary's transformations apply, at the cost
// of the exact-bytes guarantee (see ResourceType).
//
// A key maps to a public ID. Cloudinary refuses ? & # \ % < > + and some
// other characters in public IDs, so every byte outside [A-Za-z0-9._-] is
// written "~XX" in a segment (and "~" itself as "~7E"); keys come back
// decoded. Resources use Config.Type: "authenticated" (the default, read
// only through signed requests), "private" (the original only through
// signed requests, derived versions public) or "upload" (public delivery
// URLs, which the driver then offers as PublicURL).
//
// Reads use Cloudinary's signed download endpoint with a short expiry, so
// they are fresh after an overwrite and work for both types. Stat and
// List use the Admin API, which Cloudinary rate-limits per hour (500 on
// the free plan): a disk that stats on every request needs a plan that
// allows it.
//
// Writes are one streamed multipart upload, so a body above the plan's
// raw-upload limit (10 MB free, 100 MB paid) is CodeTooLarge; chunked
// uploads are not implemented. The API secret signs every Upload API
// request and is the Admin API's Basic password; Config prints and logs
// it redacted, the driver keeps it inside closures, and it is sent only to
// the API host.
package cloudinary

import (
	"cmp"
	"context"
	"crypto/sha1" // Cloudinary's default signature algorithm
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// Default endpoints.
const (
	DefaultAPIURL      = "https://api.cloudinary.com/v1_1"
	DefaultDeliveryURL = "https://res.cloudinary.com"
)

// Type is a resource's delivery type.
type Type string

// The delivery types the driver supports.
const (
	// TypeAuthenticated resources are read only through signed requests.
	TypeAuthenticated Type = "authenticated"
	// TypeUpload resources have public delivery URLs.
	TypeUpload Type = "upload"
	// TypePrivate resources keep the original behind signed requests
	// while their transformed versions are public, the usual setting for
	// image resources whose thumbnails are shown but originals are not.
	TypePrivate Type = "private"
)

// TypeValues lists every Type the driver accepts.
var TypeValues = []Type{TypeAuthenticated, TypeUpload, TypePrivate}

// ResourceType is the Cloudinary resource type keys are stored as.
type ResourceType string

// The resource types the driver supports. "auto" is not one: Cloudinary
// would pick the type per upload, and every later Stat, Read, List and
// Delete must name the type the resource ended up with.
const (
	// ResourceRaw stores the exact bytes, with the extension in the
	// public ID; no transformations. The default.
	ResourceRaw ResourceType = "raw"
	// ResourceImage stores image resources, which Cloudinary can
	// transform and deliver in other formats.
	ResourceImage ResourceType = "image"
	// ResourceVideo stores video (and audio) resources, likewise.
	ResourceVideo ResourceType = "video"
)

// ResourceTypeValues lists every ResourceType the driver accepts.
var ResourceTypeValues = []ResourceType{ResourceRaw, ResourceImage, ResourceVideo}

// Algorithm is the account's signature algorithm (Settings, Security).
type Algorithm string

// The signature algorithms Cloudinary offers.
const (
	SHA1   Algorithm = "sha1"
	SHA256 Algorithm = "sha256"
)

// DownloadExpiry is how long a read's signed download URL is valid; the
// driver uses it at once.
const DownloadExpiry = 5 * time.Minute

// DefaultTimeout bounds each request of the client New makes when
// Config.Client is nil.
const DefaultTimeout = 10 * time.Minute

// Config configures a Driver.
type Config struct {
	// CloudName, APIKey and APISecret identify the account. Required.
	CloudName, APIKey, APISecret string
	// Type is the delivery type of stored resources; empty means
	// TypeAuthenticated.
	Type Type
	// ResourceType is the resource type keys are stored as; empty means
	// ResourceRaw, which keeps the exact bytes. ResourceImage and
	// ResourceVideo let Cloudinary's transformations and derived URLs work
	// on what the disk stores, but weaken what a read returns: Cloudinary
	// treats the key's extension as a delivery format, so a read (and
	// PublicURL) asks for the format the extension names, and the bytes
	// are the uploaded ones only when that extension matches the uploaded
	// format (a key without an extension reads the original). Cloudinary
	// may also refuse an upload it cannot parse as that type. Choose them
	// for media the application serves through Cloudinary, never for
	// files that must round-trip byte for byte.
	ResourceType ResourceType
	// Clock is the time source for request timestamps and the expiry of
	// a read's signed download URL; nil means time.Now. Cloudinary
	// refuses a timestamp more than an hour off its own clock, so pass one
	// synchronised with it on a host whose clock drifts.
	Clock func() time.Time
	// Algorithm is the account's signature algorithm; empty means SHA1,
	// Cloudinary's default.
	Algorithm Algorithm
	// Client sends the requests; nil means a client with DefaultTimeout.
	Client *http.Client
	// APIURL and DeliveryURL override the endpoints, for a proxy or a test
	// server; https, or http on loopback only.
	APIURL, DeliveryURL string
}

const redacted = "[redacted]"

// String implements fmt.Stringer with the secret redacted.
func (c Config) String() string {
	return fmt.Sprintf("cloudinary.Config{CloudName: %q, APIKey: %q, APISecret: %s, Type: %q}", c.CloudName, c.APIKey, redact(c.APISecret), c.Type)
}

// GoString implements fmt.GoStringer like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so no verb prints the secret.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer with the secret redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("cloud_name", c.CloudName), slog.String("api_key", c.APIKey), slog.String("api_secret", redact(c.APISecret)), slog.String("type", string(c.Type)))
}

func redact(s string) string {
	if s == "" {
		return `""`
	}
	return redacted
}

// Driver is the Cloudinary backend. Create it with New; it is safe for
// concurrent use.
type Driver struct {
	api      string // APIURL + "/" + cloud name
	delivery string // DeliveryURL + "/" + cloud name
	typ      Type
	resource ResourceType
	now      func() time.Time
	apiKey   string
	client   *http.Client
	// sign and basic close over the secret; fmt prints funcs as addresses.
	sign  func(params url.Values) string
	basic func(*http.Request)
}

// New checks cfg and returns a driver. It makes no request.
func New(cfg Config) (*Driver, error) {
	if cfg.CloudName == "" || cfg.APIKey == "" || cfg.APISecret == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary: CloudName, APIKey and APISecret are required")
	}
	if strings.ContainsAny(cfg.CloudName, "/?#%") {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary: CloudName %q is not a cloud name", cfg.CloudName)
	}
	typ := cfg.Type
	if typ == "" {
		typ = TypeAuthenticated
	}
	if !slices.Contains(TypeValues, typ) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary: Type must be one of %v", TypeValues)
	}
	resource := cmp.Or(cfg.ResourceType, ResourceRaw)
	if !slices.Contains(ResourceTypeValues, resource) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary: ResourceType must be one of %v", ResourceTypeValues)
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	newHash := sha1.New
	switch cfg.Algorithm {
	case "", SHA1:
	case SHA256:
		newHash = sha256.New
	default:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary: Algorithm must be %q or %q", SHA1, SHA256)
	}
	api, err := endpoint(cfg.APIURL, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	delivery, err := endpoint(cfg.DeliveryURL, DefaultDeliveryURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: DefaultTimeout}
	if cfg.Client != nil {
		c := *cfg.Client
		client = &c
	}
	key, secret := cfg.APIKey, cfg.APISecret
	return &Driver{
		api:      api + "/" + cfg.CloudName,
		delivery: delivery + "/" + cfg.CloudName,
		typ:      typ,
		resource: resource,
		now:      now,
		apiKey:   key,
		client:   client,
		sign:     func(p url.Values) string { return signature(p, secret, newHash) },
		basic:    func(r *http.Request) { r.SetBasicAuth(key, secret) },
	}, nil
}

func endpoint(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || !secure(u) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "cloudinary: endpoint %q must be an https URL (http only on loopback)", raw)
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

// signature is Cloudinary's request signature: the parameters sorted by
// name as "k=v" joined with "&", the secret appended, hashed, in hex.
// file, cloud_name, resource_type, api_key and signature are not signed.
func signature(p url.Values, secret string, newHash func() hash.Hash) string {
	names := make([]string, 0, len(p))
	for k := range p {
		switch k {
		case "file", "cloud_name", "resource_type", "api_key", "signature":
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + strings.Join(p[k], ",")
	}
	h := newHash()
	_, _ = io.WriteString(h, strings.Join(parts, "&")+secret)
	return hex.EncodeToString(h.Sum(nil))
}

// signed adds the timestamp, API key and signature to p.
func (d *Driver) signed(p url.Values) url.Values {
	p.Set("timestamp", strconv.FormatInt(d.now().Unix(), 10))
	p.Set("signature", d.sign(p))
	p.Set("api_key", d.apiKey)
	return p
}

// Native returns the *http.Client, for the filesystem.As escape hatch.
func (d *Driver) Native() *http.Client { return d.client }

// Capabilities reports what the driver offers on raw resources. No Copy
// (Cloudinary has no server-side copy of a resource), no presigning
// (Cloudinary's signed delivery URLs do not expire without the paid token
// feature), no conditional writes, no content type (raw resources keep
// none).
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Move:         true,
		DeletePrefix: true,
		DeleteKeys:   true,
		PublicURL:    d.typ == TypeUpload,
	}
}

// apiError is the body of an error answer.
type apiError struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

const maxErrorBody = 8 << 10

// maxJSONBody caps what is read of a JSON answer, far above the largest
// page the API sends, so a hostile or broken server cannot make the
// driver buffer without bound.
const maxJSONBody = 16 << 20

// statusRateLimited is the 420 the Admin API answers when the hourly
// limit is spent.
const statusRateLimited = 420

func statusError(resp *http.Response, what string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e apiError
	_ = json.Unmarshal(raw, &e)
	cause := fmt.Errorf("cloudinary: %s: %s %s", what, resp.Status, e.Error.Message)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return filesystem.NewError(filesystem.CodeNotFound, cause)
	case resp.StatusCode == http.StatusRequestEntityTooLarge || strings.Contains(strings.ToLower(e.Error.Message), "file size too large"):
		return filesystem.NewError(filesystem.CodeTooLarge, cause)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case resp.StatusCode == statusRateLimited || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return filesystem.NewError(filesystem.CodeUnavailable, cause).WithRetryAfter(retryAfter(resp))
	case resp.StatusCode == http.StatusBadRequest:
		return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
	}
	return filesystem.NewError(filesystem.CodeUnknown, cause)
}

// retryAfter reads a Retry-After header in seconds; 0 when absent.
func retryAfter(resp *http.Response) time.Duration {
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// do sends req. A transport error never carries the URL's query, where
// the API key, the signature and the expiry of a working signed request
// are.
func (d *Driver) do(req *http.Request) (*http.Response, error) {
	resp, err := d.client.Do(req)
	if err != nil {
		err = redactURLError(err, d.apiOrigin)
		var be bodyError
		switch {
		case errors.As(err, &be):
			return nil, be.err
		case req.Context().Err() != nil:
			return nil, req.Context().Err()
		}
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return resp, nil
}

// apiOrigin reports the API origin, whose paths hold no credential.
func (d *Driver) apiOrigin(o string) bool {
	u, err := url.Parse(d.api)
	return err == nil && o == u.Scheme+"://"+u.Host
}

// decode reads a 2xx JSON answer into out (nil discards it).
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
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("cloudinary: %s: decoding the answer: %w", what, err))
	}
	return nil
}

// upload calls a signed Upload API endpoint with form parameters.
func (d *Driver) upload(ctx context.Context, action string, p url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.api+"/"+string(d.resource)+"/"+action, strings.NewReader(d.signed(p).Encode()))
	if err != nil {
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.do(req)
	if err != nil {
		return err
	}
	return decode(resp, action, out)
}

// admin calls the Admin API with Basic authentication.
func (d *Driver) admin(ctx context.Context, method, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, d.api+"/resources/"+string(d.resource)+"/"+string(d.typ)+path+"?"+q.Encode(), nil)
	if err != nil {
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	d.basic(req)
	resp, err := d.do(req)
	if err != nil {
		return err
	}
	return decode(resp, "admin "+method, out)
}

// resource is a raw resource as the APIs describe it.
type resource struct {
	PublicID  string    `json:"public_id"`
	Bytes     int64     `json:"bytes"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func entry(key string, r resource) filesystem.Entry {
	return filesystem.Entry{Key: key, Size: r.Bytes, ModTime: r.CreatedAt, ETag: strconv.FormatInt(r.Version, 10)}
}

// Stat implements filesystem.Driver with the Admin API.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	var r resource
	if err := d.admin(ctx, http.MethodGet, "/"+escapePath(encodeKey(key)), url.Values{}, &r); err != nil {
		return filesystem.Entry{}, err
	}
	return entry(key, r), nil
}

// escapePath escapes each segment of a public ID for a URL path.
func escapePath(id string) string {
	segs := strings.Split(id, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Read implements filesystem.Driver with the signed download endpoint (a
// URL valid for DownloadExpiry) and a Range header; a server answering 200
// is cut to the range here.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	q := url.Values{
		"public_id":  {encodeKey(key)},
		"type":       {string(d.typ)},
		"expires_at": {strconv.FormatInt(d.now().Add(DownloadExpiry).Unix(), 10)},
	}
	if f := d.format(key); f != "" {
		q.Set("format", f)
	}
	p := d.signed(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.api+"/"+string(d.resource)+"/download?"+p.Encode(), nil)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	if rng.Offset > 0 || rng.Length > 0 {
		v := "bytes=" + strconv.FormatInt(rng.Offset, 10) + "-"
		if rng.Length > 0 {
			v += strconv.FormatInt(rng.Offset+rng.Length-1, 10)
		}
		req.Header.Set("Range", v)
	}
	resp, err := d.do(req)
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
	return nil, statusError(resp, "download")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Write implements filesystem.Driver with one signed upload whose file
// part streams body; overwrite and invalidate are set, so the key is
// replaced and cached copies are purged.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, _ filesystem.WriteOptions) (filesystem.Entry, error) {
	p := d.signed(url.Values{
		"public_id":  {encodeKey(key)},
		"type":       {string(d.typ)},
		"overwrite":  {"true"},
		"invalidate": {"true"},
	})
	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	go func() {
		for k, vs := range p {
			for _, v := range vs {
				if err := form.WriteField(k, v); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			}
		}
		part, err := form.CreateFormFile("file", "blob")
		if err == nil {
			_, err = io.Copy(part, bodyReader{body})
		}
		if err == nil {
			err = form.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.api+"/"+string(d.resource)+"/upload", pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return filesystem.Entry{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := d.do(req)
	_ = pr.Close() // stops the writer if the request ended early
	if err != nil {
		return filesystem.Entry{}, err
	}
	var r resource
	if err := decode(resp, "upload", &r); err != nil {
		return filesystem.Entry{}, err
	}
	return entry(key, r), nil
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

// Delete implements filesystem.Driver with a signed destroy; a missing key
// succeeds ("not found" is Cloudinary's answer, with status 200).
func (d *Driver) Delete(ctx context.Context, key string) error {
	return d.upload(ctx, "destroy", url.Values{"public_id": {encodeKey(key)}, "type": {string(d.typ)}, "invalidate": {"true"}}, nil)
}

// Move implements filesystem.Mover with a signed rename, overwriting dst.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if _, err := d.Stat(ctx, src); err != nil {
		return filesystem.Entry{}, err
	}
	if src != dst {
		p := url.Values{
			"from_public_id": {encodeKey(src)},
			"to_public_id":   {encodeKey(dst)},
			"type":           {string(d.typ)},
			"overwrite":      {"true"},
			"invalidate":     {"true"},
		}
		if err := d.upload(ctx, "rename", p, nil); err != nil {
			return filesystem.Entry{}, err
		}
	}
	return d.Stat(ctx, dst)
}

// DeleteBatch is how many public IDs one Admin API delete names (its
// maximum).
const DeleteBatch = 100

// deleted is the answer of an Admin API delete.
type deleted struct {
	Deleted    map[string]string `json:"deleted"`
	Partial    bool              `json:"partial"`
	NextCursor string            `json:"next_cursor"`
}

// DeleteKeys implements filesystem.KeysDeleter, DeleteBatch keys per Admin
// API request; a failed request reports its keys in a *BatchError and the
// other batches are still tried.
func (d *Driver) DeleteKeys(ctx context.Context, keys []string) error {
	var failed []filesystem.KeyError
	for start := 0; start < len(keys); start += DeleteBatch {
		batch := keys[start:min(start+DeleteBatch, len(keys))]
		q := url.Values{"invalidate": {"true"}}
		for _, k := range batch {
			q.Add("public_ids[]", encodeKey(k))
		}
		if err := d.admin(ctx, http.MethodDelete, "", q, nil); err != nil {
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

// DeletePrefix implements filesystem.PrefixDeleter with Admin API deletes
// by prefix, repeated while Cloudinary reports a partial deletion.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	q := url.Values{"prefix": {encodeKey(dir) + "/"}, "invalidate": {"true"}}
	for {
		var out deleted
		if err := d.admin(ctx, http.MethodDelete, "", q, &out); err != nil {
			return err
		}
		if !out.Partial {
			return nil
		}
		q.Set("next_cursor", out.NextCursor)
	}
}

// listPageSize is the max_results asked of each Admin API listing (its
// maximum).
const listPageSize = 500

// List implements filesystem.Driver with Admin API listings by prefix,
// following next_cursor, then pages with filesystem.PageOf. Public IDs
// this driver did not write are skipped.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	q := url.Values{"max_results": {strconv.Itoa(listPageSize)}}
	if dir != "" {
		q.Set("prefix", encodeKey(dir)+"/")
	}
	var all []filesystem.Entry
	for {
		var page struct {
			Resources  []resource `json:"resources"`
			NextCursor string     `json:"next_cursor"`
		}
		if err := d.admin(ctx, http.MethodGet, "", q, &page); err != nil {
			return filesystem.Page{}, err
		}
		for _, r := range page.Resources {
			if k, err := decodeKey(r.PublicID); err == nil && filesystem.KeyInDir(k, dir) {
				all = append(all, entry(k, r))
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Set("next_cursor", page.NextCursor)
	}
	return filesystem.PageOf(all, dir, opts), nil
}

// PublicURL implements filesystem.PublicURLer for TypeUpload: the
// resource's delivery URL. For image and video resources the key's
// extension is repeated as the delivery format (see Config.ResourceType),
// since Cloudinary would otherwise read it as a format and look for the
// public ID without it.
func (d *Driver) PublicURL(_ context.Context, key string) (string, error) {
	if d.typ != TypeUpload {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "cloudinary: %s resources have no public URL", d.typ)
	}
	u := d.delivery + "/" + string(d.resource) + "/upload/" + escapePath(encodeKey(key))
	if f := d.format(key); f != "" {
		u += "." + f
	}
	return u, nil
}

// format is the delivery format a read of key names: none for raw
// resources (their public ID holds the whole name), the key's extension
// for image and video resources (see Config.ResourceType).
func (d *Driver) format(key string) string {
	if d.resource == ResourceRaw {
		return ""
	}
	ext := path.Ext(key)
	if ext == "" || !idSafeString(ext[1:]) {
		return ""
	}
	return ext[1:]
}
