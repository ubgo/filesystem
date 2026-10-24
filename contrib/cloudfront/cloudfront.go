// Package cloudfront signs Amazon CloudFront URLs and cookies for a disk
// whose objects are served through a CloudFront distribution with
// restricted viewer access (a trusted key group).
//
// A Signer implements filesystem.URLSigner, so Disk.SignedURL returns a
// CloudFront link that expires, signed with the private key of a public key
// in the distribution's key group:
//
//	signer, err := cloudfront.New(cloudfront.Config{
//		BaseURL:    "https://cdn.example.com/media",
//		KeyPairID:  "K2JCJMDEHXQW5F",
//		PrivateKey: key, // e.g. from sign.LoadPEMPrivKey
//	})
//	disk, err := filesystem.NewDisk("media", drv, filesystem.WithURLSigner(signer))
//
// SignCookies issues the three CloudFront signed cookies for every key
// under a prefix, for pages that load many private files (an HLS playlist
// and its segments, a photo gallery).
//
// CloudFront serves the object as its origin stored it: it cannot apply a
// per-link Content-Type or Content-Disposition. A signed link that needs
// one (an explicit override, or the attachment the Disk forces on active
// types such as HTML and SVG) is refused with CodeUnsupported, unless
// Config.ResponseOverrides says the distribution forwards S3's
// response-content-type and response-content-disposition query parameters
// to an origin that honours them. A link is never issued that would
// silently render active content inline.
package cloudfront

import (
	"context"
	"crypto"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign"

	"github.com/ubgo/filesystem"
)

// S3 query parameters that override the response's headers. CloudFront
// forwards them only when the cache policy includes them, and S3 honours
// them only on an authenticated (origin access control) request.
const (
	ParamResponseContentType        = "response-content-type"
	ParamResponseContentDisposition = "response-content-disposition"
)

// redacted stands in for the private key wherever a Config or Signer is
// printed.
const redacted = "[REDACTED]"

// wildcard is the CloudFront policy wildcard: any sequence of characters.
const wildcard = "*"

// Config configures a Signer.
//
// A Config never prints its PrivateKey: fmt (every verb), slog and
// anything calling String show the key pair ID and "[REDACTED]", so a
// config logged by mistake does not leak the key.
type Config struct {
	// BaseURL is the absolute https URL the disk's keys are served under,
	// e.g. "https://d111111abcdef8.cloudfront.net" or
	// "https://cdn.example.com/media". Required, no query.
	BaseURL string
	// KeyPairID is the ID CloudFront gave the public key (the
	// Key-Pair-Id parameter). Required.
	KeyPairID string
	// PrivateKey is the private half of that public key; an
	// *rsa.PrivateKey from sign.LoadPEMPrivKey works. Required.
	PrivateKey crypto.Signer
	// HashAlgorithm chooses the signature hash: empty or sign.HashSHA1
	// (CloudFront's default, accepted by every distribution), or
	// sign.HashSHA256, which adds Hash-Algorithm=SHA256 to every URL and
	// a CloudFront-Hash-Algorithm cookie.
	HashAlgorithm sign.HashAlgorithm
	// ResponseOverrides puts PresignOptions.ContentType and
	// ContentDisposition on the URL as S3's response-content-type and
	// response-content-disposition parameters, under the signature. Set
	// it only when the distribution's cache policy forwards both to an S3
	// origin reached through origin access control; otherwise a link
	// asking for an override is CodeUnsupported.
	ResponseOverrides bool
	// CookieDomain is the Domain of the cookies SignCookies returns;
	// empty means the host that sets them.
	CookieDomain string
	// CookiePath is the Path of those cookies; "" means "/", every path
	// of the domain. Narrow it ("/media/") when other paths of the
	// domain must not receive them. It must start with "/" and hold no
	// ";" , "," or control character.
	CookiePath string
	// CookiePathFor returns the Path of the cookies one SignCookies call
	// returns, given its cleaned prefix ("" for the whole base URL), so
	// two grants can live side by side: CloudFront's cookie names are
	// fixed, so two grants on one Path replace each other, while grants on
	// "/ws/a/" and "/ws/b/" are both sent (a member of two workspaces, a
	// course plus a shared library). Nil, or a "" return, uses CookiePath.
	// The result is checked like CookiePath (it must start with "/" and
	// hold no ";", "," or control character); a refused path or an error
	// fails the call, so no cookie is issued.
	//
	// The Path only scopes which requests carry the cookie; the signed
	// policy still names the prefix, so a wider Path cannot widen what the
	// cookie grants.
	CookiePathFor func(prefix string) (string, error)
	// CookieSameSite is their SameSite attribute; 0 means
	// http.SameSiteLaxMode. http.SameSiteNoneMode lets a CDN on another
	// site use them (a page on app.example.com loading media from
	// cdn.example.net); http.SameSiteStrictMode sends them only from the
	// CDN's own site. http.SameSiteDefaultMode, which would leave the
	// choice to the browser, is refused. The cookies stay Secure and
	// HttpOnly whatever is chosen.
	CookieSameSite http.SameSite
	// Clock replaces time.Now, for tests. Nil means time.Now.
	Clock func() time.Time
}

