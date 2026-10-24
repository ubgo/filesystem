# contrib/remote

`github.com/ubgo/filesystem/contrib/remote` (package `remote`) caches media that lives on other servers: link previews with their `og:image` stored, an HMAC-signed image proxy that is never an open proxy, and a federated media cache with purge and an LRU/size sweep. Every request, redirect and `og:image` goes through the upload package's SSRF guard; every stored object passes an `upload.Policy` decided from its bytes; keys are the SHA-256 of the canonical URL. It imports only the core and `golang.org/x/net/html` (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/remote
```

The guide, with output from a run: [caching remote media](../../docs/plugins/remote.md). The cache of [`examples/remote-media`](../../examples/remote-media/main.go):

<!-- ds:block id=newcache-p8tqkkv4 -->
**newCache** · [`examples/remote-media/main.go:53-60`](../../examples/remote-media/main.go#L53-L60)

```go
func newCache(disk *filesystem.Disk, allow func(netip.Addr) bool) (*remote.Cache, error) {
	return remote.New(disk, remote.Options{
		Dir:        "remote",
		Policy:     upload.Policy{MaxSize: 5 << 20, Allowed: []string{"image/*"}},
		Fetch:      upload.FetchOptions{Timeout: 10 * time.Second, AllowAddr: allow},
		PreviewTTL: 24 * time.Hour,
	})
}
```
<!-- /ds:block hash=d26a4b -->

## API

| Name | What it is |
|---|---|
| `New(disk, Options) (*Cache, error)` | a cache on `disk` under `Options.Dir` (required); refuses an empty `Dir`, an invalid policy, `AllowActiveContent`, `AllowActive` and `Settle` |
| `Options{Dir, Policy, Fetch, TouchInterval, PreviewTTL, MaxConcurrentFetches, MaxPageBytes, MaxTags, Now, CacheKey, Extract, KeepMeta, OEmbed, MaxOEmbedBytes}` | where it lives, what may be stored, the guarded client, LRU touching, preview freshness, the cap on fetches running at once, page bounds, the clock |
| `Options.CacheKey func(canonical string) string` | maps a canonical URL to the form its cache key is made from (drop `utm_*`, `fbclid`), so such URLs share one entry; it never changes what is fetched, and an empty result keys by the canonical URL |
| `Options.Extract func(ctx, pageURL, meta map[string]string) (map[string]string, error)`, `Preview.Extra` | adds fields to a link preview from the parsed `og:*`, `twitter:*` and description tags (`og:video`, `twitter:player`); it cannot change the built-in fields, and its error fails the preview |
| `Options.KeepMeta func(name string) bool`, `MaxJSONLD` | adds more of the head to the map `Extract` gets: other `<meta>` names (lower-cased: `author`, `article:author`, `product:price:amount`), `link:<rel>` for each rel of a `<link>` (its href unresolved: `link:icon`, `link:canonical`), and `jsonld:<n>` for the head's JSON-LD blocks (at most 8, text unparsed). Bounded like the rest (`MaxTags`, the first value per name, 2048 bytes per value, 64 KiB per JSON-LD block); a page cannot spoof those names or `<title>` with a `<meta>`. Nil (the default) adds nothing |
| `Options.OEmbed []OEmbedProvider`, `OEmbedProvider{Schemes, Endpoint}`, `Options.MaxOEmbedBytes`, `Preview.OEmbed *OEmbed`, `Preview.OEmbedError` | opt-in oEmbed from an allowlist the application gives: a page URL matching a provider's `Schemes` (oEmbed `*` wildcards, the host matched apart from the path) is sent to its `Endpoint` with `url` and `format=json`; a page's `<link rel="alternate" type="application/json+oembed">` is followed only when it names an allowlisted endpoint. Every request goes through the SSRF guard, redirects included; the response is read up to `MaxOEmbedBytes` (64 KiB, `CodeTooLarge` beyond) and only the spec's fields are kept, typed: `OEmbed{Type, Title, AuthorName, AuthorURL, ProviderName, ProviderURL, URL, HTML, Width, Height, ThumbnailURL, ThumbnailWidth, ThumbnailHeight}`, URLs http(s) only, `HTML` at most 16 KiB and to be rendered only in a sandboxed iframe. A failed oEmbed sets `OEmbedError` and leaves the preview. Nil asks no endpoint |
| `Cache.Get(ctx, url) (Entry, error)`, `Cache.Open(ctx, url)` | the cached media of a URL, fetched and stored on a miss (once for concurrent misses) |
| `Cache.LinkPreview(ctx, url) (Preview, error)` | a page's title, description, site name and image, the image cached; stored until `PreviewTTL`. `ImageURL` is always http(s); the text fields are the page's, to be escaped where rendered |
| `Preview{URL, Title, Description, SiteName, ImageURL, ImageKey, ImageType, ImageSize, ImageError, FetchedAt, Extra, OEmbed, OEmbedError}` | a link preview |
| `Cache.Purge(ctx, url)` | removes a URL's media and preview |
| `Cache.Sweep(ctx, SweepOptions{MaxBytes, MaxAge}) (SweepResult, error)` | evicts entries older than `MaxAge`, then the least recently used until under `MaxBytes` |
| `Cache.MediaKey(url)` | the disk key of a URL's media |
| `NewProxy(cache, ProxyOptions{Base, Signer, MaxAge}) (*Proxy, error)`, `Proxy.URL(ctx, url, lifetime)` | the image proxy (`http.Handler`) and its signed URLs |
| `Canonical(url)` | the form the cache keys and signs |
| `DefaultTouchInterval`, `DefaultMaxPageBytes`, `DefaultMaxTags`, `DefaultMaxConcurrentFetches`, `DefaultProxyMaxAge`, `ParamURL` | defaults and the proxy's URL parameter |

## Limits

- LRU needs `Capabilities.SetModTime` (local, memory); elsewhere eviction is by fetch time.
- The proxy serves whole objects; it does not answer Range requests.
