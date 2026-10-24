package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"syscall"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// Defaults of FetchOptions.
const (
	// DefaultFetchTimeout bounds a whole fetch, body included.
	DefaultFetchTimeout = time.Minute
	// DefaultMaxRedirects is how many redirects a fetch follows.
	DefaultMaxRedirects = 5
	// fetchDialTimeout and fetchHeaderTimeout bound the connection and the
	// wait for response headers, so a silent server fails fast.
	fetchDialTimeout   = 10 * time.Second
	fetchHeaderTimeout = 30 * time.Second
	// fetchFallbackName is the file name when neither the response nor the
	// URL offers one.
	fetchFallbackName = "download"
)

// FetchOptions configures Fetch and FromURL.
type FetchOptions struct {
	// Timeout bounds the whole fetch; 0 means DefaultFetchTimeout.
	Timeout time.Duration
	// MaxRedirects caps redirects; 0 means DefaultMaxRedirects, a negative
	// value follows none.
	MaxRedirects int
	// MaxSize refuses a response that announces a larger Content-Length
	// before reading it (CodeTooLarge); 0 does not check. FromURL sets it
	// from the policy. The body itself is not capped here: Receive and
	// Disk.Write count it as it streams.
	MaxSize int64
	// AllowAddr permits an address PublicAddr refuses, for imports from a
	// known internal server. Every connection, redirects included, is
	// checked against PublicAddr and then AllowAddr.
	AllowAddr func(netip.Addr) bool
	// Now is the clock a Retry-After given as an HTTP date is measured
	// from, for the wait a CodeUnavailable error carries
	// (filesystem.Error.RetryAfter); nil means time.Now.
	Now func() time.Time
	// Header is sent with every request of the fetch, redirects included
	// (Go drops Authorization, Cookie and WWW-Authenticate on a redirect
	// to another host, so a credential meant for one server does not
	// follow a redirect elsewhere): a User-Agent for sites that refuse
	// Go's default, an Authorization for an internal source, an Accept.
	// Host cannot be set through it (the URL's host is always used). nil
	// sends Go's defaults only.
	Header http.Header
	// Proxy, when set, sends every connection through this HTTP proxy
	// with CONNECT (Smokescreen, Squid, an egress gateway): only
	// "http://host:port", optionally with user:password for
	// Proxy-Authorization. The SSRF guard still decides the destination:
	// the fetch resolves the target's name itself, checks the address
	// against PublicAddr and AllowAddr exactly as without a proxy, and
	// asks the proxy to CONNECT to that address, never to the name, so
	// the proxy cannot resolve it elsewhere (DNS rebinding) and an
	// internal address is refused before the proxy is asked. TLS still
	// verifies the target's name end to end. The proxy's own address is
	// not checked: it is the operator's choice, not the user's. nil
	// connects directly, and the environment's HTTP_PROXY is never read.
	Proxy *url.URL
}

// Remote is a fetched response, ready to stream. The caller closes Body.
type Remote struct {
	Body io.ReadCloser
	// Filename comes from Content-Disposition, else the URL's last path
	// segment, else "download", and is made safe with SafeFilename.
	Filename string
	// Size is the announced Content-Length, or -1.
	Size int64
	// URL is the URL finally fetched, after redirects. Relative links in
	// the body (an HTML page's og:image) resolve against it, not against
	// the URL first asked for.
	URL string
}

// nat64 is the well-known NAT64 prefix (RFC 6052), whose addresses reach
// IPv4 hosts, private ones included.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// specialV4 lists IPv4 ranges outside the public internet that the netip
// predicates do not cover: "this network", shared address space (CGNAT),
// IETF protocol assignments, benchmarking, and reserved.
var specialV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// PublicAddr reports whether a is an address of the public internet. It
// refuses loopback, private (RFC 1918, unique local), link-local (cloud
// metadata services live at 169.254.169.254), multicast, unspecified,
// CGNAT, reserved and NAT64 addresses, and IPv4-mapped forms of any of
// them. Use it wherever a server connects to an address a user chose:
// imports, webhooks, link previews.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || nat64.Contains(a) {
		return false
	}
	for _, p := range specialV4 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// errBlocked is returned by the dialer for a refused address.
var errBlocked = errors.New("address is not on the public internet")

// allowed reports whether the guard lets a connection reach a.
func (o FetchOptions) allowed(a netip.Addr) bool {
	return PublicAddr(a) || (o.AllowAddr != nil && o.AllowAddr(a.Unmap()))
}

// guard checks the address a connection is about to be made to: after DNS
// resolution, so a hostname that resolves (or re-resolves, as in DNS
// rebinding) to an internal address is caught.
func (o FetchOptions) guard(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err // the dialer always passes a resolved "ip:port"; defensive
	}
	if o.allowed(ap.Addr()) {
		return nil
	}
	return fmt.Errorf("%w: %s", errBlocked, ap.Addr())
}

