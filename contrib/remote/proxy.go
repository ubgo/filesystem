package remote

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/sniff"
)

const (
	// DefaultProxyMaxAge is how long a browser may keep a proxied image
	// (ProxyOptions.MaxAge), never past the URL's expiry.
	DefaultProxyMaxAge = 24 * time.Hour
	// ParamURL is the query parameter of a proxy URL naming the remote URL.
	ParamURL = "u"
	// proxyScope is signed in the place of a disk name, so a proxy
	// signature never verifies as a file URL of the same signer, nor the
	// other way round.
	proxyScope = "remote-proxy"
	// proxyCSP forbids every fetch and sandboxes any document, in case a
	// browser renders a proxied file as one.
	proxyCSP = "default-src 'none'; sandbox"
)

// ProxyOptions configures a Proxy.
type ProxyOptions struct {
	// Base is the absolute URL the Proxy is mounted at
	// ("https://app.example.com/media/proxy"); URLs are Base?u=…&exp=…&sig=….
	// Required.
	Base string
	// Signer signs and verifies proxy URLs (HMAC-SHA256, with key
	// rotation). Required. Give the proxy its own keys rather than those
	// of the application's file URLs: a key that leaks then opens one
	// door, not two.
	Signer *signedurl.Signer
	// MaxAge is the Cache-Control max-age of a proxied image, capped at
	// the URL's remaining lifetime; 0 means DefaultProxyMaxAge.
	MaxAge time.Duration
}

// Proxy serves remote media through the application, from the Cache, so
// a page shows third-party images without leaking its visitors to the
// third party and without mixed content. It answers only URLs it signed
// (URL), so it is never an open proxy; it serves only what the cache's
// policy accepts, re-checked from the bytes it sends, with nosniff, a
// sandboxing Content-Security-Policy and caching headers.
type Proxy struct {
	cache *Cache
	opts  ProxyOptions
}

// NewProxy returns a Proxy serving from c. It fails with
// CodeInvalidArgument without a signer or an absolute http(s) Base
// without a query.
func NewProxy(c *Cache, opts ProxyOptions) (*Proxy, error) {
	if c == nil || opts.Signer == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: a proxy needs a cache and a signer")
	}
	u, err := url.Parse(opts.Base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: proxy base %q is not an absolute http(s) URL without a query", opts.Base)
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultProxyMaxAge
	}
	return &Proxy{cache: c, opts: opts}, nil
}

// URL returns the proxy URL of rawURL, valid for expires. It fetches
// nothing: the proxy fetches on the first request.
func (p *Proxy) URL(ctx context.Context, rawURL string, expires time.Duration) (string, error) {
	canon, err := Canonical(rawURL)
	if err != nil {
		return "", err
	}
	sr, err := p.opts.Signer.SignURL(ctx, proxyScope, canon, filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: expires})
	if err != nil {
		return "", err
	}
	signed, err := url.Parse(sr.URL)
	if err != nil {
		return "", err
	}
	q := signed.Query()
	q.Set(ParamURL, canon)
	return p.opts.Base + "?" + q.Encode(), nil
}

// ServeHTTP answers GET and HEAD for a URL signed by URL: 403 for a bad,
// expired or missing signature, 415 when the bytes are not a type the
// cache's policy accepts, and the status of the fetch's error otherwise
// (fshttp.StatusOf), with a client-safe message.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	raw := q.Get(ParamURL)
	claims, err := p.opts.Signer.Verify(proxyScope, raw, filesystem.MethodGet, q)
	if err != nil {
		fail(w, err)
		return
	}
	rc, e, err := p.cache.Open(r.Context(), raw)
	if err != nil {
		fail(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	body := bufio.NewReaderSize(rc, sniff.HeadSize)
	head, err := body.Peek(sniff.HeadSize)
	if err != nil && !errors.Is(err, io.EOF) {
		fail(w, filesystem.NewError(filesystem.CodeUnavailable, err))
		return
	}
	// The stored type is not trusted: the bytes about to be sent decide.
	contentType := sniff.Detect(head, "")
	if err := p.cache.opts.Policy.Accepts(contentType); err != nil {
		fail(w, err)
		return
	}
	if limit := p.cache.opts.Policy.MaxSize; e.Size > limit {
		fail(w, filesystem.Errorf(filesystem.CodeTooLarge, "remote: cached object over the policy's size"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set(fshttp.HeaderContentTypeOptions, "nosniff")
	h.Set(fshttp.HeaderCSP, proxyCSP)
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(p.maxAge(claims.ExpiresAt).Seconds())))
	etag := e.SHA256
	if etag == "" {
		etag = e.ETag
	}
	if etag != "" {
		etag = `"` + strings.Trim(etag, `"`) + `"`
		h.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Length", strconv.FormatInt(e.Size, 10))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, body) // the client went away; nothing to answer
}

// maxAge is how long a browser may keep a response to a URL expiring at
// exp: MaxAge, but never past exp.
func (p *Proxy) maxAge(exp time.Time) time.Duration {
	left := exp.Sub(p.cache.opts.Now()).Truncate(time.Second)
	return max(min(p.opts.MaxAge, left), 0)
}

// fail answers err with its status and a message that never carries a
// key, an address or a backend's text.
func fail(w http.ResponseWriter, err error) {
	fshttp.SetRetryAfter(w.Header(), err)
	http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
}
