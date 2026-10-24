// Package signedurl signs and verifies URLs for disks whose backend cannot
// sign its own (local disk, memory), so that signed downloads and direct
// uploads work the same in development as on S3. The fshttp package serves
// the URLs this package issues.
//
// A URL has the form
//
//	<base>/<disk>/<escaped key>?m=GET&exp=<unix>&ct=…&cd=…&kid=…&sig=<hex>
//
// plus, for a PUT that stores HTTP headers, hcc, hcd and hce (the stored
// Cache-Control, Content-Disposition and Content-Encoding), and, for a URL
// restricted to an address range (PresignOptions.AllowedIP), ip,
// where sig is HMAC-SHA256 over every one of those values, so none can be
// changed without invalidating it. Signing keys can be rotated: the newest
// key signs, and older keys still verify until they are removed.
package signedurl

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// headerContentType is the request header a PUT must carry when the URL
// fixes the content type.
const headerContentType = "Content-Type"

// MinKeyBytes is the shortest accepted secret: 256 bits, the HMAC-SHA256
// block-strength recommendation.
const MinKeyBytes = 32

// Query parameter names of a signed URL.
const (
	ParamMethod      = "m"
	ParamExpires     = "exp"
	ParamContentType = "ct"
	ParamDisposition = "cd"
	ParamKeyID       = "kid"
	ParamSignature   = "sig"
	// The stored headers of a PUT (PresignOptions.Headers).
	ParamCacheControl      = "hcc"
	ParamStoredDisposition = "hcd"
	ParamContentEncoding   = "hce"
	// The address range a request must come from (PresignOptions.AllowedIP).
	ParamAllowedIP = "ip"
)

// Key is one signing secret and the ID that names it in URLs. IDs let a
// verifier pick the right key directly during a rotation.
//
// A Key never prints its Secret: fmt (every verb, %v, %+v and %#v
// included), slog and anything calling String show the ID and
// "[REDACTED]", so a key logged by mistake, alone or inside a config
// struct, does not leak. Read Secret directly where it is really needed.
type Key struct {
	ID     string
	Secret []byte
}

// redacted stands in for a secret wherever a Key or Signer is printed.
const redacted = "[REDACTED]"

// String implements fmt.Stringer without the secret.
func (k Key) String() string {
	return "signedurl.Key{ID: " + strconv.Quote(k.ID) + ", Secret: " + redacted + "}"
}