// Client returns the guarded HTTP client Fetch uses, for any other
// request to an address a user chose (webhooks, link previews): every
// connection, redirects included, goes only to addresses PublicAddr (or
// AllowAddr) accepts, checked after DNS resolution so a name that resolves
// or rebinds to an internal address is refused; it uses no proxy (a proxy
// would be dialed instead of the target, defeating the check), bounds the
// dial, TLS handshake and response headers, and follows at most
// MaxRedirects http(s) redirects (none when negative, where a redirect is
// an error). Timeout is not applied to it, since Fetch bounds each fetch
// with its context: set Client.Timeout on the result to bound a request.
// It keeps no connection alive, so build one per caller and reuse it.
func (o FetchOptions) Client() *http.Client {
	dialer := &net.Dialer{Timeout: fetchDialTimeout, Control: o.guard}
	dial := dialer.DialContext
	if o.Proxy != nil {
		dial = o.tunnel
	}
	transport := &http.Transport{
		// Never the environment's proxy: a proxy the guard does not
		// know about would be dialed instead of the target. Proxy
		// support goes through o.tunnel, which keeps the guard.
		Proxy:                 nil,
		DialContext:           dial,
		TLSHandshakeTimeout:   fetchDialTimeout,
		ResponseHeaderTimeout: fetchHeaderTimeout,
		ForceAttemptHTTP2:     true,
		// Each fetch has its own transport, so a kept-alive connection
		// would never be reused and never closed.
		DisableKeepAlives: true,
	}
	limit := o.MaxRedirects
	if limit == 0 {
		limit = DefaultMaxRedirects
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > limit {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: more than %d redirects", max(limit, 0))
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL allows absolute http and https URLs without credentials.
func checkURL(u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: only http and https URLs can be fetched")
	}
	if u.User != nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: URLs with credentials are refused")
	}
	return nil
}

// cancelOnClose ends the fetch's context when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// Fetch GETs rawURL with the SSRF guard and returns the response to
// stream. Errors: CodeInvalidArgument for a refused URL, address, redirect
// or 4xx answer other than those below; CodeNotFound for 404 and 410;
// CodeTooLarge for an announced size over MaxSize; CodeUnavailable for
// network failures, timeouts, 429 and 5xx answers.
func Fetch(ctx context.Context, rawURL string, o FetchOptions) (*Remote, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: invalid URL")
	}
	if err := checkURL(u); err != nil {
		return nil, err
	}
	if err := checkProxy(o.Proxy); err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// Built from the parsed URL, which NewRequest would only parse again.
	header := o.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	req := (&http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: header}).WithContext(ctx)
	resp, err := o.Client().Do(req)
	if err != nil {
		cancel()
		return nil, fetchErr(err)
	}
	if err := checkResponse(resp, o.MaxSize, o.Now); err != nil {
		_ = resp.Body.Close()
		cancel()
		return nil, err
	}
	return &Remote{
		Body:     cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
		Filename: remoteName(resp),
		Size:     resp.ContentLength,
		URL:      resp.Request.URL.String(),
	}, nil
}

// fetchErr classifies a failed request: refusals by the guard or the
// redirect policy are the caller's input, everything else is the network.
func fetchErr(err error) error {
	var fe *filesystem.Error
	switch {
	case errors.As(err, &fe):
		return fe
	case errors.Is(err, errBlocked):
		return filesystem.NewError(filesystem.CodeInvalidArgument, errBlocked)
	}
	return filesystem.NewError(filesystem.CodeUnavailable, err)
}

func checkResponse(resp *http.Response, maxSize int64, now func() time.Time) error {
	switch status := resp.StatusCode; {
	case status == http.StatusNotFound || status == http.StatusGone:
		return filesystem.Errorf(filesystem.CodeNotFound, "upload: the remote server answered %d", status)
	case status >= 500 || status == http.StatusTooManyRequests:
		// 429 is the remote throttling us: transient, so retryable, after
		// the wait it asked for.
		err := filesystem.Errorf(filesystem.CodeUnavailable, "upload: the remote server answered %d", status)
		if after, ok := fshttp.ParseRetryAfter(resp.Header.Get(fshttp.HeaderRetryAfter), now()); ok {
			err = err.WithRetryAfter(after)
		}
		return err
	case status < 200 || status > 299:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: the remote server answered %d", status)
	}
	if maxSize > 0 && resp.ContentLength > maxSize {
		return filesystem.Errorf(filesystem.CodeTooLarge, "upload: the remote file is %d bytes, over the limit of %d", resp.ContentLength, maxSize)
	}
	return nil
}

// remoteName picks the file name of a response (see Remote.Filename).
func remoteName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		return SafeFilename(params["filename"])
	}
	if base := path.Base(resp.Request.URL.Path); base != "/" && base != "." {
		return SafeFilename(base)
	}
	return fetchFallbackName
}

// FromURL fetches rawURL (see Fetch) and stores it on disk under policy p
// (see Receive): the type is detected from the bytes, never from the
// server's Content-Type, and the size is capped as the body streams. A
// Remote's announced size over p.MaxSize is refused before any byte is
// read.
func FromURL(ctx context.Context, disk *filesystem.Disk, rawURL string, p Policy, o FetchOptions) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	o.MaxSize = p.MaxSize
	remote, err := Fetch(ctx, rawURL, o)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = remote.Body.Close() }()
	return Receive(ctx, disk, remote.Body, remote.Filename, p)
}
