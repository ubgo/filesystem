# contrib/cloudflare

`github.com/ubgo/filesystem/contrib/cloudflare` signs URLs for Cloudflare token authentication: a WAF custom rule that blocks a request unless `is_timed_hmac_valid_v0` accepts the token in its last query parameter. It needs only the standard library; it is a module of its own, like every vendor integration, so the core never names a vendor.

```go
signer, err := cloudflare.New(cloudflare.Config{BaseURL: "https://downloads.example.com/files", Secret: secret, TTL: 3 * time.Hour})
disk, err := filesystem.NewDisk("downloads", drv, filesystem.WithURLSigner(signer))
```

`drv` can be the S3 driver for R2 behind the zone, or a GCS or Azure driver: a signer given with `WithURLSigner` signs every download, even on a disk whose driver presigns, while upload (PUT) links stay with the driver's own presigning and go straight to the bucket. See [which path signs which method](../../docs/concepts/serving-files.md#which-path-signs-which-method).

with the rule (8 is `len("?verify=")`, 10800 the TTL in seconds):

```text
(http.host eq "downloads.example.com" and not is_timed_hmac_valid_v0("<secret>", http.request.uri, 10800, http.request.timestamp.sec, 8))
```

| Name | What it is |
|---|---|
| `New(Config)` | checks the config: `BaseURL` an absolute https URL without a query, `Secret` at least `MinSecretBytes` (32), `TTL` whole seconds in [1s, `filesystem.MaxPresignExpiry`], `Param` URL-safe; anything else is `CodeInvalidArgument` |
| `Config` | `BaseURL`, `Secret` (the rule's key, copied), `TTL` (the rule's ttl), `Param` (empty: `DefaultParam`, `verify`), `URLSafeMAC` (for a rule using the `s` flag), `Clock` (tests). It prints, in every `fmt` verb and in `slog`, with the secret `[REDACTED]` |
| `Signer.SignURL` | implements `filesystem.URLSigner`: `<base>/<escaped key>?verify=<timestamp>-<mac>`, where the MAC is HMAC-SHA256(secret, path + timestamp), standard base64, URL-encoded (Cloudflare's documented algorithm). The token is issued as if at now + `Expires` - `TTL`, so the rule expires it exactly `Expires` from now; `Expires` beyond `TTL` is `CodeInvalidArgument`. A PUT, or any `ContentType` or `ContentDisposition` (Cloudflare cannot apply one, and the Disk sets an attachment for active types), is `CodeUnsupported` |

Cloudflare matches the path as the request carries it: with [URL normalization](https://developers.cloudflare.com/rules/normalization/) on, keys with characters it rewrites may not verify, so keep such keys out of a token-protected zone or check them with a real request. Tests port `is_timed_hmac_valid_v0` from its documentation and check that it accepts each link until its expiry, and rejects it a second later, on another path or under another secret; the token is compared byte for byte with Cloudflare's own Python example. The guide is [Serving files](../../docs/concepts/serving-files.md#step-11--signed-cdn-links).
