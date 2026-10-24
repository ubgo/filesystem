package fshttp

import (
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/signedurl"
)

// Disks resolves a disk by name. *filesystem.Manager implements it.
type Disks interface {
	Disk(name string) (*filesystem.Disk, error)
}

// Verifier checks a signed request. *signedurl.Signer implements it.
// Signed hands it the disk and key from the path ("/{disk}/{key}") and the
// query string; a token carried anywhere else needs RequestVerifier.
type Verifier interface {
	Verify(disk, key string, method filesystem.HTTPMethod, query url.Values) (signedurl.Claims, error)
}

// RequestVerifier is an optional upgrade of Verifier: when the verifier
// given to Signed implements it, Signed calls VerifyRequest with the whole
// request instead of splitting the path and calling Verify. It is how a
// token in a header (Authorization: Bearer), a cookie or the path itself,
// or another signing scheme (a ?signature= query parameter), reaches Signed
// without wrapping its ResponseWriter.
//
// The returned claims name what may be served: Claims.Disk and Claims.Key
// (the request's path is not read again), Claims.Method and
// Claims.ExpiresAt. Signed fails closed on top of the verifier: an error
// is the error's status (CodePermission for 403), and claims without a
// disk or key, for another method than the request's, without an expiry
// or already expired (by SignedOptions.Now) are 403, so a verifier that
// returns zero claims with a nil error grants nothing. AllowedIP,
// ContentType, ContentDisposition and Headers are enforced exactly as for
// a Verifier.
type RequestVerifier interface {
	VerifyRequest(r *http.Request, method filesystem.HTTPMethod) (signedurl.Claims, error)
}

// DefaultMaxUploadBytes caps a signed PUT: 5 GiB, S3's single-request limit,
// so a flow built against local disk behaves the same on S3.
const DefaultMaxUploadBytes int64 = 5 << 30

// SignedOptions configure Signed.
type SignedOptions struct {
	// MaxUploadBytes caps the body of a signed PUT. 0 means
	// DefaultMaxUploadBytes.
	MaxUploadBytes int64
	// CacheControl is sent on every signed download, in place of the
	// object's stored one. Empty means DefaultSignedCacheControl (stored
	// by no cache).
	CacheControl string
	// StoredCacheControl sends an object's own stored Cache-Control, when
	// it has one, instead of CacheControl. Off by default: a stored value
	// is usually chosen for public serving ("public, immutable"), and on
	// a private download it would let a shared cache keep the file past
	// the link's expiry. Turn it on only when every stored value is right
	// for signed links.
	StoredCacheControl bool
	// Sendfile, when set, leaves the body of a download to the front
	// proxy (see Sendfile). Nil streams every download.
	Sendfile *Sendfile
	// TrustedProxies are the address ranges of the proxies in front of
	// the handler. A URL restricted to an address range
	// (PresignOptions.AllowedIP) is checked against the request's
	// RemoteAddr; only when RemoteAddr is inside one of these ranges is
	// X-Forwarded-For read, from the right, skipping trusted hops, to find
	// the client. Empty, X-Forwarded-For is never read, because any client
	// can send one.
	TrustedProxies []netip.Prefix
	// InlinePDF serves a PDF that is not an attachment with InlinePDFCSP
	// instead of the sandbox CSP, so the browser's PDF viewer can show
	// it. Off by default; see PublicOptions.InlinePDF and InlinePDFCSP.
	InlinePDF bool
	// Errors renders every failure (405, 404, 403, the Disk's errors);
	// see ErrorWriter. nil answers the status text as text/plain.
	Errors ErrorWriter
	// Now is the clock claims from a RequestVerifier are checked against
	// (Claims.ExpiresAt); nil means time.Now. A Verifier (the
	// signedurl.Signer) checks expiry with its own clock.
	Now func() time.Time
}

// Signed returns a handler for signed URLs, to mount where the signer's base
// URL points, with the mount prefix stripped:
//
//	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(manager, signer)))
//
// GET and HEAD download (with ranges and conditional requests); PUT uploads
// when the URL was signed for PUT. A signed content type on a PUT must match
// the request's Content-Type. A URL restricted to an address range answers
// only requests from inside it (see SignedOptions.TrustedProxies). Every
// failure of signature, method, expiry or address is 403 with no detail
// (or what SignedOptions.Errors renders). A verifier implementing
// RequestVerifier sees the whole request instead of the path and query.
func Signed(disks Disks, verifier Verifier, opts ...SignedOptions) http.Handler {
	var o SignedOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.MaxUploadBytes <= 0 {
		o.MaxUploadBytes = DefaultMaxUploadBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var method filesystem.HTTPMethod
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			method = filesystem.MethodGet
		case http.MethodPut:
			method = filesystem.MethodPut
		default:
			w.Header().Set("Allow", "GET, HEAD, PUT")
			fail(o.Errors, w, r, http.StatusMethodNotAllowed, errMethod)
			return
		}
		claims, found, err := verify(verifier, r, method, o.Now)
		if !found {
			notFound(o.Errors, w, r)
			return
		}
		if err != nil {
			failErr(o.Errors, w, r, err)
			return
		}
		if claims.AllowedIP.IsValid() {
			client, ok := clientAddr(r, o.TrustedProxies)
			if !ok || !claims.AllowedIP.Contains(client) {
				fail(o.Errors, w, r, http.StatusForbidden, errMismatch)
				return
			}
		}
		d, err := disks.Disk(claims.Disk)
		if err != nil {
			failErr(o.Errors, w, r, err)
			return
		}
		if method == filesystem.MethodPut {
			upload(w, r, d, claims, o.MaxUploadBytes, o.Errors)
			return
		}
		e, err := d.Stat(r.Context(), claims.Key)
		if err != nil {
			failErr(o.Errors, w, r, err)
			return
		}
		serveObject(w, r, d, e, serving{
			contentType:  claims.ContentType,
			disposition:  claims.ContentDisposition,
			cacheControl: cacheControl(e.Headers.CacheControl, o.CacheControl, o.StoredCacheControl, DefaultSignedCacheControl),
			sendfile:     o.Sendfile,
			inlinePDF:    o.InlinePDF,
			errors:       o.Errors,
		})
	})
}

