package fshttp

// Cache-Control defaults. Public sends an object's stored Cache-Control
// when it has one (the application's choice for serving it publicly) and
// its default otherwise; Signed sends its own value unless told to use the
// stored one (SignedOptions.StoredCacheControl), because a value stored for
// public serving would let a shared cache keep a private download. Both
// defaults are safe for any file: neither lets a shared cache keep a copy
// a client could get without asking the server.
const (
	// DefaultPublicCacheControl is sent by Public for an object that
	// stored no Cache-Control of its own: caches may keep the object but
	// must revalidate it (by ETag or Last-Modified) on every use, so a
	// file made private or deleted stops being served at once. Set
	// PublicOptions.CacheControl to a long max-age only for files that
	// never change visibility, or whose keys change with their content.
	DefaultPublicCacheControl = "public, no-cache"
	// DefaultSignedCacheControl is sent by Signed: a signed URL is a
	// short-lived capability for one person, and its response must not be
	// stored by any cache (a CDN would otherwise serve it, or a shared
	// browser keep it, past the URL's expiry).
	DefaultSignedCacheControl = "private, no-store"
)

// PublicOptions configure Public.
type PublicOptions struct {
	// CacheControl is sent for objects that stored no Cache-Control of
	// their own. Empty means DefaultPublicCacheControl.
	CacheControl string
	// ForceCacheControl sends CacheControl (or the default) even for
	// objects that stored their own.
	ForceCacheControl bool
	// Sendfile, when set, leaves the body of a download to the front
	// proxy (see Sendfile). Nil streams every download.
	Sendfile *Sendfile
	// InlinePDF lets a PDF that is not an attachment be shown in the
	// browser's PDF viewer: it is served with InlinePDFCSP instead of the
	// sandbox CSP, which makes viewers show a blank page. Off by default,
	// because the viewer's script then runs with the file's origin; see
	// InlinePDFCSP. No other type is affected, and nosniff stays.
	InlinePDF bool
	// Errors renders every failure (405, 404 for a missing or private
	// object, the Disk's errors); see ErrorWriter. nil answers the status
	// text as text/plain, and a private object http.NotFound's body.
	Errors ErrorWriter
}

// cacheControl is the Cache-Control a handler sends for an object: the
// stored value when useStored, else the configured one, else def.
func cacheControl(stored, configured string, useStored bool, def string) string {
	switch {
	case stored != "" && useStored:
		return stored
	case configured != "":
		return configured
	}
	return def
}