// GoString implements fmt.GoStringer (%#v) without the secret.
func (k Key) GoString() string { return k.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (k Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// LogValue implements slog.LogValuer: the ID, and the secret redacted.
func (k Key) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", k.ID), slog.String("secret", redacted))
}

// Signer issues and verifies signed URLs. It implements
// filesystem.URLSigner. It is safe for concurrent use.
type Signer struct {
	base string
	keys []Key
	now  func() time.Time
}

// String implements fmt.Stringer: the base and the key IDs, never a
// secret. Signer's own fields are unexported, which fmt prints raw without
// calling Key's methods, so Signer redacts itself too.
func (s Signer) String() string {
	ids := make([]string, len(s.keys))
	for i, k := range s.keys {
		ids[i] = strconv.Quote(k.ID)
	}
	return "signedurl.Signer{base: " + strconv.Quote(s.base) + ", keys: [" + strings.Join(ids, " ") + "], secrets: " + redacted + "}"
}

// GoString implements fmt.GoStringer (%#v) like String.
func (s Signer) GoString() string { return s.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (s Signer) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue implements slog.LogValuer: the base and the key IDs.
func (s Signer) LogValue() slog.Value {
	ids := make([]string, len(s.keys))
	for i, k := range s.keys {
		ids[i] = k.ID
	}
	return slog.GroupValue(slog.String("base", s.base), slog.Any("key_ids", ids))
}

// Option configures a Signer.
type Option func(*Signer)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(s *Signer) { s.now = now } }

// New returns a Signer issuing URLs under base (the absolute URL where the
// fshttp handler is mounted, e.g. "https://app.example.com/files"). keys[0]
// signs; every key verifies. Each secret must be at least MinKeyBytes long
// and each ID unique and URL-safe.
func New(base string, keys []Key, opts ...Option) (*Signer, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "signed URL base %q is not an absolute http(s) URL without a query", base)
	}
	if len(keys) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "no signing keys")
	}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if len(k.Secret) < MinKeyBytes {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "signing key %q is %d bytes, shorter than %d", k.ID, len(k.Secret), MinKeyBytes)
		}
		if k.ID == "" || k.ID != url.PathEscape(k.ID) || seen[k.ID] {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "signing key ID %q is empty, not URL-safe or repeated", k.ID)
		}
		seen[k.ID] = true
	}
	s := &Signer{base: strings.TrimRight(base, "/"), keys: keys, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Claims are what a verified URL authorises.
type Claims struct {
	Disk               string
	Key                string
	Method             filesystem.HTTPMethod
	ExpiresAt          time.Time
	ContentType        string
	ContentDisposition string
	// Headers are what a PUT must store (PresignOptions.Headers); the
	// request must carry them, and fshttp.Signed checks that it does.
	Headers filesystem.HTTPHeaders
	// AllowedIP is the range the request must come from
	// (PresignOptions.AllowedIP); the zero Prefix means any address.
	// fshttp.Signed checks it against the request's address.
	AllowedIP netip.Prefix
}

// PinsHeaders implements filesystem.HeaderPinner: a PUT URL carries
// PresignOptions.Headers under its signature, and fshttp.Signed refuses a
// request without them and stores them.
func (s *Signer) PinsHeaders() bool { return true }

// RestrictsIP implements filesystem.IPRestricter: a URL carries
// PresignOptions.AllowedIP under its signature, and fshttp.Signed refuses a
// request from an address outside it.
func (s *Signer) RestrictsIP() bool { return true }

// headerParams pairs each stored-header parameter with its field.
func headerParams(h *filesystem.HTTPHeaders) []struct {
	param string
	value *string
} {
	return []struct {
		param string
		value *string
	}{
		{ParamCacheControl, &h.CacheControl},
		{ParamStoredDisposition, &h.ContentDisposition},
		{ParamContentEncoding, &h.ContentEncoding},
	}
}

// SignURL implements filesystem.URLSigner.
func (s *Signer) SignURL(_ context.Context, disk, key string, opts filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	if err := filesystem.ValidatePresign(opts); err != nil {
		return filesystem.SignedRequest{}, err
	}
	expires := s.now().Add(opts.Expires).Truncate(time.Second)
	c := Claims{Disk: disk, Key: key, Method: opts.Method, ExpiresAt: expires, ContentType: opts.ContentType, ContentDisposition: opts.ContentDisposition, Headers: opts.Headers}
	if opts.AllowedIP != "" {
		// ValidatePresign has checked it, so the parse cannot fail.
		c.AllowedIP, _ = filesystem.ParseAllowedIP(opts.AllowedIP)
	}
	k := s.keys[0]
	q := url.Values{}
	q.Set(ParamMethod, string(c.Method))
	q.Set(ParamExpires, strconv.FormatInt(expires.Unix(), 10))
	if c.ContentType != "" {
		q.Set(ParamContentType, c.ContentType)
	}
	if c.ContentDisposition != "" {
		q.Set(ParamDisposition, c.ContentDisposition)
	}
	for _, p := range headerParams(&c.Headers) {
		if *p.value != "" {
			q.Set(p.param, *p.value)
		}
	}
	if c.AllowedIP.IsValid() {
		q.Set(ParamAllowedIP, c.AllowedIP.String())
	}
	q.Set(ParamKeyID, k.ID)
	q.Set(ParamSignature, sign(k.Secret, c))
	req := filesystem.SignedRequest{
		Method:    opts.Method,
		URL:       s.base + "/" + url.PathEscape(disk) + "/" + filesystem.EscapeKey(key) + "?" + q.Encode(),
		ExpiresAt: expires,
	}
	if opts.Method == filesystem.MethodPut {
		h := http.Header{}
		if opts.ContentType != "" {
			h.Set(headerContentType, opts.ContentType)
		}
		opts.Headers.SetOn(h)
		if len(h) > 0 {
			req.Header = h
		}
	}
	return req, nil
}

// Verify checks a request for disk and key (already taken from the URL path
// by the caller) made with method and carrying query. It fails with
// CodePermission for a bad or missing signature, a wrong method, an unknown
// key ID or an expired URL; the error never says which, so a forger learns
// nothing.
func (s *Signer) Verify(disk, key string, method filesystem.HTTPMethod, query url.Values) (Claims, error) {
	exp, err := strconv.ParseInt(query.Get(ParamExpires), 10, 64)
	if err != nil {
		return Claims{}, denied()
	}
	c := Claims{
		Disk:               disk,
		Key:                key,
		Method:             filesystem.HTTPMethod(query.Get(ParamMethod)),
		ExpiresAt:          time.Unix(exp, 0),
		ContentType:        query.Get(ParamContentType),
		ContentDisposition: query.Get(ParamDisposition),
	}
	for _, p := range headerParams(&c.Headers) {
		*p.value = query.Get(p.param)
	}
	if ip := query.Get(ParamAllowedIP); ip != "" {
		if c.AllowedIP, err = filesystem.ParseAllowedIP(ip); err != nil {
			return Claims{}, denied()
		}
	}
	secret, ok := s.secret(query.Get(ParamKeyID))
	if !ok {
		return Claims{}, denied()
	}
	got, err := hex.DecodeString(query.Get(ParamSignature))
	want, _ := hex.DecodeString(sign(secret, c))
	if err != nil || !hmac.Equal(got, want) {
		return Claims{}, denied()
	}
	if c.Method != method || !s.now().Before(c.ExpiresAt) {
		return Claims{}, denied()
	}
	return c, nil
}

func (s *Signer) secret(id string) ([]byte, bool) {
	for _, k := range s.keys {
		if k.ID == id {
			return k.Secret, true
		}
	}
	return nil, false
}

func denied() error {
	return filesystem.Errorf(filesystem.CodePermission, "signed URL is invalid or expired")
}

// ipField tags the allowed IP range inside the signed fields.
const ipField = "ip="

// sign returns the hex HMAC of the claims in a canonical form. Fields are
// length-prefixed, so no value can be shifted into its neighbour to forge a
// different claim with the same bytes. The stored headers are appended only
// when one is set, so URLs without them sign exactly as before they
// existed and stay valid across the upgrade; with them, the field count
// differs, so no URL of one form can verify as the other. The allowed IP
// range is appended the same way, tagged, so it can never be read as a
// stored header.
func sign(secret []byte, c Claims) string {
	m := hmac.New(sha256.New, secret)
	fields := []string{string(c.Method), c.Disk, c.Key, strconv.FormatInt(c.ExpiresAt.Unix(), 10), c.ContentType, c.ContentDisposition}
	if !c.Headers.IsZero() {
		fields = append(fields, c.Headers.CacheControl, c.Headers.ContentDisposition, c.Headers.ContentEncoding)
	}
	if c.AllowedIP.IsValid() {
		fields = append(fields, ipField+c.AllowedIP.String())
	}
	for _, f := range fields {
		m.Write([]byte(strconv.Itoa(len(f)) + ":" + f + "\n"))
	}
	return hex.EncodeToString(m.Sum(nil))
}
