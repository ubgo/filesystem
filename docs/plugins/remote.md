# Caching remote media

You will have link previews with their image stored on your own disk, third-party images shown through your server rather than hot-linked, and the media of federated posts cached with a purge and a size bound. The package is `contrib/remote`: one `Cache` on a Disk you give it, `LinkPreview` for pages, `Proxy` for signed image URLs, and `Purge` and `Sweep` to keep it bounded.

## Before you start

- Why this needs care: every one of these features makes your server fetch a URL a user chose, which is server-side request forgery (SSRF) unless every request is checked. `contrib/remote` sends every request, every redirect and every `og:image` it finds through the upload package's guard (`upload.FetchOptions`), which refuses loopback, private, link-local (cloud metadata at `169.254.169.254`), CGNAT, reserved and NAT64 addresses after DNS resolution, so a name that rebinds to an internal address is refused too.
- `go get github.com/ubgo/filesystem/contrib/remote`. It is its own module and imports only the core and `golang.org/x/net/html` (pure Go; `deps_test.go` enforces it).
- The example is [`examples/remote-media`](../../examples/remote-media/main.go), its own module: `go run .` from that directory runs a page, an image and a hostile SVG on in-process servers.

## Step 1 — Make the cache

`remote.New(disk, opts)` keeps everything under `opts.Dir` on the disk. `opts.Policy` is an ordinary `upload.Policy`: the type of every stored object is detected from its bytes and must match `Allowed`, so an SVG or HTML file served as `image/png` is refused (`CodeRejected`) and never stored. `AllowActiveContent`, `AllowActive` and `Settle` are refused at construction, since the first two would let SVG or HTML through and the second would move objects away from their keys. `Process` and `Inspect` run as on any upload, so EXIF stripping or a virus scan can be added the usual way.

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

Objects are keyed by the SHA-256 of the canonical URL (`remote.Canonical`: scheme and host lower-cased, default port and fragment dropped), never by the URL itself, so nothing a remote server chose ends up in a key. `Cache.MediaKey(url)` returns the key, for building a public or signed disk URL to a cached image.

| Option | What it does | Default |
|---|---|---|
| `Dir` | where the cache lives on the disk (`Dir/media`, `Dir/preview`); required, and the cache's alone, since `Purge` and `Sweep` delete under it | none: an empty `Dir` is `CodeInvalidArgument`, an unclean one `CodeInvalidKey` |
| `Policy` | what may be stored; `MaxSize` and `Allowed` required | none |
| `Fetch` | the SSRF-guarded client: `Timeout` (which bounds each fetch, body included, so a page that drips its body is cut off), `MaxRedirects`, `AllowAddr` for a known internal server | the guard's defaults (one minute) |
| `TouchInterval` | how old an entry's time must be before a hit refreshes it, for LRU eviction | `DefaultTouchInterval` (1 hour); negative never touches |
| `PreviewTTL` | how long a stored link preview stays fresh | 0: until `Purge` |
| `MaxConcurrentFetches` | how many fetches run at once; a new fetch beyond it is `CodeUnavailable`, never queued, while a request for a URL already being fetched joins it | `DefaultMaxConcurrentFetches` (64) |
| `MaxPageBytes`, `MaxTags` | how much of a page `LinkPreview` reads | 1 MiB, 2000 tags |
| `Now` | the clock | `time.Now` |
| `OEmbed`, `MaxOEmbedBytes` | the oEmbed providers `LinkPreview` may ask (see below), and how much of an answer it reads | nil: no oEmbed; 64 KiB |

## Step 2 — Link previews

`cache.LinkPreview(ctx, url)` returns a `Preview`: the title, description and site name from Open Graph tags (falling back to Twitter card tags, `<title>` and `<meta name="description">`), and the image, resolved against the page's final URL after redirects, fetched through the guard and stored in the media cache (`ImageKey`, `ImageType`, `ImageSize`). The page is read through a bounded reader: at most `MaxPageBytes`, at most `MaxTags` tags, at most 64 KiB per token, and parsing stops at `</head>` or `<body>`. A page that does not sniff as HTML is `CodeRejected`. `ImageURL` is only ever an absolute http or https URL in canonical form: an image named `javascript:…`, `data:…` or anything else is left out and `ImageError` says `CodeInvalidArgument`. An image that cannot be cached leaves the preview usable and records why in `ImageError` (a `filesystem.Code`). `Title`, `Description` and `SiteName` are the page's own text, unescaped: escape them where you render them, as any user input. The preview is stored as JSON and served from the disk until `PreviewTTL` passes.

