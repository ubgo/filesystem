# adapters/cloudinary

`github.com/ubgo/filesystem/adapters/cloudinary` is the `filesystem.Driver` for Cloudinary, storing each key as a raw resource (exact bytes, no transformation) through the Upload and Admin REST APIs on `net/http`, with no dependency beyond the library; `ResourceType` opts into image or video resources instead. It is its own module.

The guide is [docs/adapters/cloudinary.md](../../docs/adapters/cloudinary.md).

```sh
go get github.com/ubgo/filesystem/adapters/cloudinary
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `CloudName`, `APIKey`, `APISecret` | `string` | required | The account's credentials |
| `Type` | `Type` | `TypeAuthenticated` | Delivery type: `TypeAuthenticated` (signed reads only), `TypeUpload` (public delivery URLs) or `TypePrivate` (originals signed, derived versions public) |
| `ResourceType` | `ResourceType` | `ResourceRaw` | `ResourceRaw` keeps exact bytes. `ResourceImage` and `ResourceVideo` let Cloudinary transform what the disk stores, but a read (and `PublicURL`) names the key's extension as the delivery format, so the bytes are the uploaded ones only when that extension matches the uploaded format. `auto` is not offered: every later call must name the type the resource became |
| `Clock` | `func() time.Time` | `time.Now` | Request timestamps and a read's download expiry; Cloudinary refuses timestamps more than an hour off |
| `Algorithm` | `Algorithm` | `SHA1` | The account's signature algorithm, `SHA1` or `SHA256` |
| `Client` | `*http.Client` | a client with `DefaultTimeout` (10 min) | Transport, proxy, TLS |
| `APIURL`, `DeliveryURL` | `string` | `DefaultAPIURL`, `DefaultDeliveryURL` | Endpoints, for a proxy or a test server; https, or http on loopback only |

`New(cfg)` makes no request; an unknown `Type` or `ResourceType` is `CodeInvalidArgument`. `Config` prints and logs with the secret redacted, and the driver keeps it inside closures.

## DSN

`cloudinary://<api_key>@<cloud_name>?api_secret_env=<VAR>`, registered with `mux.Register(cloudinary.Scheme, cloudinary.Opener)`: Cloudinary's own `CLOUDINARY_URL` shape without the secret, which is read from the named variable. A secret in the URL, a missing or empty secret variable, a path and an unknown parameter are `CodeInvalidArgument`.

| Constant | Parameter | Meaning |
|---|---|---|
| `ParamSecretEnv` | `api_secret_env` | environment variable holding the API secret; required |
| `ParamType` | `type` | `Config.Type` |
| `ParamResourceType` | `resource_type` | `Config.ResourceType` |
| `ParamAlgorithm` | `algorithm` | `Config.Algorithm` |

## Capabilities

`Move` (signed `rename`, overwriting), `DeleteKeys` (Admin API, `DeleteBatch` = 100 per request), `DeletePrefix` (Admin API by prefix, repeated while partial), and `PublicURL` for `TypeUpload`. Never: `Copy`, presigning (signed delivery URLs do not expire without Cloudinary's paid token feature), conditional writes, content type, visibility, metadata, headers, multipart.

## Behaviour

- A key is a public ID; every byte outside `[A-Za-z0-9._-]` (and `/` between segments) is written `~XX`, since Cloudinary refuses `? & # \ % < > +`. IDs the driver did not write are skipped in listings.
- Writes are one signed upload streaming the body as a multipart form, with `overwrite` and `invalidate`; a body above the plan's raw-upload limit (10 MB free, 100 MB paid) is `CodeTooLarge`, since chunked uploads are not implemented.
- Reads use the signed download endpoint with a `DownloadExpiry` (5 min) expiry, fresh after an overwrite and valid for both types, with a `Range` header.
- `Stat` and `List` use the Admin API, which Cloudinary rate-limits per hour (500 on the free plan); 420 and 429 are `CodeUnavailable`.
- Transport errors never carry a request's query, where the API key, signature and expiry of a working signed read are.
- `Native()` returns the `*http.Client`.

## Tests

`go test ./...` runs the conformance suite for both delivery types against an in-process fake that checks every signature with the secret and pages listings and prefix deletes, plus escaping, public URLs, SHA-256, ranges, body failure, batch and prefix deletes, redaction and error mapping. Not verified without a real account: the exact set of characters Cloudinary refuses in a public ID, and whether the download endpoint honours `Range` (the driver cuts a full answer to the range either way).
