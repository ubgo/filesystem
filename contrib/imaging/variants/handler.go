package variants

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
)

// ScopeFunc returns the scope a request may read, typically the tenant of
// the signed-in user, or an error to refuse the request with 403. The
// handler never takes a scope from the URL.
type ScopeFunc func(r *http.Request) (string, error)

// DefaultLinkLifetime is how long the signed URL a request redirects to
// lives when HandlerOptions.LinkLifetime is 0.
const DefaultLinkLifetime = 10 * time.Minute

// DefaultCacheControl is the Cache-Control of every Handler response when
// HandlerOptions.CacheControl is "": private, and short, because a
// redirect points at a signed URL that expires, and the handler answers
// by the requester's scope, so a shared cache must not keep it.
const DefaultCacheControl = "private, max-age=60"

// Mode is how Handler hands a variant to the client.
type Mode string

// The closed set of modes.
const (
	// ModeRedirect answers 302 to the variant's URL (Store.URL: signed
	// for a private file, public for a public one). The default.
	ModeRedirect Mode = "redirect"
	// ModeProxy streams the variant's bytes itself: the client and a CDN in front see one URL, and the
	// bucket stays private and unreachable. It serves through
	// fshttp.ServeEntry, so Range requests (206), conditional requests
	// (If-None-Match, If-Modified-Since: 304) and ranged backend reads
	// work as they do on fshttp's handlers, and every response carries
	// X-Content-Type-Options: nosniff and fshttp's sandbox Content-
	// Security-Policy. The read is reported with Store.Served
	// (ServeVariant, the variant's size) before a byte is written, so
	// egress metering and an access log see it, and a hook that refuses
	// it refuses the request; a 304 or a range is reported at the
	// variant's full size too, an upper bound.
	ModeProxy Mode = "proxy"
)

// ModeValues lists every Mode.
var ModeValues = []Mode{ModeRedirect, ModeProxy}

// HandlerOptions configure Handler.
type HandlerOptions struct {
	// Mode is how a variant is handed out: ModeRedirect ("" too) or
	// ModeProxy. Another value makes every request fail with 500, so a
	// typo never silently serves the other way.
	Mode Mode
	// CacheControl is the Cache-Control header of every response;
	// "" is DefaultCacheControl. A public value ("public,
	// max-age=86400") lets a CDN keep proxied variants: use it only when
	// every file the handler serves is public, because the handler takes
	// the scope from the request and a shared cache keys by URL alone.
	// In ModeRedirect keep max-age below LinkLifetime, or a cached
	// redirect outlives the signed URL it points at.
	CacheControl string
	// LinkLifetime of the signed URLs it redirects to (public variants get
	// their public URL instead).
	LinkLifetime time.Duration
	// Negotiate lists output formats to serve instead of a variant's
	// declared one when the request's Accept header names them
	// explicitly, in order of preference: {FormatAVIF, FormatWebP} sends
	// AVIF to browsers that take it, WebP to the others that take that,
	// and the declared format to the rest. Each is a sibling of the
	// variant, generated on first request like it. Formats the plugin's
	// processor cannot be shown to write (by an Encodes method, as
	// *imaging.Pipeline has) are dropped here. With any format listed,
	// every response says Vary: Accept, so a cache keeps one per format.
	Negotiate []imaging.Format
	// Density serves a base variant declared with WithDensities in the
	// density version a request's device pixel ratio needs: the DPRParam
	// query parameter, else the HeaderSecCHDPR or HeaderDPR client hint,
	// rounded up to the next declared factor (the densest when it exceeds
	// them all). No ratio, or one of 1 or less, gets the base. Responses
	// then say Vary: Sec-CH-DPR, DPR, so a cache keeps one per ratio. A
	// browser sends the hint only after a page answers with
	// "Accept-CH: Sec-CH-DPR"; the query parameter needs nothing. A
	// density version requested by its own name is served as it is.
	Density bool
	// Errors renders every failure (405, 404 for a path naming no
	// variant, 403 when ScopeFunc refuses, the store's and disk's
	// errors) in the application's own envelope; see fshttp.ErrorWriter.
	// nil keeps the plain status text (http.Error, and http.NotFound's
	// body for a 404 path), the handler's historical answer.
	Errors fshttp.ErrorWriter
	// Sendfile, in ModeProxy, leaves the variant's body to the front
	// proxy (nginx X-Accel-Redirect, X-Sendfile) once the handler has
	// checked the request and set every header; see fshttp.Sendfile and
	// fshttp.ServeOptions.Sendfile. Nil streams the bytes. ModeRedirect
	// ignores it.
	Sendfile *fshttp.Sendfile
}

// Refusals the handler makes itself, passed to a HandlerOptions.Errors
// writer so it sees a real error naming what happened; never shown to
// clients by the default.
var (
	// errMethod is a method other than GET or HEAD (405).
	errMethod = filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: method not allowed")
	// errPath is a path that is not "<fileID>/<name>" (404).
	errPath = filesystem.Errorf(filesystem.CodeNotFound, "variants: no such variant path")
)