Concurrent requests for one URL, preview or media, share one fetch. It runs detached from every caller's cancellation, bounded by `Fetch.Timeout` plus 30 seconds to store the result, and each caller stops waiting when its own context ends, so one caller cancelling never fails the others. When the last caller has gone the fetch is cancelled, which is safe at any point because the Disk's writes are atomic.

Put together with the proxy of step 3, this is the whole "someone posted a link" path. The card carries the page's own text and an image URL the proxy signed, so the visitor's browser never contacts the remote site:

<!-- ds:block id=linkcard-t5m2w8kq -->
**linkCard** · [`examples/remote-media/main.go:89-103`](../../examples/remote-media/main.go#L89-L103)

```go
func linkCard(ctx context.Context, cache *remote.Cache, proxy *remote.Proxy, postURL string) (card, error) {
	p, err := cache.LinkPreview(ctx, postURL)
	if err != nil {
		return card{}, err
	}
	c := card{Title: p.Title, Description: p.Description}
	if p.ImageURL == "" || p.ImageError != "" {
		return c, nil
	}
	c.ImageSrc, err = proxy.URL(ctx, p.ImageURL, proxyLifetime)
	if err != nil {
		return card{}, err
	}
	return c, nil
}
```
<!-- /ds:block hash=d0d1bc -->

Check: the example prints `card      "Release notes", image through the proxy: true`.

### Tracking parameters and extra preview fields

Two options change what the cache keys and what a preview holds:

```go
cache, err := remote.New(disk, remote.Options{
	Dir: "remote", Policy: policy,
	// URLs that differ only in tracking parameters share one entry.
	CacheKey: func(canonical string) string {
		u, _ := url.Parse(canonical)
		q := u.Query()
		for k := range q {
			if strings.HasPrefix(k, "utm_") || k == "fbclid" {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
		return u.String()
	},
	// Keep the page's video and player in Preview.Extra.
	Extract: func(ctx context.Context, pageURL string, meta map[string]string) (map[string]string, error) {
		return map[string]string{"video": meta["og:video"], "player": meta["twitter:player"]}, nil
	},
})
```

`CacheKey` never changes what is fetched, only where it is kept, so a later URL is served an earlier fetch; an empty result keys by the canonical URL. `Extract` gets the parsed `og:*`, `twitter:*` and description tags and cannot change the built-in fields; its error fails the preview. Its values are the page's own text: escape them where you render them.

An author, a price, the favicon or a schema.org Product live elsewhere in the head. `KeepMeta` names what else `Extract` gets: other `<meta>` names (lower-cased), `link:<rel>` for each rel of a `<link>`, and `jsonld:<n>` for each JSON-LD block in the head:

```go
KeepMeta: func(name string) bool {
	switch name {
	case "author", "article:author", "product:price:amount", "link:icon", "link:canonical", "jsonld:0":
		return true
	}
	return false
},
Extract: func(ctx context.Context, pageURL string, meta map[string]string) (map[string]string, error) {
	return map[string]string{"author": meta["author"], "price": meta["product:price:amount"], "icon": meta["link:icon"], "product": meta["jsonld:0"]}, nil
},
```

The bounds stay: at most `MaxTags` tags read, the first value of each name, 2048 bytes per value, at most 8 JSON-LD blocks of 64 KiB, the head only. A link's href is as the page wrote it, so resolve it against `pageURL` before fetching it (through the cache, which keeps the SSRF guard). A page cannot spoof `link:`, `jsonld:` or the `<title>` with a `<meta>` tag. Nil `KeepMeta`, the default, adds nothing.

### oEmbed from providers you allow

`Options.OEmbed` adds what an oEmbed provider says about a page (`Preview.OEmbed`: type, title, author, provider, thumbnail, embed size and HTML). Only the providers you list are ever asked:

```go
remote.Options{
	// ...
	OEmbed: []remote.OEmbedProvider{
		{Schemes: []string{"https://www.youtube.com/watch*", "https://youtu.be/*"}, Endpoint: "https://www.youtube.com/oembed"},
		{Schemes: []string{"https://vimeo.com/*"}, Endpoint: "https://vimeo.com/api/oembed.{format}"},
		// Discovery only: followed when a page's link names this endpoint.
		{Endpoint: "https://publish.twitter.com/oembed"},
	},
}
```

A page URL matching a provider's scheme (`*` matches any run; the host is matched apart from the path, so `https://*.vimeo.com/*` never matches `https://evil.example/.vimeo.com/`) is sent to its endpoint with `url` and `format=json`. Otherwise a page's `<link rel="alternate" type="application/json+oembed">` is followed only when it names an endpoint on the list, its query aside; a page cannot send the cache anywhere else. Every request goes through the SSRF guard, redirects included, at most `MaxOEmbedBytes` (64 KiB) is read, and only the spec's fields are kept, typed: URLs must be http(s), `HTML` is dropped when over 16 KiB. `HTML` is third-party markup: render it only inside a sandboxed iframe, never into your page. A failed or refused answer sets `Preview.OEmbedError` and the preview stands. Without `OEmbed` no endpoint is asked.

Try it: [`examples/remote-media`](../../examples/remote-media) runs this section in `oembed.go` ([`newOEmbedCache`](ds:block?id=newoembedcache-s8w3k6pn), [`oembedPreviews`](ds:block?id=oembedpreviews-f2q7v9hc)): `go run .` from `examples/remote-media`, then look for `off-list  discovery link followed: false, oEmbed false, preview stands: "A blog post"`.

## Step 3 — The image proxy

`remote.NewProxy(cache, opts)` returns an `http.Handler` that serves remote media from the cache. It is never an open proxy: it answers only URLs that `proxy.URL(ctx, url, lifetime)` signed (HMAC-SHA256 through `signedurl.Signer`, with key rotation), and a missing, forged, altered or expired signature is `403` before anything is fetched. Give the proxy its own signing keys.

<!-- ds:block id=newproxy-uq85q2ww -->
**newProxy** · [`examples/remote-media/main.go:64-74`](../../examples/remote-media/main.go#L64-L74)

```go
func newProxy(cache *remote.Cache, base string) (*remote.Proxy, error) {
	secret := make([]byte, signedurl.MinKeyBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	signer, err := signedurl.New(base, []signedurl.Key{{ID: "proxy-1", Secret: secret}})
	if err != nil {
		return nil, err
	}
	return remote.NewProxy(cache, remote.ProxyOptions{Base: base, Signer: signer})
}
```
<!-- /ds:block hash=182993 -->

Every response re-sniffs the bytes it is about to send and refuses (`415`) a type the policy does not accept, even for an entry something else wrote to the disk. It sets `Content-Type` from that sniff, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; sandbox`, an `ETag` (answering `If-None-Match` with `304`) and `Cache-Control: public, max-age=…` of `MaxAge` (one day by default) but never past the URL's expiry. Fetch errors answer with their status (`fshttp.StatusOf`) and a message that never names the address. Ranges are not served.

## Step 4 — Purge and sweep

`cache.Purge(ctx, url)` removes the media and the preview of a URL (a deleted federated post, a takedown). `cache.Sweep(ctx, remote.SweepOptions{MaxBytes, MaxAge})` evicts by modification time: first everything older than `MaxAge`, then the oldest until the rest fits in `MaxBytes`. It lists only the cache's own directories. On a disk with `Capabilities.SetModTime` (local, memory) a hit refreshes an entry's time at most once per `TouchInterval`, so eviction is least recently used; on a disk without it (S3, GCS, Azure) eviction is least recently fetched. Schedule it with the jobs package:

```go
jobs.Schedule{
	Job: jobs.Func("remote.sweep", func(ctx context.Context, _ time.Time) (jobs.Result, error) {
		res, err := cache.Sweep(ctx, remote.SweepOptions{MaxBytes: 50 << 30, MaxAge: 30 * 24 * time.Hour})
		return jobs.Result{Count: int64(res.Deleted)}, err
	}),
	Every: time.Hour,
}
```

## Run it

```text
preview   "Release notes" / "What shipped this week."
image     image/png 33 bytes, stored under the URL's hash: true
card      "Release notes", image through the proxy: true
signed    200 image/png 33 bytes, nosniff, public, max-age=599
forged    403
svg-png   415
sweep     kept 1 entries, 33 bytes; deleted 0
```

The forged line is a proxy URL naming the cloud metadata address without a signature; the svg-png line is an SVG with a script served as `image/png`, refused from its bytes.

## Limits

- The cache keeps no index: `Sweep` lists the cache's directories, so run it on a schedule rather than per request.
- Concurrent misses for one URL are fetched once per `Cache` value, not across processes; two instances may each fetch it once.
- A failed touch on a hit is ignored: it only makes the entry look older to `Sweep`.
