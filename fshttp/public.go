package fshttp

import (
	"net/http"
	"strings"

	"github.com/ubgo/filesystem"
)

// Public returns a handler serving the public objects of d by key, for
// mounting under the prefix a disk's public URL points to:
//
//	mux.Handle("/public/", http.StripPrefix("/public", fshttp.Public(disk)))
//
// An object is served only when its visibility is public, or, on a disk
// without per-object visibility, when the disk's default is public. Private
// and missing objects both answer 404, so the handler does not reveal that a
// private key exists. Only GET and HEAD are allowed. Cache-Control is the
// object's stored one (unless PublicOptions.ForceCacheControl), else
// PublicOptions.CacheControl, else DefaultPublicCacheControl (cacheable,
// revalidated on every use). Every failure is answered through
// PublicOptions.Errors (plain status text by default).
func Public(d *filesystem.Disk, opts ...PublicOptions) http.Handler {
	var o PublicOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(o.Errors, w, r, http.StatusMethodNotAllowed, errMethod)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		e, err := d.Stat(r.Context(), key)
		if err != nil {
			failErr(o.Errors, w, r, err)
			return
		}
		if !isPublic(d, e) {
			notFound(o.Errors, w, r)
			return
		}
		serveObject(w, r, d, e, serving{
			cacheControl: cacheControl(e.Headers.CacheControl, o.CacheControl, !o.ForceCacheControl, DefaultPublicCacheControl),
			sendfile:     o.Sendfile,
			inlinePDF:    o.InlinePDF,
			errors:       o.Errors,
		})
	})
}

func isPublic(d *filesystem.Disk, e filesystem.Entry) bool {
	if e.Visibility != "" {
		return e.Visibility == filesystem.VisibilityPublic
	}
	return !d.Capabilities().SetVisibility && d.DefaultVisibility() == filesystem.VisibilityPublic
}
