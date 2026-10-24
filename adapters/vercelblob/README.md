# adapters/vercelblob

`github.com/ubgo/filesystem/adapters/vercelblob` is the `filesystem.Driver` for Vercel Blob, over its REST API on `net/http`, with no dependency beyond the library (Vercel ships only a JavaScript SDK). It is its own module.

The guide is [docs/adapters/vercelblob.md](../../docs/adapters/vercelblob.md).

```sh
go get github.com/ubgo/filesystem/adapters/vercelblob
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Token` | `string` | required | The store's read-write token (`BLOB_READ_WRITE_TOKEN`) |
| `Access` | `Access` | required | `AccessPublic` or `AccessPrivate`, matching the store |
| `Client` | `*http.Client` | a client with `DefaultTimeout` (10 min) | Transport, proxy, TLS; the driver's copy follows no redirect |
| `APIURL`, `BlobURL` | `string` | `DefaultAPIURL`; `https://<store>.<access>.blob.vercel-storage.com` from the token | Endpoints, for a proxy or a test server; https, or http on loopback only |
| `CacheMaxAge` | `time.Duration` | 0 (Vercel's default, one month) | Sent as `x-cache-control-max-age` on every write and copy: how long Vercel's CDN and browsers may cache the blob. Whole seconds, at least `MinCacheMaxAge` (one minute) |

`New(cfg)` makes no request; a `CacheMaxAge` under a minute or with a fraction of a second is `CodeInvalidArgument`.

## DSN

`vercelblob://<access>?<parameters>`, the host being the store's access mode (`public` or `private`), registered with `mux.Register(vercelblob.Scheme, vercelblob.Opener)`. The token is read from an environment variable, never the DSN. Credentials in the URL, a path, an empty token variable, another access mode and an unknown parameter are `CodeInvalidArgument`.

| Constant | Parameter | Meaning |
|---|---|---|
| `ParamTokenEnv` | `token_env` | environment variable holding the read-write token; default `DefaultTokenEnv` (`BLOB_READ_WRITE_TOKEN`) |
| `ParamCacheMaxAge` | `cache_max_age` | `Config.CacheMaxAge` in seconds | `Config` prints and logs with the token redacted, and the driver keeps it inside a closure.

## Capabilities

`Copy` (server-side `fromUrl`), `DeleteKeys` (`DeleteBatch` = 100 per request), `CompareAndSwap` (`x-if-match`), `ContentType`, and `PublicURL` for a public store. Never: `IfNotExists` (Vercel refuses an existing pathname without a documented status), presigning, `Move`, visibility, metadata, headers, multipart.

## Behaviour

- A key is the blob's pathname. Writes are one streamed `PUT` with no random suffix and overwriting allowed, then a head for size and time; a failing body aborts the request and its own error is returned.
- Reads `GET` the blob's URL with `Range`; private blobs with the token and `cache=0`. Public blobs are served through Vercel's CDN, so a read after an overwrite or delete can return the old bytes for up to a minute; `Stat` and `List` go to the API and are current.
- `List` is the API's prefix listing, following its cursor; folder placeholders (pathnames ending in `/`) are skipped.
- The token goes only to the API and, for a private store, to the store's blob host.
- Errors by the API's code: `not_found` `CodeNotFound`, `precondition_failed` `CodeConflict`, `file_too_large` `CodeTooLarge`, `content_type_not_allowed` `CodeRejected`, `forbidden`/`store_suspended` `CodePermission`, `rate_limited`/`service_unavailable`/5xx `CodeUnavailable`.
- `Native()` returns the `*http.Client` (with the token transport).

## Tests

`go test ./...` runs the conformance suite for a private and a public store against an in-process fake of the API and blob host (the public host records any request carrying the token), plus batching, body failure, redirects, redaction and error mapping. Not verified without a real store: the exact URL Vercel gives a pathname holding characters that need escaping, and CDN staleness.
