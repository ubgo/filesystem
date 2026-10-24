package fshttp

import (
	"net/http"

	"github.com/ubgo/filesystem"
)

// ServeOptions configure ServeEntry. The zero value serves the object as
// stored.
type ServeOptions struct {
	// ContentType overrides the stored Content-Type; "" keeps it
	// (application/octet-stream when none is stored).
	ContentType string
	// ContentDisposition overrides the stored Content-Disposition; ""
	// keeps it. Active content is sent as an attachment whatever it says.
	ContentDisposition string
	// CacheControl replaces the object's stored Cache-Control; "" keeps
	// the stored one.
	CacheControl string
	// Sendfile, when set, leaves the body to the front proxy (nginx
	// X-Accel-Redirect, X-Sendfile) once every header is set, exactly as
	// PublicOptions.Sendfile and SignedOptions.Sendfile do, so a handler
	// that authorised the request itself (a variant proxy) can still hand
	// the bytes off. Nil streams the body.
	Sendfile *Sendfile
	// InlinePDF serves a PDF that is not an attachment with InlinePDFCSP
	// instead of the sandbox CSP, as PublicOptions.InlinePDF does. Off by
	// default; no other type is affected, and nosniff stays.
	InlinePDF bool
	// Errors renders the refusal of an entry whose key the disk refuses,
	// and a read that fails before the status is sent (see ServeEntry);
	// nil is PlainErrors.
	Errors ErrorWriter
}

// ServeEntry streams the object e of d for a GET or HEAD through the same
// path Public and Signed serve with, for an application handler that has
// already authorised the request and found the object (a variant proxy, a
// download behind the application's own access check).
//
// Why it is exported: a handler that streams an object with io.Copy loses
// what this path gives, Range requests (206 and 416), conditional requests
// (If-None-Match against e.ETag, If-Modified-Since against e.ModTime, 304)
// and ranged backend reads that fetch only the bytes asked for.
//
// Invariant: it keeps every guarantee of the package's handlers: every
// response carries X-Content-Type-Options: nosniff and the sandbox
// Content-Security-Policy, and active content (HTML, SVG, XML, script) is
// sent as an attachment, whatever opts say; nothing in ServeOptions can
// loosen that. Call it before anything is written, with an Entry from
// d.Stat (its Size, ETag and ModTime drive the ranges and validators).
// The method is the caller's to check: anything but GET and HEAD is served
// as a GET. An entry whose key the disk refuses (a traversal, a key not
// from d.Stat) is answered with StatusOf the refusal (400) through
// opts.Errors before any header is set. On a plain GET (no Range, no
// conditional header) the read is opened before the status too, so an
// object deleted since Stat, a driver failure or a throttled backend is
// answered with StatusOf the error (404, 503 with Retry-After) through
// opts.Errors rather than as an empty 200; a ranged, HEAD or conditional
// request reads lazily, after the status, as http.ServeContent does.
func ServeEntry(w http.ResponseWriter, r *http.Request, d *filesystem.Disk, e filesystem.Entry, opts ServeOptions) {
	if _, err := d.CleanKey(e.Key); err != nil {
		failErr(opts.Errors, w, r, err)
		return
	}
	cc := opts.CacheControl
	if cc == "" {
		cc = e.Headers.CacheControl
	}
	serveObject(w, r, d, e, serving{contentType: opts.ContentType, disposition: opts.ContentDisposition, cacheControl: cc, sendfile: opts.Sendfile, inlinePDF: opts.InlinePDF, errors: opts.Errors})
}