// errClaims refuses claims a RequestVerifier returned without an error but
// that grant nothing: no disk or key, another method, no or a past expiry.
var errClaims = filesystem.Errorf(filesystem.CodePermission, "fshttp: the verifier's claims do not grant this request")

// verify checks r with v: through VerifyRequest when v implements
// RequestVerifier, else by splitting "/{disk}/{key}" from the path and
// calling Verify. found is false only for a path Verify cannot be asked
// about (no disk or no key), which is answered 404 as before.
func verify(v Verifier, r *http.Request, method filesystem.HTTPMethod, now func() time.Time) (claims signedurl.Claims, found bool, err error) {
	rv, ok := v.(RequestVerifier)
	if !ok {
		diskName, key, ok := splitPath(r.URL.Path)
		if !ok {
			return signedurl.Claims{}, false, nil
		}
		claims, err := v.Verify(diskName, key, method, r.URL.Query())
		return claims, true, err
	}
	claims, err = rv.VerifyRequest(r, method)
	switch {
	case err != nil:
		return signedurl.Claims{}, true, err
	case claims.Disk == "" || claims.Key == "" || claims.Method != method,
		claims.ExpiresAt.IsZero() || !now().Before(claims.ExpiresAt):
		return signedurl.Claims{}, true, errClaims
	}
	return claims, true, nil
}

// upload stores a signed PUT body. The client's own Content-Type is used
// only when the URL fixed one; otherwise the Disk detects the type from the
// bytes, because a client-declared type cannot be trusted. When the URL
// pins stored headers (Claims.Headers), the request's Cache-Control,
// Content-Disposition and Content-Encoding must be exactly those, as a
// backend that signs them demands, and they are stored. A URL that pins
// none stores none, whatever the request sends, so a client cannot choose
// how its upload is served.
func upload(w http.ResponseWriter, r *http.Request, d *filesystem.Disk, c signedurl.Claims, limit int64, ew ErrorWriter) {
	secure(w)
	if c.ContentType != "" && r.Header.Get("Content-Type") != c.ContentType {
		fail(ew, w, r, http.StatusForbidden, errMismatch)
		return
	}
	if !c.Headers.IsZero() && filesystem.HTTPHeadersFrom(r.Header) != c.Headers {
		fail(ew, w, r, http.StatusForbidden, errMismatch)
		return
	}
	// The Disk's MaxSize is the one authoritative limit: it streams, stops
	// at limit+1 bytes and guarantees nothing larger is stored.
	e, err := d.Write(r.Context(), c.Key, r.Body, filesystem.WriteOptions{ContentType: c.ContentType, Headers: c.Headers, MaxSize: limit, SizeHint: max(r.ContentLength, 0)})
	if err != nil {
		failErr(ew, w, r, err)
		return
	}
	if e.ETag != "" {
		w.Header().Set("ETag", `"`+e.ETag+`"`)
	}
	w.WriteHeader(http.StatusOK)
}

// headerForwardedFor lists the addresses a request passed through, the
// client first, each proxy appending the address it received from.
const headerForwardedFor = "X-Forwarded-For"

// clientAddr returns the address of the client that made r. It is
// RemoteAddr unless that is a trusted proxy; then X-Forwarded-For is
// walked from the right, past every trusted hop, and the first untrusted
// address is the client, since only the hops after it were appended by
// proxies that can be believed. An address that does not parse, or a
// chain made only of trusted proxies, gives false, and the caller denies.
func clientAddr(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	addr := ap.Addr().Unmap()
	if !inAny(trusted, addr) {
		return addr, true
	}
	hops := strings.Split(strings.Join(r.Header.Values(headerForwardedFor), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{}, false
		}
		if hop = hop.Unmap(); !inAny(trusted, hop) {
			return hop, true
		}
	}
	return netip.Addr{}, false
}

// inAny reports whether addr is inside one of prefixes.
func inAny(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
