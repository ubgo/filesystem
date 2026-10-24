// Package fshttp serves disks over net/http: signed downloads and uploads
// (the URLs the signedurl package issues), and public files.
//
// It is what gives a local or memory disk the same URL behaviour as an
// object store: SignedURL on a local disk returns a link to this handler,
// and the handler checks the signature, then streams the object with range,
// ETag and conditional-request support.
//
// Every response carries "X-Content-Type-Options: nosniff" and a sandboxing
// Content-Security-Policy, because the files are user content served from
// the application's own origin: an uploaded HTML or SVG file must never run
// script there.
package fshttp

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
)

// Security headers set on every response.
const (
	HeaderContentTypeOptions = "X-Content-Type-Options"
	HeaderCSP                = "Content-Security-Policy"
	nosniff                  = "nosniff"
	// sandboxCSP forbids every fetch and runs any document in a sandbox
	// with a unique origin. It does not affect images or media embedded by
	// other pages, which is how served files are normally used.
	sandboxCSP = "default-src 'none'; sandbox"
	// InlinePDFCSP is the Content-Security-Policy sent instead of the
	// sandbox one for a PDF served inline when the handler's InlinePDF
	// option is on (PublicOptions.InlinePDF, SignedOptions.InlinePDF).
	//
	// Why: browsers show a PDF through a built-in viewer that the page
	// embeds as a plugin object. The sandbox directive turns plugins off
	// (the HTML sandboxed-plugins flag), and default-src 'none' implies
	// object-src 'none', which Chromium and WebKit apply to their PDF
	// viewers: either way the PDF shows as a blank grey page. So this
	// keeps default-src 'none' (no fetch, no script, no style of the
	// page's own) and allows only object-src 'self', the PDF itself.
	//
	// Invariant: it is never sent for any other type, nor for a PDF
	// served as an attachment; nosniff is still sent, so a file that is
	// not a PDF cannot be rendered as one. The cost of the opt-in is that
	// the PDF viewer's own script runs with the file's origin, which is
	// the application's when files are served from it: turn it on only
	// for PDFs from trusted users, or serve files from another origin.
	InlinePDFCSP = "default-src 'none'; object-src 'self'"
	// pdfType is the only content type InlinePDFCSP applies to.
	pdfType = "application/pdf"
)

// StatusOf maps an error to the HTTP status a handler should answer with.
// It is exported so application handlers built on Disk answer consistently.
func StatusOf(err error) int {
	switch filesystem.CodeOf(err) {
	case "":
		return http.StatusOK
	case filesystem.CodeNotFound:
		return http.StatusNotFound
	case filesystem.CodeExists, filesystem.CodeConflict:
		return http.StatusConflict
	case filesystem.CodeNotModified:
		return http.StatusNotModified
	case filesystem.CodeInvalidKey, filesystem.CodeInvalidArgument:
		return http.StatusBadRequest
	case filesystem.CodeUnsupported:
		return http.StatusNotImplemented
	case filesystem.CodeReadOnly, filesystem.CodePermission:
		return http.StatusForbidden
	case filesystem.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case filesystem.CodeRejected:
		return http.StatusUnsupportedMediaType
	case filesystem.CodeQuotaExceeded:
		return http.StatusInsufficientStorage
	case filesystem.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// HeaderRetryAfter is the response header SetRetryAfter sets.
const HeaderRetryAfter = "Retry-After"

// SetRetryAfter passes a backend's requested wait (filesystem.RetryAfterOf)
// on to an HTTP client as a Retry-After header in whole seconds, rounded
// up so the client never comes back early. It sets nothing when err carries
// no wait. Call it before WriteHeader; the handlers in this package and in
// upload do.
func SetRetryAfter(h http.Header, err error) {
	after, ok := filesystem.RetryAfterOf(err)
	if !ok {
		return
	}
	secs := (after + time.Second - 1) / time.Second
	h.Set(HeaderRetryAfter, strconv.FormatInt(int64(secs), 10))
}

// ParseRetryAfter reads a Retry-After header value as a backend sent it:
// delay-seconds ("120") or an HTTP date, measured from now. It returns false
// for an empty, malformed or negative value, and for a date that has
// passed, so a caller records no wait rather than a wrong one. Drivers use
// it to fill filesystem.Error.RetryAfter from a 429 or 503 answer.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs <= 0 || secs > maxRetryAfterSeconds {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil || !at.After(now) {
		return 0, false
	}
	return at.Sub(now), true
}

// maxRetryAfterSeconds bounds a delay-seconds value so the conversion to a
// time.Duration cannot overflow; a day is far beyond any retry budget.
const maxRetryAfterSeconds = 24 * 60 * 60

func secure(w http.ResponseWriter) {
	w.Header().Set(HeaderContentTypeOptions, nosniff)
	w.Header().Set(HeaderCSP, sandboxCSP)
}
