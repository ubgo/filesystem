// Package remote caches media that lives on other servers: the image of a
// link preview (LinkPreview), third-party images served through the
// application (Proxy), and the media of federated posts (Cache.Get,
// Purge, Sweep).
//
// Every outbound request, redirects included, goes through the upload
// package's SSRF guard (upload.FetchOptions), so a URL a user chose never
// reaches a loopback, private, link-local or metadata address. Every stored
// object goes through an upload.Policy decided from its magic bytes, so an
// SVG or HTML file announced as an image is refused, never cached. Objects
// are keyed by the SHA-256 of the canonical URL, never by the URL itself.
// The cache lives on a Disk the application gives; the package keeps no
// state of its own beyond in-flight requests.
package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

const (
	// DefaultTouchInterval is how stale an entry's modification time may
	// get before a cache hit refreshes it (Options.TouchInterval): Sweep
	// evicts by that time, so a hit must move it, but not on every hit.
	DefaultTouchInterval = time.Hour
	// DefaultMaxPageBytes is how much of a page LinkPreview reads: Open
	// Graph tags live in <head>, near the top.
	DefaultMaxPageBytes int64 = 1 << 20
	// DefaultMaxTags is how many tags LinkPreview reads before giving up.
	DefaultMaxTags = 2000
	// DefaultMaxConcurrentFetches caps the fetches a Cache runs at once
	// (Options.MaxConcurrentFetches).
	DefaultMaxConcurrentFetches = 64

	// mediaDir and previewDir split the cache directory, so Sweep and
	// Purge touch only what the cache wrote.
	mediaDir   = "media"
	previewDir = "preview"
	previewExt = ".json"
	// sharedWriteGrace is how long a shared fetch may spend storing its
	// result after the fetch's own timeout (see Cache.sharedTimeout).
	sharedWriteGrace = 30 * time.Second
	// shardLen is how many hex digits of the hash name a sub-directory,
	// so no directory grows to millions of entries.
	shardLen = 2
)

