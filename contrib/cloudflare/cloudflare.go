// Package cloudflare signs URLs for Cloudflare token authentication: a WAF
// custom rule that blocks a request unless its last query parameter carries
// a valid timed HMAC, checked at the edge by is_timed_hmac_valid_v0.
//
// A Signer implements filesystem.URLSigner, so Disk.SignedURL returns a
// link Cloudflare lets through until it expires:
//
//	signer, err := cloudflare.New(cloudflare.Config{
//		BaseURL: "https://downloads.example.com/files",
//		Secret:  secret, // the rule's key
//		TTL:     3 * time.Hour, // the rule's ttl
//	})
//	disk, err := filesystem.NewDisk("downloads", drv, filesystem.WithURLSigner(signer))
//
// paired with the rule (separator length 8 is len("?verify="))
//
//	(http.host eq "downloads.example.com" and not
//	 is_timed_hmac_valid_v0("<secret>", http.request.uri, 10800, http.request.timestamp.sec, 8))
//
// The token follows Cloudflare's documented algorithm: the message is the
// URL path exactly as sent, the MAC is HMAC-SHA256(secret, path +
// timestamp), standard base64, URL-encoded (or URL-safe base64 without
// padding with Config.URLSafeMAC, for a rule using the "s" flag), and the
// parameter is "<param>=<timestamp>-<mac>".
//
// Cloudflare's token carries the time it was issued, and the rule's ttl
// decides when it expires. To give each link its own lifetime, the Signer
// issues the token as if at now + Expires - TTL, so it expires exactly
// Expires from now; Expires may therefore not exceed Config.TTL.
//
// Cloudflare serves what the origin returns: a link cannot override the
// response's Content-Type or Content-Disposition, so a link that needs one
// (including the attachment the Disk forces on active types such as HTML
// and SVG) is CodeUnsupported rather than issued without it.
package cloudflare

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// DefaultParam is the query parameter carrying the token, as in
// Cloudflare's documentation.
const DefaultParam = "verify"

// MinSecretBytes is the shortest accepted secret: 256 bits, the
// HMAC-SHA256 block-strength recommendation (signedurl uses the same).
const MinSecretBytes = 32

// redacted stands in for the secret wherever a Config or Signer is
// printed.
const redacted = "[REDACTED]"

// Config configures a Signer. Every field except URLSafeMAC, Param and
// Clock must match the WAF rule.
//
// A Config never prints its Secret: fmt (every verb), slog and anything
// calling String show "[REDACTED]".
type Config struct {
	// BaseURL is the absolute https URL the disk's keys are served under,
	// e.g. "https://downloads.example.com/files". Required, no query.
	BaseURL string
	// Secret is the key given to is_timed_hmac_valid_v0. At least
	// MinSecretBytes.
	Secret []byte
	// TTL is the rule's ttl argument: required, positive, whole seconds,
	// at most filesystem.MaxPresignExpiry. It is the longest Expires a
	// link may ask for.
	TTL time.Duration
	// Param is the query parameter name; empty means DefaultParam. The
	// rule's separator length is len(Param) + 2 ("?" and "=").
	Param string
	// URLSafeMAC encodes the MAC as URL-safe base64 without padding, for a
	// rule called with the "s" flag. Off: standard base64, URL-encoded.
	URLSafeMAC bool
	// Clock replaces time.Now, for tests. Nil means time.Now.
	Clock func() time.Time
}

// String implements fmt.Stringer without the secret.
func (c Config) String() string {
	return "cloudflare.Config{BaseURL: " + strconv.Quote(c.BaseURL) + ", TTL: " + c.TTL.String() + ", Param: " + strconv.Quote(c.Param) + ", Secret: " + redacted + "}"
}

// GoString implements fmt.GoStringer (%#v) like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer: the base URL and TTL, the secret
// redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("base_url", c.BaseURL), slog.Duration("ttl", c.TTL), slog.String("secret", redacted))
}

