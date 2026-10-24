package fshttp

import (
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/ubgo/filesystem"
)

// Response headers a front proxy reads to send a file itself.
const (
	// HeaderAccelRedirect is nginx's: the value is a URI in an internal
	// location, e.g. "/protected/avatars/a.png".
	HeaderAccelRedirect = "X-Accel-Redirect"
	// HeaderSendfile is Apache mod_xsendfile's and lighttpd's: the value
	// is an absolute file path.
	HeaderSendfile = "X-Sendfile"
)

// Conditional-request headers the offload path answers itself.
const (
	headerIfNoneMatch     = "If-None-Match"
	headerIfModifiedSince = "If-Modified-Since"
	headerLastModified    = "Last-Modified"
	// anyETag is the If-None-Match value matching every current object.
	anyETag = "*"
	// weakPrefix marks a weak entity tag; If-None-Match compares weakly.
	weakPrefix = "W/"
)

// Sendfile hands the body of a download to the front proxy (nginx, Apache,
// lighttpd) instead of streaming it through Go: the handler still checks
// the request (signature, visibility, method), still sets every response
// header (Content-Type, Content-Disposition, Cache-Control, ETag,
// Last-Modified, X-Content-Type-Options, Content-Security-Policy, and the
// attachment for active types), answers If-None-Match and
// If-Modified-Since itself with 304 from the object's own ETag and
// modification time, and otherwise answers with Header set to the value
// Path returns and no body. It suits a local disk whose files the proxy
// can read.
//
// Path is the only link between a key and a file: it receives the disk's
// name and the object's driver-level key (Disk.DriverKey: cleaned, with
// every scope prefix applied, so the path relative to the backend's root
// directory), never the request path, so a request cannot choose what the
// proxy sends and a scoped disk cannot name another scope's file. ok false
// (a disk the proxy cannot reach, say) streams the object as usual, and so
// does a value that is empty or holds a control character. A Path that
// builds a URI should escape the key with filesystem.EscapeKey.
//
// A proxy may drop headers on an internal redirect: nginx keeps
// Content-Type, Content-Disposition and Cache-Control from the handler
// but not X-Content-Type-Options or Content-Security-Policy, so its
// internal location must add them (add_header ... always). Range requests
// are left to the proxy.
type Sendfile struct {
	// Header is the response header the proxy reads; empty means
	// HeaderAccelRedirect.
	Header string
	// Path maps a disk name and a driver-level key to Header's value.
	Path func(disk, key string) (value string, ok bool)
}

// target returns the value of Header for e when s maps it. It reports
// false (no Sendfile, no mapping, a bad value) when the caller must stream
// the object instead. It is decided before any header is set, so a
// streamed body can be opened first (see serveObject).
func (s *Sendfile) target(d *filesystem.Disk, e filesystem.Entry) (string, bool) {
	if s == nil || s.Path == nil {
		return "", false
	}
	key, err := d.DriverKey(e.Key)
	if err != nil {
		return "", false
	}
	v, ok := s.Path(d.Name(), key)
	if !ok || v == "" || strings.ContainsFunc(v, unicode.IsControl) {
		return "", false
	}
	return v, true
}

// serve answers r for e with the value target returned, after serveObject
// has set the headers: 304 when the request's validators match, else
// Header set to v with no body.
func (s *Sendfile) serve(w http.ResponseWriter, r *http.Request, e filesystem.Entry, v string) {
	if !e.ModTime.IsZero() {
		w.Header().Set(headerLastModified, e.ModTime.UTC().Format(http.TimeFormat))
	}
	if notModified(r, w.Header().Get("ETag"), e.ModTime) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header := s.Header
	if header == "" {
		header = HeaderAccelRedirect
	}
	w.Header().Set(header, v)
}

// notModified evaluates If-None-Match (weak comparison against etag) and,
// only without it, If-Modified-Since (against modTime at second
// precision), as RFC 9110 orders them for a GET or HEAD.
func notModified(r *http.Request, etag string, modTime time.Time) bool {
	if inm := r.Header.Get(headerIfNoneMatch); inm != "" {
		if etag == "" {
			return false
		}
		for tag := range strings.SplitSeq(inm, ",") {
			tag = strings.TrimSpace(tag)
			if tag == anyETag || strings.TrimPrefix(tag, weakPrefix) == strings.TrimPrefix(etag, weakPrefix) {
				return true
			}
		}
		return false
	}
	ims := r.Header.Get(headerIfModifiedSince)
	if ims == "" || modTime.IsZero() {
		return false
	}
	since, err := http.ParseTime(ims)
	return err == nil && !modTime.Truncate(time.Second).After(since)
}