// Options configures a Cache.
type Options struct {
	// Dir is the directory of the disk the cache keeps its objects in
	// (under Dir/media and Dir/preview). Required, and the cache's alone:
	// Purge and Sweep delete under it, so it must hold nothing else.
	Dir string
	// Policy decides what remote media may be stored: MaxSize and Allowed
	// are required (Allowed: []string{"image/*"} for an image cache), and
	// the type is detected from the bytes. Process and Inspect run as on
	// any upload (strip EXIF, scan). The cache sets Key and Collision
	// itself; AllowActiveContent, AllowActive and Settle are refused, since
	// the first two would let SVG or HTML through and the second would move objects
	// away from their cache keys.
	Policy upload.Policy
	// Fetch configures the SSRF-guarded client every request uses
	// (timeout, redirects, AllowAddr for a known internal server). MaxSize
	// is set from Policy for media and left unset for pages.
	Fetch upload.FetchOptions
	// TouchInterval is how old an entry's modification time must be
	// before a hit refreshes it, so Sweep evicts the least recently used
	// entries. 0 means DefaultTouchInterval, a negative value never
	// touches. Touching needs Capabilities.SetModTime; on a disk without
	// it Sweep evicts the least recently fetched entries instead.
	TouchInterval time.Duration
	// PreviewTTL is how long a stored link preview stays fresh before
	// LinkPreview fetches the page again. 0 keeps it until Purge.
	PreviewTTL time.Duration
	// MaxPageBytes caps how much of a page LinkPreview reads; 0 means
	// DefaultMaxPageBytes. Bytes past it are never read.
	MaxPageBytes int64
	// MaxTags caps how many tags LinkPreview parses; 0 means
	// DefaultMaxTags.
	MaxTags int
	// MaxConcurrentFetches caps how many fetches (media and pages) run at
	// once, so callers who ask for many URLs and leave cannot hold open
	// connections without limit. A fetch beyond it fails with
	// CodeUnavailable; a request for a URL already being fetched joins
	// that fetch and needs no slot. 0 means DefaultMaxConcurrentFetches.
	MaxConcurrentFetches int
	// Now replaces time.Now, for tests and for runners with their own
	// clock.
	Now func() time.Time
	// CacheKey, when set, maps a URL's canonical form (Canonical) to the
	// form its cache key is made from, so URLs that differ only in what
	// does not change the content share one entry: dropping utm_* or
	// fbclid parameters, a CDN's size or signature parameters. It never
	// changes what is fetched, only where it is kept, and so which
	// earlier fetch a later URL is served. It must be deterministic; an
	// empty result keys by the canonical URL. Nil keys by the canonical
	// URL.
	CacheKey func(canonical string) string
	// Extract, when set, adds fields to every link preview: it gets the
	// page's final URL and the meta tags the preview parsed (og:*,
	// twitter:* and description, each at most 2048 bytes of the page's
	// own text, at most MaxTags of them), and returns fields stored in
	// Preview.Extra: og:video, twitter:player, an author or price. It
	// cannot change the built-in fields. An error fails the preview, and
	// nothing is stored. Its values are the page's text: escape them
	// where they are rendered.
	Extract func(ctx context.Context, pageURL string, meta map[string]string) (map[string]string, error)
	// KeepMeta, when set, adds more of the page's head to the map Extract
	// gets, besides the og:*, twitter:* and description tags it always
	// holds: every name it returns true for, among
	//   - other <meta> names, lower-cased ("author", "article:author",
	//     "product:price:amount", "citation_doi");
	//   - "link:<rel>" for each rel of a <link> tag, its href as the page
	//     wrote it, unresolved ("link:icon", "link:canonical");
	//   - "jsonld:<n>" for the n-th <script type="application/ld+json">
	//     in the head (from 0, at most MaxJSONLD), its text unparsed
	//     (a schema.org Product or Recipe).
	// The same bounds hold: MaxTags tags read, the first value of each
	// name kept, 2048 bytes for a meta or link value and maxTokenBytes for
	// a JSON-LD block. Only the head is read, so a tag in the body is not
	// seen. Nil adds nothing, the default. It does not change the
	// built-in fields, and it matters only with Extract.
	KeepMeta func(name string) bool
	// OEmbed, when set, adds the oEmbed data of every link preview
	// (Preview.OEmbed) from these providers and no others: a page URL
	// matching a provider's Schemes is sent to its Endpoint, and a page's
	// <link rel="alternate" type="application/json+oembed"> is followed
	// only when it names an Endpoint here. Every request goes through the
	// SSRF guard with Fetch, redirects included; the response is read up
	// to MaxOEmbedBytes and only the spec's fields are kept, typed. A
	// failed oEmbed is recorded in Preview.OEmbedError and the preview
	// stands. Nil, the default, asks no oEmbed endpoint at all.
	OEmbed []OEmbedProvider
	// MaxOEmbedBytes caps an oEmbed response (CodeTooLarge beyond); 0
	// means DefaultMaxOEmbedBytes.
	MaxOEmbedBytes int64
}

// Cache is a cache of remote media on a Disk. It is safe for concurrent
// use; concurrent misses for one URL fetch it once.
type Cache struct {
	disk     *filesystem.Disk
	opts     Options
	media    flight[filesystem.Entry]
	previews flight[Preview]
}

// New returns a Cache keeping its objects on disk under opts.Dir. It
// fails with CodeInvalidArgument for an empty Dir (CodeInvalidKey for an
// unclean one), negative limits, an invalid policy, a policy allowing active
// content or settling uploads, or a negative page limit.
func New(disk *filesystem.Disk, opts Options) (*Cache, error) {
	if disk == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: no disk")
	}
	if err := opts.Policy.Validate(); err != nil {
		return nil, err
	}
	if opts.Policy.AllowActiveContent || len(opts.Policy.AllowActive) > 0 || opts.Policy.Settle != nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: a cache policy may not allow active content (AllowActiveContent, AllowActive) or settle uploads")
	}
	if opts.MaxPageBytes < 0 || opts.MaxTags < 0 || opts.MaxConcurrentFetches < 0 || opts.MaxOEmbedBytes < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: negative limits")
	}
	if opts.MaxConcurrentFetches == 0 {
		opts.MaxConcurrentFetches = DefaultMaxConcurrentFetches
	}
	if opts.TouchInterval == 0 {
		opts.TouchInterval = DefaultTouchInterval
	}
	if opts.MaxPageBytes == 0 {
		opts.MaxPageBytes = DefaultMaxPageBytes
	}
	if opts.MaxTags == 0 {
		opts.MaxTags = DefaultMaxTags
	}
	if opts.MaxOEmbedBytes == 0 {
		opts.MaxOEmbedBytes = DefaultMaxOEmbedBytes
	}
	if err := checkProviders(opts.OEmbed); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: Options.Dir must name a directory the cache alone uses")
	}
	dir, err := disk.CleanKey(opts.Dir)
	if err != nil {
		return nil, err
	}
	opts.Dir = dir
	c := &Cache{disk: disk, opts: opts}
	sem := make(chan struct{}, opts.MaxConcurrentFetches)
	c.media.sem, c.previews.sem = sem, sem
	return c, nil
}

