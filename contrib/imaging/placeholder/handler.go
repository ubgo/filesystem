package placeholder

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// Query parameters the handler reads.
const (
	ParamName  = "name"
	ParamSize  = "size"
	ParamStyle = "style"
)

// pngSuffix may end the seed segment, so URLs can look like files.
const pngSuffix = ".png"

// Cache-Control values. Everything a placeholder depends on is in its URL
// unless HandlerOptions.Name looks the name up, so by default it never
// changes and can be cached for a year; a looked-up name can change under
// the same URL, so it is cached for an hour and revalidated by ETag.
const (
	CacheControlImmutable = "public, max-age=31536000, immutable"
	CacheControlLookup    = "public, max-age=3600"
)

// HandlerOptions configures Handler.
type HandlerOptions struct {
	// Name, when set, returns the display name for a seed, and the name
	// query parameter is ignored: names stay out of URLs, and a client
	// cannot put arbitrary text on another user's fallback. An error is
	// answered with the status of its code (fshttp.StatusOf). Responses
	// then carry CacheControlLookup.
	Name func(ctx context.Context, seed string) (string, error)
	// Style is used when the request has no style parameter; empty means
	// StyleInitials.
	Style Style
	// CacheControl overrides the Cache-Control header; empty means
	// CacheControlImmutable, or CacheControlLookup with Name.
	CacheControl string
}

// Handler serves placeholders at GET and HEAD /{seed} or /{seed}.png, with
// the optional query parameters name, size (pixels, MinSize to MaxSize)
// and style (initials or identicon). Mount it with its prefix stripped:
//
//	mux.Handle("/avatars/fallback/", http.StripPrefix("/avatars/fallback/", placeholder.Handler(placeholder.HandlerOptions{})))
//
// Every response carries an ETag computed before drawing, so a matching
// If-None-Match is answered 304 without rendering. An invalid request is
// 400 with no body beyond the status text.
func Handler(opts HandlerOptions) http.Handler {
	cache := opts.CacheControl
	if cache == "" {
		cache = CacheControlImmutable
		if opts.Name != nil {
			cache = CacheControlLookup
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		req, err := parse(r, opts)
		if err != nil {
			status := fshttp.StatusOf(err)
			http.Error(w, http.StatusText(status), status)
			return
		}
		res, err := resolve(req)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		tag := res.etag()
		h := w.Header()
		h.Set("ETag", tag)
		h.Set("Cache-Control", cache)
		if matches(r.Header.Get("If-None-Match"), tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body, err := res.png()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		h.Set("Content-Type", "image/png")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}

// parse reads a Request from the path and query. The seed is the one path
// segment left after the mount prefix was stripped.
func parse(r *http.Request, opts HandlerOptions) (Request, error) {
	seed := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), pngSuffix)
	if seed == "" || strings.Contains(seed, "/") {
		return Request{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "placeholder: the path must be one seed")
	}
	q := r.URL.Query()
	req := Request{Seed: seed, Name: q.Get(ParamName), Style: Style(q.Get(ParamStyle))}
	if req.Style == "" {
		req.Style = opts.Style
	}
	if s := q.Get(ParamSize); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return Request{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "placeholder: size %q is not a number", s)
		}
		req.Size = n
	}
	if opts.Name != nil {
		name, err := opts.Name(r.Context(), seed)
		if err != nil {
			return Request{}, err
		}
		req.Name = name
	}
	return req, nil
}

// matches reports whether an If-None-Match header lists tag, or is "*".
// Weak tags match too: the comparison is weak, as RFC 9110 requires for
// If-None-Match.
func matches(header, tag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == tag {
			return true
		}
	}
	return false
}