// String implements fmt.Stringer without the private key.
func (c Config) String() string {
	return "cloudfront.Config{BaseURL: " + strconv.Quote(c.BaseURL) + ", KeyPairID: " + strconv.Quote(c.KeyPairID) + ", PrivateKey: " + redacted + "}"
}

// GoString implements fmt.GoStringer (%#v) like String.
func (c Config) GoString() string { return c.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (c Config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue implements slog.LogValuer: the base URL and key pair ID.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(slog.String("base_url", c.BaseURL), slog.String("key_pair_id", c.KeyPairID), slog.String("private_key", redacted))
}

// defaultCookiePath is the cookies' Path when Config.CookiePath is "".
const defaultCookiePath = "/"

// checkCookiePath refuses a cookie Path that is relative or could inject
// another cookie attribute (";", ",") or break the header (control
// characters), whether it comes from Config.CookiePath or CookiePathFor.
func checkCookiePath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsFunc(p, func(r rune) bool { return r == ';' || r == ',' || r < ' ' || r == 0x7f }) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "CloudFront cookie path %q must start with / and hold no ; , or control character", p)
	}
	return nil
}

// cookiePath is the Path of the cookies granting dir: Config.CookiePathFor's
// answer when it gives one, else Config.CookiePath.
func (s *Signer) cookiePath(dir string) (string, error) {
	if s.cfg.CookiePathFor == nil {
		return s.cfg.CookiePath, nil
	}
	p, err := s.cfg.CookiePathFor(dir)
	if err != nil {
		return "", fmt.Errorf("CloudFront cookie path for %q: %w", dir, err)
	}
	if p == "" {
		return s.cfg.CookiePath, nil
	}
	if err := checkCookiePath(p); err != nil {
		return "", err
	}
	return p, nil
}

// Signer signs CloudFront URLs and cookies. It implements
// filesystem.URLSigner. It is safe for concurrent use; one Signer serves
// one distribution path, so give each disk its own.
type Signer struct {
	cfg     Config
	base    string
	urls    *sign.URLSigner
	cookies *sign.CookieSigner
}

// String implements fmt.Stringer without the private key.
func (s *Signer) String() string { return "cloudfront.Signer{" + s.cfg.String() + "}" }

// GoString implements fmt.GoStringer (%#v) like String.
func (s *Signer) GoString() string { return s.String() }

// Format implements fmt.Formatter, so every verb prints String.
func (s *Signer) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue implements slog.LogValuer like Config.LogValue.
func (s *Signer) LogValue() slog.Value { return s.cfg.LogValue() }

// New checks cfg and returns a Signer. BaseURL must be an absolute https
// URL without a query, and KeyPairID and PrivateKey are required.
func New(cfg Config) (*Signer, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "CloudFront base URL %q is not an absolute https URL without a query", cfg.BaseURL)
	}
	if cfg.KeyPairID == "" || cfg.KeyPairID != url.QueryEscape(cfg.KeyPairID) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "CloudFront key pair ID %q is empty or not URL-safe", cfg.KeyPairID)
	}
	if cfg.PrivateKey == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "CloudFront private key is nil")
	}
	switch cfg.HashAlgorithm {
	case "", sign.HashSHA1, sign.HashSHA256:
	default:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "CloudFront hash algorithm %q is not SHA1 or SHA256", cfg.HashAlgorithm)
	}
	if cfg.CookiePath == "" {
		cfg.CookiePath = defaultCookiePath
	}
	if err := checkCookiePath(cfg.CookiePath); err != nil {
		return nil, err
	}
	switch cfg.CookieSameSite {
	case 0:
		cfg.CookieSameSite = http.SameSiteLaxMode
	case http.SameSiteLaxMode, http.SameSiteStrictMode, http.SameSiteNoneMode:
	default:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "CloudFront cookie SameSite %d is not Lax, Strict or None", cfg.CookieSameSite)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	urls := sign.NewURLSigner(cfg.KeyPairID, cfg.PrivateKey)
	urls.HashAlg = cfg.HashAlgorithm
	cookies := sign.NewCookieSigner(cfg.KeyPairID, cfg.PrivateKey)
	cookies.HashAlg = cfg.HashAlgorithm
	return &Signer{cfg: cfg, base: strings.TrimRight(cfg.BaseURL, "/"), urls: urls, cookies: cookies}, nil
}