// Signer issues Cloudflare token-authentication URLs. It implements
// filesystem.URLSigner and is safe for concurrent use; one Signer serves
// one rule and base URL, so give each disk its own.
type Signer struct {
	cfg Config
	// origin is the base URL's scheme and host; base its escaped path,
	// which starts every signed message.
	origin, base string
}

// String implements fmt.Stringer without the secret.
func (s *Signer) String() string { return "cloudflare.Signer{" + s.cfg.String() + "}" }

// GoString implements fmt.GoStringer (%#v) like String.
func (s *Signer) GoString() string { return s.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (s *Signer) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue implements slog.LogValuer like Config.LogValue.
func (s *Signer) LogValue() slog.Value { return s.cfg.LogValue() }

// New checks cfg and returns a Signer. It copies Secret, so a caller
// clearing its own slice afterwards does not change the Signer.
func New(cfg Config) (*Signer, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "Cloudflare base URL %q is not an absolute https URL without a query", cfg.BaseURL)
	}
	if len(cfg.Secret) < MinSecretBytes {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "Cloudflare token secret is %d bytes, shorter than %d", len(cfg.Secret), MinSecretBytes)
	}
	if cfg.TTL < time.Second || cfg.TTL > filesystem.MaxPresignExpiry || cfg.TTL%time.Second != 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "Cloudflare token TTL %s is not whole seconds in [1s, %s]", cfg.TTL, filesystem.MaxPresignExpiry)
	}
	if cfg.Param == "" {
		cfg.Param = DefaultParam
	}
	if cfg.Param != url.QueryEscape(cfg.Param) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "Cloudflare token parameter %q is not URL-safe", cfg.Param)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	cfg.Secret = append([]byte(nil), cfg.Secret...)
	return &Signer{cfg: cfg, base: strings.TrimRight(u.EscapedPath(), "/"), origin: u.Scheme + "://" + u.Host}, nil
}

// SignURL implements filesystem.URLSigner for GET and HEAD of exactly key,
// until opts.Expires from now. The disk name is not part of the URL. A PUT
// is CodeUnsupported (Cloudflare guards a read path), and so is a content
// type or disposition override, which Cloudflare cannot apply. Expires
// beyond Config.TTL is CodeInvalidArgument.
func (s *Signer) SignURL(_ context.Context, _, key string, opts filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	if err := filesystem.ValidatePresign(opts); err != nil {
		return filesystem.SignedRequest{}, err
	}
	if opts.Method != filesystem.MethodGet {
		return filesystem.SignedRequest{}, filesystem.Errorf(filesystem.CodeUnsupported, "a Cloudflare token URL is for downloads only")
	}
	if opts.ContentType != "" || opts.ContentDisposition != "" {
		return filesystem.SignedRequest{}, filesystem.Errorf(filesystem.CodeUnsupported, "Cloudflare token authentication cannot override the response's Content-Type or Content-Disposition")
	}
	if opts.Expires > s.cfg.TTL {
		return filesystem.SignedRequest{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "expiry %s is longer than the rule's TTL %s", opts.Expires, s.cfg.TTL)
	}
	expires := s.cfg.Clock().Add(opts.Expires).Truncate(time.Second)
	issued := expires.Add(-s.cfg.TTL)
	path := s.base + "/" + filesystem.EscapeKey(key)
	return filesystem.SignedRequest{
		Method:    filesystem.MethodGet,
		URL:       s.origin + path + "?" + s.cfg.Param + "=" + s.token(path, issued),
		ExpiresAt: expires,
	}, nil
}

// token is "<timestamp>-<mac>" for path issued at issued, per Cloudflare's
// documented algorithm.
func (s *Signer) token(path string, issued time.Time) string {
	ts := strconv.FormatInt(issued.Unix(), 10)
	m := hmac.New(sha256.New, s.cfg.Secret)
	_, _ = io.WriteString(m, path+ts)
	sum := m.Sum(nil)
	if s.cfg.URLSafeMAC {
		return ts + "-" + base64.RawURLEncoding.EncodeToString(sum)
	}
	return ts + "-" + url.QueryEscape(base64.StdEncoding.EncodeToString(sum))
}