// defaultPorts are the ports Canonical drops, per scheme.
var defaultPorts = map[string]string{"http": "80", "https": "443"}

// Canonical returns the form of rawURL the cache keys and signs: scheme
// and host lower-cased, the scheme's default port and the fragment
// dropped, an empty path made "/". The query is kept as it is, since
// servers may read its order. Only absolute http and https URLs without
// credentials are accepted (CodeInvalidArgument otherwise).
func Canonical(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: invalid URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	port, ok := defaultPorts[u.Scheme]
	if !ok || u.Hostname() == "" {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: only absolute http and https URLs are cached")
	}
	if u.User != nil {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: URLs with credentials are refused")
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && p != port {
		host = net.JoinHostPort(host, p)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host, u.Fragment, u.RawFragment = host, "", ""
	if u.Path == "" {
		u.Path, u.RawPath = "/", ""
	}
	return u.String(), nil
}

// key is where the object of kind for canonical URL canon lives: the URL's
// SHA-256 (after Options.CacheKey), never the URL, so no part of a remote
// address reaches a key.
func (c *Cache) key(kind, canon string) string {
	if c.opts.CacheKey != nil {
		if k := c.opts.CacheKey(canon); k != "" {
			canon = k
		}
	}
	sum := sha256.Sum256([]byte(canon))
	h := hex.EncodeToString(sum[:])
	return filesystem.JoinKey(c.opts.Dir, kind, h[:shardLen], h)
}

// MediaKey returns the disk key under which the media of rawURL is
// cached, whether or not it is there yet; for building a public or signed
// disk URL to it.
func (c *Cache) MediaKey(rawURL string) (string, error) {
	canon, err := Canonical(rawURL)
	if err != nil {
		return "", err
	}
	return c.key(mediaDir, canon), nil
}

// Get returns the cached entry of rawURL, fetching and storing it first
// on a miss. Errors are those of upload.FromURL: CodeInvalidArgument for a
// refused URL or address, CodeRejected for a type the policy refuses,
// CodeTooLarge, CodeNotFound, CodeUnavailable.
func (c *Cache) Get(ctx context.Context, rawURL string) (filesystem.Entry, error) {
	canon, err := Canonical(rawURL)
	if err != nil {
		return filesystem.Entry{}, err
	}
	key := c.key(mediaDir, canon)
	e, err := c.disk.Stat(ctx, key)
	switch {
	case err == nil:
		c.touch(ctx, e)
		return c.typed(ctx, e)
	case filesystem.CodeOf(err) != filesystem.CodeNotFound:
		return filesystem.Entry{}, err
	}
	return c.fill(ctx, key, canon)
}

// fill fetches canon into key, once for any number of concurrent callers.
func (c *Cache) fill(ctx context.Context, key, canon string) (filesystem.Entry, error) {
	return c.media.do(ctx, key, c.sharedTimeout(), func(ctx context.Context) (filesystem.Entry, error) {
		p := c.opts.Policy
		p.Key = func(upload.Info) (string, error) { return key, nil }
		p.Collision = upload.CollisionOverwrite
		r, err := upload.FromURL(ctx, c.disk, canon, p, c.opts.Fetch)
		if err != nil {
			return filesystem.Entry{}, err
		}
		e, err := c.disk.Stat(ctx, r.Key)
		if e.ContentType == "" {
			e.ContentType = r.ContentType
		}
		return e, err
	})
}

// sharedTimeout bounds a fetch shared by concurrent callers, which no
// caller's context bounds: the fetch's own timeout plus sharedWriteGrace
// for storing what it read.
func (c *Cache) sharedTimeout() time.Duration {
	t := c.opts.Fetch.Timeout
	if t <= 0 {
		t = upload.DefaultFetchTimeout
	}
	return t + sharedWriteGrace
}

// typed fills in the type of an entry whose disk stores none (the local
// driver), from its first bytes, as the policy judged them when stored.
func (c *Cache) typed(ctx context.Context, e filesystem.Entry) (filesystem.Entry, error) {
	if e.ContentType != "" {
		return e, nil
	}
	rc, err := c.disk.ReadRange(ctx, e.Key, filesystem.ByteRange{Length: sniff.HeadSize})
	if err != nil {
		return filesystem.Entry{}, err
	}
	defer func() { _ = rc.Close() }()
	head, err := io.ReadAll(rc)
	if err != nil {
		return filesystem.Entry{}, err
	}
	e.ContentType = sniff.Detect(head, "")
	return e, nil
}

// Open returns the cached media of rawURL to read, fetching it on a miss
// (see Get). The caller closes the reader.
func (c *Cache) Open(ctx context.Context, rawURL string) (io.ReadCloser, filesystem.Entry, error) {
	e, err := c.Get(ctx, rawURL)
	if err != nil {
		return nil, filesystem.Entry{}, err
	}
	rc, err := c.disk.Read(ctx, e.Key)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		// Swept or purged between Stat and Read: fetch it again.
		canon, _ := Canonical(rawURL) // Get accepted it
		if e, err = c.fill(ctx, e.Key, canon); err == nil {
			rc, err = c.disk.Read(ctx, e.Key)
		}
	}
	if err != nil {
		return nil, filesystem.Entry{}, err
	}
	return rc, e, nil
}

// touch moves a hit entry's modification time to now when it is older
// than TouchInterval, so Sweep sees it as recently used. It is best
// effort: a failed touch only makes the entry look older to Sweep, which
// is no reason to fail the read.
func (c *Cache) touch(ctx context.Context, e filesystem.Entry) {
	now := c.opts.Now()
	if c.opts.TouchInterval < 0 || !c.disk.Capabilities().SetModTime || now.Sub(e.ModTime) < c.opts.TouchInterval {
		return
	}
	_ = c.disk.SetModTime(ctx, e.Key, now)
}

// Purge removes everything cached for rawURL: its media and its link
// preview. Purging what is not cached succeeds.
func (c *Cache) Purge(ctx context.Context, rawURL string) error {
	canon, err := Canonical(rawURL)
	if err != nil {
		return err
	}
	return c.disk.DeleteKeys(ctx, []string{c.key(mediaDir, canon), c.key(previewDir, canon) + previewExt})
}

// flight runs one call per key at a time and hands its result to every
// caller that asked for the key meanwhile. sem, shared by every flight of
// a Cache, caps how many calls run at once.
type flight[T any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[T]
	sem   chan struct{}
}

type flightCall[T any] struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int // callers still waiting; guarded by flight.mu
	value   T
	err     error
}