// SignURL implements filesystem.URLSigner with a canned policy: the URL
// works for GET and HEAD of exactly key until opts.Expires from now. The
// disk name is not part of a CloudFront URL. A PUT is CodeUnsupported (a
// distribution is a read path; sign uploads with the bucket's own
// presigning), and so is a content type or disposition override without
// Config.ResponseOverrides.
func (s *Signer) SignURL(_ context.Context, _, key string, opts filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	if err := filesystem.ValidatePresign(opts); err != nil {
		return filesystem.SignedRequest{}, err
	}
	if opts.Method != filesystem.MethodGet {
		return filesystem.SignedRequest{}, filesystem.Errorf(filesystem.CodeUnsupported, "a CloudFront signed URL is for downloads only")
	}
	raw := s.base + "/" + filesystem.EscapeKey(key)
	if opts.ContentType != "" || opts.ContentDisposition != "" {
		if !s.cfg.ResponseOverrides {
			return filesystem.SignedRequest{}, filesystem.Errorf(filesystem.CodeUnsupported, "CloudFront cannot override the response's Content-Type or Content-Disposition without Config.ResponseOverrides")
		}
		q := url.Values{}
		if opts.ContentType != "" {
			q.Set(ParamResponseContentType, opts.ContentType)
		}
		if opts.ContentDisposition != "" {
			q.Set(ParamResponseContentDisposition, opts.ContentDisposition)
		}
		raw += "?" + q.Encode()
	}
	expires := s.cfg.Clock().Add(opts.Expires).Truncate(time.Second)
	signed, err := s.urls.Sign(raw, expires)
	if err != nil {
		return filesystem.SignedRequest{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return filesystem.SignedRequest{Method: filesystem.MethodGet, URL: signed, ExpiresAt: expires}, nil
}

// SignCookies returns the CloudFront signed cookies (CloudFront-Policy,
// CloudFront-Signature, CloudFront-Key-Pair-Id, and
// CloudFront-Hash-Algorithm with SHA256) granting GET on every key under
// prefix until expires from now, and that expiry. prefix is cleaned like a
// Disk directory and "" means the whole base URL. expires follows the
// signed-URL rule: required, positive, at most filesystem.MaxPresignExpiry.
// The cookies are Secure and HttpOnly, expire with the policy, and carry
// Config.CookieDomain, Config.CookiePathFor's or Config.CookiePath's Path
// (default "/") and Config.CookieSameSite (default Lax); set each with http.SetCookie.
//
// The policy's wildcard also matches any query string, so the cookies
// serve every object under prefix as it is stored: an HTML or SVG file
// stored without "Content-Disposition: attachment" renders inline. Store
// active files with that header or keep them out of cookie-signed
// prefixes. With Config.ResponseOverrides,
// SignCookies is CodeUnsupported: the holder could add
// response-content-type and response-content-disposition to any URL under
// the wildcard and turn any object into inline HTML.
func (s *Signer) SignCookies(_ context.Context, prefix string, expires time.Duration) ([]*http.Cookie, time.Time, error) {
	if s.cfg.ResponseOverrides {
		return nil, time.Time{}, filesystem.Errorf(filesystem.CodeUnsupported, "CloudFront signed cookies are refused with Config.ResponseOverrides: the policy wildcard would let the holder choose the response's type and disposition")
	}
	if err := filesystem.ValidatePresign(filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: expires}); err != nil {
		return nil, time.Time{}, err
	}
	dir, err := filesystem.CleanDir(prefix)
	if err != nil {
		return nil, time.Time{}, err
	}
	// EscapeKey escapes "*" and "?", so a prefix cannot widen the
	// policy's wildcard.
	resource := s.base + "/"
	if dir != "" {
		resource += filesystem.EscapeKey(dir) + "/"
	}
	path, err := s.cookiePath(dir)
	if err != nil {
		return nil, time.Time{}, err
	}
	at := s.cfg.Clock().Add(expires).Truncate(time.Second)
	policy := &sign.Policy{Statements: []sign.Statement{{
		Resource:  resource + wildcard,
		Condition: sign.Condition{DateLessThan: sign.NewAWSEpochTime(at)},
	}}}
	cookies, err := s.cookies.SignWithPolicy(policy, func(o *sign.CookieOptions) {
		o.Path = path
		o.Domain = s.cfg.CookieDomain
		o.Secure = true
		o.SameSite = s.cfg.CookieSameSite
		o.Expires = at
	})
	if err != nil {
		return nil, time.Time{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return cookies, at, nil
}
