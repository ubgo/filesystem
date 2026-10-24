# contrib/cloudfront

`github.com/ubgo/filesystem/contrib/cloudfront` signs Amazon CloudFront URLs and cookies for a disk served through a distribution with restricted viewer access (a trusted key group). It is a module of its own because it needs `github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign`, which the core does not import.

```go
signer, err := cloudfront.New(cloudfront.Config{BaseURL: "https://cdn.example.com/media", KeyPairID: keyPairID, PrivateKey: key})
disk, err := filesystem.NewDisk("media", drv, filesystem.WithURLSigner(signer))
link, err := disk.SignedURL(ctx, "videos/42/poster.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 10 * time.Minute})
```

`drv` is typically the S3 driver of the distribution's origin bucket, and it works the same on a GCS or Azure disk: a signer given with `WithURLSigner` signs every download, even on a disk whose driver presigns, while upload (PUT) links stay with the driver's own presigning and go straight to the bucket. See [which path signs which method](../../docs/concepts/serving-files.md#which-path-signs-which-method).

| Name | What it is |
|---|---|
| `New(Config)` | checks the config: `BaseURL` an absolute https URL without a query, `KeyPairID` URL-safe, `PrivateKey` set, `HashAlgorithm` empty, `sign.HashSHA1` or `sign.HashSHA256`; anything else is `CodeInvalidArgument` |
| `Config` | `BaseURL`, `KeyPairID`, `PrivateKey` (a `crypto.Signer`, e.g. the `*rsa.PrivateKey` from `sign.LoadPEMPrivKey`), `HashAlgorithm`, `ResponseOverrides`, `CookieDomain`, `Clock` (tests). It prints, in every `fmt` verb and in `slog`, with the key `[REDACTED]` |
| `Signer.SignURL` | implements `filesystem.URLSigner`: a canned-policy URL for GET and HEAD of exactly the key, expiring after `PresignOptions.Expires` (required, at most `filesystem.MaxPresignExpiry`). The disk name is not part of the URL. A PUT is `CodeUnsupported` |
| `Config.ResponseOverrides` | off: a link that asks for a `ContentType` or `ContentDisposition`, including the attachment the Disk forces on HTML, SVG, XML and JavaScript, is `CodeUnsupported`, because CloudFront serves what the origin stored. On: they travel as S3's `response-content-type` and `response-content-disposition` parameters under the signature; turn it on only when the cache policy forwards both to an S3 origin reached through origin access control |
| `Signer.SignCookies(ctx, prefix, expires)` | the `CloudFront-Policy`, `CloudFront-Signature` and `CloudFront-Key-Pair-Id` cookies (plus `CloudFront-Hash-Algorithm` with SHA-256) for every key under `prefix` (cleaned like a Disk directory; `""` is the whole base URL; `*` and `?` in it are escaped, so it cannot widen the wildcard), with the expiry. Secure, HttpOnly, SameSite=Lax, `Path=/`, expiring with the policy; set each with `http.SetCookie`. The wildcard also matches any query string, so the cookies serve every object under the prefix as stored: an HTML or SVG file stored without `Content-Disposition: attachment` renders inline, so store active files with that header or keep them out of cookie-signed prefixes. With `ResponseOverrides` it is `CodeUnsupported`, since the holder could add the override parameters to any URL under the wildcard |
| `Config.CookiePath`, `Config.CookiePathFor`, `Config.CookieSameSite` | the cookies' `Path` (default `/`; must start with `/`, no `;`, `,` or control character); `CookiePathFor(prefix)` picks it per `SignCookies` call so grants on two prefixes coexist (the cookie names are fixed, so two grants on one `Path` replace each other), nil or `""` keeping `CookiePath`, its answer checked the same way and an error issuing no cookie; and `SameSite` (default Lax; `http.SameSiteNoneMode` for a CDN on another site, Strict, while the browser default is refused). The cookies stay `Secure` and `HttpOnly` |
| `ParamResponseContentType`, `ParamResponseContentDisposition` | the S3 override parameter names |

Tests sign with a generated RSA key and verify every URL and cookie signature with the public key against CloudFront's documented policy format. A real distribution is needed only to see CloudFront itself accept the links. The guide is [Serving files](../../docs/concepts/serving-files.md#step-11--signed-cdn-links).
