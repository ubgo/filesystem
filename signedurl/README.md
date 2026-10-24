# signedurl

`github.com/ubgo/filesystem/signedurl` signs and verifies URLs for disks whose backend cannot sign its own (local disk, memory, SFTP), so signed downloads and direct uploads work the same in development as on S3. `fshttp.Signed` serves the URLs it issues.

The concepts and a runnable walk-through are in [Serving files](../docs/concepts/serving-files.md).

A link signed with the previous key still verifies while that key stays in the list:

<!-- ds:block id=rotation-dzyj6rqc -->
**rotation** · [`examples/concepts-serving/main.go:222-237`](../examples/concepts-serving/main.go#L222-L237)

```go
func rotation(ctx context.Context, a *app, out io.Writer) error {
	before, err := signedurl.New(a.srv.URL+signedPrefix, []signedurl.Key{oldKey})
	if err != nil {
		return err
	}
	link, err := before.SignURL(ctx, "private", "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed with the old key %d\n", resp.StatusCode)
	return nil
}
```
<!-- /ds:block hash=6f20d1 -->

A URL has the form `<base>/<disk>/<escaped key>?m=GET&exp=<unix>&ct=…&cd=…&kid=…&sig=<hex>`. A PUT that stores HTTP headers adds `hcc`, `hcd` and `hce` (the stored `Cache-Control`, `Content-Disposition` and `Content-Encoding`). The signature is HMAC-SHA256 over the method, disk, key, expiry, content type and disposition, plus the three stored headers when any is set, each length-prefixed, so none can be changed or dropped without invalidating it. A URL without stored headers signs exactly as it did before they existed, so links issued before an upgrade keep working.

## API

| Name | What it is |
|---|---|
| `New(base, keys, options...)` | a `Signer` issuing URLs under `base` (an absolute http(s) URL without a query, where `fshttp.Signed` is mounted); `keys[0]` signs, every key verifies; `CodeInvalidArgument` for a bad base, no keys, a short secret, or an empty, non-URL-safe or repeated ID |
| `Signer` | implements `filesystem.URLSigner` and `fshttp.Verifier`; safe for concurrent use |
| `Signer.SignURL(ctx, disk, key, PresignOptions)` | a signed GET or PUT; options are checked with `filesystem.ValidatePresign` (expiry in `(0, MaxPresignExpiry]`); a PUT returns in `SignedRequest.Header` the content type and stored headers the client must send |
| `Signer.PinsHeaders()` | always true: implements `filesystem.HeaderPinner`, so a disk with this signer issues PUT URLs carrying `PresignOptions.Headers` when its driver stores headers |
| `Signer.RestrictsIP()` | always true: implements `filesystem.IPRestricter`, so a disk with this signer issues URLs carrying `PresignOptions.AllowedIP`; the range is signed, and `fshttp.Signed` answers 403 to a request from outside it (see `SignedOptions.TrustedProxies` for proxies) |
| `Signer.Verify(disk, key, method, query)` | the `Claims` of a valid request; `CodePermission` for a bad or missing signature, a wrong method, an unknown key ID or an expired URL, never saying which |
| `Claims` | what a verified URL authorises: `Disk`, `Key`, `Method`, `ExpiresAt`, `ContentType`, `ContentDisposition`, `Headers` (what a PUT must carry and store), `AllowedIP` (the `netip.Prefix` the request must come from; zero for any address) |
| `Key` | one secret and the ID that names it in URLs: `ID`, `Secret`. It never prints its secret: `String`, `GoString`, `Format` (every fmt verb, `%v`, `%+v` and `%#v` included, alone or inside a struct or slice) and `LogValue` (slog) show the ID and `[REDACTED]` |
| `Signer` printing | `String`, `GoString`, `Format` and `LogValue` show the base and the key IDs, never a secret, so logging a signer or a config holding one is safe |
| `MinKeyBytes` | 32, the shortest accepted secret |
| `Option` | configures a `Signer` |
| `WithClock(now)` | replaces `time.Now`, for tests |
| `ParamMethod`, `ParamExpires`, `ParamContentType`, `ParamDisposition`, `ParamKeyID`, `ParamSignature` | the query parameter names: `m`, `exp`, `ct`, `cd`, `kid`, `sig` |
| `ParamCacheControl`, `ParamStoredDisposition`, `ParamContentEncoding` | the stored-header parameter names of a PUT: `hcc`, `hcd`, `hce` |
| `ParamAllowedIP` | `ip`, the address range of a restricted URL |

## Rotating keys

Put the new key first and keep the previous one after it until every URL it signed has expired (at most `filesystem.MaxPresignExpiry`, seven days), then remove it. The key ID travels in the URL, so verification picks the right secret directly. See [Serving files](../docs/concepts/serving-files.md#step-4--rotating-keys).