// errBusy refuses a new fetch while MaxConcurrentFetches are running.
func errBusy() error {
	return filesystem.Errorf(filesystem.CodeUnavailable, "remote: too many fetches in flight")
}

// do runs fn for key unless a call for key is running, and waits for the
// call's result or for ctx, whichever comes first. The call runs in its
// own goroutine on a context detached from every caller (keeping their
// values, losing their cancellation) and bounded by timeout, so a caller
// that gives up never fails the fetch for the others waiting on it. When
// the last waiter gives up the call is cancelled: every write the call
// makes is atomic on the Disk, so cancelling at any point stores nothing
// partial, and the next request starts afresh. A new call needs a slot of
// sem; with none free it fails with CodeUnavailable rather than queueing.
func (f *flight[T]) do(ctx context.Context, key string, timeout time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	f.mu.Lock()
	call, ok := f.calls[key]
	if !ok {
		select {
		case f.sem <- struct{}{}:
		default:
			f.mu.Unlock()
			return zero, errBusy()
		}
		if f.calls == nil {
			f.calls = map[string]*flightCall[T]{}
		}
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		call = &flightCall[T]{done: make(chan struct{}), cancel: cancel}
		f.calls[key] = call
		go func() {
			defer func() { <-f.sem }()
			defer cancel()
			call.value, call.err = fn(shared)
			f.mu.Lock()
			f.forget(key, call)
			f.mu.Unlock()
			close(call.done)
		}()
	}
	call.waiters++
	f.mu.Unlock()
	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		f.mu.Lock()
		if call.waiters--; call.waiters == 0 {
			f.forget(key, call)
			call.cancel()
		}
		f.mu.Unlock()
		return zero, ctx.Err()
	}
}

// forget removes call from the running calls unless a newer call for key
// replaced it. f.mu must be held.
func (f *flight[T]) forget(key string, call *flightCall[T]) {
	if f.calls[key] == call {
		delete(f.calls, key)
	}
}