// fail answers err with status through ew. A nil ew is the historical
// answer: the status text as text/plain. A custom ew gets the Retry-After
// the backend asked for already set, as fshttp.ErrorWriter promises.
func fail(ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, status int, err error) {
	if ew == nil {
		http.Error(w, http.StatusText(status), status)
		return
	}
	fshttp.SetRetryAfter(w.Header(), err)
	ew(w, r, status, err)
}

// failErr answers a storage error with fshttp.StatusOf(err) through ew.
func failErr(ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, err error) {
	fail(ew, w, r, fshttp.StatusOf(err), err)
}

// proxy streams the variant itself (ModeProxy) through fshttp's serve
// path, with ranges and conditional requests.
func proxy(w http.ResponseWriter, r *http.Request, s *records.Store, p *Plugin, scope, fileID, name string, format imaging.Format, opts HandlerOptions) {
	ctx := context.WithoutCancel(r.Context())
	f, v, err := p.servable(ctx, s, scope, fileID, name, format)
	if err != nil {
		failErr(opts.Errors, w, r, err)
		return
	}
	disk, err := s.Disks().Disk(v.Disk)
	if err != nil {
		failErr(opts.Errors, w, r, err)
		return
	}
	// Reported before a byte is written: nothing is served that metering
	// or an access log did not see.
	if err := s.Served(ctx, records.Served{Kind: records.ServeVariant, Scope: f.Scope, FileID: f.ID, Subject: v.Name, Bytes: v.Size}); err != nil {
		failErr(opts.Errors, w, r, err)
		return
	}
	// The Stat gives the ETag, modification time and size the ranges and
	// validators are answered from.
	e, err := disk.Stat(r.Context(), v.Key)
	if err != nil {
		failErr(opts.Errors, w, r, err)
		return
	}
	fshttp.ServeEntry(w, r, disk, e, fshttp.ServeOptions{ContentType: v.ContentType, CacheControl: opts.CacheControl, Sendfile: opts.Sendfile})
}

// Handler serves GET /{fileID}/{name}: it generates the variant on first
// request and redirects to it (or, with ModeProxy, streams it). Mount it with the prefix stripped:
//
//	mux.Handle("/variants/", http.StripPrefix("/variants/", variants.Handler(store, plugin, scopeOf, variants.HandlerOptions{})))
//
// Only declared variant names exist; any other is 404, so clients cannot
// request arbitrary sizes.
func Handler(s *records.Store, p *Plugin, scope ScopeFunc, opts HandlerOptions) http.Handler {
	if opts.LinkLifetime <= 0 {
		opts.LinkLifetime = DefaultLinkLifetime
	}
	if opts.Mode == "" {
		opts.Mode = ModeRedirect
	}
	if opts.CacheControl == "" {
		opts.CacheControl = DefaultCacheControl
	}
	formats := p.negotiable(opts.Negotiate)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(opts.Errors, w, r, http.StatusMethodNotAllowed, errMethod)
			return
		}
		fileID, name, ok := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
		if !ok || fileID == "" || name == "" || strings.Contains(name, "/") {
			if opts.Errors == nil {
				http.NotFound(w, r)
				return
			}
			fail(opts.Errors, w, r, http.StatusNotFound, errPath)
			return
		}
		if len(opts.Negotiate) > 0 {
			w.Header().Add("Vary", "Accept")
		}
		if opts.Density {
			w.Header().Add("Vary", HeaderSecCHDPR+", "+HeaderDPR)
			name = p.densityFor(name, requestDPR(r))
		}
		sc, err := scope(r)
		if err != nil {
			fail(opts.Errors, w, r, http.StatusForbidden, err)
			return
		}
		// Generation continues if the client disconnects mid-way: the work
		// is cached for the next request rather than wasted.
		format := p.negotiate(r.Header.Get("Accept"), formats)
		switch opts.Mode {
		case ModeRedirect:
			// The link is reported as served by Store.URL, which issues
			// none it could not report.
			link, err := p.urlIn(context.WithoutCancel(r.Context()), s, sc, fileID, name, format, opts.LinkLifetime)
			if err != nil {
				failErr(opts.Errors, w, r, err)
				return
			}
			// Redirects to signed URLs must not be cached past their
			// lifetime; the variant itself is cached by the URL it
			// redirects to.
			w.Header().Set("Cache-Control", opts.CacheControl)
			http.Redirect(w, r, link, http.StatusFound)
		case ModeProxy:
			proxy(w, r, s, p, sc, fileID, name, format, opts)
		default:
			failErr(opts.Errors, w, r, filesystem.Errorf(filesystem.CodeUnknown, "variants: unknown handler mode %q", opts.Mode))
		}
	})
}
