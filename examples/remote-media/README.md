# Remote media: link previews, an image proxy and a media cache

This program runs a "remote site" and an application on in-process servers and uses `github.com/ubgo/filesystem/contrib/remote` on a local disk: it builds a link preview of a page and caches its `og:image`, serves that image through the signed proxy, shows a forged proxy URL and an SVG posing as a PNG refused, then purges the page and sweeps the cache. The guide [Caching remote media](../../docs/plugins/remote.md) explains each step.

## Run it

```sh
cd examples/remote-media
go run .
go test .
```

It needs no environment variables and removes its temporary directory before exiting. The SSRF guard refuses loopback, so the demo allows `127.0.0.1` with `AllowAddr`; a real deployment leaves it unset.

## What it does

```text
preview   "Release notes" / "What shipped this week."
image     image/png 33 bytes, stored under the URL's hash: true
card      "Release notes", image through the proxy: true
signed    200 image/png 33 bytes, nosniff, public, max-age=599
forged    403
svg-png   415
sweep     kept 1 entries, 33 bytes; deleted 0
oembed    video "Launch clip" by "Example Studio" from "Tube", 640x360, thumbnail https://img.example/1.jpg
oembed    javascript: provider URL dropped: true, html is an iframe for a sandbox: true
off-list  discovery link followed: false, oEmbed false, preview stands: "A blog post"
endpoint  asked 1 time(s) in all
```

The `max-age` of the signed line counts down from the proxy's 10-minute link lifetime, so it can differ by a second.

## oEmbed from providers you allow

`oembed.go` runs a fake oEmbed provider in-process (on 127.0.0.1, which `AllowAddr` lets through as above; the SSRF guard itself is unchanged) and a cache built by [`newOEmbedCache`](ds:block?id=newoembedcache-s8w3k6pn) with one `remote.OEmbedProvider`: pages matching `<provider>/watch*` go to `<provider>/oembed`. [`oembedPreviews`](ds:block?id=oembedpreviews-f2q7v9hc) then previews two pages:

1. The watch page matches the scheme, so the endpoint is asked with `url` and `format=json`. `Preview.OEmbed` holds the spec's fields, typed: a video, its title, author, provider, size and thumbnail. The answer's `javascript:` provider URL is dropped and its unknown `secret_token` field never kept; `HTML` is the provider's iframe, to render only inside a sandboxed iframe.
2. A blog post on the same host carries a discovery link to `/evil/oembed`, which is not on the list. The cache never requests it, `OEmbed` stays nil, and the page's own preview stands. The allowlisted endpoint was asked once in all, for the watch page.

The test `TestOffListEndpointAnswers` shows the off-list endpoint does answer a direct request with the same JSON, so it is the allowlist alone that keeps the cache away.

## Files

- `main.go`: the link preview, the image proxy, the refusals, purge and sweep.
- `oembed.go`: oEmbed from an allowlisted provider, and a discovery link off the list refused.
- `main_test.go`: runs the program and checks every line above, and pins that the off-list endpoint is live.
